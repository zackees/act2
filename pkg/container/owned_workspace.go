package container

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// RestoreOwnedTree reconciles a Docker archive only into runner-owned state.
// Extraction never follows archive symlinks and completes before the old tree is replaced.
func (e *HostEnvironment) RestoreOwnedTree(ctx context.Context, destination string, archive io.Reader) error {
	if err := e.validateOwnedTree(destination); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(e.OwnedRoot, ".docker-action-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = extractOwnedArchive(ctx, stage, destination, archive); err != nil {
		return err
	}
	backup := stage + "-previous"
	if err = os.Rename(destination, backup); err != nil {
		return err
	}
	if err = os.Rename(stage, destination); err != nil {
		return errors.Join(err, os.Rename(backup, destination))
	}
	return os.RemoveAll(backup)
}

func (e *HostEnvironment) validateOwnedTree(destination string) error {
	if e.OwnedRoot == "" || (destination != e.Path && destination != e.ActPath) || filepath.Dir(destination) != e.OwnedRoot {
		return errors.New("refusing to reconcile an unowned workspace")
	}
	realRoot, err := filepath.EvalSymlinks(e.OwnedRoot)
	rootInfo, statErr := os.Lstat(e.OwnedRoot)
	if err != nil || statErr != nil || !rootInfo.IsDir() {
		return errors.New("owned workspace root must be a real directory")
	}
	realDestination, err := filepath.EvalSymlinks(destination)
	if err != nil || realDestination != filepath.Join(realRoot, filepath.Base(destination)) {
		return errors.New("owned workspace destination must be a real directory")
	}
	info, err := os.Stat(destination)
	if err != nil || !info.IsDir() {
		return errors.New("owned workspace must be a directory")
	}
	return nil
}

type archiveLink struct {
	name, target string
	hard         bool
}

type archiveDirectory struct {
	name     string
	mode     os.FileMode
	modified time.Time
}

type ownedArchiveExtractor struct {
	root        string
	links       []archiveLink
	directories []archiveDirectory
	seen        map[string]bool
	original    *os.Root
	ctx         context.Context
}

func extractOwnedArchive(ctx context.Context, root, original string, archive io.Reader) error {
	originalRoot, err := os.OpenRoot(original)
	if err != nil {
		return err
	}
	defer originalRoot.Close()
	reader := tar.NewReader(archive)
	extractor := ownedArchiveExtractor{root: root, original: originalRoot, ctx: ctx, seen: map[string]bool{}}
	var bytes int64
	for entries := 0; ; entries++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		// Bound both archive metadata and expanded payload; no allocation follows header.Size.
		if entries >= 1_000_000 || header.Size < 0 || header.Size > (64<<30)-bytes {
			return errors.New("Docker action archive exceeds workspace limits")
		}
		bytes += header.Size
		if err = extractor.entry(header, reader); err != nil {
			return err
		}
	}
	return extractor.finish(root, original)
}

func (e *ownedArchiveExtractor) finish(root, original string) error {
	// Symlinks are created last, so no archive entry can write through a symlink.
	for _, link := range e.links {
		if link.hard {
			if err := createOwnedHardlink(link); err != nil {
				return err
			}
		}
	}
	for _, link := range e.links {
		if link.hard {
			continue
		}
		if err := os.Symlink(link.target, link.name); err != nil {
			return err
		}
		if err := preserveUnchangedSymlink(root, original, link); err != nil {
			return err
		}
	}
	for i := len(e.directories) - 1; i >= 0; i-- {
		if err := os.Chmod(e.directories[i].name, e.directories[i].mode); err != nil {
			return err
		}
		modified, err := e.unchangedTime(e.directories[i].name, e.directories[i].modified, true)
		if err != nil {
			return err
		}
		if err := restoreOwnedTime(e.directories[i].name, modified); err != nil {
			return err
		}
	}
	return nil
}

func preserveUnchangedSymlink(stage, original string, link archiveLink) error {
	relative, err := filepath.Rel(stage, link.name)
	if err != nil {
		return err
	}
	old := filepath.Join(original, relative)
	parent, err := filepath.EvalSymlinks(filepath.Dir(old))
	realOriginal, originalErr := filepath.EvalSymlinks(original)
	if err != nil || originalErr != nil || (parent != realOriginal && !strings.HasPrefix(parent, realOriginal+string(filepath.Separator))) {
		return nil
	}
	target, err := os.Readlink(old)
	if err != nil || target != link.target {
		return nil
	}
	// Linking the unchanged symlink inode preserves its exact original metadata
	// without following the link or changing any file in the original workspace.
	if err = os.Remove(link.name); err != nil {
		return err
	}
	return os.Link(old, link.name)
}

