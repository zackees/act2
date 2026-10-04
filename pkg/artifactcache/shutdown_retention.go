package artifactcache

import "context"

// RetentionOnClose returns the bounded outcome of automatic shutdown
// maintenance. It is nil before Close or when no aggregate policy is enabled.
// As with Close, the caller must serialize access to the handler lifecycle.
func (h *Handler) RetentionOnClose() *CohortReport {
	if h == nil {
		return nil
	}
	return h.retentionOnClose
}

func (h *Handler) maintainIdleCohort(ctx context.Context) {
	if h.policy.CohortRoot == "" || h.policy.CohortMaxBytes == 0 {
		return
	}
	// The shared root lease has been released. Other live servers or transfer
	// leases cause a bounded deferred outcome; no active peer is interrupted.
	// MaintainCohort owns its five-second deadline and full preflight checks.
	report := MaintainCohort(ctx, h.policy.CohortRoot, h.policy.CohortMaxBytes, h.policy)
	h.retentionOnClose = &report
	logger := h.logger.WithField("cache_retention", report)
	if report.Partial {
		logger.Warn("shutdown cache retention deferred or incomplete")
	} else if report.BudgetMet != nil && !*report.BudgetMet {
		logger.Warn("shutdown cache budget exceeded by protected archives")
	} else {
		logger.Info("shutdown cache retention complete")
	}
}
