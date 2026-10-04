package artifactcache

import (
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
func (h *Handler) trimBudget(db *bolthold.Store) error {
	if h.policy.MaxBytes <= 0 {
		return nil
	}
	var caches []*Cache
	if err := db.Find(&caches, bolthold.Where("Complete").Eq(true)); err != nil {
		return fmt.Errorf("list cache budget: %w", err)
	}
	var total int64
	sizes := make(map[uint64]int64, len(caches))
	for _, cache := range caches {
		info, err := os.Lstat(h.storage.filename(cache.ID))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("measure cache %d: %w", cache.ID, err)
		}
		if !info.Mode().IsRegular() || info.Size() > math.MaxInt64-total {
			return fmt.Errorf("cache %d cannot be measured safely", cache.ID)
		}
		sizes[cache.ID] = info.Size()
		total += info.Size()
	}
	sort.Slice(caches, func(i, j int) bool {
		if caches[i].UsedAt != caches[j].UsedAt {
			return caches[i].UsedAt < caches[j].UsedAt
		}
		return caches[i].ID < caches[j].ID
	})
	for _, cache := range caches {
		if total <= h.policy.MaxBytes {
			break
		}
		if time.Since(time.Unix(cache.UsedAt, 0)) < keepOld {
			continue
		}
		if err := h.deleteCache(db, cache); err != nil {
			return err
		}
		total -= sizes[cache.ID]
		h.logger.Infof("deleted cache under byte ceiling: id=%d size=%d", cache.ID, sizes[cache.ID])
	}
	if total > h.policy.MaxBytes {
		h.logger.Warnf("cache byte ceiling exceeded: bytes=%d ceiling=%d; recently used entries are protected", total, h.policy.MaxBytes)
	}
	return nil
}

// deleteCache keeps metadata when removal fails, so retry and accounting can
// still discover the archive instead of silently forgetting allocated bytes.
func (h *Handler) deleteCache(db *bolthold.Store, cache *Cache) error {
	if err := h.storage.Remove(cache.ID); err != nil {
		return fmt.Errorf("expire cache %d: %w", cache.ID, err)
	}
	if err := db.Delete(cache.ID, cache); err != nil {
		return fmt.Errorf("expire cache metadata %d: %w", cache.ID, err)
	}
	h.logger.Infof("deleted cache: id=%d", cache.ID)
	return nil
}
