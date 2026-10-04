//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

const toolManifestLimit = 16 * 1024 * 1024
const toolStoreMarker = ".tool-snapshot-store-v1"
const toolStoreMagic = "bosn-tool-snapshots-v1\n"
const toolStoreLock = ".tool-snapshot-publish-v1.bolt"

type toolManifest struct {
	SchemaVersion int         `json:"schema_version"`
	Completion    string      `json:"completion"`
	Entries       []toolEntry `json:"entries"`
}

type toolEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Mtime  int64  `json:"mtime"`
	Bytes  int64  `json:"bytes"`
	Digest string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
}

func publishToolSnapshot(ctx context.Context, source, root string, maxBytes int64) (report ToolSnapshotReport) {
	report = ToolSnapshotReport{SchemaVersion: 1, Source: source}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	if err := validateToolSnapshotPaths(source, root, maxBytes); err != nil {
		report.fail(err)
		return report
	}
	completion, err := toolSourceCompletion(source)
	if err != nil {
		report.fail(err)
		return report
	}
	manifest, total, err := scanToolTree(ctx, source, completion, maxBytes)
	if err != nil {
		report.fail(err)
		return report
	}
	data, err := json.Marshal(manifest)
	if err != nil || len(data) > toolManifestLimit {
		report.fail(fmt.Errorf("tool manifest invalid or exceeds bound"))
		return report
	}
	digest := sha256.Sum256(data)
	report.ID = hex.EncodeToString(digest[:])
	report.Destination = filepath.Join(root, report.ID)
	report.Completion, report.Bytes = completion, &total
	count := len(manifest.Entries)
	report.Entries = &count
	lease, err := prepareToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	defer lease.Close()
	if _, err = os.Lstat(report.Destination); err == nil {
		if err = verifyToolObject(ctx, report.Destination, data, completion, maxBytes); err != nil {
			report.fail(err)
			return report
		}
		report.Published, report.Reused = true, true
		if err = syncToolDirectory(root); err != nil {
			report.fail(err)
		}
		return report
	} else if !os.IsNotExist(err) {
		report.fail(err)
		return report
	}
	stage, err := os.MkdirTemp(root, ".tool-stage-")
	if err != nil {
		report.fail(err)
		return report
	}
	report.PendingStage = stage
	defer func() {
		if report.PendingStage != "" {
			if err := os.RemoveAll(stage); err != nil {
				report.fail(fmt.Errorf("tool stage cleanup failed: %w", err))
			} else {
				report.PendingStage = ""
			}
		}
	}()
	if err = fillToolSnapshot(ctx, source, stage, manifest, data, maxBytes); err != nil {
		report.fail(err)
		return report
	}
	if err = ctx.Err(); err != nil {
		report.fail(err)
		return report
	}
	// Atomic no-replace preserves even an unexpected existing destination.
	if err = unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, report.Destination, unix.RENAME_NOREPLACE); err != nil {
		report.fail(err)
		return report
	}
	report.Published, report.PendingStage = true, ""
	if err = syncToolDirectory(root); err != nil {
		report.fail(err)
	}
	return report
}

func validateToolSnapshotPaths(source, root string, maxBytes int64) error {
	if maxBytes <= 0 || len(source) > 4096 || len(root) > 4096 || !filepath.IsAbs(source) || !filepath.IsAbs(root) ||
		filepath.Clean(source) != source || filepath.Clean(root) != root || source == root ||
		strings.HasPrefix(source, root+string(os.PathSeparator)) || strings.HasPrefix(root, source+string(os.PathSeparator)) {
		return fmt.Errorf("tool snapshot requires distinct canonical absolute paths and positive byte limit")
	}
	for _, path := range []string{source, filepath.Dir(root)} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path {
			return fmt.Errorf("tool snapshot path has missing or aliased ancestry")
		}
	}
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("tool source is not a directory")
	}
	if info, err := os.Lstat(root); err == nil && !info.IsDir() || err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("tool object root is invalid")
	}
	return nil
}

func toolSourceCompletion(source string) (string, error) {
	for _, marker := range []struct{ path, style string }{{filepath.Join(source, ".complete"), "inner-v1"}, {source + ".complete", "sibling-v1"}} {
		info, err := os.Lstat(marker.path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("tool completion marker is not a regular file")
			}
			return marker.style, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("tool source has no completed install marker")
}

func prepareToolSnapshotStore(root string) (transferLease, error) {
	if err := os.Mkdir(root, 0755); err != nil && !os.IsExist(err) {
		return nil, err
	}
	marker := filepath.Join(root, toolStoreMarker)
	if _, err := os.Lstat(marker); os.IsNotExist(err) {
		if err := verifyToolStoreInitialContents(root); err != nil {
			return nil, err
		}
	}
	lockPath := filepath.Join(root, toolStoreLock)
	if _, err := os.Lstat(lockPath); os.IsNotExist(err) {
		// Initialization creates the coordination database. Later opens use the
		// existing read-only descriptor and cannot silently recreate it.
		db, err := bbolt.Open(lockPath, 0600, &bbolt.Options{Timeout: 100 * time.Millisecond})
		if err != nil {
			return nil, err
		}
		if err := db.Close(); err != nil {
			return nil, err
		}
	}
	lease, err := openTransferLease(lockPath, false, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if err = ensureToolStoreMarker(marker); err == nil {
		err = syncToolDirectory(filepath.Dir(root))
	}
	if err != nil {
		_ = lease.Close()
		return nil, err
	}
	return lease, nil
}

func verifyToolStoreInitialContents(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Only the single coordination file may precede the marker. Reading two
	// entries is enough to reject an unknown directory regardless of its width.
	entries, err := dir.ReadDir(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != toolStoreLock {
			return fmt.Errorf("tool root is not a recognized empty or participating store")
		}
	}
	return nil
}

func ensureToolStoreMarker(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Size() != int64(len(toolStoreMagic)) {
			return fmt.Errorf("unsupported tool store marker")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != toolStoreMagic {
			return fmt.Errorf("invalid tool store marker")
		}
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(toolStoreMagic)
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func verifyToolObject(ctx context.Context, object string, expected []byte, completion string, maxBytes int64) error {
	info, err := os.Lstat(object)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("tool object is not a closed directory")
	}
	manifest := filepath.Join(object, "manifest.json")
	info, err = os.Lstat(manifest)
	if err != nil || !info.Mode().IsRegular() || info.Size() > toolManifestLimit {
		return fmt.Errorf("tool object manifest is invalid or oversized")
	}
	data, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(data, expected) {
		return fmt.Errorf("tool object manifest differs; existing object preserved")
	}
	audit, _, err := scanToolTree(ctx, filepath.Join(object, "tree"), completion, maxBytes)
	if err != nil {
		return err
	}
	actual, err := json.Marshal(audit)
	if err != nil || !bytes.Equal(actual, expected) {
		return fmt.Errorf("tool object payload differs; existing object preserved")
	}
	return nil
}

func syncToolDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
