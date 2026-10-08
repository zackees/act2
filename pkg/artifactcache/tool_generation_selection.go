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
// PendingSelection names an owned private directory when selection staging or
// its cleanup is uncertain. RetireToolStages can expire it under catalog exclusion
// without removing the selected pointer or generation.
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

type toolSelectionMode uint8

const (
	toolSelectionMerge toolSelectionMode = iota
	toolSelectionInitialize
	toolSelectionReplace
)

// ReplaceToolGeneration selects exactly the supplied completed install set.
// expectedGeneration must match the selection under the original catalog writer.
// A stale complete manifest cannot overwrite a concurrent selection change.
// The caller must coordinate its complete successor manifest with other
// publishers; ordinary incremental updates should use UpdateToolGeneration.
// Existing selected data must validate. Older generations remain immutable and
// reader-leased; this operation does not retire their payload or bootstrap a
// missing selection. It enables bounded warm-cache rollover without implicit
// eviction from the concurrent merge operation.
func ReplaceToolGeneration(ctx context.Context, root, expectedGeneration string, installs ToolGenerationSpec, maxBytes int64) ToolGenerationUpdateReport {
	return replaceToolGeneration(ctx, root, expectedGeneration, installs, maxBytes)
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
