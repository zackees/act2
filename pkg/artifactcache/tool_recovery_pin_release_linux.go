//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func releaseToolRecoveryPin(ctx context.Context, root string, pin ToolRecoveryPin) ToolRecoveryPinReleaseReport {
	return releaseToolRecoveryPinWithSync(ctx, root, pin, syncToolDirectory)
}

func releaseToolRecoveryPinWithSync(ctx context.Context, root string, pin ToolRecoveryPin, syncParent func(string) error) (report ToolRecoveryPinReleaseReport) {
	report = ToolRecoveryPinReleaseReport{SchemaVersion: 1, Root: root, Pin: pin}
	if err := pin.validate(); err != nil {
		report.fail(err)
		return report
	}
	if err := ctx.Err(); err != nil {
		report.fail(err)
		return report
	}
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	defer catalog.Close()
	records, err := readToolRecoveryPinRecords(ctx, root, time.Now().UTC())
	if err != nil {
		report.fail(err)
		return report
	}
	if existing, ok := records[pin.Owner]; ok {
		previous, previousErr := json.Marshal(existing)
		expected, expectedErr := json.Marshal(pin)
		if previousErr != nil || expectedErr != nil || !bytes.Equal(previous, expected) {
			report.fail(fmt.Errorf("recovery pin differs from immutable release intent"))
			return report
		}
		if err := ctx.Err(); err != nil {
			report.fail(err)
			return report
		}
		if err := removeToolRecoveryPinLocked(root, pin); err != nil {
			report.fail(err)
			return report
		}
		report.Removed = true
	}
	// Even an absent retry syncs the namespace: a previous unlink may have lost
	// its durable acknowledgement. A never-created namespace requires root sync.
	directory := filepath.Join(root, toolRecoveryPinDirectory)
	if _, err := os.Lstat(directory); os.IsNotExist(err) {
		directory = root
	} else if err != nil {
		report.fail(err)
		return report
	}
	if err := syncParent(directory); err != nil {
		report.fail(err)
		return report
	}
	report.Absent = true
	return report
}
