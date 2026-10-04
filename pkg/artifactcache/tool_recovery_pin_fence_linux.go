//go:build linux

package artifactcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const toolStoreRecoveryMagic = "bosn-tool-snapshots-v2\n"

// Older runtimes validate the marker under this same original catalog lock.
// Fence them before promising durable protection they cannot enforce. The
// payloads, selection and both original coordination inodes stay unchanged.
func fenceToolRecoveryStore(ctx context.Context, catalog transferLease, root string, syncParent func(string) error) (pending string, err error) {
	marker := filepath.Join(root, toolStoreMarker)
	data, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	if string(data) == toolStoreRecoveryMagic {
		return "", syncParent(root)
	}
	if string(data) != toolStoreMagic {
		return "", fmt.Errorf("unsupported recovery store epoch")
	}
	stage, err := createOwnedToolStage(catalog, root, root, ".tool-stage-")
	pending = stage
	if err != nil {
		return pending, err
	}
	defer func() {
		cleanupErr := cleanupOwnedToolStage(ctx, catalog, root, stage)
		if cleanupErr == nil {
			pending = ""
		}
		err = errors.Join(err, cleanupErr)
	}()
	path := filepath.Join(stage, "marker")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return pending, err
	}
	_, writeErr := file.WriteString(toolStoreRecoveryMagic)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return pending, err
	}
	if err := syncToolDirectory(stage); err != nil {
		return pending, err
	}
	if err := ctx.Err(); err != nil {
		return pending, err
	}
	if err := os.Rename(path, marker); err != nil {
		return pending, err
	}
	return pending, syncParent(root)
}
