package search

import (
	"context"
	"errors"
	"fmt"
)

// Errors an engine or the core returns.
var (
	// ErrUnsupported reports an operation an engine cannot perform. Always
	// better than a silent no-op: an import that reports success against an
	// engine with no index of its own is worse than one that refuses.
	ErrUnsupported = errors.New("search: operation not supported by this engine")

	// ErrVisibilityUnenforceable reports that an engine cannot enforce a
	// source's visibility rule, so it refuses the source rather than
	// searching rows it has no way to filter.
	//
	// Returning fewer sources than asked for is a visible failure; returning
	// somebody's private post to a stranger is not.
	ErrVisibilityUnenforceable = errors.New("search: this engine cannot enforce the source's visibility rule")

	// ErrSourceMismatch reports a descriptor declared for another engine — a
	// Table, which is SQL, handed to an engine that reads none.
	ErrSourceMismatch = errors.New("search: this source was declared for a different engine")

	// ErrBadKey reports a document key an engine cannot store. See ValidKey.
	ErrBadKey = errors.New("search: a document key must be alphanumerics, hyphen or underscore")

	// ErrBadViewer reports a Query.Viewer of a type no engine can bind.
	ErrBadViewer = errors.New("search: a viewer must be nil, a string, an integer or a []byte")

	// ErrBadField reports a filter naming something that is not a plain
	// field name. See ValidField.
	ErrBadField = errors.New("search: a filter field must be a plain unqualified name")

	// ErrBadFilter reports a filter an engine cannot act on — an unknown
	// operator, the wrong number of values, or a value of a type no engine
	// can render.
	ErrBadFilter = errors.New("search: a filter is malformed")

	// ErrEngineContract reports an engine that broke its own contract, such
	// as returning a different number of results than it was given requests.
	// It is a bug in the engine, not in the caller's input.
	ErrEngineContract = errors.New("search: the engine broke its contract")
)

// Source is one searchable collection, from the core's point of view.
//
// An engine declares its own descriptor type — Table here, an index
// descriptor for an engine that owns its documents — and the core needs only
// the three things it cannot get from anywhere else: what to call the source
// in a Hit, what to label its group, and the visibility rule to hand the
// engine. Everything else is between a descriptor and the engine that
// understands it, which is why an engine type-asserts rather than reading
// fields.
//
// The method names are deliberately not Name, Label and Visible: Table has
// fields with those names, and a method cannot share a name with a field.
type Source interface {
	SourceName() string
	SourceLabel() string
	Visibility() Visibility
}

// Visibility is how a source restricts a search to the rows one viewer may
// see.
//
// It is required: a source without one is refused, because the safe default
// is unknowable from here and the unsafe one is "every row". Search is the
// one read path that touches every row in a table rather than the ones
// somebody navigated to, so it is where a forgotten rule surfaces.
//
// Two forms, and a source may declare both, because the engines that can
// enforce them cannot read each other's. Nothing here is parsed by this
// module: an engine takes the form it can evaluate and refuses a source that
// offers none it understands.
type Visibility struct {
	// SQL is an arbitrary predicate over the source's own rows, with
	// ViewerPlaceholder where the viewer's key belongs.
	//
	// Only an engine reading those rows can evaluate it, and that is the
	// point: the rules that matter are not column comparisons. A real one is
	// three nested EXISTS over a follow graph and two membership tables with
	// the viewer appearing twice, and a search that reimplemented it as
	// something simpler would be a second copy of a rule that is hard enough
	// to get right once.
	SQL string

	// Filter is an expression over the source's indexed fields in the
	// engine's own filter dialect, with ViewerPlaceholder likewise.
	//
	// Enough for "deleted = false AND published = true", and for
	// "author = {{viewer}} OR visibility = 'public'". Not enough for the
	// paragraph above, which is the honest limit of an index that does not
	// hold the follow graph.
	Filter string

	// AtIndexTime declares that the rule is enforced by what is in the index
	// rather than by a query: a Conditional model keeps the unreadable rows
	// out and removes them when they stop being readable.
	//
	// A source may set this only when every indexed document is visible to
	// every viewer. A published-or-not rule qualifies; a per-viewer rule
	// never does, and setting it for one is how an index becomes a
	// disclosure.
	AtIndexTime bool
}

// Declared reports whether any form of the rule was given. The zero
// Visibility declares nothing and is refused.
func (v Visibility) Declared() bool {
	return v.SQL != "" || v.Filter != "" || v.AtIndexTime
}

