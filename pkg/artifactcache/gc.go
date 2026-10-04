package artifactcache

import (
	"context"
	"errors"
	"fmt"
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.collect(ctx, db, nil); err != nil {
		h.logger.Warnf("cache byte retention: %v", err)
	}
}

func (h *Handler) expireAges(ctx context.Context, db *bolthold.Store, report *RetentionReport) error {
	now := time.Now()
	queries := []*bolthold.Query{
		bolthold.Where("UsedAt").Lt(now.Add(-keepTemp).Unix()).And("Complete").Eq(false),
		bolthold.Where("UsedAt").Lt(now.Add(-h.policy.UnusedAge).Unix()).
			And("UsedAt").Lt(now.Add(-keepOld).Unix()),
		bolthold.Where("CreatedAt").Lt(now.Add(-h.policy.MaxAge).Unix()).
			And("UsedAt").Lt(now.Add(-keepOld).Unix()),
	}
	reasons := []EvictionReason{EvictionIncomplete, EvictionUnused, EvictionMaxAge}
	for i, query := range queries {
		var caches []*Cache
		if err := db.Find(&caches, query); err != nil {
			return fmt.Errorf("find expired caches: %w", err)
		}
		for _, cache := range caches {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := h.deleteCache(db, cache, reasons[i], report); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Handler) expireSuperseded(ctx context.Context, db *bolthold.Store, report *RetentionReport) error {
	results, err := db.FindAggregate(&Cache{}, bolthold.Where("Complete").Eq(true), "Key", "Version")
	if err != nil {
		return fmt.Errorf("find aggregate caches: %w", err)
	}
	for _, result := range results {
		if result.Count() <= 1 {
			continue
		}
		result.Sort("CreatedAt")
		var caches []*Cache
		result.Reduction(&caches)
		for _, cache := range caches[:len(caches)-1] {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Protect a recent lookup-to-download even after a newer entry appears.
			if time.Since(time.Unix(cache.UsedAt, 0)) < keepOld {
				continue
			}
			if err := h.deleteCache(db, cache, EvictionSuperseded, report); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Handler) collect(ctx context.Context, db *bolthold.Store, report *RetentionReport) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.expireAges(ctx, db, report); err != nil {
		return err
	}
	if err := h.expireSuperseded(ctx, db, report); err != nil {
		return err
	}
	return h.trimBudget(ctx, db, report)
}
