//go:build linux

package artifactcache

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const toolStagePendingRecord = ".pending"

func newToolStageOwnership(root, relative string, stat *syscall.Stat_t, mount uint64) (toolStageOwnership, error) {
	var row toolStageOwnership
	info, err := os.Lstat(root)
	if err != nil {
		return row, err
	}
	rootStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return row, fmt.Errorf("stage root inode unavailable")
	}
	rootMount, err := toolMountID(root)
	if err != nil || rootMount != mount {
		return row, fmt.Errorf("stage crosses current root mount boundary")
	}
	return toolStageOwnership{SchemaVersion: 2, Relative: relative, Device: stat.Dev, Inode: stat.Ino, MountID: mount, RootDevice: rootStat.Dev, RootInode: rootStat.Ino, CreatedAt: time.Now().UTC()}, nil
}

func validToolStageSchema(row toolStageOwnership) bool {
	return (row.SchemaVersion == 1 && row.RootDevice == 0 && row.RootInode == 0) || (row.SchemaVersion == 2 && row.RootInode != 0)
}

func verifyToolStageRoot(root, stage string, row toolStageOwnership) error {
	mount, err := toolMountID(stage)
	if err != nil {
		return err
	}
	if row.SchemaVersion == 1 {
		if mount != row.MountID {
			return fmt.Errorf("legacy stage mount identity changed; durable root proof unavailable")
		}
		return nil
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Dev != row.RootDevice || stat.Ino != row.RootInode {
		return fmt.Errorf("stage root identity changed; replacement preserved")
	}
	currentMount, err := toolMountID(root)
	if err != nil || currentMount != mount {
		return fmt.Errorf("stage crosses current root mount boundary")
	}
	return nil
}

// The original catalog serializes this one reserved scratch leaf. Partial JSON
// never appears under the hashed authority filename. On interruption, .pending
// has no publication authority and is recoverable independently of its contents.
func writeToolStageOwnership(directory string, row toolStageOwnership, data []byte) error {
	pending := filepath.Join(directory, toolStagePendingRecord)
	// #nosec G703 -- Fixed reserved scratch leaf in verified private ownership namespace; exclusive creation under original catalog writer.
	file, err := os.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	final := filepath.Join(directory, toolStageRecordName(row.Relative))
	if err := unix.Renameat2(unix.AT_FDCWD, pending, unix.AT_FDCWD, final, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return syncToolDirectory(directory)
}

func recoverPendingToolStageRecord(root, directory string) error {
	rootMount, err := toolMountID(root)
	if err != nil {
		return err
	}
	directoryMount, err := toolMountID(directory)
	if err != nil || directoryMount != rootMount {
		return fmt.Errorf("stage ownership namespace crosses current root mount boundary")
	}
	if err := recoverEmptyToolStageAllocation(directory, rootMount); err != nil {
		return err
	}
	pending := filepath.Join(directory, toolStagePendingRecord)
	info, err := os.Lstat(pending)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1024 {
		return fmt.Errorf("unknown pending stage record preserved")
	}
	pendingMount, err := toolMountID(pending)
	if err != nil || pendingMount != rootMount {
		return fmt.Errorf("pending stage record crosses current root mount boundary")
	}
	// #nosec G703 -- Reserved non-authoritative scratch leaf verified regular, bounded and on current root mount under catalog exclusion.
	if err := os.Remove(pending); err != nil {
		return err
	}
	return syncToolDirectory(directory)
}
