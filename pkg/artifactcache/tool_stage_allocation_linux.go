//go:build linux

package artifactcache

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const toolStagePendingAllocation = ".creating"

// The one reserved directory remains empty until durable ownership publication
// and rename. A crash before registration cannot leave an unowned payload stage.
func allocateOwnedToolStage(root, parent, prefix, directory string, mount uint64) (string, error) {
	scratch := filepath.Join(directory, toolStagePendingAllocation)
	if err := os.Mkdir(scratch, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(scratch)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return "", fmt.Errorf("stage allocation identity unavailable")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	stage := filepath.Join(parent, prefix+hex.EncodeToString(nonce))
	relative, err := filepath.Rel(root, stage)
	if err != nil {
		return "", err
	}
	row, err := newToolStageOwnership(root, relative, stat, mount)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(row)
	if err != nil {
		return "", err
	}
	if err := writeToolStageOwnership(directory, row, data); err != nil {
		return "", err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, scratch, unix.AT_FDCWD, stage, unix.RENAME_NOREPLACE); err != nil {
		return "", err
	}
	if err := syncToolDirectory(parent); err != nil {
		return stage, err
	}
	return stage, syncToolDirectory(directory)
}

func recoverEmptyToolStageAllocation(directory string, rootMount uint64) error {
	scratch := filepath.Join(directory, toolStagePendingAllocation)
	info, err := os.Lstat(scratch)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("unknown stage allocation preserved")
	}
	mount, err := toolMountID(scratch)
	if err != nil || mount != rootMount {
		return fmt.Errorf("stage allocation crosses current root mount boundary")
	}
	handle, err := os.Open(scratch)
	if err != nil {
		return err
	}
	entries, readErr := handle.ReadDir(1)
	closeErr := handle.Close()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) != 0 {
		return fmt.Errorf("nonempty stage allocation preserved")
	}
	// #nosec G703 -- Exact reserved empty control directory verified nonsymlink, mode0700 and on current root mount under original catalog exclusion; nonrecursive removal preserves unexpected entries.
	if err := os.Remove(scratch); err != nil {
		return err
	}
	return syncToolDirectory(directory)
}
