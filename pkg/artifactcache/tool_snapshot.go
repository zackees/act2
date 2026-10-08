package artifactcache

import "context"

// ToolSnapshotReport describes a closed install object, not generation enrollment
// or current source liveness. Published may remain true on a durability error.
type ToolSnapshotReport struct {
	SchemaVersion int    `json:"schema_version"`
	Source        string `json:"source"`
	Destination   string `json:"destination,omitempty"`
	ID            string `json:"id,omitempty"`
	Completion    string `json:"completion,omitempty"`
	Published     bool   `json:"published"`
	Reused        bool   `json:"reused"`
	Partial       bool   `json:"partial"`
	Error         string `json:"error,omitempty"`
	PendingStage  string `json:"pending_stage,omitempty"`
	Bytes         *int64 `json:"bytes"`
	Entries       *int   `json:"entries"`
}

func (r *ToolSnapshotReport) fail(err error) {
	r.Partial = true
	r.Error = toolReportError(err)
}

func toolReportError(err error) string {
	message := []rune(err.Error())
	if len(message) > 256 {
		message = message[:256]
	}
	return string(message)
}

// PublishToolSnapshot requires a completed, quiescent install. It copies into a
// private stage, validates both source and copied tree, then publishes without
// replacing an existing object. It never links mutable source inodes into a
// published object. Generation assembly, reader protection and GC are separate.
func PublishToolSnapshot(ctx context.Context, source, root string, maxBytes int64) ToolSnapshotReport {
	return publishToolSnapshot(ctx, source, root, maxBytes)
}

// PlanToolSnapshot validates and hashes a completed source without creating a
// store, acquiring publication authority, or claiming that an object is warm.
func PlanToolSnapshot(ctx context.Context, source, root string, maxBytes int64) ToolSnapshotReport {
	return planToolSnapshot(ctx, source, root, maxBytes)
}

// PublishPlannedToolSnapshot rejects a source whose canonical object digest no
// longer matches the plan before creating or modifying the object store.
func PublishPlannedToolSnapshot(ctx context.Context, source, root string, maxBytes int64, expected string) ToolSnapshotReport {
	return publishPlannedToolSnapshot(ctx, source, root, maxBytes, expected)
}
