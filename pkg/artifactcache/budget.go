package artifactcache

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/timshannon/bolthold"
)

// trimBudget expires least recently used completed archives, preserving
// recently served/reserved entries. The database lock serializes metadata;
// transfer protection across servers is a separate namespace lock.
func (h *Handler) trimBudget(ctx context.Context, db *bolthold.Store, report *RetentionReport) error {
	if h.policy.MaxBytes <= 0 {
		return nil
	}
	var caches []*Cache
	if err := db.Find(&caches, bolthold.Where("Complete").Eq(true)); err != nil {
		return fmt.Errorf("list cache budget: %w", err)
	}
	total, sizes, err := h.measureBudget(ctx, caches)
	if err != nil {
		return err
	}
	sort.Slice(caches, func(i, j int) bool {
		if caches[i].UsedAt != caches[j].UsedAt {
			return caches[i].UsedAt < caches[j].UsedAt
		}
		return caches[i].ID < caches[j].ID
	})
	for _, cache := range caches {
		if err := ctx.Err(); err != nil {
			return err
		}
		if total <= h.policy.MaxBytes {
			break
		}
		if time.Since(time.Unix(cache.UsedAt, 0)) < keepOld {
			continue
		}
		if err := h.deleteCache(db, cache, EvictionBudget, report); err != nil {
			return err
		}
		total -= sizes[cache.ID]
		h.logger.Infof("deleted cache under byte ceiling: id=%d size=%d", cache.ID, sizes[cache.ID])
	}
	if report != nil {
		var protected int64
		for _, cache := range caches {
			if time.Since(time.Unix(cache.UsedAt, 0)) < keepOld {
				protected += sizes[cache.ID]
			}
		}
		met := total <= h.policy.MaxBytes
		report.RemainingCompletedBytes = &total
		report.ProtectedBytes = &protected
		report.BudgetMet = &met
	}
	if total > h.policy.MaxBytes {
		h.logger.Warnf("cache byte ceiling exceeded: bytes=%d ceiling=%d; recently used entries are protected", total, h.policy.MaxBytes)
	}
	return nil
}

// deleteCache keeps metadata when removal fails, so retry and accounting can
// still discover the archive instead of silently forgetting allocated bytes.
func (h *Handler) deleteCache(db *bolthold.Store, cache *Cache, reason EvictionReason, report *RetentionReport) error {
	var size int64
	if cache.Complete {
		info, err := os.Lstat(h.storage.filename(cache.ID))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("measure deletion %d: %w", cache.ID, err)
		}
		if err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("archive %d is not regular", cache.ID)
			}
			size = info.Size()
		}
	}
	if err := h.storage.Remove(cache.ID); err != nil {
		return fmt.Errorf("expire cache %d: %w", cache.ID, err)
	}
	if err := db.Delete(cache.ID, cache); err != nil {
		return fmt.Errorf("expire cache metadata %d: %w", cache.ID, err)
	}
	report.record(cache.ID, reason, size)
	h.logger.Infof("deleted cache: id=%d reason=%s bytes=%d", cache.ID, reason, size)
	return nil
}

func (h *Handler) measureBudget(ctx context.Context, caches []*Cache) (int64, map[uint64]int64, error) {
	var total int64
	sizes := make(map[uint64]int64, len(caches))
	for _, cache := range caches {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		info, err := os.Lstat(h.storage.filename(cache.ID))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, nil, fmt.Errorf("measure cache %d: %w", cache.ID, err)
		}
		if !info.Mode().IsRegular() || info.Size() > math.MaxInt64-total {
			return 0, nil, fmt.Errorf("cache %d cannot be measured safely", cache.ID)
		}
		sizes[cache.ID] = info.Size()
		total += info.Size()
	}
	return total, sizes, nil
}