func (e *ownedArchiveExtractor) entry(header *tar.Header, reader io.Reader) error {
	name := path.Clean(header.Name)
	mode := os.FileMode(header.Mode) & os.ModePerm
	if name == "." && header.Typeflag == tar.TypeDir {
		e.directories = append(e.directories, archiveDirectory{e.root, mode, header.ModTime})
		return nil
	}
	if !safeArchivePath(name) || strings.Contains(name, "\\") || e.seen[name] {
		return fmt.Errorf("invalid Docker action archive path %q", header.Name)
	}
	e.seen[name] = true
	destination := filepath.Join(e.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	switch header.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return err
		}
		e.directories = append(e.directories, archiveDirectory{destination, mode, header.ModTime})
	case tar.TypeReg:
		if err := extractOwnedFile(destination, mode, reader); err != nil {
			return err
		}
		modified, err := e.unchangedTime(destination, header.ModTime, false)
		if err != nil {
			return err
		}
		return restoreOwnedTime(destination, modified)
	case tar.TypeSymlink:
		if !safeArchiveSymlink(name, header.Linkname) {
			return fmt.Errorf("Docker action symlink escapes workspace: %q", name)
		}
		e.links = append(e.links, archiveLink{name: destination, target: header.Linkname})
	case tar.TypeLink:
		target := path.Clean(header.Linkname)
		if !safeArchivePath(target) || strings.Contains(target, "\\") {
			return fmt.Errorf("Docker action hardlink escapes workspace: %q", name)
		}
		e.links = append(e.links, archiveLink{name: destination, target: filepath.Join(e.root, filepath.FromSlash(target)), hard: true})
	default:
		return fmt.Errorf("unsupported Docker action archive entry: %q", name)
	}
	return nil
}

// Docker's archive endpoint drops subsecond timestamps. Only unchanged content
// and matching coarse metadata justify retaining the original exact timestamp.
func (e *ownedArchiveExtractor) unchangedTime(destination string, modified time.Time, directory bool) (time.Time, error) {
	if modified.Nanosecond() != 0 {
		return modified, nil
	}
	name, err := filepath.Rel(e.root, destination)
	if err != nil {
		return modified, err
	}
	original, err := e.original.Lstat(name)
	if err != nil || original.ModTime().Unix() != modified.Unix() || original.ModTime().Nanosecond() == 0 {
		return modified, nil
	}
	current, err := os.Lstat(destination)
	if err != nil {
		return modified, err
	}
	if current.Mode() != original.Mode() || current.Size() != original.Size() && !directory {
		return modified, nil
	}
	var same bool
	if directory {
		same, err = sameOwnedDirectory(e.ctx, e.original, name, destination)
	} else {
		same, err = sameOwnedFile(e.ctx, e.original, name, destination)
	}
	if same && err == nil {
		return original.ModTime(), nil
	}
	return modified, err
}

func sameOwnedDirectory(ctx context.Context, original *os.Root, name, destination string) (bool, error) {
	before, err := fs.ReadDir(original.FS(), filepath.ToSlash(name))
	if err != nil {
		return false, err
	}
	after, err := os.ReadDir(destination)
	if err != nil || len(before) != len(after) {
		return false, err
	}
	for i, entry := range before {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if entry.Name() != after[i].Name() || entry.Type() != after[i].Type() {
			return false, nil
		}
	}
	return true, nil
}

func sameOwnedFile(ctx context.Context, original *os.Root, name, destination string) (bool, error) {
	before, err := original.Open(name)
	if err != nil {
		return false, err
	}
	defer before.Close()
	after, err := os.Open(destination)
	if err != nil {
		return false, err
	}
	defer after.Close()
	beforeHash, afterHash := sha256.New(), sha256.New()
	_, beforeErr := io.Copy(&ownedArchiveWriter{ctx: ctx, writer: beforeHash, remaining: ownedArchiveByteLimit}, before)
	_, afterErr := io.Copy(&ownedArchiveWriter{ctx: ctx, writer: afterHash, remaining: ownedArchiveByteLimit}, after)
	err = errors.Join(beforeErr, afterErr)
	return string(beforeHash.Sum(nil)) == string(afterHash.Sum(nil)), err
}

func restoreOwnedTime(destination string, modified time.Time) error {
	if modified.IsZero() {
		return nil
	}
	return os.Chtimes(destination, modified, modified)
}

func createOwnedHardlink(link archiveLink) error {
	info, err := os.Lstat(link.target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("Docker action hardlink target must be a regular file")
	}
	return os.Link(link.target, link.name)
}

func safeArchiveSymlink(name, target string) bool {
	// Validate the resolved archive-relative target; no filesystem access follows it.
	if path.IsAbs(target) || strings.Contains(target, "\\") {
		return false
	}
	resolved := path.Clean(path.Dir(name) + "/" + target)
	return safeArchivePath(resolved)
}

func safeArchivePath(name string) bool {
	return name != "." && name != ".." && !path.IsAbs(name) && !strings.HasPrefix(name, "../")
}

func extractOwnedFile(destination string, mode os.FileMode, reader io.Reader) error {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, reader)
	return errors.Join(copyErr, file.Close(), os.Chmod(destination, mode))
}
