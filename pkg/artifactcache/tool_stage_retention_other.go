//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
	"time"
)

func retireToolStages(context.Context, string, time.Time, int) ToolStageRetentionReport {
	report := ToolStageRetentionReport{SchemaVersion: 1}
	report.fail(fmt.Errorf("tool stage expiry requires Linux"))
	return report
}
