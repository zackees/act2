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
