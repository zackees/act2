//go:build linux

package artifactcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const toolStageOwnershipDirectory = ".tool-publication-ownership-v1"

type toolStageOwnership struct {
	SchemaVersion int       `json:"schema_version"`
	Relative      string    `json:"relative"`
	Device        uint64    `json:"device"`
	Inode         uint64    `json:"inode"`
	MountID       uint64    `json:"mount_id"`
	RootDevice    uint64    `json:"root_device,omitempty"`
	RootInode     uint64    `json:"root_inode,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Caller holds the original catalog writer. Ownership files never change the
// read-only coordination descriptor or initialize missing catalog state.
func createOwnedToolStage(_ transferLease, root, parent, prefix string) (string, error) {
	validParent := (parent == root && prefix == ".tool-stage-") || (parent == filepath.Join(root, toolGenerationDirectory) && prefix == ".tool-generation-stage-")
	if !validParent {
		return "", fmt.Errorf("stage namespace is invalid")
	}
	mount, err := toolMountID(root)
	if err != nil {
		return "", err
	}
	parentMount, err := toolMountID(parent)
	if err != nil || mount != parentMount {
		return "", fmt.Errorf("stage parent crosses mount boundary")
	}
	directory, err := prepareToolStageOwnershipDirectory(root)
	if err != nil {
		return "", err
	}
	rows, err := loadToolStageOwnership(nil, root)
	if err != nil {
		return "", err
	}
	if len(rows) >= 10000 {
		return "", fmt.Errorf("stage ownership ledger at capacity")
	}
	return allocateOwnedToolStage(root, parent, prefix, directory, mount)
}

func toolStageRecordName(relative string) string {
	digest := sha256.Sum256([]byte(relative))
	return hex.EncodeToString(digest[:]) + ".json"
}

func prepareToolStageOwnershipDirectory(root string) (string, error) {
	directory := filepath.Join(root, toolStageOwnershipDirectory)
	if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("stage ownership namespace invalid")
	}
	expected, err := toolMountID(root)
	if err != nil {
		return "", err
	}
	actual, err := toolMountID(directory)
	if err != nil || actual != expected {
		return "", fmt.Errorf("stage ownership namespace crosses mount boundary")
	}
	if err := syncToolDirectory(root); err != nil {
		return "", err
	}
	return directory, nil
}

func loadToolStageOwnership(_ transferLease, root string) ([]toolStageOwnership, error) {
	directory := filepath.Join(root, toolStageOwnershipDirectory)
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("stage ownership namespace invalid")
	}
	if err := recoverPendingToolStageRecord(root, directory); err != nil {
		return nil, err
	}
	handle, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	entries, err := handle.ReadDir(10001)
	closeErr := handle.Close()
	if err != nil && err != io.EOF {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > 10000 {
		return nil, fmt.Errorf("stage ownership inventory exceeds bound")
	}
	rows := make([]toolStageOwnership, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024 {
			return nil, fmt.Errorf("invalid stage ownership record")
		}
		// #nosec G703 -- Bounded verified regular ownership record under nonsymlink catalog-controlled namespace.
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var row toolStageOwnership
		if err := json.Unmarshal(data, &row); err != nil {
			return nil, err
		}
		if err := validateToolStageRecord(row, data, entry.Name()); err != nil {
			return nil, err
		}

		rows = append(rows, row)
	}
	return rows, nil
}

func validToolStageRelative(relative string) bool {
	if relative != filepath.Clean(relative) || filepath.IsAbs(relative) {
		return false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) == 1 {
		return strings.HasPrefix(parts[0], ".tool-stage-") && len(parts[0]) > len(".tool-stage-")
	}
	return len(parts) == 2 && parts[0] == toolGenerationDirectory && strings.HasPrefix(parts[1], ".tool-generation-stage-") && len(parts[1]) > len(".tool-generation-stage-")
}

func forgetOwnedToolStage(_ transferLease, root, relative string) error {
	directory := filepath.Join(root, toolStageOwnershipDirectory)
	// #nosec G703 -- Fixed ownership namespace and SHA-256 leaf derived from validated typed relative stage record under original catalog exclusion.
	if err := os.Remove(filepath.Join(directory, toolStageRecordName(relative))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncToolDirectory(directory)
}

func validateToolStageRecord(row toolStageOwnership, data []byte, name string) error {
	canonical, err := json.Marshal(row)
	if err != nil || !bytes.Equal(data, canonical) || !validToolStageSchema(row) || row.CreatedAt.IsZero() || !validToolStageRelative(row.Relative) || name != toolStageRecordName(row.Relative) {
		return fmt.Errorf("invalid stage ownership record")
	}
	return nil
}
