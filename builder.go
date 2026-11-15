package search

import (
	"context"
	"errors"
	"fmt"
)

// Errors the builder returns for its own misuse, before any engine is asked.
var (
	// ErrNoHydrator means Get or Each was called on a builder that was never
	// told how to load a model. There is no default, because this module has
	// no ORM and inventing one is what would make it unusable from anything.
	ErrNoHydrator = errors.New("search: no hydrator; call Hydrate before Get or Each")

	// ErrPagingConflict means both Skip and After were given. They are two
	// answers to the same question and an engine honours exactly one of them,
	// so taking both and silently preferring one is how a page of results
	// turns out to be a different page than the caller asked for.
	ErrPagingConflict = errors.New("search: Skip and After are alternatives; use the one the engine supports")
)

// For starts a typed search over one source.
//
// The grouped API answers "what is on this site about X" across every source
// at once, which is what a search box needs. This answers "which of these
// does X" about one collection, hydrated into the model the application
// already has, which is what everything else needs — a filtered list, an
// autocomplete, a background job walking matches.
//
//	posts, err := search.For[*Post](engine, postsIndex, "postgres").
//	    Where("status", "published").
//	    Take(20).
//	    Hydrate(search.HydratorFunc[*Post](loadPosts)).
//	    Get(ctx)
//
// It is a free function and not a method, because T is the model type and a
// generic method can never satisfy an interface — so putting this on Finder
// would stop Finder being reachable through SourceSearcher. That is the same
// constraint cache.Cache records, arriving from the other side.
//
// Misuse is collected rather than returned from each call: a chain cannot
// report an error without a variable at every link, so the first one is kept
// and reported by whichever terminal is reached. A builder that is never
// terminated reports nothing, which is correct — nothing happened.
func For[T Indexable](e Engine, src Source, text string) *Builder[T] {
	b := &Builder[T]{engine: e, source: src, text: text}
	if e == nil {
		b.err = errors.New("search: For was given a nil engine")
	}
	if src == nil {
		b.err = ErrNoTable
	}
	return b
}

// Builder is a search over one source, answered as models.
//
// Not safe for concurrent use, and each method returns the same builder
// rather than a copy: a chain is built in one place and then run, and
// copy-on-write would mean a caller who holds an intermediate builder gets a
// different query than the one they are looking at.
type Builder[T Indexable] struct {
	engine   Engine
	source   Source
	text     string
	prefix   bool
	viewer   any
	filters  []Filter
	limit    int
	offset   int
	after    string
	hydrator Hydrator[T]

	// err is the first misuse, reported by the terminal.
	err error
}

// Prefix matches an incomplete final word, for an as-you-type box. See
// Query.Prefix for why it is off by default.
func (b *Builder[T]) Prefix() *Builder[T] { b.prefix = true; return b }

// Viewer is whose visibility the search runs under. Nil, the default, is the
// anonymous viewer — which is the safe default and the reason this is not
// inferred from anything.
func (b *Builder[T]) Viewer(v any) *Builder[T] {
	if err := validateViewer(v); err != nil && b.err == nil {
		b.err = err
	}
	b.viewer = v
	return b
}

// Where restricts to documents whose field equals value.
func (b *Builder[T]) Where(field string, value any) *Builder[T] {
	return b.filter(field, OpEq, value)
}

// WhereNot restricts to documents whose field does not equal value.
func (b *Builder[T]) WhereNot(field string, value any) *Builder[T] {
	return b.filter(field, OpNe, value)
}

// WhereLt, WhereLte, WhereGt and WhereGte compare a field against one value.
func (b *Builder[T]) WhereLt(field string, value any) *Builder[T] {
	return b.filter(field, OpLt, value)
}

func (b *Builder[T]) WhereLte(field string, value any) *Builder[T] {
	return b.filter(field, OpLte, value)
}

func (b *Builder[T]) WhereGt(field string, value any) *Builder[T] {
	return b.filter(field, OpGt, value)
}

func (b *Builder[T]) WhereGte(field string, value any) *Builder[T] {
	return b.filter(field, OpGte, value)
}

// WhereIn restricts to documents whose field is one of values.
//
// An empty set is a mistake rather than a filter that matches nothing: a
// caller passing a slice that happened to be empty almost always means "no
// restriction", and an engine that read it as "match nothing" would answer an
// empty page that looks like a corpus problem. So it is refused.
func (b *Builder[T]) WhereIn(field string, values ...any) *Builder[T] {
	if len(values) == 0 {
		return b.fail(fmt.Errorf("search: WhereIn(%q) was given no values", field))
	}
	return b.filter(field, OpIn, values...)
}

