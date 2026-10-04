//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func retainToolStore(context.Context, string, ToolRetentionPolicy) ToolRetentionReport {
	report := ToolRetentionReport{SchemaVersion: 1}
	report.fail(fmt.Errorf("tool retention requires Linux"))
	return report
}
