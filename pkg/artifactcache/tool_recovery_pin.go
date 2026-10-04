package artifactcache

import (
	"context"
	"time"
)

// A protection request, never evidence of source ownership or quiescence.
type ToolRecoveryPin struct {
	SchemaVersion int       `json:"schema_version"`
	Owner         string    `json:"owner"`
	Generation    string    `json:"generation"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type ToolRecoveryPinReport struct {
	SchemaVersion int             `json:"schema_version"`
	Root          string          `json:"root"`
	Pin           ToolRecoveryPin `json:"pin"`
	Published     bool            `json:"published"`
	Reused        bool            `json:"reused"`
	Partial       bool            `json:"partial"`
	Error         string          `json:"error,omitempty"`
	PendingStage  string          `json:"pending_stage,omitempty"`
}

func (report *ToolRecoveryPinReport) fail(err error) {
	report.Partial, report.Error = true, toolReportError(err)
}

// PublishToolRecoveryPin durably reserves a verified immutable lower. The
// caller first persists intent and separately proves source ownership. Any
// partial result requires reconciliation; it never establishes pin absence.
func PublishToolRecoveryPin(ctx context.Context, root string, pin ToolRecoveryPin, maxBytes int64) ToolRecoveryPinReport {
	return publishToolRecoveryPin(ctx, root, pin, maxBytes)
}

// Absent is a durable acknowledgement only when Partial is false. Removed
// records may still need a parent-directory sync before absence is acknowledged.
type ToolRecoveryPinReleaseReport struct {
	SchemaVersion int             `json:"schema_version"`
	Root          string          `json:"root"`
	Pin           ToolRecoveryPin `json:"pin"`
	Removed       bool            `json:"removed"`
	Absent        bool            `json:"absent"`
	Partial       bool            `json:"partial"`
	Error         string          `json:"error,omitempty"`
}

func (report *ToolRecoveryPinReleaseReport) fail(err error) {
	report.Partial, report.Error = true, toolReportError(err)
}

// ReleaseToolRecoveryPin checks the complete immutable intent before removal.
// The caller must persist completion and exclude further publication by this
// owner before release. This API proves neither source quiescence nor ownership.
func ReleaseToolRecoveryPin(ctx context.Context, root string, pin ToolRecoveryPin) ToolRecoveryPinReleaseReport {
	return releaseToolRecoveryPin(ctx, root, pin)
}
