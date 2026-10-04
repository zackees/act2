package artifactcache

import (
	"fmt"
	"time"
)

// Policy bounds completed archives without discarding the entire shared store.
// MaxBytes=0 disables the byte ceiling; age limits remain enabled.
// Five-minute incomplete/recent-use grace periods protect normal transfers.
type Policy struct {
	// CohortRoot enrolls new namespaces in exclusive aggregate maintenance.
	// Existing legacy namespaces cannot be enrolled implicitly.
	CohortRoot string
	// CohortMaxBytes enables aggregate maintenance after cohort lease release.
	// Zero leaves aggregate maintenance to an explicit caller.
	CohortMaxBytes int64
	MaxBytes       int64
	MaxAge         time.Duration
	UnusedAge      time.Duration
	GCInterval     time.Duration
}

func DefaultPolicy() Policy {
	return Policy{MaxAge: keepUsed, UnusedAge: keepUnused, GCInterval: 5 * time.Minute}
}

func (p Policy) Validate() error {
	if p.CohortMaxBytes < 0 {
		return fmt.Errorf("cohort maximum bytes must not be negative")
	}
	if p.CohortMaxBytes > 0 && p.CohortRoot == "" {
		return fmt.Errorf("cohort maximum bytes requires an explicit cohort root")
	}
	if p.MaxBytes < 0 {
		return fmt.Errorf("cache maximum bytes must not be negative")
	}
	if p.MaxAge <= 0 || p.UnusedAge <= 0 || p.GCInterval <= 0 {
		return fmt.Errorf("cache ages and maintenance interval must be positive")
	}
	return nil
}

func (h *Handler) startMaintenance() {
	h.stopMaintenance = make(chan struct{})
	h.maintenanceDone = make(chan struct{})
	go func() {
		defer close(h.maintenanceDone)
		ticker := time.NewTicker(h.policy.GCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-h.stopMaintenance:
				return
			case <-ticker.C:
				h.gcCache()
			}
		}
	}()
}