// Capabilities is what an engine can do.
//
// Every field is something a caller or the core branches on; nothing is here
// to be merely informative. And nothing here excuses an engine from Name,
// Capabilities, Supports or MultiSearch — there is no flag for opting out of
// those, which is what stops this struct becoming a list of excuses.
//
// It must be constant for the lifetime of the engine: callers are entitled to
// read it once at boot.
type Capabilities struct {
	// RelationalVisibility means this engine evaluates Visibility.SQL by
	// reading the rows themselves, so an arbitrary predicate is enforceable.
	RelationalVisibility bool

	// FilterVisibility means this engine evaluates Visibility.Filter, an
	// expression over indexed fields in its own dialect.
	FilterVisibility bool

	// ExactTotals means Group.Total is a count. False means it is an
	// estimate, and a page that renders it as "23 results" is asserting
	// something it was not told.
	ExactTotals bool

	// OffsetPaging means Query.Offset is honoured. False means only Cursor
	// is, which is the normal state of an engine that caps how deep it will
	// page.
	OffsetPaging bool

	// Prefix means Query.Prefix is honoured. An engine that always matches
	// prefixes, or never does, reports false and ignores the field.
	Prefix bool

	// Highlighting means the engine fills in RawHit.Headline. False means
	// Hit.Snippet is always empty, and a result list that leans on a snippet
	// for context needs a different layout on that engine.
	Highlighting bool

	// Filters means SearchRequest.Filters are honoured. An engine reporting
	// false must refuse a request carrying any, rather than returning
	// unfiltered rows.
	Filters bool

	// Asynchronous means a write is not immediately visible to a search, so
	// the engine owes a Settler. An engine that reports this and implements
	// no Settler cannot be tested: every index-then-search assertion is a
	// race, so the contract suite fails rather than skips.
	Asynchronous bool
}

// Engine is the driver contract: one backend that can answer a query.
//
// Four methods, where Laravel Scout's engine contract has twelve, and the
// eight that are absent are not simplifications. mapIds, keys, map, lazyMap
// and getTotalCount exist in Scout because its search() returns PHP's mixed —
// the engine's own raw payload — so the engine has to be handed the payload
// back and asked what it meant. MultiSearch returns a typed SearchResult, so
// the keys, the totals and the ordering have nowhere left to hide. paginate()
// is search() with a page number, which SearchRequest carries. update(),
// delete(), flush(), createIndex() and deleteIndex() are real operations but
// not every engine has them, so they are the optional interfaces below rather
// than five methods a Postgres engine would implement as refusals.
//
// An Engine is built once at boot and shared by every request, so it must be
// safe for concurrent use.
//
// There is deliberately no Close. The Postgres engine is handed a Querier it
// does not own, and closing it would close the application's pool. An engine
// that does own something implements Close itself and the provider
// type-asserts for it.
type Engine interface {
	// Name is the name this engine was registered under. It exists so a
	// command can say which backend it just flushed, and so an error about a
	// refused source can name the engine that refused it.
	Name() string

	// Capabilities describes what this engine can and cannot do.
	Capabilities() Capabilities

	// Supports reports whether this engine can serve a source, and refuses
	// with a reason when it cannot.
	//
	// This is where an engine that cannot evaluate a source's visibility rule
	// says so, before any query runs — rather than searching rows it has no
	// way to filter and returning somebody's private post to a stranger. It
	// must do no I/O and must not be slow: it is called once per source on
	// every search.
	Supports(src Source) error

	// MultiSearch answers every request and returns exactly one result per
	// request, in the same order.
	//
	// Not named Search, and that is forced rather than stylistic: *Postgres
	// keeps Search(ctx, Query, ...Table) for the callers it already has, and
	// one type cannot have two methods of one name.
	//
	// Batched because for an engine over a network the round trip dominates:
	// eight sources is either eight sequential HTTP requests or one
	// multi-search. Postgres runs its eight statements in sequence on one
	// connection and loses nothing by being asked this way.
	//
	// The text in a request is already trimmed, bounded to MaxQueryBytes and
	// known to contain something searchable, so an engine never has to defend
	// against an empty or enormous query. What an engine must never do is
	// return an error because of the content of that text: a search box
	// answers 500 the first time somebody types "a & | b" otherwise, which is
	// what websearch_to_tsquery exists to prevent.
	MultiSearch(ctx context.Context, reqs []SearchRequest) ([]SearchResult, error)
}

// The interfaces below are capabilities an engine opts into. Callers
// type-assert for them and fall back to ErrUnsupported, which is how
// search:import against the Postgres engine says there is nothing to import
// rather than reporting that it imported nothing.

