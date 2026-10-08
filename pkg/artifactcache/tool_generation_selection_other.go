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

func replaceToolGeneration(ctx context.Context, root, _ string, installs ToolGenerationSpec, maxBytes int64) ToolGenerationUpdateReport {
	return updateToolGeneration(ctx, root, installs, maxBytes, false)
}