// WhereNotIn restricts to documents whose field is none of values.
func (b *Builder[T]) WhereNotIn(field string, values ...any) *Builder[T] {
	if len(values) == 0 {
		return b.fail(fmt.Errorf("search: WhereNotIn(%q) was given no values", field))
	}
	return b.filter(field, OpNotIn, values...)
}

// Take is how many hits to return. Zero means DefaultPerSource and anything
// above MaxPerSource is clamped to it.
func (b *Builder[T]) Take(n int) *Builder[T] { b.limit = n; return b }

// Skip pages by offset, for an engine whose Capabilities report OffsetPaging.
func (b *Builder[T]) Skip(n int) *Builder[T] { b.offset = n; return b }

// After resumes from a document key, for an engine whose Capabilities report
// OffsetPaging false.
//
// Against an engine that pages by offset this is refused rather than ignored,
// which is the only honest answer: a cursor an engine silently discards is a
// walk that returns page one forever.
func (b *Builder[T]) After(key string) *Builder[T] { b.after = key; return b }

// Hydrate is how to load the models for the keys an engine returns.
//
// Required by Get and Each, and unused by Keys and Raw. Load may return the
// models in any order and may return fewer than it was asked for — see
// Hydrator — and this package restores the engine's order afterwards.
func (b *Builder[T]) Hydrate(h Hydrator[T]) *Builder[T] { b.hydrator = h; return b }

// Keys returns the matching document keys, in the engine's order.
//
// The cheapest terminal: no models are loaded, so it is what a caller wants
// when the keys are the answer — a set intersection, a job queueing one task
// per match, an existence check.
func (b *Builder[T]) Keys(ctx context.Context) ([]string, error) {
	res, err := b.raw(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		keys = append(keys, hit.Key)
	}
	return keys, nil
}

// Raw returns the engine's own answer as a Group, snippets and totals
// included, without loading any model.
//
// For rendering a result list straight from the index, which is what an
// engine holding title, subtitle and URL in the document is for. A Group
// rather than a Results, because a builder searches exactly one source and
// wrapping one group in the grouped shape would invite a caller to index into
// Groups[0] forever.
func (b *Builder[T]) Raw(ctx context.Context) (Group, error) {
	res, err := b.raw(ctx)
	if err != nil {
		return Group{}, err
	}
	return groupOf(b.source, res), nil
}

// Get returns the matching models, in the engine's order.
//
// Restoring that order is the whole reason this is not two lines at the call
// site. A hydrator loading by key issues something shaped like
// `WHERE key IN (...)`, and a database returns those rows in whatever order
// its plan produces — so hydrating without re-sorting silently discards the
// ranking the search was for, and the symptom is a result list that looks
// ordered by id. It is a quiet bug: every row is correct, only the order is
// wrong, and nothing fails.
//
// A key with no model is dropped rather than returned as a zero value: a
// document can outlive its row, briefly after a delete or indefinitely after
// one that was never synced, and a nil in the middle of a result list is a
// panic at the call site rather than a missing entry.
func (b *Builder[T]) Get(ctx context.Context) ([]T, error) {
	if b.hydrator == nil {
		return nil, ErrNoHydrator
	}
	res, err := b.raw(ctx)
	if err != nil {
		return nil, err
	}
	return b.hydrate(ctx, res.Hits)
}

// Each walks every matching model, page by page, calling fn in the engine's
// order and stopping at the first error fn returns.
//
// Pages by whichever mechanism the engine supports, so a caller writes no
// paging. The page size is Take or DefaultPerSource, in the per-source
// vocabulary rather than the import one, because this walks a result set and
// not a corpus — see Importable and search:import for that.
//
// A walk over an index that is being written to may repeat a document or miss
// one, and under offset paging it will: a document indexed above the cursor
// shifts every later page down by one. That is the same caveat Cursor
// records, and it is why a reindex uses Importable and not this.
func (b *Builder[T]) Each(ctx context.Context, fn func(T) error) error {
	if b.hydrator == nil {
		return ErrNoHydrator
	}
	if err := b.check(); err != nil {
		return err
	}
	if !Searchable(b.text) {
		return nil
	}

	size := ClampPerSource(b.limit)
	offset, after := b.offset, b.after

	for {
		req := b.request(size, offset, after)
		res, err := one(ctx, b.engine, req)
		if err != nil {
			return err
		}
		if len(res.Hits) == 0 {
			return nil
		}

		models, err := b.hydrate(ctx, res.Hits)
		if err != nil {
			return err
		}
		for _, m := range models {
			if err := fn(m); err != nil {
				return err
			}
		}

		// Stop on the engine's own answer rather than on a count comparison.
		// With an estimated total there is nothing correct to compare, which
		// is the reason SearchResult.More is reported rather than derived.
		if !res.More {
			return nil
		}
		// Both advance, and the engine reads the one its capability
		// declares — which check() has already made sure is the one the
		// caller did not supply. One branch fewer here than there are
		// paging mechanisms.
		offset += len(res.Hits)
		after = res.Hits[len(res.Hits)-1].Key
	}
}

