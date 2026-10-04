//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const toolGenerationCurrent = ".tool-current-v1.json"

func updateToolGeneration(ctx context.Context, root string, updates ToolGenerationSpec, maxBytes int64, initialize bool) (report ToolGenerationUpdateReport) {
	return updateToolGenerationWithSelectionSync(ctx, root, updates, maxBytes, initialize, syncToolDirectory)
}

func updateToolGenerationWithSelectionSync(ctx context.Context, root string, updates ToolGenerationSpec, maxBytes int64, initialize bool, syncSelection func(string) error) (report ToolGenerationUpdateReport) {
	report.SchemaVersion = 1
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	if _, err := validateToolGenerationSpec(root, updates, maxBytes); err != nil {
		report.fail(err)
		return report
	}
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	defer catalog.Close()
	selection, err := readToolGenerationSelection(root)
	var installs []ToolGenerationInstall
	if err == nil {
		if initialize {
			report.fail(fmt.Errorf("tool generation selection already exists; use ordinary update"))
			return report
		}
		manifest, verifyErr := verifySelectedToolGeneration(ctx, root, selection, maxBytes)
		if verifyErr != nil {
			report.fail(verifyErr)
			return report
		}
		installs = manifest.Installs
	} else if !os.IsNotExist(err) || !initialize {
		report.fail(err)
		return report
	}
	merged := make(map[string]ToolGenerationInstall, len(installs)+len(updates.Installs))
	for _, install := range installs {
		merged[install.Path] = install
	}
	for _, update := range updates.Installs {
		merged[update.Path] = update
	}
	spec := ToolGenerationSpec{SchemaVersion: 1, Installs: make([]ToolGenerationInstall, 0, len(merged))}
	for _, install := range merged {
		spec.Installs = append(spec.Installs, install)
	}
	installs, err = validateToolGenerationSpec(root, spec, maxBytes)
	if err != nil {
		report.fail(err)
		return report
	}
	report.Generation = publishToolGenerationLocked(ctx, root, installs, maxBytes)
	if report.Generation.Partial || !report.Generation.Published {
		report.fail(fmt.Errorf("successor publication incomplete: %s", report.Generation.Error))
		return report
	}
	if err := ctx.Err(); err != nil {
		report.fail(err)
		return report
	}
	selection = ToolGenerationSelection{SchemaVersion: 1, ID: report.Generation.ID}
	report.Selected, report.PendingSelection, err = writeToolGenerationSelection(root, selection, syncSelection)
	if err != nil {
		report.fail(err)
	}
	return report
}

func currentToolGeneration(ctx context.Context, root string, maxBytes int64) (ToolGenerationSelection, error) {
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	guard := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "selection", ObjectID: strings.Repeat("0", 64)}}}
	if _, err := validateToolGenerationSpec(root, guard, maxBytes); err != nil {
		return ToolGenerationSelection{}, err
	}
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		return ToolGenerationSelection{}, err
	}
	defer catalog.Close()
	selection, err := readToolGenerationSelection(root)
	if err != nil {
		return ToolGenerationSelection{}, err
	}
	_, err = verifySelectedToolGeneration(ctx, root, selection, maxBytes)
	return selection, err
}

func readToolGenerationSelection(root string) (ToolGenerationSelection, error) {
	var selection ToolGenerationSelection
	path := filepath.Join(root, toolGenerationCurrent)
	info, err := os.Lstat(path)
	if err != nil {
		return selection, err
	}
	if !info.Mode().IsRegular() || info.Size() > 512 {
		return selection, fmt.Errorf("current tool generation must be a bounded regular selection")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return selection, err
	}
	if err := json.Unmarshal(data, &selection); err != nil {
		return selection, err
	}
	canonical, err := json.Marshal(selection)
	if err != nil || !bytes.Equal(data, canonical) || selection.SchemaVersion != 1 {
		return selection, fmt.Errorf("current tool generation is not canonical schema 1")
	}
	if err := validateToolInstall(ToolGenerationInstall{Path: "selection", ObjectID: selection.ID}); err != nil {
		return selection, err
	}
	return selection, nil
}

func verifySelectedToolGeneration(ctx context.Context, root string, selection ToolGenerationSelection, maxBytes int64) (toolGenerationManifest, error) {
	path := filepath.Join(root, toolGenerationDirectory, selection.ID)
	manifest, data, err := readToolGenerationManifest(path, selection.ID)
	if err != nil {
		return manifest, err
	}
	if _, err := validateToolGenerationSpec(root, ToolGenerationSpec{SchemaVersion: manifest.SchemaVersion, Installs: manifest.Installs}, maxBytes); err != nil {
		return manifest, err
	}
	if err := verifyToolGeneration(ctx, path, data, manifest.Tree, maxBytes); err != nil {
		return manifest, err
	}
	reader, err := openTransferLease(filepath.Join(path, toolGenerationReaderLock), true, 100*time.Millisecond)
	if err != nil {
		return manifest, err
	}
	return manifest, reader.Close()
}

func writeToolGenerationSelection(root string, selection ToolGenerationSelection, syncSelection func(string) error) (selected bool, pending string, err error) {
	data, err := json.Marshal(selection)
	if err != nil {
		return false, "", err
	}
	stage, err := os.CreateTemp(root, ".tool-current-stage-")
	if err != nil {
		return false, "", err
	}
	name := stage.Name()
	pending = name
	defer func() {
		if pending == "" {
			return
		}
		if cleanupErr := os.Remove(name); cleanupErr != nil && !os.IsNotExist(cleanupErr) {
			err = fmt.Errorf("selection stage cleanup failed after %v: %w", err, cleanupErr)
		} else {
			pending = ""
		}
	}()
	if _, err := stage.Write(data); err != nil {
		_ = stage.Close()
		return false, pending, err
	}
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		return false, pending, err
	}
	if err := stage.Close(); err != nil {
		return false, pending, err
	}
	if err := os.Rename(name, filepath.Join(root, toolGenerationCurrent)); err != nil {
		return false, pending, err
	}
	return true, "", syncSelection(root)
}
