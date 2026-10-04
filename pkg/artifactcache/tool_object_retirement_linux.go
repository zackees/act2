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

func retireToolObject(ctx context.Context, root, id string, maxBytes int64, maxGenerations int) error {
	guard := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "retirement", ObjectID: id}}}
	if _, err := validateToolGenerationSpec(root, guard, maxBytes); err != nil {
		return err
	}
	if maxGenerations <= 0 || maxGenerations > 10000 {
		return fmt.Errorf("object retirement requires generation bound 1..10000")
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
	selection, err := readToolGenerationSelection(root)
	if err != nil {
		return err
	}
	if _, err := verifySelectedToolGeneration(ctx, root, selection, maxBytes); err != nil {
		return err
	}
	if err := verifyToolObjectUnreferenced(ctx, root, id, maxBytes, maxGenerations); err != nil {
		return err
	}
	object := filepath.Join(root, id)
	if _, err := loadToolGenerationObject(ctx, object, id, maxBytes); err != nil {
		return err
	}
	if err := verifyToolObjectRetirementLayout(ctx, root, object); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// #nosec G703 -- Verified closed object under canonical root and validated SHA-256 ID; original catalog excludes publication/admission.
	if err := os.RemoveAll(object); err != nil {
		return fmt.Errorf("object retirement incomplete: %w", err)
	}
	return syncToolDirectory(root)
}

func verifyToolObjectUnreferenced(ctx context.Context, root, id string, maxBytes int64, bound int) error {
	namespace := filepath.Join(root, toolGenerationDirectory)
	info, err := os.Lstat(namespace)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("generation reference namespace invalid")
	}
	handle, err := os.Open(namespace)
	if err != nil {
		return err
	}
	defer handle.Close()
	entries, err := handle.ReadDir(bound + 1)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > bound {
		return fmt.Errorf("generation reference inventory exceeds bound")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		decoded, err := hex.DecodeString(entry.Name())
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != entry.Name() || !entry.IsDir() {
			return fmt.Errorf("unknown generation reference state preserved")
		}
		manifest, _, err := readToolGenerationManifest(filepath.Join(namespace, entry.Name()), entry.Name())
		if err != nil {
			return err
		}
		if _, err := validateToolGenerationSpec(root, ToolGenerationSpec{SchemaVersion: manifest.SchemaVersion, Installs: manifest.Installs}, maxBytes); err != nil {
			return err
		}
		for _, install := range manifest.Installs {
			if install.ObjectID == id {
				return fmt.Errorf("object is referenced by retained generation")
			}
		}
	}
	return nil
}

func verifyToolObjectRetirementLayout(ctx context.Context, root, object string) error {
	usage := auditToolStoreUsageLocked(ctx, root, 1000000)
	if usage.Partial {
		return fmt.Errorf("object retirement inventory incomplete: %s", usage.Error)
	}
	handle, err := os.Open(object)
	if err != nil {
		return err
	}
	entries, err := handle.ReadDir(3)
	closeErr := handle.Close()
	if err != nil && err != io.EOF {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) != 2 {
		return fmt.Errorf("unknown object control entries preserved")
	}
	for _, entry := range entries {
		if entry.Name() != "tree" && entry.Name() != "manifest.json" {
			return fmt.Errorf("unknown object control entries preserved")
		}
	}
	return verifyToolRetirementMounts(ctx, root, object, toolMountID)
}
