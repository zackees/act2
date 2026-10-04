//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func updateToolGeneration(context.Context, string, ToolGenerationSpec, int64, bool) ToolGenerationUpdateReport {
	report := ToolGenerationUpdateReport{SchemaVersion: 1}
	report.fail(fmt.Errorf("tool generation selection is supported only on Linux"))
	return report
}

func currentToolGeneration(context.Context, string, int64) (ToolGenerationSelection, error) {
	return ToolGenerationSelection{}, fmt.Errorf("tool generation selection is supported only on Linux")
}
