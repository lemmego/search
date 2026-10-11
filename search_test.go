package search

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// refusingQuerier fails if it is used, so a test can assert that no query ran.
type refusingQuerier struct{ t *testing.T }

func (q refusingQuerier) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	q.t.Fatal("a query was executed when none should have been")
	return nil, nil
}

func (q refusingQuerier) QueryRowContext(context.Context, string, ...any) *sql.Row {
	q.t.Fatal("a query was executed when none should have been")
	return nil
}

func people() Table {
	return Table{
		Name: "profiles", Label: "People",
		From: "users", Vector: "search_vector",
		ID: "id", Title: "name", Subtitle: "headline",
		Snippet: []string{"headline", "bio"},
		Visible: "deleted_at IS NULL AND profile_published",
	}
}

// The security boundary of the whole package. ts_headline hands back the
// document — somebody's bio — with delimiters in it, so the escape has to
// happen before the markup is introduced and not after.
func TestASnippetCannotCarryMarkupOutOfTheDocument(t *testing.T) {
	// A bio containing a script tag, with the word "postgres" matched.
	document := "I like " + startSel + "postgres" + stopSel +
		` and <script>alert(document.cookie)</script> too`

	got := string(Snippet(document))

	if strings.Contains(got, "<script>") {
		t.Fatalf("the document's own markup survived into the output: %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("the document's markup was not escaped: %q", got)
	}
	if !strings.Contains(got, "<mark>postgres</mark>") {
		t.Fatalf("the match was not marked: %q", got)
	}
}

// A document that itself contains the delimiters can at worst produce a
// stray <mark>, never a tag of its author's choosing — and an unbalanced
// pair is dropped so the element does not stay open across the page.
func TestDelimitersInTheDocumentCannotBecomeArbitraryMarkup(t *testing.T) {
	// One orphaned start delimiter, and an attempt at an attribute.
	document := "bio with " + startSel + ` onload="evil()" and nothing closing it`

	got := string(Snippet(document))

	if strings.Contains(got, "<mark") {
		t.Fatalf("an unbalanced delimiter left an open element: %q", got)
	}
	if strings.Contains(got, `onload="`) {
		t.Fatalf("an attribute survived unescaped: %q", got)
	}
}

func TestPlainSnippetStripsTheDelimiters(t *testing.T) {
	got := PlainSnippet("a " + startSel + "match" + stopSel + " here")
	if got != "a match here" {
		t.Fatalf("PlainSnippet() = %q", got)
	}
	if strings.ContainsAny(got, startSel+stopSel) {
		t.Fatal("a delimiter survived")
	}
}

// Punctuation alone parses to an empty tsquery, which matches nothing — so a
// page needs to tell "type something" apart from "no results".
func TestSearchableSeparatesAnEmptyQueryFromAFruitlessOne(t *testing.T) {
	for _, text := range []string{"", "   ", "???", "...", "!@#$%^&*()", "\t\n"} {
		if Searchable(text) {
			t.Errorf("Searchable(%q) = true, want false", text)
		}
	}
	for _, text := range []string{"a", "postgres", "go 1.27", "  spaced  "} {
		if !Searchable(text) {
			t.Errorf("Searchable(%q) = false, want true", text)
		}
	}
}

func TestALongQueryIsTruncatedAndStaysValidUTF8(t *testing.T) {
	// Multi-byte runes, so a naive byte truncation would split one.
	long := strings.Repeat("é", MaxQueryBytes)
	got := normalise(long)

	if len(got) > MaxQueryBytes {
		t.Fatalf("normalise returned %d bytes, over the %d limit", len(got), MaxQueryBytes)
	}
	if strings.ToValidUTF8(got, "") != got {
		t.Fatal("truncation produced invalid UTF-8")
	}
}

// Prefix matching needs to_tsquery, which is the one conversion function that
// raises a syntax error — so it must never see anything a person typed. Every
// term is reduced to letters and digits.
func TestPrefixTermsAreAlphanumericOnly(t *testing.T) {
	hostile := `postgres & | ! ( ) : * ' " \ ; -- DROP`
	terms := prefixTerms(hostile)

	if len(terms) == 0 {
		t.Fatal("no terms extracted")
	}
	for _, term := range terms {
		for _, r := range term {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
				t.Fatalf("term %q contains %q, which to_tsquery could object to", term, r)
			}
		}
	}
}

func TestPrefixTermsAreBounded(t *testing.T) {
	many := strings.Repeat("word ", MaxPrefixTerms*3)
	if got := len(prefixTerms(many)); got != MaxPrefixTerms {
		t.Fatalf("got %d terms, want the cap of %d", got, MaxPrefixTerms)
	}
}

