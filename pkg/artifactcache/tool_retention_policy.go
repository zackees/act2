package artifactcache

import (
	"context"
	"time"
)

// ToolRetentionPolicy bounds one coordinated generation and object sweep.
// ExpireBefore is a publication-age cutoff; pressure can also retire younger
// unselected generations and unreferenced objects. Unknown state is preserved.
type ToolRetentionPolicy struct {
	MaxAllocatedBytes int64     `json:"max_allocated_bytes"`
	ExpireBefore      time.Time `json:"expire_before"`
	MaxEntries        int       `json:"max_entries"`
	MaxCandidates     int       `json:"max_candidates"`
	MaxPayloadBytes   int64     `json:"max_payload_bytes"`
}

type ToolRetentionReport struct {
	RetiredObjects       []string       `json:"retired_objects"`
	ProtectedObjects     []string       `json:"protected_objects"`
	SchemaVersion        int            `json:"schema_version"`
	Before               ToolStoreUsage `json:"before"`
	After                ToolStoreUsage `json:"after"`
	RetiredGenerations   []string       `json:"retired_generations"`
	ProtectedGenerations []string       `json:"protected_generations"`
	ProtectedOverflow    bool           `json:"protected_overflow"`
	Partial              bool           `json:"partial"`
	Error                string         `json:"error,omitempty"`
}

func (r *ToolRetentionReport) fail(err error) { r.Partial, r.Error = true, toolReportError(err) }

// RetainToolStore runs a bounded generation/object age/pressure sweep. Totals are scoped
// allocated inode bytes, not machine-wide quota or backing-store allocation.
func RetainToolStore(ctx context.Context, root string, policy ToolRetentionPolicy) ToolRetentionReport {
	return retainToolStore(ctx, root, policy)
}
