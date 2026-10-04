package artifactcache

import "context"

// ToolGenerationSelection names the selected warm generation. Planning callers
// must still acquire its reader lease before mounting: this value is not a lease.
type ToolGenerationSelection struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
}

// ToolGenerationUpdateReport separates closed generation publication from
// selection. Selected can remain true with Partial after a final directory-sync
// failure; retry validates existing data instead of rebuilding from zero.
type ToolGenerationUpdateReport struct {
	SchemaVersion    int                `json:"schema_version"`
	Generation       ToolSnapshotReport `json:"generation"`
	Selected         bool               `json:"selected"`
	Partial          bool               `json:"partial"`
	Error            string             `json:"error,omitempty"`
	PendingSelection string             `json:"pending_selection,omitempty"`
}

func (r *ToolGenerationUpdateReport) fail(err error) {
	r.Partial, r.Error = true, toolReportError(err)
}

// UpdateToolGeneration merges only supplied install paths into the latest warm
// selection under catalog exclusion, then durably publishes and selects the
// successor. Concurrent updates cannot discard unrelated completed installs.
func UpdateToolGeneration(ctx context.Context, root string, updates ToolGenerationSpec, maxBytes int64) ToolGenerationUpdateReport {
	return updateToolGeneration(ctx, root, updates, maxBytes, false)
}

// InitializeToolGeneration explicitly establishes a previously unset selection.
// Call only during deliberate store bootstrap or explicit recovery, never as an
// automatic fallback for a new engine or an ordinary update's missing pointer.
// Existing selections are refused, including a retry after a visible rename;
// use UpdateToolGeneration to verify and converge that uncertain selection.
func InitializeToolGeneration(ctx context.Context, root string, installs ToolGenerationSpec, maxBytes int64) ToolGenerationUpdateReport {
	return updateToolGeneration(ctx, root, installs, maxBytes, true)
}

// CurrentToolGeneration validates current selection and payload under catalog
// exclusion. Missing/malformed data is an error, never an empty warm-cache claim.
func CurrentToolGeneration(ctx context.Context, root string, maxBytes int64) (ToolGenerationSelection, error) {
	return currentToolGeneration(ctx, root, maxBytes)
}
