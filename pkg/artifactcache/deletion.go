package artifactcache

import (
	"context"
	"fmt"

	"github.com/timshannon/bolthold"
	"go.etcd.io/bbolt"
)

// DeletionIntent is committed before removing files. Cache and intent are
// removed together afterward, so a crash cannot make missing files ambiguous.
type DeletionIntent struct {
	Cache  Cache          `json:"cache"`
	Reason EvictionReason `json:"reason"`
}

func deletionMatches(db *bolthold.Store, cache *Cache) bool {
	var intent DeletionIntent
	return db.Get(cache.ID, &intent) == nil && sameDeletionIdentity(intent.Cache, *cache)
}

func sameDeletionIdentity(expected, actual Cache) bool {
	// A served lookup may refresh UsedAt after a failed file removal. Every
	// immutable identity and archive field must still match the recorded intent.
	actual.UsedAt = expected.UsedAt
	return expected == actual
}

func validateDeletionIntents(ctx context.Context, db *bolthold.Store, recoverable bool) error {
	count := 0
	return db.ForEach(nil, func(intent *DeletionIntent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > auditMaxFiles {
			return fmt.Errorf("deletion intent limit exceeded")
		}
		switch intent.Reason {
		case EvictionIncomplete, EvictionUnused, EvictionMaxAge, EvictionSuperseded, EvictionBudget, EvictionAggregate, EvictionExplicit:
		default:
			return fmt.Errorf("invalid deletion intent reason")
		}
		if !recoverable {
			return fmt.Errorf("cache deletion requires maintenance recovery")
		}
		var cache Cache
		if err := db.Get(intent.Cache.ID, &cache); err != nil {
			return err
		}
		if !sameDeletionIdentity(cache, intent.Cache) {
			return fmt.Errorf("deletion intent identity mismatch")
		}
		return nil
	})
}

func finishDeletion(db *bolthold.Store, cache *Cache) error {
	return db.Bolt().Update(func(tx *bbolt.Tx) error {
		if err := db.TxDelete(tx, cache.ID, cache); err != nil {
			return err
		}
		return db.TxDelete(tx, cache.ID, &DeletionIntent{})
	})
}

func (h *Handler) recoverDeletions(ctx context.Context, db *bolthold.Store, report *RetentionReport) error {
	if err := validateDeletionIntents(ctx, db, true); err != nil {
		return err
	}
	var intents []DeletionIntent
	if err := db.ForEach(nil, func(intent *DeletionIntent) error {
		intents = append(intents, *intent)
		return nil
	}); err != nil {
		return err
	}
	for _, intent := range intents {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A file already removed by the interrupted pass contributes zero bytes
		// to this receipt; recovery never claims those bytes a second time.
		var cache Cache
		if err := db.Get(intent.Cache.ID, &cache); err != nil {
			return err
		}
		if err := h.deleteCache(db, &cache, intent.Reason, report); err != nil {
			return err
		}
	}
	return nil
}