// raw runs the search and returns the engine's answer for the one source.
func (b *Builder[T]) raw(ctx context.Context) (SearchResult, error) {
	if err := b.check(); err != nil {
		return SearchResult{}, err
	}
	// Nothing searchable reaches no engine, exactly as the grouped path
	// answers an empty box with the shape and no queries.
	if !Searchable(b.text) {
		return SearchResult{}, nil
	}
	return one(ctx, b.engine, b.request(ClampPerSource(b.limit), b.offset, b.after))
}

// request resolves the builder into the one shape an engine is given.
func (b *Builder[T]) request(limit, offset int, after string) SearchRequest {
	return SearchRequest{
		Source:  b.source,
		Text:    normalise(b.text),
		Prefix:  b.prefix,
		Viewer:  b.viewer,
		Filters: b.filters,
		Limit:   limit,
		Offset:  offset,
		Cursor:  Cursor{AfterKey: after, Limit: limit},
	}
}

// check settles everything that can be refused before a query runs.
//
// Capabilities are consulted here rather than left to the engine, so the
// error names the engine and the thing it cannot do. An engine refusing for
// itself is the backstop, not the first line: a filter silently dropped by an
// engine that reads no filters is a page of results from a query nobody asked
// for.
func (b *Builder[T]) check() error {
	if b.err != nil {
		return b.err
	}
	if b.offset != 0 && b.after != "" {
		return ErrPagingConflict
	}

	caps := b.engine.Capabilities()
	name := b.engine.Name()

	if len(b.filters) > 0 && !caps.Filters {
		return fmt.Errorf("%w: the %s engine honours no filters, so Where would be ignored",
			ErrUnsupported, name)
	}
	if b.prefix && !caps.Prefix {
		return fmt.Errorf("%w: the %s engine has no prefix matching", ErrUnsupported, name)
	}
	if b.offset != 0 && !caps.OffsetPaging {
		return fmt.Errorf("%w: the %s engine pages by cursor, so use After rather than Skip",
			ErrUnsupported, name)
	}
	if b.after != "" && caps.OffsetPaging {
		return fmt.Errorf("%w: the %s engine pages by offset, so use Skip rather than After",
			ErrUnsupported, name)
	}

	return b.engine.Supports(b.source)
}

// hydrate loads the models and restores the engine's order. See Get.
func (b *Builder[T]) hydrate(ctx context.Context, hits []RawHit) ([]T, error) {
	if len(hits) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(hits))
	for _, hit := range hits {
		keys = append(keys, hit.Key)
	}

	loaded, err := b.hydrator.Load(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("search: hydrating %d keys: %w", len(keys), err)
	}

	byKey := make(map[string]T, len(loaded))
	for _, m := range loaded {
		byKey[m.SearchKey()] = m
	}

	out := make([]T, 0, len(hits))
	for _, hit := range hits {
		if m, ok := byKey[hit.Key]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (b *Builder[T]) filter(field string, op FilterOp, values ...any) *Builder[T] {
	b.filters = append(b.filters, Filter{Field: field, Op: op, Values: values})
	return b
}

func (b *Builder[T]) fail(err error) *Builder[T] {
	if b.err == nil {
		b.err = err
	}
	return b
}

// one asks an engine for exactly one source's answer, and holds it to the
// same contract MultiSearch is held to everywhere else.
func one(ctx context.Context, e Engine, req SearchRequest) (SearchResult, error) {
	out, err := e.MultiSearch(ctx, []SearchRequest{req})
	if err != nil {
		return SearchResult{}, err
	}
	if len(out) != 1 {
		return SearchResult{}, fmt.Errorf("%w: the %s engine answered %d of 1 requests",
			ErrEngineContract, e.Name(), len(out))
	}
	return out[0], nil
}
