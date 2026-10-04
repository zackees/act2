//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func releaseToolRecoveryPin(_ context.Context, root string, pin ToolRecoveryPin) ToolRecoveryPinReleaseReport {
	report := ToolRecoveryPinReleaseReport{SchemaVersion: 1, Root: root, Pin: pin}
	report.fail(fmt.Errorf("native tool recovery pins require Linux"))
	return report
}
