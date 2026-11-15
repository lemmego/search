// Package syncgorm keeps a search engine's index in step with GORM.
//
//	syncgorm.Watch(db, engine)
//
// # The hook fires inside the transaction, and that is a real cost
//
// GORM's callbacks run as part of the statement, so there is no point at
// which this adapter can know the transaction committed. Two consequences,
// neither of which can be hidden:
//
//   - A write that is rolled back has already been indexed. The document then
//     describes a row that does not exist, and nothing afterwards says so.
//   - A subscriber told to go and refetch may look before the row is visible
//     to another connection, and find nothing.
//
// lemmego/orm's adapter does not have this problem, because orm.Observe fires
// after the commit. Bun's has it for the same reason GORM's does.
//
// The honest answers, in order of preference:
//
//  1. Call search.Sync from the service layer, after the transaction the
//     service itself owns. That is the path this module's README recommends
//     and it needs no adapter at all.
//  2. Wire WithDefer to the application's own after-commit mechanism, and
//     this adapter becomes a change detector rather than an index writer.
//  3. Accept it. For an application whose writes rarely roll back, an index
//     that is occasionally wrong about a withdrawn row is a tolerable cost —
//     and search:import --fresh is the repair.
//
// # A conditional delete is not observed
//
// `db.Where(...).Delete(&Post{})` leaves nothing identifiable in the
// statement: the model is a zero struct used only to name the table. Such a
// write is ignored rather than guessed at — see single — and the repair is
// the same reimport.
//
// Option 3 is the default, because the alternative would be an adapter that
// refuses to be wired until a decision is made, and that is a worse
// experience than one that works and documents what it cannot promise.
package syncgorm

import (
	"context"
	"fmt"
	"reflect"

	"gorm.io/gorm"

	"github.com/lemmego/search"
	"github.com/lemmego/search/internal/syncer"
)

// Option configures Watch.
type Option func(*syncer.Syncer)

// WithErrorHandler replaces the default, which logs at error level.
func WithErrorHandler(fn func(ctx context.Context, err error)) Option {
	return func(s *syncer.Syncer) {
		if fn != nil {
			s.OnError = fn
		}
	}
}

// WithDefer hands each index write to fn instead of performing it.
//
// This is the seam that makes the inside-the-transaction problem the
// application's to solve rather than something it has to live with. fn
// receives a closure that performs the write; call it after your own commit.
//
//	syncgorm.Watch(db, engine, syncgorm.WithDefer(
//	    func(ctx context.Context, apply func(context.Context) error) {
//	        unitOfWork.AfterCommit(ctx, apply)
//	    }))
//
// fn is called on the goroutine that did the write, inside the transaction,
// so it must only record the closure — doing the work there is what this
// exists to avoid.
func WithDefer(fn func(ctx context.Context, apply func(context.Context) error)) Option {
	return func(s *syncer.Syncer) { s.Defer = fn }
}

// Watch registers the callbacks.
func Watch(db *gorm.DB, engine search.Engine, options ...Option) error {
	if db == nil || engine == nil {
		return fmt.Errorf("syncgorm: Watch needs a database and an engine")
	}
	if _, ok := engine.(search.Indexer); !ok {
		return fmt.Errorf("%w: the %s engine keeps no index of its own, so there "+
			"is nothing to keep in step — it searches your rows where they are",
			search.ErrUnsupported, engine.Name())
	}

	sync := syncer.Syncer{Engine: engine, OnError: syncer.LogErrors}
	for _, option := range options {
		option(&sync)
	}

	// Registered After rather than Before, so a statement that failed never
	// reaches the index — which is the one half of the transaction problem
	// that can be solved here.
	//
	// Named callbacks, because GORM keys its registry on the name: two
	// Watch calls on one *gorm.DB replace rather than stack, which is what
	// anybody re-registering in a test wants and what stops a double-wired
	// boot indexing everything twice.
	registrations := []struct {
		register func(string, func(*gorm.DB)) error
		remove   bool
	}{
		{db.Callback().Create().After("gorm:create").Register, false},
		{db.Callback().Update().After("gorm:update").Register, false},
		{db.Callback().Delete().After("gorm:delete").Register, true},
	}
	names := []string{"search:create", "search:update", "search:delete"}

	for i, registration := range registrations {
		remove := registration.remove
		if err := registration.register(names[i], func(tx *gorm.DB) {
			if tx.Error != nil {
				return
			}
			models := entities(tx)
			if len(models) == 0 {
				return
			}
			ctx := tx.Statement.Context
			if ctx == nil {
				ctx = context.Background()
			}
			if remove {
				sync.Remove(ctx, models...)
				return
			}
			sync.Index(ctx, models...)
		}); err != nil {
			return fmt.Errorf("syncgorm: registering %s: %w", names[i], err)
		}
	}
	return nil
}

// entities pulls the written models out of a statement.
//
// Dest before Model: Dest is what the caller passed to Create or Save and so
// is the value carrying the generated primary key, where Model may be a bare
// type used only to resolve the table. A batch create puts a slice there, so
// it is flattened — one hook call, many documents, one request to the engine.
func entities(tx *gorm.DB) []any {
	if tx.Statement == nil {
		return nil
	}
	for _, candidate := range []any{tx.Statement.Dest, tx.Statement.Model} {
		if models := collect(reflect.ValueOf(candidate)); len(models) > 0 {
			return models
		}
	}
	return nil
}

// collect walks a value, a pointer to one, or a slice of either.
func collect(value reflect.Value) []any {
	if !value.IsValid() {
		return nil
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		if isIndexable(value) {
			return single(value)
		}
		return collect(value.Elem())

	case reflect.Slice, reflect.Array:
		out := make([]any, 0, value.Len())
		for i := range value.Len() {
			out = append(out, collect(value.Index(i))...)
		}
		return out

	case reflect.Struct:
		// Addressed, because a model's methods are almost always on the
		// pointer: a []Post whose *Post implements Indexable would otherwise
		// look like nothing to index.
		if value.CanAddr() {
			return single(value.Addr())
		}
		copied := reflect.New(value.Type())
		copied.Elem().Set(value)
		return single(copied)

	case reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return collect(value.Elem())
	}
	return nil
}

func isIndexable(pointer reflect.Value) bool {
	_, ok := pointer.Interface().(search.Indexable)
	return ok
}

// single returns the one model behind a pointer, unless it is a placeholder.
//
// A delete issued as a condition rather than against a loaded row —
// `db.Where(...).Delete(&Post{})` — leaves a wholly zero struct here. Its
// primary key is zero, so SearchKey() renders "0", which search.ValidKey
// accepts: unindexing it would remove whatever document happens to be keyed
// "0" rather than nothing. So a zero model is dropped.
//
// The check is on the struct rather than on the key, because what "no key"
// looks like is per model — "0" for an integer, "" for a uuid — and a zero
// struct is the same shape for all of them. A write whose model really is
// all zeroes has no key either, so nothing indexable is lost.
//
// That leaves a hole, and it is the same hole orm.DeleteAll has: a bulk
// delete is reconciled by a reimport, not by this.
func single(pointer reflect.Value) []any {
	if !isIndexable(pointer) || pointer.IsNil() {
		return nil
	}
	if pointer.Elem().Kind() == reflect.Struct && pointer.Elem().IsZero() {
		return nil
	}
	return []any{pointer.Interface()}
}