// A table with no visibility predicate is refused rather than defaulted,
// because the safe default is unknowable here and the unsafe one is "every
// row". Search is where a forgotten rule leaks: it is the one read path that
// touches every row rather than the ones somebody navigated to.
func TestATableWithoutAVisibilityPredicateIsRefused(t *testing.T) {
	table := people()
	table.Visible = ""

	_, err := New(refusingQuerier{t}).Search(context.Background(), Query{Text: "x"}, table)
	if !errors.Is(err, ErrNoVisibility) {
		t.Fatalf("err = %v, want ErrNoVisibility", err)
	}
}

func TestAnIncompleteTableIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*Table){
		"no name":   func(table *Table) { table.Name = "" },
		"no from":   func(table *Table) { table.From = "" },
		"no vector": func(table *Table) { table.Vector = "" },
		"no id":     func(table *Table) { table.ID = "" },
		"no title":  func(table *Table) { table.Title = "" },
	} {
		t.Run(name, func(t *testing.T) {
			table := people()
			mutate(&table)
			_, err := New(refusingQuerier{t}).
				Search(context.Background(), Query{Text: "x"}, table)
			if err == nil {
				t.Fatal("an incompletely declared table was accepted")
			}
		})
	}
}

// An empty box must not cost eight index scans, and the answer still has to
// have the shape a page expects.
func TestAnEmptyQueryRunsNoQueriesAndStillAnswers(t *testing.T) {
	results, err := New(refusingQuerier{t}).
		Search(context.Background(), Query{Text: "   "}, people())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !results.Empty {
		t.Fatal("Empty was not set for a blank query")
	}
	if len(results.Groups) != 1 {
		t.Fatalf("got %d groups, want 1 so the page can render its sections",
			len(results.Groups))
	}
	if results.Groups[0].Hits == nil {
		t.Fatal("Hits is nil; a nil slice marshals to null and breaks .length")
	}
}

func TestSearchNeedsATable(t *testing.T) {
	_, err := New(refusingQuerier{t}).Search(context.Background(), Query{Text: "x"})
	if !errors.Is(err, ErrNoTable) {
		t.Fatalf("err = %v, want ErrNoTable", err)
	}
}

// What somebody typed is bound, never interpolated — in both modes. Prefix
// mode is the one that matters, because it is built with to_tsquery.
func TestWhatSomebodyTypedIsAlwaysBound(t *testing.T) {
	engine := New(refusingQuerier{t})

	plain, args := engine.tsquery(Query{Text: "postgres"}, "english", 1)
	if !strings.Contains(plain, "$1") {
		t.Fatalf("a plain query did not use a placeholder: %q", plain)
	}
	if len(args) != 1 || args[0] != "postgres" {
		t.Fatalf("args = %v, want the text bound once", args)
	}
	if strings.Contains(plain, "postgres") {
		t.Fatalf("the text was interpolated into the SQL: %q", plain)
	}

	prefix, prefixArgs := engine.tsquery(Query{Text: "post gres", Prefix: true}, "english", 1)
	if !strings.Contains(prefix, "$1") || !strings.Contains(prefix, "$2") {
		t.Fatalf("a prefix query did not number from $1: %q", prefix)
	}
	if len(prefixArgs) != 2 {
		t.Fatalf("got %d prefix args, want 2", len(prefixArgs))
	}
	if strings.Contains(prefix, "post") || strings.Contains(prefix, "gres") {
		t.Fatalf("a term was interpolated into the SQL instead of bound: %q", prefix)
	}
}

// A predicate that does not care who is asking must not be forced to bind a
// viewer, because Postgres rejects a statement that supplies a parameter it
// never references — which is what a fixed $1 required.
func TestAViewerlessPredicateBindsNoViewer(t *testing.T) {
	predicate, needs := visible("deleted_at IS NULL", 2)
	if needs {
		t.Fatal("a predicate with no placeholder asked for the viewer")
	}
	if predicate != "deleted_at IS NULL" {
		t.Fatalf("predicate was rewritten to %q", predicate)
	}

	predicate, needs = visible("owner = "+ViewerPlaceholder+" OR shared_with = "+ViewerPlaceholder, 2)
	if !needs {
		t.Fatal("a predicate using the placeholder did not ask for the viewer")
	}
	if strings.Contains(predicate, ViewerPlaceholder) {
		t.Fatalf("the placeholder survived: %q", predicate)
	}
	if strings.Count(predicate, "$2") != 2 {
		t.Fatalf("both occurrences should bind the same parameter: %q", predicate)
	}
}

func TestPerSourceIsBounded(t *testing.T) {
	if got := (Query{}).perSource(); got != DefaultPerSource {
		t.Fatalf("zero Limit gave %d, want the default %d", got, DefaultPerSource)
	}
	if got := (Query{Limit: MaxPerSource * 10}).perSource(); got != MaxPerSource {
		t.Fatalf("an unbounded Limit gave %d, want the cap %d", got, MaxPerSource)
	}
	if got := (Query{Limit: -5}).perSource(); got != DefaultPerSource {
		t.Fatalf("a negative Limit gave %d", got)
	}
}
