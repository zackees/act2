//go:build linux

package artifactcache

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"strings"
	"syscall"
	"time"
)

type toolUsageInode struct {
	Device uint64
	Inode  uint64
}

func auditToolStoreUsage(ctx context.Context, root string, maxEntries int) (report ToolStoreUsage) {
	report = ToolStoreUsage{SchemaVersion: 1, Root: root, ObservedAt: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if maxEntries <= 0 || maxEntries > 1000000 {
		report.fail(fmt.Errorf("tool store inventory requires an entry bound of 1..1000000"))
		return report
	}
	guard := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "inventory", ObjectID: strings.Repeat("0", 64)}}}
	if _, err := validateToolGenerationSpec(root, guard, 1); err != nil {
		report.fail(err)
		return report
	}
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	defer catalog.Close()
	return auditToolStoreUsageLocked(ctx, root, maxEntries)
}

// Caller holds catalog exclusion. Accounting includes private stages and control
// files but reads only metadata; it does not establish eligibility for deletion.
func auditToolStoreUsageLocked(ctx context.Context, root string, maxEntries int) (report ToolStoreUsage) {
	report = ToolStoreUsage{SchemaVersion: 1, Root: root, ObservedAt: time.Now().UTC()}
	seen := make(map[toolUsageInode]struct{})
	var apparent, allocated, files, references int64
	err := walkToolTreeWithDepth(ctx, root, 0, 72, func(_ string, entry fs.DirEntry) error {
		if report.PathEntries >= maxEntries {
			return fmt.Errorf("tool store inventory entry bound exceeded")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Blocks < 0 || stat.Blocks > math.MaxInt64/512 {
			return fmt.Errorf("tool store inode allocation is invalid or oversized")
		}
		if report.FilesystemDevice == nil {
			device := stat.Dev
			report.FilesystemDevice = &device
		} else if *report.FilesystemDevice != stat.Dev {
			return fmt.Errorf("tool store inventory crosses a filesystem boundary")
		}
		report.PathEntries++
		if info.Mode().IsRegular() {
			if err := addToolUsageBytes(&references, info.Size()); err != nil {
				return err
			}
		}
		key := toolUsageInode{Device: stat.Dev, Inode: stat.Ino}
		if _, exists := seen[key]; exists {
			return nil
		}
		seen[key] = struct{}{}
		report.UniqueInodes++
		// st_size is meaningful for regular files and symlinks. Directory and
		// special-inode st_size values are not GNU du-compatible apparent bytes;
		// their allocated blocks still contribute to physical inode accounting.
		if info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0 {
			if err := addToolUsageBytes(&apparent, info.Size()); err != nil {
				return err
			}
		}
		if err := addToolUsageBytes(&allocated, stat.Blocks*512); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return addToolUsageBytes(&files, info.Size())
		}
		return nil
	})
	if err != nil {
		report.fail(err)
		return report
	}
	report.ApparentBytes, report.AllocatedBytes = &apparent, &allocated
	report.UniqueFileBytes, report.ReferencedFileBytes = &files, &references
	report.ObservedAt = time.Now().UTC()
	return report
}

func addToolUsageBytes(total *int64, size int64) error {
	if size < 0 || size > math.MaxInt64-*total {
		return fmt.Errorf("tool store byte total is invalid or oversized")
	}
	*total += size
	return nil
}