// Indexer is implemented by engines that keep their own copy of the
// documents.
//
// The Postgres engine does not: it searches the application's rows where they
// already are, which is the premise of v0.1.0 and the reason an application
// can adopt this module with no import step at all.
type Indexer interface {
	// Index writes documents, creating or replacing each wholly by Key.
	//
	// Not a merge. A model that stops setting a field must lose it, or the
	// old value stays findable forever — which is the same class of bug as a
	// stale document for a row that is no longer searchable.
	Index(ctx context.Context, docs []Document) error

	// Unindex removes documents by key.
	//
	// Named Unindex rather than Delete so it cannot be confused with
	// DeleteIndex, which removes the index itself and everything in it.
	//
	// Removing a key that is not there is not an error, and that is load
	// bearing rather than lenient: it is what lets Sync answer "this is no
	// longer searchable" with an unconditional Unindex and no before-state.
	Unindex(ctx context.Context, index string, keys []string) error

	// Flush empties one index without removing it or its configuration, and
	// must empty only what this engine's configured prefix owns.
	Flush(ctx context.Context, index string) error
}

// IndexManager is implemented by engines whose indexes must be created and
// configured before they are written to — which attribute is the primary key,
// which are searchable, which can be filtered on. An engine that infers all
// of that from the first document it is given does not implement it.
type IndexManager interface {
	CreateIndex(ctx context.Context, index string, opts IndexOptions) error
	DeleteIndex(ctx context.Context, index string) error
}

// IndexOptions is how an index is configured, in engine-neutral terms. An
// engine ignores what it has no concept of.
type IndexOptions struct {
	// Searchable are the fields matched against the query text.
	Searchable []string

	// Filterable are the fields a Filter or a Visibility.Filter may name. An
	// engine that requires attributes to be declared before they can be
	// filtered on — Meilisearch does — errors loudly at search time
	// otherwise, which is the good failure.
	Filterable []string

	// Sortable are the fields an engine may order by.
	Sortable []string
}

// Settler is implemented by an engine whose writes are not immediately
// visible to a search.
//
// It exists for two readers. The contract suite needs it because the suite
// never sleeps and never retries an assertion: a retrying assertion cannot
// tell a slow engine from a broken one except by how long somebody is willing
// to wait, and whoever debugs it raises the timeout — which is how a suite
// stops asserting anything. And search:import needs it, so the command can
// say forty thousand documents are indexed rather than that forty thousand
// were accepted.
//
// That second reader is why this is on the engine and not a test hook: the
// engine is the only thing that knows what it is waiting for.
type Settler interface {
	// Settle blocks until every write this engine has accepted is visible to
	// a search, or the context is done.
	Settle(ctx context.Context) error
}

// SearchRequest is one source's share of one query, resolved.
//
// A separate type from Query because Query is what a caller fills in, with
// zero values meaning "you decide", and a request is what an engine is given,
// with every decision already made. Resolving the page size once, here, is
// what stops each engine from having its own forgotten clamp.
type SearchRequest struct {
	Source Source

	// Text is trimmed, at most MaxQueryBytes, and contains at least one
	// letter or digit. It is bound as a parameter, never interpolated.
	Text string

	Prefix bool

	// Viewer is bound to the source's visibility rule where
	// ViewerPlaceholder appears. Nil is the anonymous viewer, which every
	// rule has to handle.
	Viewer any

	// Filters are ANDed. See Filter on why there is no OR.
	Filters []Filter

	// Limit is already clamped and always positive. An engine that receives a
	// non-positive limit has been called by something other than this
	// package and should treat it as a programming error rather than
	// substituting a default of its own.
	Limit int

	Offset int

	// Cursor is an alternative to Offset for an engine that reports
	// OffsetPaging false.
	Cursor Cursor
}

// SearchResult is one source's answer.
type SearchResult struct {
	Hits []RawHit

	// Total is how many rows matched, not how many were returned.
	Total int

	// Estimated says whether Total is a count or a guess.
	Estimated bool

	// More is whether a next page exists.
	//
	// Reported by the engine rather than derived from Total, because with an
	// estimated total there is nothing correct to derive it from: an estimate
	// below the offset hides a Next button that should be there, and
	// "a full page came back" shows one leading nowhere whenever the match
	// count is exactly a multiple of the page size. The engine knows.
	More bool
}

// RawHit is what an engine returns for one document.
//
// It is deliberately not a Hit. Hit.Snippet is a template.HTML — a promise
// that the string is safe to write into a page — and that promise is kept by
// exactly one function in this module, Snippet, which escapes the document
// before it introduces the <mark> elements. If an engine filled in a Hit, a
// new engine could keep the promise the wrong way round and publish whatever
// is in somebody's bio, and nothing would fail until it was exploited. So an
// engine returns the matched text as text and the core escapes it.
type RawHit struct {
	// Key is the document id as the engine knows it. Always set.
	Key string

	// ID is the same id as an integer when the source has an integer key, and
	// zero when it does not.
	ID int64

	Title    string
	Subtitle string
	URL      string

	// Headline is the matched text with HighlightPreTag and HighlightPostTag
	// around the matches and no other markup.
	//
	// Empty for a source with no snippet, and empty when nothing in the
	// snippetted fields matched — an unhighlighted lead fragment presented as
	// evidence of a match is worse than no evidence.
	Headline string

	// Rank is this engine's score for this document. No scale is defined
	// across engines; see Hit.Rank.
	Rank float64
}

