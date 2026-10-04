package artifactcache

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/timshannon/bolthold"
	"go.etcd.io/bbolt"
)

const maxCohortNamespaces = 64

type CohortReport struct {
	SchemaVersion           int          `json:"schema_version"`
	Root                    string       `json:"root"`
	Partial                 bool         `json:"partial"`
	Errors                  []string     `json:"errors"`
	BudgetBytes             int64        `json:"budget_bytes"`
	RemainingCompletedBytes *int64       `json:"remaining_completed_bytes"`
	ProtectedBytes          *int64       `json:"protected_bytes"`
	BudgetMet               *bool        `json:"budget_met"`
	Namespaces              []StoreAudit `json:"namespaces"`
}

type maintainedNamespace struct {
	h      *Handler
	lock   *bbolt.DB
	db     *bolthold.Store
	report *RetentionReport
}

// MaintainCohort enforces namespace and aggregate completed-archive policies.
// Every live server must hold a shared root lease. Any busy, legacy, unknown or
// incomplete namespace refuses the entire pass before deletion. Root locks
// exclude new namespaces while namespace locks also cover closing transfers.
func MaintainCohort(ctx context.Context, root string, maxBytes int64, policy Policy) CohortReport {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	report := CohortReport{SchemaVersion: 1, Root: root, BudgetBytes: maxBytes}
	if err := policy.Validate(); err != nil {
		report.fail(err)
		return report
	}
	if maxBytes < 0 {
		report.fail(fmt.Errorf("aggregate maximum bytes must not be negative"))
		return report
	}
	gate := &Handler{dir: root}
	lease, err := gate.transferLock(false)
	if err != nil {
		report.fail(fmt.Errorf("cohort unavailable or busy: %w", err))
		return report
	}
	defer lease.Close()
	stores, err := openCohort(ctx, root, policy)
	if err != nil {
		report.fail(err)
		return report
	}
	defer closeCohort(stores)
	if err := collectCohort(ctx, stores, maxBytes, &report); err != nil {
		report.fail(err)
	}
	for _, store := range stores {
		audit := auditLocked(ctx, store.h, store.db, 0)
		audit.Retention = store.report
		report.Namespaces = append(report.Namespaces, audit)
		if audit.Partial {
			report.fail(fmt.Errorf("namespace %s has incomplete final accounting", audit.Namespace))
		}
	}
	return report
}

func (r *CohortReport) fail(err error) {
	r.Partial = true
	if len(r.Errors) < 8 {
		detail := []rune(err.Error())
		if len(detail) > 128 {
			detail = detail[:128]
		}
		r.Errors = append(r.Errors, string(detail))
	}
}

func closeCohort(stores []*maintainedNamespace) {
	for i := len(stores) - 1; i >= 0; i-- {
		if stores[i].db != nil {
			_ = stores[i].db.Close()
		}
		if stores[i].lock != nil {
			_ = stores[i].lock.Close()
		}
	}
}

func openCohort(ctx context.Context, root string, policy Policy) ([]*maintainedNamespace, error) {
	// #nosec G703 -- explicit caller-selected local cohort root.
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("cohort root is not a directory")
	}
	entries, err := readDirectoryPage(root, maxCohortNamespaces+2)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxCohortNamespaces+1 {
		return nil, fmt.Errorf("cohort namespace limit exceeded")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var stores []*maintainedNamespace
	ok := false
	defer func() {
		if !ok {
			closeCohort(stores)
		}
	}()
	for _, entry := range entries {
		if entry.Name() == "transfers.bolt" {
			continue
		}
		if !entry.IsDir() {
			return nil, fmt.Errorf("unknown cohort entry %s", entry.Name())
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := filepath.Join(root, entry.Name())
		marker, err := readCohortMarker(filepath.Join(dir, cohortMarker))
		if err != nil || string(marker) != cohortIdentity {
			return nil, fmt.Errorf("namespace %s is not enrolled", entry.Name())
		}
		logger := log.New()
		h := &Handler{dir: dir, storage: &Storage{rootDir: filepath.Join(dir, "cache")}, policy: policy, logger: logger}
		store := &maintainedNamespace{h: h, report: &RetentionReport{BudgetBytes: policy.MaxBytes}}
		stores = append(stores, store)
		store.lock, err = h.transferLock(false)
		if err != nil {
			return nil, fmt.Errorf("namespace %s busy or uncoordinated: %w", entry.Name(), err)
		}
		store.db, err = openExistingMaintenanceDB(dir)
		if err != nil {
			return nil, err
		}
		before := auditInventory(ctx, h, store.db, 0, true)
		if before.Partial {
			return nil, fmt.Errorf("namespace %s has an incomplete inventory: %v", entry.Name(), before.Errors)
		}
	}
	ok = true
	return stores, nil
}

type cohortCandidate struct {
	store *maintainedNamespace
	cache *Cache
	bytes int64
}

func cohortCandidates(ctx context.Context, stores []*maintainedNamespace) ([]cohortCandidate, int64, error) {
	var candidates []cohortCandidate
	var total int64
	for _, store := range stores {
		var caches []*Cache
		if err := store.db.Find(&caches, bolthold.Where("Complete").Eq(true)); err != nil {
			return nil, 0, err
		}
		subtotal, sizes, err := store.h.measureBudget(ctx, caches)
		if err != nil {
			return nil, 0, err
		}
		if subtotal > math.MaxInt64-total {
			return nil, 0, fmt.Errorf("aggregate archive size overflow")
		}
		total += subtotal
		for _, cache := range caches {
			candidates = append(candidates, cohortCandidate{store: store, cache: cache, bytes: sizes[cache.ID]})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.cache.UsedAt != b.cache.UsedAt {
			return a.cache.UsedAt < b.cache.UsedAt
		}
		if a.store.h.dir != b.store.h.dir {
			return a.store.h.dir < b.store.h.dir
		}
		return a.cache.ID < b.cache.ID
	})
	return candidates, total, nil
}

func collectCohort(ctx context.Context, stores []*maintainedNamespace, maxBytes int64, report *CohortReport) error {
	for _, store := range stores {
		if err := store.h.collect(ctx, store.db, store.report); err != nil {
			return err
		}
	}
	candidates, total, err := cohortCandidates(ctx, stores)
	if err != nil {
		return err
	}
	var protected int64
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(time.Unix(candidate.cache.UsedAt, 0)) < keepOld {
			protected += candidate.bytes
			continue
		}
		if maxBytes == 0 || total <= maxBytes {
			continue
		}
		if err := candidate.store.h.deleteCache(candidate.store.db, candidate.cache, EvictionAggregate, candidate.store.report); err != nil {
			return err
		}
		total -= candidate.bytes
	}
	met := maxBytes == 0 || total <= maxBytes
	report.RemainingCompletedBytes = &total
	report.ProtectedBytes = &protected
	report.BudgetMet = &met
	// Refresh each namespace's budget outcome after aggregate eviction.
	for _, store := range stores {
		var caches []*Cache
		if err := store.db.Find(&caches, bolthold.Where("Complete").Eq(true)); err != nil {
			return err
		}
		total, sizes, err := store.h.measureBudget(ctx, caches)
		if err != nil {
			return err
		}
		var protected int64
		for _, cache := range caches {
			if time.Since(time.Unix(cache.UsedAt, 0)) < keepOld {
				protected += sizes[cache.ID]
			}
		}
		met := store.h.policy.MaxBytes == 0 || total <= store.h.policy.MaxBytes
		store.report.RemainingCompletedBytes = &total
		store.report.ProtectedBytes = &protected
		store.report.BudgetMet = &met
	}
	return nil
}
