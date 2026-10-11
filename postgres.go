package search

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Querier is the database handle this package needs.
//
// Two methods from database/sql, so *sql.DB, *sql.Tx and *sql.Conn all
// satisfy it — and so this module depends on no ORM. An application on GORM,
// on Bun, on lemmego/orm or on none of them passes whatever it already has.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Table declares one searchable relation.
//
// Every field naming a column or a fragment of SQL is written by the
// application, never by a request: these are interpolated into the statement
// because an identifier cannot be a bind parameter. What somebody typed is
// always a parameter. Keeping those two apart is the whole reason this is a
// declarative descriptor rather than a query builder taking strings at
// request time.
type Table struct {
	// Name identifies the source in a Hit and a Group.
	Name string
	// Label is what a person reads above the group: "People", "Launches".
	Label string

	// From is the relation, optionally with a join:
	//
	//	From: "job_posts j JOIN companies c ON c.id = j.company_id"
	//
	// Alias the columns below to match.
	From string

	// Vector is the tsvector column, unqualified or aliased to match From.
	//
	// The predicate built from it is `<Vector> @@ <query>` against the bare
	// column, because that is the form the GIN index answers. Wrapping it in
	// a function — coalesce, a cast, to_tsvector of it — silently turns every
	// search into a sequential scan.
	Vector string

	// Config is the text search configuration, matching whatever the column
	// was built with. Defaults to "english".
	//
	// A column built from more than one configuration — 'simple' for a
	// handle and 'english' for a bio, which is a reasonable thing to do — can
	// only be queried through one of them, so a term that stems differently
	// in the two will not match the half it was not parsed for. That is
	// inherent to mixing them in one vector and is worth knowing rather than
	// working around.
	Config string

	// ID, Title and Subtitle are column expressions selected into the Hit.
	// Subtitle may be empty.
	ID       string
	Title    string
	Subtitle string

	// Snippet names the columns ts_headline draws from, concatenated in
	// order. Empty means no snippet, which is right for a source whose title
	// is the whole document.
	Snippet []string

	// Visible is a SQL predicate restricting the search to rows this viewer
	// may see. Required — see ErrNoVisibility.
	//
	// Write {{viewer}} where the viewer's id belongs; it is replaced with
	// the right placeholder and bound to Query.Viewer. A named marker rather
	// than a fixed $1 because the parameter numbering depends on what the
	// query needed, and because a predicate that does not care who is asking
	// — "deleted_at IS NULL AND published" — must not be forced to mention
	// the viewer anyway. Postgres refuses a statement that binds a parameter
	// it never references, so a fixed $1 made every such predicate carry a
	// no-op comparison to satisfy it.
	//
	// It is a string rather than a structured filter because the rules that
	// matter are not expressible as column comparisons: "a post in a circle,
	// unless the circle is private and you are not in it" is an EXISTS, and a
	// search that reimplemented it as something simpler would be a second
	// copy of a rule that is already hard to get right once.
	Visible string

	// URL is a column expression for the hit's address — `'/@' || handle`,
	// `'/launch/' || slug`. Optional, and worth setting: the alternative is
	// a switch on the source name in a renderer, which is the same knowledge
	// kept somewhere that cannot be tested with the query.
	URL string

	// Tiebreak orders rows of equal rank. Rank ties are common — two rows
	// matching one term in the same column rank identically — and without a
	// tiebreak their order is whatever the plan happens to produce, which
	// makes page 2 of a search unstable.
	//
	// Defaults to the ID column descending.
	Tiebreak string
}

func (t Table) config() string {
	if strings.TrimSpace(t.Config) == "" {
		return "english"
	}
	return t.Config
}

func (t Table) tiebreak() string {
	if strings.TrimSpace(t.Tiebreak) == "" {
		return t.ID + " DESC"
	}
	return t.Tiebreak
}

func (t Table) label() string {
	if t.Label == "" {
		return t.Name
	}
	return t.Label
}

