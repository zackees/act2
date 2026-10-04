//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func publishToolGeneration(_ context.Context, root string, _ ToolGenerationSpec, _ int64) ToolSnapshotReport {
	report := ToolSnapshotReport{SchemaVersion: 1, Source: root}
	report.fail(fmt.Errorf("immutable tool generations require Linux"))
	return report
}
