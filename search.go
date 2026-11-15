// Package search is full-text search with a pluggable engine.
//
// Two layers, and the seam between them is the whole design.
//
// The **core** owns the shape of an answer: one Group per Source in the order
// asked, an empty query answered without touching the engine, every snippet
// escaped before it is marked up, the page size clamped once. An engine
// cannot opt out of any of it, because an engine never builds a Group or a
// Hit — it returns RawHits and the core turns them into results.
//
// An **engine** owns how documents are matched, and declares its own
// descriptor for them. The Postgres engine here takes a Table of SQL
// fragments and searches tsvector columns an application already has;
// search/engine/meili takes an index name and a filter expression;
// search/engine/memory keeps everything in this process. Query, Hit, Group
// and Results carry over unchanged, which is what makes changing engine a
// configuration change rather than a rewrite.
//
// Four things here are not conveniences.
//
// **The query never throws.** to_tsquery raises a syntax error on input like
// "a & | b", so a search box wired to it answers 500 for a string somebody
// can type by accident. Everything in this package goes through
// websearch_to_tsquery, which parses anything and is the only one of the four
// conversion functions designed for untrusted input. The conformance suite
// holds every engine to the same rule.
//
// **A snippet is escaped before it is marked up.** ts_headline returns the
// document — somebody's bio, a comment, a job description — with delimiters
// around the matches. Interpolating that into a page is an injection of user
// content by construction, so Snippet escapes first and introduces the <mark>
// elements afterwards, and the marker strings it asks for are chosen so they
// cannot survive escaping as anything else. An engine returns the matched
// text as text; the core is the one place a template.HTML is minted. See
// highlight.go and RawHit.
//
// **Results are grouped by source, never blended into one list.** A relevance
// score is a number about one document against one query; it is not
// comparable across documents of different shapes. A four-word product
// tagline and a three-paragraph bio that match equally well get ranks that
// differ by an order of magnitude, and merging them produces an ordering that
// looks authoritative and means nothing.
//
// **Visibility is required, not optional.** A Source with no rule is refused
// rather than quietly searching every row, and an engine that cannot enforce
// the rule it is given refuses the source rather than searching rows it has
// no way to filter. Search is where a forgotten visibility rule surfaces,
// because it is the one read path that touches every row in a table rather
// than the ones somebody navigated to.
//
// The grouped API is what a search box needs:
//
//	profiles := search.Table{
//	    Name: "profiles", Label: "People",
//	    From: "users", Vector: "search_vector",
//	    Title: "name", Subtitle: "headline", Snippet: []string{"headline", "bio"},
//	    Visible: "deleted_at IS NULL AND profile_published",
//	}
//
//	results, err := search.New(db).Search(ctx, search.Query{Text: "postgres"}, profiles, launches)
//
// For is what everything else needs — one source, filtered, hydrated into the
// model the application already has:
//
//	posts, err := search.For[*Post](engine, postsIndex, "postgres").
//	    Where("status", "published").
//	    Take(20).
//	    Hydrate(search.HydratorFunc[*Post](repo.LoadByKeys)).
//	    Get(ctx)
//
// This package is the standard library and nothing else. An engine needing a
// client library lives in its own package, registered from an init and
// reached through search/engines, the way database/sql drivers are;
// search/provider is where the api/app dependency lives. deps_test.go asserts
// both halves.
package search

import (
	"context"
	"errors"
	"html/template"
)

// Errors this package returns.
var (
	// ErrNoVisibility means a Table was declared without a Visible
	// predicate. See the package doc: this is refused rather than defaulted,
	// because the safe default is unknowable from here and the unsafe one is
	// "every row".
	ErrNoVisibility = errors.New("search: a table must declare a Visible predicate")
	// ErrNoTable means Search was called with no sources.
	ErrNoTable = errors.New("search: no tables to search")
	// ErrBadTable means a Table is missing something structural.
	ErrBadTable = errors.New("search: a table is incompletely declared")
)

// DefaultPerSource is how many hits one source returns when Query says
// nothing.
//
// Five, because the grouped layout this produces is a set of short lists with
// a "more" link, not a page of one kind of thing. A source whose results are
// the point is searched on its own with a larger Limit.
const DefaultPerSource = 5

// MaxPerSource caps what a caller can ask for from one source.
//
// A search is an unauthenticated read over a whole table on most sites, so
// the page size is a lever worth not handing to callers unbounded.
const MaxPerSource = 100

