package search_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/lemmego/search"
	_ "github.com/lib/pq"
)

// DSNEnv names an administrative Postgres DSN. Without it these are skipped:
// the unit tests prove the query building and the escaping, and these prove
// the SQL is SQL — which nothing else can, because a syntax error in a
// generated statement passes every test that does not execute it.
const DSNEnv = "SEARCH_POSTGRES_DSN"

func connect(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run the Postgres tests", DSNEnv)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("%s is set but unreachable: %v", DSNEnv, err)
	}
	return db
}

// fixture builds a table shaped like one an application would really have:
// a generated tsvector with weighted columns and a GIN index.
func fixture(t *testing.T, db *sql.DB) string {
	t.Helper()

	name := "search_test_notes"
	statements := []string{
		`DROP TABLE IF EXISTS ` + name,
		`CREATE TABLE ` + name + ` (
		    id bigserial PRIMARY KEY,
		    title text NOT NULL,
		    summary text NOT NULL DEFAULT '',
		    body text NOT NULL DEFAULT '',
		    owner_id bigint NOT NULL DEFAULT 0,
		    is_public boolean NOT NULL DEFAULT true,
		    search_vector tsvector GENERATED ALWAYS AS (
		        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
		        setweight(to_tsvector('english', coalesce(summary, '')), 'B') ||
		        setweight(to_tsvector('english', coalesce(body, '')), 'C')
		    ) STORED
		 )`,
		`CREATE INDEX ` + name + `_search_idx ON ` + name + ` USING GIN (search_vector)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("fixture: %v\n%s", err, statement)
		}
	}
	t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS ` + name) })

	rows := []struct {
		title, summary, body string
		owner                int64
		public               bool
	}{
		{"Postgres full text search", "Ranking and headlines", "A note about tsvector and GIN indexes.", 1, true},
		{"Indexing strategies", "GIN versus GiST", "Postgres offers both for text.", 1, true},
		{"A private note", "Secret", "This mentions postgres and must not be found.", 2, false},
		{"Unrelated", "Nothing here", "Entirely about bicycles.", 1, true},
		{"Script tag note", "Careful", `postgres and <script>alert(1)</script> in the body.`, 1, true},
		// A bare "<" is not a tag, so the text search parser leaves it alone
		// and it reaches the escaper — unlike a well-formed element, which
		// ts_headline drops on its own. This is the row that exercises the
		// escaping end to end.
		{"Comparison note", "Operators", `postgres where a < b && c "quoted" stays`, 1, true},
	}
	for _, row := range rows {
		if _, err := db.Exec(
			`INSERT INTO `+name+` (title, summary, body, owner_id, is_public)
			 VALUES ($1, $2, $3, $4, $5)`,
			row.title, row.summary, row.body, row.owner, row.public); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	return name
}

func notes(table string) search.Table {
	return search.Table{
		Name: "notes", Label: "Notes",
		From: table, Vector: "search_vector",
		ID: "id", Title: "title", Subtitle: "summary",
		Snippet: []string{"summary", "body"},
		URL:     "'/notes/' || id",
		// Public rows, or the viewer's own.
		Visible: "(is_public OR owner_id = " + search.ViewerPlaceholder + ")",
	}
}

func TestSearchFindsRanksAndExcludes(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)

	results, err := search.New(db).Search(context.Background(),
		search.Query{Text: "postgres", Viewer: 1}, notes(table))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results.Groups) != 1 {
		t.Fatalf("got %d groups", len(results.Groups))
	}

	group := results.Groups[0]
	if group.Total == 0 {
		t.Fatal("nothing matched a term that is in three visible rows")
	}

	// The private row belongs to owner 2 and the viewer is 1.
	for _, hit := range group.Hits {
		if hit.Title == "A private note" {
			t.Fatal("the visibility predicate did not exclude another owner's private row")
		}
		if hit.Title == "Unrelated" {
			t.Fatal("a row with no matching term was returned")
		}
	}

	// Title is weight A, body is weight C, so the row with the term in its
	// title must outrank the one that only has it in the body.
	if len(group.Hits) >= 2 && group.Hits[0].Rank < group.Hits[len(group.Hits)-1].Rank {
		t.Fatal("results are not ordered by descending rank")
	}
	if group.Hits[0].Title != "Postgres full text search" {
		t.Fatalf("expected the title match to rank first, got %q with rank %v",
			group.Hits[0].Title, group.Hits[0].Rank)
	}
}

