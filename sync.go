package search

import (
	"context"
	"errors"
	"fmt"
)

// Sync indexes models that should be searchable and removes the documents of
// models that should not.
//
// # Why a false ShouldBeSearchable removes rather than skips
//
// Scout has two more hooks than this — wasSearchableBeforeUpdate and
// wasSearchableBeforeDelete — and they exist for a real failure: a model that
// *stops* being searchable, whose last indexed copy then stays findable
// forever. Unpublish a post and it is still in the index; the only thing that
// would remove it is an event saying it changed, and the handler that skips
// non-searchable models has just discarded that event.
//
// Scout solves it by snapshotting the predicate before the change and
// comparing afterwards. That is unavailable here: none of the three ORMs
// reliably hands a hook the pre-change entity.
//
// So do not compare. Make the negative case authoritative: a model that says
// no is removed, unconditionally, with no before-state consulted. Removing a
// key that is not there is a no-op in every engine — Indexer.Unindex says so
// as part of the contract rather than as a convention — so the create case
// where nothing was ever indexed costs one request and is correct.
//
// The cost is a delete request every time somebody saves a draft. The benefit
// is that there is no path by which a stale document survives, including the
// path where the model was never searchable at creation and a
// compare-based design would never have indexed it to notice.
//
// Models need not share an index, and anything that is not Indexable is
// ignored: a global ORM observer sees every model in an application and most
// of them are not searchable.
func Sync(ctx context.Context, e Engine, models ...any) error {
	idx, err := indexerFor(e)
	if err != nil {
		return err
	}

	var (
		docs    []Document
		removes = map[string][]string{}
		errs    []error
	)

	for _, m := range models {
		model, ok := m.(Indexable)
		if !ok {
			continue
		}

		if cond, ok := model.(Conditional); ok && !cond.ShouldBeSearchable() {
			if err := ValidKey(model.SearchKey()); err != nil {
				errs = append(errs, fmt.Errorf("%T: %w", model, err))
				continue
			}
			index := model.SearchIndex()
			removes[index] = append(removes[index], model.SearchKey())
			continue
		}

		doc, err := DocumentOf(model)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		docs = append(docs, doc)
	}

	// Index before removing, so that in a batch holding both a new document
	// and a withdrawal of the same key the withdrawal wins. The reverse order
	// would leave it indexed.
	if len(docs) > 0 {
		if err := idx.Index(ctx, docs); err != nil {
			errs = append(errs, err)
		}
	}
	for index, keys := range removes {
		if err := idx.Unindex(ctx, index, keys); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Forget removes models' documents.
//
// It never consults Conditional: a deleted row has no business in an index
// whatever its predicate says. That also makes Scout's
// wasSearchableBeforeDelete vacuous here — Forget always forgets.
func Forget(ctx context.Context, e Engine, models ...any) error {
	idx, err := indexerFor(e)
	if err != nil {
		return err
	}

	removes := map[string][]string{}
	var errs []error
	for _, m := range models {
		model, ok := m.(Indexable)
		if !ok {
			continue
		}
		if err := ValidKey(model.SearchKey()); err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", model, err))
			continue
		}
		index := model.SearchIndex()
		removes[index] = append(removes[index], model.SearchKey())
	}

	for index, keys := range removes {
		if err := idx.Unindex(ctx, index, keys); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SyncOne is Sync for a call site that knows its type.
//
// Prefer it wherever the type is static: a model whose SearchDocument is
// misspelled satisfies nothing, and Sync's `...any` would silently never
// index it, where this is a build error.
func SyncOne[T Indexable](ctx context.Context, e Engine, model T) error {
	return Sync(ctx, e, model)
}

func indexerFor(e Engine) (Indexer, error) {
	idx, ok := e.(Indexer)
	if !ok {
		// Named rather than silently skipped: an application wiring sync
		// against an engine that searches its rows where they are has made a
		// wiring mistake, and the quiet version of this is a sync that
		// reports success forever while doing nothing.
		return nil, fmt.Errorf("%w: the %s engine keeps no index of its own, so "+
			"there is nothing to sync — it searches your rows where they are",
			ErrUnsupported, e.Name())
	}
	return idx, nil
}
