package artifactcache

import (
	"context"
	"time"
)

type ToolStageRetentionReport struct {
	SchemaVersion int            `json:"schema_version"`
	RetiredStages []string       `json:"retired_stages"`
	After         ToolStoreUsage `json:"after"`
	Partial       bool           `json:"partial"`
	Error         string         `json:"error,omitempty"`
}

func (r *ToolStageRetentionReport) fail(err error) { r.Partial, r.Error = true, toolReportError(err) }

// RetireToolStages expires registered crash leftovers after original catalog
// exclusion and inode/mount validation. Unregistered directories are preserved.
func RetireToolStages(ctx context.Context, root string, expireBefore time.Time, maxEntries int) ToolStageRetentionReport {
	return retireToolStages(ctx, root, expireBefore, maxEntries)
}