// The owner sees their own private row; nobody else does. Same query, two
// viewers, so this cannot pass by the predicate being ignored.
func TestTheVisibilityPredicateSeesTheViewer(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)
	engine := search.New(db)

	owner, err := engine.Search(context.Background(),
		search.Query{Text: "postgres", Viewer: 2}, notes(table))
	if err != nil {
		t.Fatalf("Search as the owner: %v", err)
	}
	stranger, err := engine.Search(context.Background(),
		search.Query{Text: "postgres", Viewer: 1}, notes(table))
	if err != nil {
		t.Fatalf("Search as a stranger: %v", err)
	}

	if owner.Groups[0].Total <= stranger.Groups[0].Total {
		t.Fatalf("the owner saw %d and a stranger saw %d; the owner's private row is missing",
			owner.Groups[0].Total, stranger.Groups[0].Total)
	}
}

// The escaping test against real Postgres output.
//
// Two things are worth knowing here. ts_headline strips well-formed HTML
// elements by itself — the default text search parser recognises a tag as a
// token type — so a <script> element in a document never reaches the
// escaper. That is not something to rely on: a lone "<" is not a tag, the
// behaviour belongs to a parser configuration an application can change, and
// the escaping is the thing that holds whatever the parser does. So this
// asserts on the row with the bare operators, which does reach it.
func TestASnippetFromARealRowIsEscaped(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)

	results, err := search.New(db).Search(context.Background(),
		search.Query{Text: "postgres", Viewer: 1, Limit: 20}, notes(table))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	var checked bool
	for _, hit := range results.Groups[0].Hits {
		snippet := string(hit.Snippet)
		if snippet == "" {
			continue
		}

		// Every snippet: the only markup in it is the highlight.
		stripped := strings.ReplaceAll(snippet, "<mark>", "")
		stripped = strings.ReplaceAll(stripped, "</mark>", "")
		if strings.ContainsAny(stripped, "<>") {
			t.Fatalf("a snippet carries markup that is not a highlight: %q", snippet)
		}
		if !strings.Contains(snippet, "<mark>") {
			t.Fatalf("a snippet carried no highlight, so it should have been dropped: %q", snippet)
		}

		if hit.Title == "Comparison note" {
			checked = true
			if !strings.Contains(snippet, "&lt;") {
				t.Fatalf("a bare < was not escaped: %q", snippet)
			}
			if !strings.Contains(snippet, "&amp;") {
				t.Fatalf("a bare & was not escaped: %q", snippet)
			}
			if !strings.Contains(snippet, "&#34;") && !strings.Contains(snippet, "&quot;") {
				t.Fatalf("a quote was not escaped: %q", snippet)
			}
		}
	}
	if !checked {
		t.Fatal("the row with bare operators was not among the results, so nothing was proven")
	}
}

