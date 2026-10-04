//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Only canonical records from the complete inventory can be removed. Matching
// content and inode are rechecked under the same original catalog exclusion.
func removeToolRecoveryPinLocked(root string, pin ToolRecoveryPin) error {
	path := filepath.Join(root, toolRecoveryPinDirectory, pin.Owner+".json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > toolRecoveryPinBytes {
		return fmt.Errorf("recovery pin removal identity is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	data, readErr := io.ReadAll(io.LimitReader(file, toolRecoveryPinBytes+1))
	closeErr := file.Close()
	if statErr != nil || readErr != nil || closeErr != nil || !os.SameFile(info, opened) || len(data) > toolRecoveryPinBytes {
		return fmt.Errorf("recovery pin changed during removal")
	}
	expected, err := json.Marshal(pin)
	if err != nil || !bytes.Equal(expected, data) {
		return fmt.Errorf("recovery pin differs from verified removal intent")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return fmt.Errorf("recovery pin removal inode changed")
	}
	return os.Remove(path)
}

func expireToolRecoveryPinsLocked(ctx context.Context, _ transferLease, root string, now time.Time, records map[string]ToolRecoveryPin, limit int, report ToolRetentionReport) ToolRetentionReport {
	owners := make([]string, 0, len(records))
	for owner, pin := range records {
		if !pin.ExpiresAt.After(now) {
			owners = append(owners, owner)
		}
	}
	sort.Strings(owners)
	report.RemainingExpiredPins = uint64(len(owners))
	for index, owner := range owners {
		if index >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			report.fail(err)
			break
		}
		if err := removeToolRecoveryPinLocked(root, records[owner]); err != nil {
			report.fail(err)
			break
		}
		report.ExpiredPins++
		report.RemainingExpiredPins--
		if len(report.ExpiredPinOwners) < 32 {
			report.ExpiredPinOwners = append(report.ExpiredPinOwners, owner)
		} else {
			report.ExpiredPinOwnersOmitted++
		}
	}
	if report.ExpiredPins > 0 {
		if err := syncToolDirectory(filepath.Join(root, toolRecoveryPinDirectory)); err != nil {
			report.fail(err)
		}
	}
	return report
}
