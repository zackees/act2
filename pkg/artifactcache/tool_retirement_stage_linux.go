//go:build linux

package artifactcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Caller has verified the closed publication and holds the original catalog
// writer, plus the generation writer when retiring a generation. The ledger is
// durable before rename: either the publication is intact, or the original inode
// is in a registered stage whose cleanup does not depend on its manifest.
func retireToolPublicationLocked(ctx context.Context, root, publication string) error {
	row, err := registerToolRetirementStage(root, publication)
	if err != nil {
		return err
	}
	stage := filepath.Join(root, row.Relative)
	if err := ctx.Err(); err != nil {
		return err
	}
	// #nosec G703 -- Verified publication and registered random stage under the same canonical namespace; catalog exclusion and NOREPLACE preserve replacements.
	if err := unix.Renameat2(unix.AT_FDCWD, publication, unix.AT_FDCWD, stage, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("retirement stage rename: %w", err)
	}
	if err := syncToolDirectory(filepath.Dir(publication)); err != nil {
		return err
	}
	_, err = retireOwnedToolStage(ctx, nil, root, row)
	return err
}

func registerToolRetirementStage(root, publication string) (toolStageOwnership, error) {
	var row toolStageOwnership
	parent := filepath.Dir(publication)
	prefix := ".tool-stage-retired-"
	if parent == filepath.Join(root, toolGenerationDirectory) {
		prefix = ".tool-generation-stage-retired-"
	} else if parent != root {
		return row, fmt.Errorf("retirement publication namespace invalid")
	}
	directory, err := prepareToolStageOwnershipDirectory(root)
	if err != nil {
		return row, err
	}
	rows, err := loadToolStageOwnership(nil, root)
	if err != nil {
		return row, err
	}
	if len(rows) >= 10000 {
		return row, fmt.Errorf("stage ownership ledger at capacity")
	}
	info, err := os.Lstat(publication)
	if err != nil {
		return row, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return row, fmt.Errorf("retirement publication inode unavailable")
	}
	mount, err := toolMountID(publication)
	if err != nil {
		return row, err
	}
	expected, err := toolMountID(root)
	if err != nil || mount != expected {
		return row, fmt.Errorf("retirement publication crosses mount boundary")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return row, err
	}
	relative, err := filepath.Rel(root, filepath.Join(parent, prefix+hex.EncodeToString(nonce)))
	if err != nil {
		return row, err
	}
	row, err = newToolStageOwnership(root, relative, stat, mount)
	if err != nil {
		return row, err
	}
	if !validToolStageRelative(relative) {
		return row, fmt.Errorf("retirement stage namespace invalid")
	}
	data, err := json.Marshal(row)
	if err != nil {
		return row, err
	}
	return row, writeToolStageOwnership(directory, row, data)
}