// websearch_to_tsquery is the only conversion function that cannot be made to
// raise a syntax error. This is the test that matters for a public search box.
func TestHostileInputDoesNotError(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)
	engine := search.New(db)

	for _, text := range []string{
		"a & | b", "!!!", "((((", `"unclosed quote`, "post:*:*", `\`, "'; DROP TABLE x; --",
		"&&&", "<>", "a <-> b", "***", "-", "- -", ":", "|&!",
	} {
		for _, prefix := range []bool{false, true} {
			_, err := engine.Search(context.Background(),
				search.Query{Text: text, Prefix: prefix, Viewer: 1}, notes(table))
			if err != nil {
				t.Errorf("Search(%q, prefix=%v) errored: %v", text, prefix, err)
			}
		}
	}
}

// Prefix mode is what makes an as-you-type box work: an incomplete word has
// to match the words that start with it.
func TestPrefixModeMatchesAnIncompleteWord(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)
	engine := search.New(db)

	exact, err := engine.Search(context.Background(),
		search.Query{Text: "postg", Viewer: 1}, notes(table))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if exact.Groups[0].Total != 0 {
		t.Fatalf("a partial word matched without prefix mode (%d hits), so the test proves nothing",
			exact.Groups[0].Total)
	}

	prefixed, err := engine.Search(context.Background(),
		search.Query{Text: "postg", Prefix: true, Viewer: 1}, notes(table))
	if err != nil {
		t.Fatalf("Search in prefix mode: %v", err)
	}
	if prefixed.Groups[0].Total == 0 {
		t.Fatal("prefix mode did not match an incomplete word")
	}
}

// Paging has to be stable, which is what Tiebreak is for: equal ranks are
// common and their order is otherwise whatever the plan produces.
func TestPagingIsStable(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)
	engine := search.New(db)

	first, err := engine.Search(context.Background(),
		search.Query{Text: "postgres", Limit: 1, Viewer: 1}, notes(table))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	second, err := engine.Search(context.Background(),
		search.Query{Text: "postgres", Limit: 1, Offset: 1, Viewer: 1}, notes(table))
	if err != nil {
		t.Fatalf("Search page 2: %v", err)
	}

	if len(first.Groups[0].Hits) != 1 || len(second.Groups[0].Hits) != 1 {
		t.Fatal("expected one hit per page")
	}
	if first.Groups[0].Hits[0].ID == second.Groups[0].Hits[0].ID {
		t.Fatal("page 2 returned the same row as page 1")
	}
	if !first.Groups[0].More {
		t.Fatal("More was not set on a page with results behind it")
	}
}

// A Total that counted only the returned page would make "see all 23" a lie.
func TestTotalCountsEveryMatchNotJustThePage(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)

	results, err := search.New(db).Search(context.Background(),
		search.Query{Text: "postgres", Limit: 1, Viewer: 1}, notes(table))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	group := results.Groups[0]
	if group.Total <= len(group.Hits) {
		t.Fatalf("Total is %d with %d hits returned; it should count the whole match set",
			group.Total, len(group.Hits))
	}
}

// A predicate that does not mention the viewer must produce a statement
// Postgres accepts. With the viewer fixed at $1 this failed outright —
// "bind message supplies 2 parameters, but prepared statement requires 1" —
// so every such predicate had to carry a no-op comparison to be bindable.
func TestAViewerlessPredicateRunsAgainstPostgres(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)

	public := notes(table)
	public.Visible = "is_public"

	results, err := search.New(db).Search(context.Background(),
		search.Query{Text: "postgres"}, public)
	if err != nil {
		t.Fatalf("a predicate with no viewer did not run: %v", err)
	}
	if results.Groups[0].Total == 0 {
		t.Fatal("nothing matched")
	}
	for _, hit := range results.Groups[0].Hits {
		if hit.Title == "A private note" {
			t.Fatal("a private row was returned")
		}
	}
}

// A hit carries its own address, so a renderer does not switch on the source
// name to work out where to link.
func TestAHitCarriesItsURL(t *testing.T) {
	db := connect(t)
	table := fixture(t, db)

	results, err := search.New(db).Search(context.Background(),
		search.Query{Text: "postgres", Viewer: 1}, notes(table))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, hit := range results.Groups[0].Hits {
		if !strings.HasPrefix(hit.URL, "/notes/") {
			t.Fatalf("hit %d has URL %q, want the declared expression", hit.ID, hit.URL)
		}
	}
}
