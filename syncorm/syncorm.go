// Package syncorm keeps a search engine's index in step with lemmego/orm.
//
// It is the one adapter of the three with nothing to apologise for.
// orm.Observe fires **after** the transaction commits, so a change reaches
// the index only once the row is actually there and a rolled-back write
// announces nothing. The GORM and Bun adapters fire inside the transaction
// and say so.
//
//	searchorm.Watch(db, engine)
//
// That is the whole wiring. From then on every Create, Update, Delete,
// soft delete, Restore and relation write on a model implementing
// search.Indexable keeps its document current, and every write on a model
// that does not is ignored.
package syncorm

import (
	"context"

	"github.com/lemmego/orm"

	"github.com/lemmego/search"
	"github.com/lemmego/search/internal/syncer"
)

// Option configures Watch.
type Option func(*syncer.Syncer)

// WithErrorHandler replaces the default, which logs at error level.
//
// A hook cannot return an error to whoever did the write, and failing the
// write would be the wrong trade — a briefly stale index is a worse page, a
// refused write is lost data. So this is where a failure goes, and the
// default is loud: an index that quietly stopped updating is the hardest
// kind of outage to account for.
func WithErrorHandler(fn func(ctx context.Context, err error)) Option {
	return func(s *syncer.Syncer) {
		if fn != nil {
			s.OnError = fn
		}
	}
}

// Watch registers the observer.
//
// Returns an error rather than panicking when the engine keeps no index of
// its own — the postgres engine's position — because wiring sync against it
// is a mistake worth reporting at boot rather than once per write.
func Watch(db *orm.DB, engine search.Engine, options ...Option) error {
	if db == nil || engine == nil {
		return errNilArgument
	}
	if _, ok := engine.(search.Indexer); !ok {
		return indexlessError(engine)
	}

	sync := syncer.Syncer{Engine: engine, OnError: syncer.LogErrors}
	for _, option := range options {
		option(&sync)
	}

	db.Observe(func(ctx context.Context, change orm.Change) {
		switch change.Op {
		case orm.ChangeDelete:
			// A soft delete arrives here too, and must: orm reports it as a
			// delete because what it means downstream is that the row is
			// gone, and an index that kept the document would keep serving
			// it.
			sync.Remove(ctx, change.Entity)
		default:
			// Create, Update and Restore all go through Sync, which consults
			// Conditional — so a model that has stopped being searchable is
			// unindexed here rather than skipped. See search.Sync.
			sync.Index(ctx, change.Entity)
		}
	})
	return nil
}
