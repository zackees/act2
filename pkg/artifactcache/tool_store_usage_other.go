//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
	"time"
)

func auditToolStoreUsage(_ context.Context, root string, _ int) ToolStoreUsage {
	report := ToolStoreUsage{SchemaVersion: 1, Root: root, ObservedAt: time.Now().UTC()}
	report.fail(fmt.Errorf("tool store inode accounting is supported only on Linux"))
	return report
}