// validate refuses a descriptor that cannot produce a correct query.
func (t Table) validate() error {
	switch {
	case strings.TrimSpace(t.Name) == "":
		return fmt.Errorf("%w: a table has no Name", ErrBadTable)
	case strings.TrimSpace(t.From) == "":
		return fmt.Errorf("%w: %q has no From", ErrBadTable, t.Name)
	case strings.TrimSpace(t.Vector) == "":
		return fmt.Errorf("%w: %q has no Vector", ErrBadTable, t.Name)
	case strings.TrimSpace(t.ID) == "":
		return fmt.Errorf("%w: %q has no ID", ErrBadTable, t.Name)
	case strings.TrimSpace(t.Title) == "":
		return fmt.Errorf("%w: %q has no Title", ErrBadTable, t.Name)
	case strings.TrimSpace(t.Visible) == "":
		return fmt.Errorf("%w: %q", ErrNoVisibility, t.Name)
	}
	return nil
}

// Postgres searches tsvector columns.
type Postgres struct {
	db Querier
}

// New returns a Searcher over this handle.
func New(db Querier) *Postgres { return &Postgres{db: db} }

// Search runs the query against each table and groups the results.
//
// One statement per table, run in sequence. Not a UNION ALL, and not
// concurrently:
//
// Not a union, because the ranks are not comparable across tables (see the
// package doc) so there is nothing to gain by merging them in SQL, and a
// union of eight differently-shaped selects has to coerce every column to a
// common type — which means casting ids and titles to text and losing the
// ability to order each group on its own terms.
//
// Not concurrently, because each query is an index scan answering in
// single-digit milliseconds and eight of them share one connection; fanning
// out over a pool to save six milliseconds trades a connection per source
// under load, which is the resource a search page is most likely to exhaust.
func (p *Postgres) Search(ctx context.Context, q Query, tables ...Table) (*Results, error) {
	if len(tables) == 0 {
		return nil, ErrNoTable
	}
	for _, table := range tables {
		if err := table.validate(); err != nil {
			return nil, err
		}
	}

	results := &Results{Text: q.Text, Groups: make([]Group, 0, len(tables))}

	// Nothing searchable: answer with the shape and no queries. Eight index
	// scans for an empty box is work nobody asked for, and the page needs to
	// know the difference between "type something" and "no results".
	if !Searchable(q.Text) {
		results.Empty = true
		for _, table := range tables {
			results.Groups = append(results.Groups,
				Group{Source: table.Name, Label: table.label(), Hits: []Hit{}})
		}
		return results, nil
	}

	for _, table := range tables {
		group, err := p.searchOne(ctx, q, table)
		if err != nil {
			return nil, fmt.Errorf("search %s: %w", table.Name, err)
		}
		results.Groups = append(results.Groups, group)
		results.Total += group.Total
	}

	return results, nil
}

// ViewerPlaceholder is the marker a Visible predicate uses for the viewer's
// id. See Table.Visible.
const ViewerPlaceholder = "{{viewer}}"

// tsquery builds the query expression and the arguments it binds, numbering
// placeholders from `from`.
func (p *Postgres) tsquery(q Query, config string, from int) (string, []any) {
	if !q.Prefix {
		// websearch_to_tsquery: handles quoted phrases, OR and -exclusion,
		// and cannot raise a syntax error whatever arrives.
		return fmt.Sprintf("websearch_to_tsquery('%s', $%d)", config, from),
			[]any{normalise(q.Text)}
	}

	terms := prefixTerms(q.Text)
	if len(terms) == 0 {
		return fmt.Sprintf("websearch_to_tsquery('%s', $%d)", config, from),
			[]any{normalise(q.Text)}
	}

	// to_tsquery is the only one that understands ":*", and the only one
	// that throws — so it is handed an expression assembled from terms
	// prefixTerms produced, each still bound as a parameter. The ":*" and
	// the "&" are ours; the words are theirs.
	pieces := make([]string, 0, len(terms))
	args := make([]any, 0, len(terms))
	for i, term := range terms {
		pieces = append(pieces, fmt.Sprintf("$%d || ':*'", from+i))
		args = append(args, term)
	}
	return fmt.Sprintf("to_tsquery('%s', %s)", config,
		strings.Join(pieces, " || ' & ' || ")), args
}

// visible resolves {{viewer}} and reports whether the viewer must be bound.
func visible(predicate string, at int) (string, bool) {
	if !strings.Contains(predicate, ViewerPlaceholder) {
		return predicate, false
	}
	return strings.ReplaceAll(predicate, ViewerPlaceholder, fmt.Sprintf("$%d", at)), true
}

