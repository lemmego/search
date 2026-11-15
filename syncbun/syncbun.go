// Package syncbun keeps a search engine's index in step with Bun.
//
//	syncbun.Watch(db, engine)
//
// # The hook fires inside the transaction, and Bun's sees less than GORM's
//
// Bun's query hooks run as part of the statement, so everything the
// syncgorm package documents about rollbacks applies here unchanged, and
// WithDefer is the same seam for the same reason.
//
// Bun has a second limitation GORM does not. A QueryEvent carries the model
// the query was built from, and for a statement built without one — a
// `NewDelete().Model((*Post)(nil)).Where(...)`, or any raw query — there is
// nothing identifiable to index or remove. Such a write is ignored rather
// than guessed at, and the repair is the same as for a bulk mutation
// anywhere else: search:import --fresh.
//
// So the order of preference is the one syncgorm gives: call search.Sync from
// the service layer after your own commit; or wire WithDefer; or accept the
// cost knowingly.
package syncbun

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/uptrace/bun"

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

// WithDefer hands each index write to fn instead of performing it. See
// syncgorm.WithDefer, which it mirrors exactly.
func WithDefer(fn func(ctx context.Context, apply func(context.Context) error)) Option {
	return func(s *syncer.Syncer) { s.Defer = fn }
}

// Watch installs the query hook.
func Watch(db *bun.DB, engine search.Engine, options ...Option) error {
	if db == nil || engine == nil {
		return fmt.Errorf("syncbun: Watch needs a database and an engine")
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

	db.AddQueryHook(&hook{sync: sync})
	return nil
}

type hook struct{ sync syncer.Syncer }

var _ bun.QueryHook = (*hook)(nil)

// BeforeQuery does nothing. The index is touched only once the statement has
// returned without error, which is the one half of the transaction problem
// this adapter can solve.
func (h *hook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *hook) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	if event == nil || event.Err != nil {
		return
	}

	models := flatten(event.Model)
	if len(models) == 0 {
		return
	}

	// Operation() is derived from the statement text, so it is the one
	// reading that works for a hand-written query as well as a built one.
	switch strings.ToUpper(event.Operation()) {
	case "INSERT", "UPDATE":
		h.sync.Index(ctx, models...)
	case "DELETE":
		h.sync.Remove(ctx, models...)
	}
	// Everything else — SELECT, DDL, whatever Bun could not name — is
	// ignored. A read must never write to the index, and a hook that reindexed
	// on SELECT would turn every page view into a write.
}

// flatten turns a QueryEvent's model into the models to sync.
//
// Bun does not hand a hook the caller's value directly: event.Model is an
// internal table model (*bun.structTableModel, *bun.sliceTableModel), and
// the struct behind it comes from the Value() method on bun's Model
// interface. Reaching for the table model itself finds nothing indexable,
// which is a silent no-op — and the tests that would have caught it pass
// vacuously, since "the document is absent" is true when nothing was ever
// indexed. Found exactly that way.
func flatten(model bun.Model) []any {
	valuer, ok := model.(interface{ Value() any })
	if !ok {
		return nil
	}
	return collect(reflect.ValueOf(valuer.Value()))
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
	}
	return nil
}

func isIndexable(pointer reflect.Value) bool {
	_, ok := pointer.Interface().(search.Indexable)
	return ok
}

// single returns the one model behind a pointer, unless it is a placeholder.
//
// Bun names the table for a conditional statement in two ways, and both
// arrive here as a placeholder rather than a row. `Model((*Post)(nil))`
// gives a typed nil; `Model(&Post{})` gives a non-nil pointer to a zero
// struct. Measured, not assumed — the second one is what makes the zero
// check load-bearing rather than defensive.
//
// Acting on either would be wrong, and the UPDATE case is the worse of the
// two: a zero model's SearchKey() renders "0", which search.ValidKey
// accepts, so a conditional update would write a document keyed "0" with
// every field empty and a conditional delete would remove whatever document
// is already keyed there.
//
// The check is on the struct rather than on the key, because what "no key"
// looks like is per model — "0" for an integer, "" for a uuid — and a zero
// struct is the same shape for all of them. A write whose model really is
// all zeroes has no key either, so nothing indexable is lost.
func single(pointer reflect.Value) []any {
	if !isIndexable(pointer) || pointer.IsNil() {
		return nil
	}
	if pointer.Elem().Kind() == reflect.Struct && pointer.Elem().IsZero() {
		return nil
	}
	return []any{pointer.Interface()}
}