// Filter restricts a search to documents whose field compares as given.
//
// Field names are written by the application and values come from a request —
// the same split as Table's, and for the same reason: a field name is an
// identifier and cannot be a bind parameter, so it is interpolated, so it is
// validated against a conservative pattern and refused otherwise. Values are
// always bound or quoted by the engine.
//
// Filters are ANDed, with no grouping, no OR and no nesting. An
// engine-portable boolean tree is a filter DSL, and the rules that need one —
// "a post in a circle, unless the circle is private and you are not in it" —
// are exactly the rules no DSL can express. Those belong in Visibility, where
// each engine gets the dialect it can actually evaluate.
//
// That refusal is written down rather than left as a gap, so adding OR is a
// decision somebody has to reverse.
type Filter struct {
	Field string
	Op    FilterOp

	// Values holds one value for every Op except OpIn and OpNotIn.
	Values []any
}

// FilterOp is a comparison.
type FilterOp uint8

const (
	OpEq FilterOp = iota + 1
	OpNe
	OpLt
	OpLte
	OpGt
	OpGte
	OpIn
	OpNotIn
)

// String names the operator, for an error message.
func (op FilterOp) String() string {
	switch op {
	case OpEq:
		return "="
	case OpNe:
		return "!="
	case OpLt:
		return "<"
	case OpLte:
		return "<="
	case OpGt:
		return ">"
	case OpGte:
		return ">="
	case OpIn:
		return "in"
	case OpNotIn:
		return "not in"
	}
	return "unknown"
}

// ValidField reports whether a name may be used as a filter field.
//
// Letters, digits and underscore, not starting with a digit — which is the
// intersection of what an unquoted SQL identifier and a Meilisearch
// attribute both accept, so a filter that works against one engine works
// against the others.
//
// This exists because a field name cannot be a bind parameter: every engine
// here interpolates it into a statement or an expression, so it is the one
// part of a filter that is not escaped on the way through. Validating against
// a conservative pattern and refusing everything else is what makes that
// safe, and refusing is the whole of it — there is no transformation, because
// a quietly rewritten field name queries something the caller did not name.
func ValidField(field string) error {
	if field == "" {
		return fmt.Errorf("%w: a field name may not be empty", ErrBadField)
	}
	for i, r := range field {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return fmt.Errorf("%w: %q contains %q", ErrBadField, field, r)
		}
	}
	return nil
}

// ValidValue reports whether a filter value is of a type every engine here
// can render or bind.
//
// Deliberately narrow. A value of some other type would be rendered by
// whatever fmt.Sprint makes of it, which differs per engine and silently
// matches nothing — a struct becomes "{7 true}" in one engine's expression
// and a bind error in another's statement. Refusing it here means the caller
// finds out at the call site instead of from an empty result page.
//
// Nil is refused too, and that is the one worth stating. "Does this document
// lack the field" and "does this field equal nothing" are different
// questions, engines answer them with different syntax or not at all, and a
// nil that each engine renders its own way is a filter whose meaning depends
// on which engine is configured. A field that may be absent is given a
// present default in SearchDocument instead.
func ValidValue(v any) error {
	switch v.(type) {
	case bool, string, []byte,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return nil
	}
	return fmt.Errorf("%w: a value of type %T", ErrBadFilter, v)
}

// Validate checks a filter before any engine is handed it.
//
// Every engine calls this rather than carrying its own copy of the rules,
// so a filter that one engine refuses is refused by all of them — which is
// what makes "this works on the other engine" a true statement rather than
// an accident of which checks each one happened to write.
func (f Filter) Validate() error {
	if err := ValidField(f.Field); err != nil {
		return err
	}
	switch f.Op {
	case OpIn, OpNotIn:
		if len(f.Values) == 0 {
			return fmt.Errorf("%w: %s %s needs at least one value",
				ErrBadFilter, f.Field, f.Op)
		}
	case OpEq, OpNe, OpLt, OpLte, OpGt, OpGte:
		if len(f.Values) != 1 {
			return fmt.Errorf("%w: %s %s takes one value, got %d",
				ErrBadFilter, f.Field, f.Op, len(f.Values))
		}
	default:
		return fmt.Errorf("%w: unknown operator %d on %s", ErrBadFilter, f.Op, f.Field)
	}
	for _, v := range f.Values {
		if err := ValidValue(v); err != nil {
			return fmt.Errorf("%s %s: %w", f.Field, f.Op, err)
		}
	}
	return nil
}

// ValidateFilters checks every filter in a request.
func ValidateFilters(filters []Filter) error {
	for _, f := range filters {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return nil
}