// searchOne runs the count and the page for one table.
func (p *Postgres) searchOne(ctx context.Context, q Query, table Table) (Group, error) {
	group := Group{Source: table.Name, Label: table.label(), Hits: []Hit{}}

	// The query's own parameters come first, then the viewer if the
	// predicate asks for it, then the page bounds. Nothing is bound that the
	// statement does not reference: Postgres rejects a bind message that
	// supplies more parameters than the statement uses.
	query, args := p.tsquery(q, table.config(), 1)
	predicate, needsViewer := visible(table.Visible, len(args)+1)
	if needsViewer {
		args = append(args, q.Viewer)
	}

	// The count first, so a group can say "23 people" while showing five.
	// Separate from the page query rather than a window function over it: a
	// COUNT(*) on the same predicate is answered from the index, where a
	// window function makes every page recompute the total.
	countSQL := fmt.Sprintf(
		`SELECT count(*) FROM %s WHERE (%s) AND %s @@ %s`,
		table.From, predicate, table.Vector, query)
	if err := p.db.QueryRowContext(ctx, countSQL, args...).Scan(&group.Total); err != nil {
		return group, fmt.Errorf("counting: %w", err)
	}
	if group.Total == 0 {
		return group, nil
	}

	pageArgs := append(append([]any{}, args...), q.perSource(), q.Offset)
	limitParam := len(pageArgs) - 1
	offsetParam := len(pageArgs)

	// ts_headline is the expensive part of this statement: it re-parses the
	// document to find the matches. It sits in the select list of a query
	// that LIMITs, so it runs once per returned row and never over the
	// matched set. Putting it in a subquery that is then ordered and limited
	// would compute a snippet for every match in the table to show five.
	snippet := "''"
	if len(table.Snippet) > 0 {
		columns := make([]string, 0, len(table.Snippet))
		for _, column := range table.Snippet {
			columns = append(columns, fmt.Sprintf("coalesce(%s, '')", column))
		}
		snippet = fmt.Sprintf("ts_headline('%s', concat_ws(' … ', %s), %s, '%s')",
			table.config(), strings.Join(columns, ", "), query, headlineOptions)
	}

	subtitle := "''"
	if strings.TrimSpace(table.Subtitle) != "" {
		subtitle = fmt.Sprintf("coalesce(%s, '')", table.Subtitle)
	}
	url := "''"
	if strings.TrimSpace(table.URL) != "" {
		url = fmt.Sprintf("coalesce(%s, '')", table.URL)
	}

	// ts_rank_cd rather than ts_rank: it accounts for how close the matched
	// lexemes are to each other, so a bio containing both words of a
	// two-word query in one phrase outranks one that mentions them in
	// different paragraphs. The weight array is {D,C,B,A} — Postgres takes
	// it in that order, which is the reverse of how the setweight calls that
	// built the column read, and getting it backwards silently inverts the
	// ranking.
	pageSQL := fmt.Sprintf(`
SELECT %s AS id,
       coalesce(%s, '') AS title,
       %s AS subtitle,
       %s AS url,
       %s AS snippet,
       ts_rank_cd('{0.1, 0.2, 0.4, 1.0}', %s, %s) AS rank
  FROM %s
 WHERE (%s) AND %s @@ %s
 ORDER BY rank DESC, %s
 LIMIT $%d OFFSET $%d`,
		table.ID, table.Title, subtitle, url, snippet,
		table.Vector, query,
		table.From,
		predicate, table.Vector, query,
		table.tiebreak(),
		limitParam, offsetParam)

	rows, err := p.db.QueryContext(ctx, pageSQL, pageArgs...)
	if err != nil {
		return group, fmt.Errorf("selecting: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id                                 int64
			title, subtitleText, address, head string
			rank                               float64
		)
		if err := rows.Scan(&id, &title, &subtitleText, &address, &head, &rank); err != nil {
			return group, fmt.Errorf("scanning: %w", err)
		}
		group.Hits = append(group.Hits, Hit{
			Source:   table.Name,
			ID:       id,
			Title:    title,
			Subtitle: subtitleText,
			URL:      address,
			Snippet:  Snippet(head),
			Rank:     rank,
		})
	}
	if err := rows.Err(); err != nil {
		return group, err
	}

	group.More = group.Total > q.Offset+len(group.Hits)
	return group, nil
}
