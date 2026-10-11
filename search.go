// Package search finds things in a Postgres database that already has
// tsvector columns.
//
// It is deliberately not an indexing engine. An application that has added
// generated tsvector columns and GIN indexes — which is the cheap thing to do
// up front and an expensive retrofit later — already has an index; what it
// lacks is a safe way to turn what somebody typed into a query against it,
// and a shape for the answer. That is what this is.
//
// Four things here are not conveniences.
//
// **The query never throws.** to_tsquery raises a syntax error on input like
// "a & | b", so a search box wired to it answers 500 for a string somebody
// can type by accident. Everything in this package goes through
// websearch_to_tsquery, which parses anything and is the only one of the four
// conversion functions designed for untrusted input.
//
// **A snippet is escaped before it is marked up.** ts_headline returns the
// document — somebody's bio, a comment, a job description — with delimiters
// around the matches. Interpolating that into a page is an injection of user
// content by construction, so Snippet escapes first and introduces the <mark>
// elements afterwards, and the marker strings it asks Postgres for are chosen
// so they cannot survive escaping as anything else. See highlight.go.
//
// **Results are grouped by source, never blended into one list.** ts_rank is
// a number about one document against one query; it is not comparable across
// documents of different shapes. A four-word product tagline and a
// three-paragraph bio that match equally well get ranks that differ by an
// order of magnitude, and merging them produces an ordering that looks
// authoritative and means nothing.
//
// **Visibility is required, not optional.** A Table with no Visible predicate
// is refused at construction rather than quietly searching every row. Search
// is where a forgotten visibility rule surfaces, because it is the one read
// path that touches every row in a table rather than the ones a person
// navigated to.
//
//	profiles := search.Table{
//	    Name: "profiles", Label: "People",
//	    From: "users", Vector: "search_vector",
//	    Title: "name", Subtitle: "headline", Snippet: []string{"headline", "bio"},
//	    Visible: "deleted_at IS NULL AND profile_published",
//	}
//
//	results, err := search.New(db).Search(ctx, search.Query{Text: "postgres"}, profiles, launches)
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
	Viewer int64
}

// perSource resolves the page size.
func (q Query) perSource() int {
	switch {
	case q.Limit <= 0:
		return DefaultPerSource
	case q.Limit > MaxPerSource:
		return MaxPerSource
	default:
		return q.Limit
	}
}

// Hit is one result.
type Hit struct {
	// Source is the Table.Name it came from, so a renderer can group and
	// label without consulting anything else.
	Source string `json:"source"`
	ID     int64  `json:"id"`

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
