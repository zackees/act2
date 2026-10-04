//go:build linux

package artifactcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func retireToolStages(ctx context.Context, root string, expireBefore time.Time, maxEntries int) (report ToolStageRetentionReport) {
	report.SchemaVersion = 1
	if expireBefore.IsZero() || maxEntries <= 0 || maxEntries > 1000000 {
		report.fail(fmt.Errorf("stage expiry requires explicit cutoff and positive bounded entries"))
		return report
	}
	guard := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "stage-expiry", ObjectID: strings.Repeat("0", 64)}}}
	if _, err := validateToolGenerationSpec(root, guard, 1); err != nil {
		report.fail(err)
		return report
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	defer catalog.Close()
	rows, err := loadToolStageOwnership(catalog, root)
	if err != nil {
		report.fail(err)
		return report
	}
	report.After = auditToolStoreUsageLocked(ctx, root, maxEntries)
	if report.After.Partial {
		report.fail(fmt.Errorf("stage inventory incomplete: %s", report.After.Error))
		return report
	}
	for _, row := range rows {
		if !row.CreatedAt.Before(expireBefore) {
			continue
		}
		retired, err := retireOwnedToolStage(ctx, catalog, root, row)
		if retired {
			report.RetiredStages = append(report.RetiredStages, filepath.Join(root, row.Relative))
		}
		if err != nil {
			report.fail(err)
		}
		report.After = auditToolStoreUsageLocked(ctx, root, maxEntries)
		if report.After.Partial {
			report.fail(fmt.Errorf("post-stage inventory incomplete: %s", report.After.Error))
			break
		}
	}
	return report
}

func retireOwnedToolStage(ctx context.Context, catalog transferLease, root string, row toolStageOwnership) (bool, error) {
	stage := filepath.Join(root, row.Relative)
	info, err := os.Lstat(stage)
	if os.IsNotExist(err) {
		// Publication may have renamed the original stage. Only clear its stale
		// record after syncing the namespace; no published object is touched.
		if err := syncToolDirectory(filepath.Dir(stage)); err != nil {
			return false, err
		}
		return false, forgetOwnedToolStage(catalog, root, row.Relative)
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Dev != row.Device || stat.Ino != row.Inode {
		return false, fmt.Errorf("stage identity changed; replacement preserved")
	}
	mount, err := toolMountID(stage)
	if err != nil || mount != row.MountID {
		return false, fmt.Errorf("stage mount identity changed")
	}
	if err := verifyToolRetirementMounts(ctx, root, stage, toolMountID); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// #nosec G703 -- Typed registered relative stage under canonical root with original inode/mount identity and catalog exclusion.
	if err := os.RemoveAll(stage); err != nil {
		return false, fmt.Errorf("stage retirement incomplete: %w", err)
	}
	if err := syncToolDirectory(filepath.Dir(stage)); err != nil {
		return true, err
	}
	return true, forgetOwnedToolStage(catalog, root, row.Relative)
}

func cleanupOwnedToolStage(ctx context.Context, catalog transferLease, root, stage string) error {
	rows, err := loadToolStageOwnership(catalog, root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, stage)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Relative == relative {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_, err := retireOwnedToolStage(ctx, catalog, root, row)
			return err
		}
	}
	return fmt.Errorf("stage ownership unavailable; private footprint preserved")
}

func finishOwnedToolStage(catalog transferLease, root, stage string) error {
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		return fmt.Errorf("published stage path still exists or cannot be inspected")
	}
	relative, err := filepath.Rel(root, stage)
	if err != nil {
		return err
	}
	if !validToolStageRelative(relative) {
		return fmt.Errorf("published stage path invalid")
	}
	return forgetOwnedToolStage(catalog, root, relative)
}