// Query is what somebody typed and how much to return.
type Query struct {
	// Text is the raw input. It is never interpolated into SQL — it is a bind
	// parameter to websearch_to_tsquery, which accepts anything.
	Text string

	// Limit is hits per source. Zero means DefaultPerSource.
	Limit int

	// Offset pages within one source. Only meaningful when searching a single
	// source: across several, each has its own offset and a shared one would
	// mean nothing.
	Offset int

	// Prefix matches an incomplete final word, for an as-you-type box.
	//
	// Off by default because it changes what the query means: with it,
	// "go" matches "golang" and "gorilla", which is right while somebody is
	// still typing and wrong once they have pressed enter.
	Prefix bool

	// Viewer is the id passed to each table's Visible predicate as $1. Zero
	// is the anonymous viewer, which every predicate has to handle.
	// Widened from int64 so that a uuid key works and a filter expression
	// can take it as a string. The cost is that a wrong type is no longer a
	// compile error, which validateViewer buys back by refusing it before
	// any query runs rather than letting it surface from inside a driver.
	//
	// Nil is the anonymous viewer and binds SQL NULL, so a predicate like
	// `f.follower_id = {{viewer}}` is never true for one — which is correct,
	// and is why it must not be "fixed" to 0.
	Viewer any
}

// perSource resolves the page size.
func (q Query) perSource() int { return ClampPerSource(q.Limit) }

// ClampPerSource applies DefaultPerSource and MaxPerSource.
//
// Exported because the core resolves the page size once, before handing a
// SearchRequest to an engine, so that each engine does not carry its own
// forgotten clamp. Not to be confused with ClampLimit, which bounds a
// batch size for an import.
func ClampPerSource(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPerSource
	case limit > MaxPerSource:
		return MaxPerSource
	default:
		return limit
	}
}

// Hit is one result.
type Hit struct {
	// Source is the Table.Name it came from, so a renderer can group and
	// label without consulting anything else.
	Source string `json:"source"`
	// Key is the document id as the engine knows it, and is always set.
	//
	// The field to use. An engine whose ids are strings — a uuid, a slug —
	// can only fill this one, and ID is zero for every hit.
	Key string `json:"key"`

	// ID is Key parsed as an integer, or zero when it does not parse.
	//
	// It predates Key and keeps its type deliberately: a handler putting it
	// into a JSON payload is putting a number there, and changing it to a
	// string would compile everywhere and silently turn a numeric prop into
	// a string one in whatever renders it. A new caller should use Key.
	ID int64 `json:"id"`

	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`

	// URL is where the hit lives, from Table.URL. Empty when the table did
	// not declare one.
	URL string `json:"url"`

	// Snippet is the matched text with <mark> around the matches. It is
	// already escaped — see highlight.go — and is the only field in this
	// package that is HTML rather than text.
	Snippet template.HTML `json:"snippet"`

	// Rank is ts_rank_cd for this row against this query. Comparable within
	// a source and meaningless across them; exposed because a caller
	// re-sorting within one source is legitimate.
	Rank float64 `json:"rank"`
}

// Group is one source's hits.
type Group struct {
	Source string `json:"source"`
	Label  string `json:"label"`
	Hits   []Hit  `json:"hits"`

	// Total is how many rows matched, not how many are returned.
	//
	// Counted with a second query, deliberately: a window function would get
	// it in one, but then the count is computed over the matched set on every
	// page of a paginated search, and the count is the half nobody scrolls.
	Total int `json:"total"`

	// Estimated reports that Total is not a count but an engine's guess,
	// which is what every external engine returns. A page rendering an
	// estimate as "23 results" is asserting something it was not told, so it
	// wants "about 23" — or nothing — when this is set.
	Estimated bool `json:"estimated"`

	// More is whether Total exceeds what was returned, which is what a "see
	// all" link should be driven by rather than by comparing numbers in a
	// template.
	More bool `json:"more"`
}

// Results is the whole answer.
type Results struct {
	// Text is the query as typed, echoed back so a page can say what it
	// searched for without trusting its own URL.
	Text string `json:"text"`

	// Groups are in the order the tables were passed, not in rank order.
	// See the package doc on why they are not merged.
	Groups []Group `json:"groups"`

	// Total is the sum across groups.
	Total int `json:"total"`

	// Empty is whether the query had nothing searchable in it, which is
	// different from having found nothing: one asks for a prompt and the
	// other for "no results".
	Empty bool `json:"empty"`
}

// Searcher is the engine.
//
// An interface so that the Postgres implementation here can be replaced by
// one fronting a dedicated engine without the application's handlers
// changing. The Table descriptors are Postgres-shaped, which is the honest
// seam: a Meilisearch implementation would take its own descriptors and the
// Query, Hit, Group and Results types would carry over unchanged.
type Searcher interface {
	Search(ctx context.Context, q Query, tables ...Table) (*Results, error)
}
