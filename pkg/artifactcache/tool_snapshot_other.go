//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func publishToolSnapshot(_ context.Context, source, _ string, _ int64) ToolSnapshotReport {
	report := ToolSnapshotReport{SchemaVersion: 1, Source: source}
	report.fail(fmt.Errorf("immutable tool publication requires a Linux Docker engine"))
	return report
}

func planToolSnapshot(ctx context.Context, source, root string, maxBytes int64) ToolSnapshotReport {
	return publishToolSnapshot(ctx, source, root, maxBytes)
}

func publishPlannedToolSnapshot(ctx context.Context, source, root string, maxBytes int64, _ string) ToolSnapshotReport {
	return publishToolSnapshot(ctx, source, root, maxBytes)
}
