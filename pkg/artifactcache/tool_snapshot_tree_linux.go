//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

func scanToolTree(ctx context.Context, root, completion string, maxBytes int64) (toolManifest, int64, error) {
	manifest := toolManifest{SchemaVersion: 1, Completion: completion}
	var total int64
	err := walkToolTree(ctx, root, 0, func(path string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(manifest.Entries) >= 100000 {
			return fmt.Errorf("tool object entry bound exceeded")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !utf8.ValidString(relative) || len(relative) > 4096 {
			return fmt.Errorf("tool entry path is invalid or oversized")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		row, err := toolEntryMetadata(relative, info)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			row.Kind = "directory"
		case info.Mode().IsRegular():
			row.Kind, row.Bytes = "file", info.Size()
			if row.Bytes < 0 || row.Bytes > maxBytes-total || row.Bytes == int64(^uint64(0)>>1) {
				return fmt.Errorf("tool object exceeds byte bound or has invalid size")
			}
			row.Digest, err = archiveChecksum(ctx, path, row.Bytes)
			if err != nil {
				return err
			}
			total += row.Bytes
		case info.Mode()&os.ModeSymlink != 0:
			row.Kind, row.Mtime = "symlink", 0
			row.Link, err = boundedToolSymlink(root, path, relative)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("tool snapshot rejects special filesystem objects")
		}
		manifest.Entries = append(manifest.Entries, row)
		return nil
	})
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	if err == nil && total == 0 {
		err = fmt.Errorf("completed tool object has no payload bytes")
	}
	return manifest, total, err
}

// Page directory reads before applying the global entry bound. Depth limits
// both recursion and open descriptors; no symlink is followed during the walk.
func walkToolTree(ctx context.Context, path string, depth int, visit func(string, fs.DirEntry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := visit(path, fs.FileInfoToDirEntry(info)); err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if depth >= 64 {
		return fmt.Errorf("tool inventory depth limit exceeded")
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			if err := walkToolTree(ctx, filepath.Join(path, entry.Name()), depth+1, visit); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func boundedToolSymlink(root, path, relative string) (string, error) {
	link, err := os.Readlink(path)
	if err != nil || !utf8.ValidString(link) || len(link) > 4096 {
		return "", fmt.Errorf("tool symlink is invalid or oversized")
	}
	// Relative targets must remain inside the closed object. Reject all
	// absolute links: even a link into source would retain mutable source data.
	resolved := filepath.Clean(filepath.Join(filepath.Dir(relative), link))
	if filepath.IsAbs(link) || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", fmt.Errorf("tool symlink escapes the closed install")
	}
	// Resolve the actual link, without first cleaning its target. A directory
	// symlink followed by .. can escape despite a lexically confined target.
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("tool symlink is unresolved: %w", err)
	}
	inside, err := filepath.Rel(root, physical)
	if err != nil || inside == ".." || strings.HasPrefix(inside, "../") {
		return "", fmt.Errorf("tool symlink resolves outside the closed install")
	}
	return link, nil
}

func toolEntryMetadata(relative string, info fs.FileInfo) (toolEntry, error) {
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return toolEntry{}, fmt.Errorf("unsupported tool ownership or special permissions")
	}
	return toolEntry{Path: relative, Mode: uint32(info.Mode().Perm()), UID: int(metadata.Uid), GID: int(metadata.Gid), Mtime: info.ModTime().UnixNano()}, nil
}

func fillToolSnapshot(ctx context.Context, source, stage string, manifest toolManifest, expected []byte, maxBytes int64) error {
	payload := filepath.Join(stage, "tree")
	if err := os.Mkdir(payload, 0755); err != nil {
		return err
	}
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Path == "." {
			continue
		}
		target := filepath.Join(payload, entry.Path)
		if err := copyToolEntry(ctx, filepath.Join(source, entry.Path), target, entry); err != nil {
			return err
		}
	}
	for index := len(manifest.Entries) - 1; index >= 0; index-- {
		if err := applyToolMetadata(filepath.Join(payload, manifest.Entries[index].Path), manifest.Entries[index]); err != nil {
			return err
		}
	}
	completion, err := toolSourceCompletion(source)
	if err != nil || completion != manifest.Completion {
		return fmt.Errorf("tool completion changed during snapshot")
	}
	for _, tree := range []string{source, payload} {
		audit, _, err := scanToolTree(ctx, tree, completion, maxBytes)
		if err != nil {
			return err
		}
		data, err := json.Marshal(audit)
		if err != nil || !bytes.Equal(data, expected) {
			return fmt.Errorf("tool source or copied tree changed during snapshot")
		}
	}
	file, err := os.OpenFile(filepath.Join(stage, "manifest.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(expected)
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return syncToolDirectory(stage)
}

func copyToolEntry(ctx context.Context, source, destination string, entry toolEntry) error {
	switch entry.Kind {
	case "directory":
		return os.Mkdir(destination, 0755)
	case "symlink":
		return os.Symlink(entry.Link, destination)
	case "file":
		digest, err := copyVerifiedArchive(ctx, source, destination, entry.Bytes)
		if err != nil {
			return err
		}
		if digest != entry.Digest {
			return fmt.Errorf("tool file changed from planned fingerprint")
		}
		return nil
	default:
		return fmt.Errorf("unsupported tool entry")
	}
}

func applyToolMetadata(path string, entry toolEntry) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	ownership, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("tool ownership unknown")
	}
	if int(ownership.Uid) != entry.UID || int(ownership.Gid) != entry.GID {
		if err := os.Lchown(path, entry.UID, entry.GID); err != nil {
			return err
		}
	}
	if entry.Kind == "symlink" {
		return nil
	}
	if err := os.Chmod(path, os.FileMode(entry.Mode)); err != nil {
		return err
	}
	stamp := time.Unix(0, entry.Mtime)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		return err
	}
	return syncToolDirectory(path)
}
