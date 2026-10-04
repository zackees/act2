package artifactcache

// RetentionReport records successful metadata/file removal and budget outcomes.
// Receipt detail is bounded; totals cover every successful removal. Reclaimed
// bytes count completed archive lengths, not allocated blocks or temporary data.
type RetentionReport struct {
	DeletedCount            uint64            `json:"deleted_count"`
	ReclaimedArchiveBytes   int64             `json:"reclaimed_archive_bytes"`
	Receipts                []EvictionReceipt `json:"receipts"`
	ReceiptsOmitted         uint64            `json:"receipts_omitted"`
	BudgetBytes             int64             `json:"budget_bytes"`
	RemainingCompletedBytes *int64            `json:"remaining_completed_bytes"`
	ProtectedBytes          *int64            `json:"protected_bytes"`
	BudgetMet               *bool             `json:"budget_met"`
}

type EvictionReason string

const (
	EvictionIncomplete EvictionReason = "incomplete"
	EvictionUnused     EvictionReason = "unused_age"
	EvictionMaxAge     EvictionReason = "absolute_age"
	EvictionSuperseded EvictionReason = "superseded"
	EvictionBudget     EvictionReason = "byte_budget"
)

type EvictionReceipt struct {
	ID           uint64         `json:"id"`
	Reason       EvictionReason `json:"reason"`
	ArchiveBytes int64          `json:"archive_bytes"`
}

func (r *RetentionReport) record(id uint64, reason EvictionReason, size int64) {
	if r == nil {
		return
	}
	r.DeletedCount++
	r.ReclaimedArchiveBytes += size
	if len(r.Receipts) < 32 {
		r.Receipts = append(r.Receipts, EvictionReceipt{ID: id, Reason: reason, ArchiveBytes: size})
	} else {
		r.ReceiptsOmitted++
	}
}
