package artifactcache

import (
	"errors"
	"time"

	"github.com/timshannon/bolthold"
	boltErrors "go.etcd.io/bbolt/errors"
)

const (
	keepUsed   = 30 * 24 * time.Hour
	keepUnused = 7 * 24 * time.Hour
	keepTemp   = 5 * time.Minute
	keepOld    = 5 * time.Minute
)

func (h *Handler) gcCache() {
	if !h.gcing.CompareAndSwap(false, true) {
		return
	}
	defer h.gcing.Store(false)
	if time.Since(h.gcAt) < h.policy.GCInterval {
		return
	}
	transfer, err := h.transferLock(false)
	if err != nil {
		if !errors.Is(err, boltErrors.ErrTimeout) {
			h.logger.Warnf("gc transfer coordination: %v", err)
		}
		return // Active transfers defer GC without consuming its retry interval.
	}
	defer transfer.Close()
	db, err := h.openDB()
	if err != nil {
		h.logger.Warnf("gc metadata: %v", err)
		return
	}
	defer db.Close()
	h.gcAt = time.Now()
	h.logger.Debugf("gc: %v", h.gcAt.String())
	h.expireAges(db)
	h.expireSuperseded(db)
	if err := h.trimBudget(db); err != nil {
		h.logger.Warnf("cache byte retention: %v", err)
	}
}

func (h *Handler) expireAges(db *bolthold.Store) {
	now := time.Now()
	queries := []*bolthold.Query{
		bolthold.Where("UsedAt").Lt(now.Add(-keepTemp).Unix()).And("Complete").Eq(false),
		bolthold.Where("UsedAt").Lt(now.Add(-h.policy.UnusedAge).Unix()).
			And("UsedAt").Lt(now.Add(-keepOld).Unix()),
		bolthold.Where("CreatedAt").Lt(now.Add(-h.policy.MaxAge).Unix()).
			And("UsedAt").Lt(now.Add(-keepOld).Unix()),
	}
	for _, query := range queries {
		var caches []*Cache
		if err := db.Find(&caches, query); err != nil {
			h.logger.Warnf("find expired caches: %v", err)
			continue
		}
		for _, cache := range caches {
			if err := h.deleteCache(db, cache); err != nil {
				h.logger.Warnf("delete cache: %v", err)
			}
		}
	}
}

func (h *Handler) expireSuperseded(db *bolthold.Store) {
	results, err := db.FindAggregate(&Cache{}, bolthold.Where("Complete").Eq(true), "Key", "Version")
	if err != nil {
		h.logger.Warnf("find aggregate caches: %v", err)
		return
	}
	for _, result := range results {
		if result.Count() <= 1 {
			continue
		}
		result.Sort("CreatedAt")
		var caches []*Cache
		result.Reduction(&caches)
		for _, cache := range caches[:len(caches)-1] {
			// Protect a recent lookup-to-download even after a newer entry appears.
			if time.Since(time.Unix(cache.UsedAt, 0)) < keepOld {
				continue
			}
			if err := h.deleteCache(db, cache); err != nil {
				h.logger.Warnf("delete superseded cache: %v", err)
			}
		}
	}
}
