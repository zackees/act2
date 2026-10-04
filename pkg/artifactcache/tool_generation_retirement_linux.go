//go:build linux

package artifactcache

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func retireToolGeneration(ctx context.Context, root, id string, maxBytes int64) error {
	guard := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "retirement", ObjectID: id}}}
	if _, err := validateToolGenerationSpec(root, guard, maxBytes); err != nil {
		return err
	}
	decoded, _ := hex.DecodeString(id)
	id = hex.EncodeToString(decoded)
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		return err
	}
	defer catalog.Close()
	return retireToolGenerationLocked(ctx, root, id, maxBytes)
}

// Caller holds the original catalog writer throughout retirement.
func retireToolGenerationLocked(ctx context.Context, root, id string, maxBytes int64) error {
	selection, err := readToolGenerationSelection(root)
	if err != nil {
		return err
	}
	if _, err := verifySelectedToolGeneration(ctx, root, selection, maxBytes); err != nil {
		return err
	}
	if selection.ID == id {
		return fmt.Errorf("selected tool generation cannot be retired")
	}
	generation := filepath.Join(root, toolGenerationDirectory, id)
	manifest, data, err := readToolGenerationManifest(generation, id)
	if err != nil {
		return err
	}
	if _, err := validateToolGenerationSpec(root, ToolGenerationSpec{SchemaVersion: manifest.SchemaVersion, Installs: manifest.Installs}, maxBytes); err != nil {
		return err
	}
	if err := verifyToolGeneration(ctx, generation, data, manifest.Tree, maxBytes); err != nil {
		return err
	}
	if err := verifyToolRetirementLayout(ctx, root, generation); err != nil {
		return err
	}
	writer, err := openTransferLease(filepath.Join(generation, toolGenerationReaderLock), false, 100*time.Millisecond)
	if err != nil {
		return err
	}
	defer writer.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Both original locks remain held through unlink and parent-directory sync.
	// #nosec G703 -- Verified nonsymlink generation beneath canonical store, validated SHA-256 ID, closed tree and fixed control leaves.
	if err := os.RemoveAll(generation); err != nil {
		return fmt.Errorf("generation retirement incomplete: %w", err)
	}
	return syncToolDirectory(filepath.Join(root, toolGenerationDirectory))
}

func verifyToolRetirementLayout(ctx context.Context, root, generation string) error {
	// Verify the entire store's bounded metadata inventory before deletion. This
	// refuses mounted filesystem crossings, including unexpected nested mounts.
	usage := auditToolStoreUsageLocked(ctx, root, 1000000)
	if usage.Partial {
		return fmt.Errorf("retirement inventory incomplete: %s", usage.Error)
	}
	// Only the canonical publication layout is eligible. Unknown root entries are
	// preserved rather than recursively deleted with an otherwise valid payload.
	directory, err := os.Open(generation)
	if err != nil {
		return err
	}
	entries, err := directory.ReadDir(4)
	closeErr := directory.Close()
	if err != nil && err != io.EOF {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) != 3 {
		return fmt.Errorf("generation contains unknown control entries")
	}
	for _, entry := range entries {
		if entry.Name() != "tree" && entry.Name() != "manifest.json" && entry.Name() != toolGenerationReaderLock {
			return fmt.Errorf("generation contains unknown control entries")
		}
	}
	return verifyToolRetirementMounts(ctx, root, generation, toolMountID)
}
