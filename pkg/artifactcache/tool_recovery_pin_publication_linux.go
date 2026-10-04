//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func publishToolRecoveryPin(ctx context.Context, root string, pin ToolRecoveryPin, maxBytes int64) ToolRecoveryPinReport {
	return publishToolRecoveryPinWithSync(ctx, root, pin, maxBytes, syncToolDirectory)
}

// Per-call sync hook covers lost acknowledgement after atomic publication.
type toolRecoveryPinVerifier func(context.Context, string, []byte, toolManifest, int64) error

func publishToolRecoveryPinWithSync(ctx context.Context, root string, pin ToolRecoveryPin, maxBytes int64, syncParent func(string) error) ToolRecoveryPinReport {
	return publishToolRecoveryPinWithValidation(ctx, root, pin, maxBytes, syncParent, verifyToolGeneration)
}

func publishToolRecoveryPinWithValidation(ctx context.Context, root string, pin ToolRecoveryPin, maxBytes int64, syncParent func(string) error, verify toolRecoveryPinVerifier) (report ToolRecoveryPinReport) {
	report = ToolRecoveryPinReport{SchemaVersion: 1, Root: root, Pin: pin}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if err := pin.validate(); err != nil {
		report.fail(err)
		return report
	}
	if pin.CreatedAt.After(now) || !pin.ExpiresAt.After(now) {
		report.fail(fmt.Errorf("recovery pin is future-created or expired"))
		return report
	}
	guard := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "recovery", ObjectID: pin.Generation}}}
	if _, err := validateToolGenerationSpec(root, guard, maxBytes); err != nil {
		report.fail(err)
		return report
	}
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	catalogHeld := true
	defer func() {
		if catalogHeld {
			_ = catalog.Close()
		}
	}()
	generation := filepath.Join(root, toolGenerationDirectory, pin.Generation)
	manifest, data, err := readToolGenerationManifest(generation, pin.Generation)
	if err != nil {
		report.fail(err)
		return report
	}
	if _, err := validateToolGenerationSpec(root, ToolGenerationSpec{SchemaVersion: manifest.SchemaVersion, Installs: manifest.Installs}, maxBytes); err != nil {
		report.fail(err)
		return report
	}
	reader, err := openTransferLease(filepath.Join(generation, toolGenerationReaderLock), true, 100*time.Millisecond)
	if err != nil {
		report.fail(err)
		return report
	}
	defer reader.Close()
	// Validate payload outside the machine-wide catalog while the original
	// reader excludes retirement, then re-read publication state under lock.
	if err := catalog.Close(); err != nil {
		report.fail(err)
		return report
	}
	catalogHeld = false
	if err := verify(ctx, generation, data, manifest.Tree, maxBytes); err != nil {
		report.fail(err)
		return report
	}
	if err := ctx.Err(); err != nil {
		report.fail(err)
		return report
	}
	catalog, err = prepareExistingToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	catalogHeld = true
	now = time.Now().UTC()
	if !pin.ExpiresAt.After(now) {
		report.fail(fmt.Errorf("recovery pin expired during validation"))
		return report
	}
	return publishVerifiedToolRecoveryPin(ctx, catalog, root, pin, now, syncParent, report)
}

func publishVerifiedToolRecoveryPin(ctx context.Context, catalog transferLease, root string, pin ToolRecoveryPin, now time.Time, syncParent func(string) error, report ToolRecoveryPinReport) ToolRecoveryPinReport {
	records, err := readToolRecoveryPinRecords(ctx, root, now)
	if err != nil {
		report.fail(err)
		return report
	}
	encoded, err := json.Marshal(pin)
	if err != nil || len(encoded) > toolRecoveryPinBytes {
		report.fail(fmt.Errorf("recovery pin encoding exceeds bound"))
		return report
	}
	directory := filepath.Join(root, toolRecoveryPinDirectory)
	if existing, ok := records[pin.Owner]; ok {
		previous, err := json.Marshal(existing)
		if err != nil || !bytes.Equal(previous, encoded) {
			report.fail(fmt.Errorf("recovery pin differs from immutable owner intent"))
			return report
		}
		report.PendingStage, err = fenceToolRecoveryStore(ctx, catalog, root, syncParent)
		if err != nil {
			report.fail(err)
			return report
		}
		report.Published, report.Reused = true, true
		if err := syncParent(directory); err != nil {
			report.fail(err)
		}
		return report
	}
	if len(records) >= toolRecoveryPinLimit {
		report.fail(fmt.Errorf("recovery pin inventory at capacity"))
		return report
	}
	report.PendingStage, err = fenceToolRecoveryStore(ctx, catalog, root, syncParent)
	if err != nil {
		report.fail(err)
		return report
	}
	if err := prepareToolRecoveryPinDirectory(root, directory, syncParent); err != nil {
		report.fail(err)
		return report
	}
	return linkToolRecoveryPin(ctx, catalog, root, directory, pin, encoded, syncParent, report)
}

func linkToolRecoveryPin(ctx context.Context, catalog transferLease, root, directory string, pin ToolRecoveryPin, encoded []byte, syncParent func(string) error, initial ToolRecoveryPinReport) (report ToolRecoveryPinReport) {
	report = initial
	stage, err := createOwnedToolStage(catalog, root, root, ".tool-stage-")
	report.PendingStage = stage
	if err != nil {
		report.fail(err)
		return report
	}
	defer func() {
		if err := cleanupOwnedToolStage(ctx, catalog, root, stage); err != nil {
			report.fail(fmt.Errorf("recovery pin stage cleanup: %w", err))
		} else {
			report.PendingStage = ""
		}
	}()
	path := filepath.Join(stage, "pin.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		report.fail(err)
		return report
	}
	written, writeErr := file.Write(encoded)
	if written != len(encoded) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		report.fail(err)
		return report
	}
	if err := syncToolDirectory(stage); err != nil {
		report.fail(err)
		return report
	}
	if err := ctx.Err(); err != nil {
		report.fail(err)
		return report
	}
	// Atomic no-replace publication leaves conflicting owner records intact.
	if err := os.Link(path, filepath.Join(directory, pin.Owner+".json")); err != nil {
		report.fail(err)
		return report
	}
	report.Published = true
	if err := syncParent(directory); err != nil {
		report.fail(err)
	}
	return report
}

func prepareToolRecoveryPinDirectory(root, directory string, syncParent func(string) error) error {
	if _, err := os.Lstat(directory); os.IsNotExist(err) {
		if err := os.Mkdir(directory, 0700); err != nil {
			return err
		}
		if err := syncParent(root); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	rootMount, err := toolMountID(root)
	if err != nil {
		return err
	}
	pinMount, err := toolMountID(directory)
	if err != nil || rootMount != pinMount {
		return fmt.Errorf("recovery pin namespace crosses mount boundary")
	}
	return nil
}
