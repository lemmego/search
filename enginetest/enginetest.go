// Package enginetest is the conformance suite every search engine must pass.
//
// It exists because the whole argument for shipping two engines together is
// that a seam fitted against one implementation is fitted by accident. It was
// written before the second engine, deliberately, so that the contract is
// what the engines are held to rather than a description of what the first
// one happened to do.
//
// Run it from an engine's own test file:
//
//	func TestPostgresConformsToTheEngineContract(t *testing.T) {
//	    enginetest.Run(t, func(t *testing.T) enginetest.Harness { … })
//	}
//
// It is exported so an engine written outside this module can prove itself
// against the same contract the built-in ones are.
//
// # The fixed corpus
//
// Cases assert on content, so the suite owns a schema and the harness makes
// it searchable by whatever means its engine has. Every document has:
//
//	title      string   — "Postgres full text search", …
//	body       string   — the longer prose a snippet is drawn from
//	status     string   — "published" or "draft"
//	owner      int64    — who it belongs to
//	rank_hint  int64    — a stable tiebreak, ascending
//
// An engine whose index *is* the application's rows — the postgres engine —
// satisfies Put with an INSERT. An engine holding its own copy satisfies it
// with Index followed by Settle. The suite does not know which, and that
// asymmetry is the reason Put exists rather than the suite calling Index
// directly: "index then search" does not mean the same thing for both, and a
// suite written against only the second would never notice.
package enginetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lemmego/search"
)

// Index names the suite uses. Two, because several cases are about one index
// not disturbing another.
const (
	IndexA = "enginetest_a"
	IndexB = "enginetest_b"
)

// Harness is what an engine supplies so the suite can drive it.
type Harness struct {
	// Engine is the engine under test.
	Engine search.Engine

	// Put makes documents searchable by whatever means this engine has.
	// Required.
	//
	// It must return only once they are searchable, so an asynchronous engine
	// settles here.
	Put func(ctx context.Context, docs []search.Document) error

	// Source describes the suite's schema to this engine.
	//
	// visibleOnly asks for a descriptor whose visibility rule admits only
	// status = "published", which is what the visibility cases search
	// through. Otherwise the rule must admit every document — and it must
	// still *be* a rule, because a source without one is refused.
	Source func(index string, visibleOnly bool) search.Source

	// Unenforceable is a descriptor carrying a visibility rule this engine
	// cannot evaluate, for the refusal case. Nil when every rule this engine
	// is offered is one it can enforce — the postgres engine's position.
	Unenforceable func(index string) search.Source

	// StringKeys is whether this engine can store a document key that is not
	// a decimal integer.
	//
	// The postgres engine cannot: a Table's ID is a SQL expression scanned
	// into an int64, because the rows it searches have integer primary keys.
	// That is a real difference between an engine whose index is the
	// application's tables and one holding its own copy, and it is the reason
	// ValidKey's charset is wider than what every engine can store — so a
	// model with a uuid key is portable to Meilisearch and not to this one.
	StringKeys bool

	// Caps is what the engine claims. The suite skips the cases that do not
	// apply, and the last case checks the claims against observed behaviour
	// so a capability cannot be decorative.
	Caps search.Capabilities
}

// Factory returns a fresh harness over an empty engine.
type Factory func(t *testing.T) Harness

// Run executes the whole contract.
func Run(t *testing.T, newHarness Factory) {
	t.Helper()

	cases := []struct {
		name string
		run  func(t *testing.T, h Harness)
	}{
		{"a document that was put is findable", aDocumentIsFindable},
		{"groups come back one per source, in the order asked", groupsComeBackInOrder},
		{"results are grouped and never blended", resultsAreGrouped},
		{"an empty query is answered without searching", emptyQueryIsAnswered},
		{"hostile input never errors", hostileInputNeverErrors},
		{"visibility is enforced", visibilityIsEnforced},
		{"a visibility rule the engine cannot enforce is refused", unenforceableIsRefused},
		{"the limit is honoured and capped", limitIsHonouredAndCapped},
		{"paging does not repeat or skip", pagingDoesNotRepeatOrSkip},
		{"totals are exact", totalsAreExact},
		{"totals are at least what was returned", totalsAreAtLeast},
		{"a key round-trips exactly", aKeyRoundTrips},
		{"a snippet is escaped before it is marked", snippetIsEscapedThenMarked},
		{"a snippet with no match is empty", snippetWithoutAMatchIsEmpty},
		{"indexing the same key replaces rather than duplicates", upsertReplaces},
		{"indexing replaces the whole document", indexReplacesWholly},
		{"unindexing removes from results", unindexRemoves},
		{"unindexing an absent document is not an error", unindexAbsentIsNotAnError},
		{"flush empties one index and leaves the other", flushEmptiesOneIndex},
		{"a filter restricts the results", aFilterRestrictsTheResults},
		{"a negative filter excludes rather than widens", aNegativeFilterExcludes},
		{"an IN filter matches any of its values", anInFilterMatchesAnyValue},
		{"a range filter compares numerically", aRangeFilterComparesNumerically},
		{"a filter cannot replace the visibility rule", aFilterCannotReplaceVisibility},
		{"a filter field that is not a field name is refused", aBadFilterFieldIsRefused},
		{"the engine's capabilities agree with its behaviour", capabilitiesAgree},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh harness per case, so no case depends on another's
			// leftovers.
			h := newHarness(t)
			requireHarness(t, h)
			tc.run(t, h)
		})
	}
}

// requireHarness refuses an incomplete harness rather than letting the suite
// pass having asserted nothing.
func requireHarness(t *testing.T, h Harness) {
	t.Helper()

	if h.Engine == nil {
		t.Fatal("the harness supplies no Engine")
	}
	if h.Put == nil {
		t.Fatal("the harness supplies no Put, so no case can make anything searchable")
	}
	if h.Source == nil {
		t.Fatal("the harness supplies no Source")
	}

	// An asynchronous engine with no Settler cannot be tested: every
	// index-then-search assertion is a race, and skipping them is the
	// vacuous outcome rather than the safe one.
	if h.Caps.Asynchronous {
		if _, ok := h.Engine.(search.Settler); !ok {
			t.Fatalf("the %s engine reports Asynchronous but implements no "+
				"search.Settler, so every case here would be a race. Implement "+
				"Settle — it is also what lets search:import say documents are "+
				"indexed rather than accepted.", h.Engine.Name())
		}
	}
}

// --- the corpus -------------------------------------------------------------

func doc(index, key, title, body, status string, owner, rank int64) search.Document {
	return search.Document{
		Index: index,
		Key:   key,
		Fields: map[string]any{
			"title":     title,
			"body":      body,
			"status":    status,
			"owner":     owner,
			"rank_hint": rank,
		},
	}
}

// corpus is the standard set: four published, one draft.
func corpus(index string) []search.Document {
	return []search.Document{
		doc(index, "1", "Postgres full text search", "A tsvector is a sorted list of lexemes.", "published", 1, 1),
		doc(index, "2", "Searching with Postgres", "Ranking uses cover density.", "published", 1, 2),
		doc(index, "3", "Quince preserves", "Nothing to do with databases.", "published", 2, 3),
		doc(index, "4", "Damson jam", "Also nothing to do with databases.", "published", 2, 4),
		doc(index, "5", "Postgres draft", "An unpublished note about postgres.", "draft", 1, 5),
	}
}

func put(t *testing.T, h Harness, docs []search.Document) {
	t.Helper()
	if err := h.Put(context.Background(), docs); err != nil {
		t.Fatalf("making documents searchable: %v", err)
	}
}

func find(t *testing.T, h Harness, q search.Query, sources ...search.Source) *search.Results {
	t.Helper()
	got, err := search.NewFinder(h.Engine).Search(context.Background(), q, sources...)
	if err != nil {
		t.Fatalf("searching for %q: %v", q.Text, err)
	}
	return got
}

func keysOf(g search.Group) []string {
	keys := make([]string, 0, len(g.Hits))
	for _, h := range g.Hits {
		keys = append(keys, h.Key)
	}
	return keys
}

func contains(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

func indexer(t *testing.T, h Harness) search.Indexer {
	t.Helper()
	idx, ok := h.Engine.(search.Indexer)
	if !ok {
		t.Skipf("the %s engine keeps no index of its own, so there is nothing "+
			"to write — it searches rows where they already are", h.Engine.Name())
	}
	return idx
}

func settle(t *testing.T, h Harness) {
	t.Helper()
	if s, ok := h.Engine.(search.Settler); ok {
		if err := s.Settle(context.Background()); err != nil {
			t.Fatalf("settling: %v", err)
		}
	}
}

// --- the cases --------------------------------------------------------------

func aDocumentIsFindable(t *testing.T, h Harness) {
	put(t, h, corpus(IndexA))

	got := find(t, h, search.Query{Text: "quince"}, h.Source(IndexA, false))
	if len(got.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(got.Groups))
	}
	if !contains(keysOf(got.Groups[0]), "3") {
		t.Fatalf("searching for quince found %v, want the quince document",
			keysOf(got.Groups[0]))
	}
}

func groupsComeBackInOrder(t *testing.T, h Harness) {
	put(t, h, corpus(IndexA))
	put(t, h, corpus(IndexB))

	a, b := h.Source(IndexA, false), h.Source(IndexB, false)
	got := find(t, h, search.Query{Text: "postgres"}, a, b)

	if len(got.Groups) != 2 {
		t.Fatalf("got %d groups, want one per source including empty ones", len(got.Groups))
	}
	if got.Groups[0].Source != a.SourceName() || got.Groups[1].Source != b.SourceName() {
		t.Fatalf("groups came back as %q, %q, want %q, %q",
			got.Groups[0].Source, got.Groups[1].Source, a.SourceName(), b.SourceName())
	}
}

// resultsAreGrouped checks a group holds only its own source's hits.
//
// It does *not* compare Hit.Source against Group.Source, which was the first
// version and was vacuous by construction: the core sets Hit.Source from the
// request it issued, so an engine that returned every hit under every source
// would be silently corrected and the case could never fail. The meta-test in
// this package's own tests caught that.
//
// So the assertion is against a second search: whatever a source returns
// alone is what it must return alongside another.
func resultsAreGrouped(t *testing.T, h Harness) {
	put(t, h, corpus(IndexA))
	put(t, h, corpus(IndexB))

	a, b := h.Source(IndexA, false), h.Source(IndexB, false)
	q := search.Query{Text: "postgres", Limit: 50}

	alone := find(t, h, q, a)
	if len(alone.Groups[0].Hits) == 0 {
		t.Fatal("the source found nothing alone, so this case proves nothing")
	}

	together := find(t, h, q, a, b)
	if len(together.Groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(together.Groups))
	}

	want, got := keysOf(alone.Groups[0]), keysOf(together.Groups[0])
	if len(want) != len(got) {
		t.Errorf("searching %s alone returned %v, and alongside %s it returned "+
			"%v.\nRanks are not comparable across sources, so results must never "+
			"be merged — a blended answer looks authoritative and means nothing.",
			a.SourceName(), want, b.SourceName(), got)
		return
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("source %s returned %v alone and %v alongside another",
				a.SourceName(), want, got)
			return
		}
	}
}

func emptyQueryIsAnswered(t *testing.T, h Harness) {
	put(t, h, corpus(IndexA))

	for _, text := range []string{"", "   ", "???", "!!!"} {
		got := find(t, h, search.Query{Text: text}, h.Source(IndexA, false))
		if !got.Empty {
			t.Errorf("%q did not set Empty; a page cannot tell "+
				"'type something' from 'no results'", text)
		}
		if len(got.Groups) != 1 {
			t.Errorf("%q returned %d groups, want the shape anyway", text, len(got.Groups))
		}
		for _, g := range got.Groups {
			if g.Hits == nil {
				t.Errorf("%q left Hits nil, so it marshals as null rather than []", text)
			}
			if len(g.Hits) != 0 {
				t.Errorf("%q returned %d hits for an unsearchable query", text, len(g.Hits))
			}
		}
	}
}

// hostileInputNeverErrors is the case an engine is most likely to fail, and
// the one with a 500 behind it.
//
// A search box wired to something that raises on syntax answers 500 the first
// time somebody types "a & | b" by accident. These are the strings the
// postgres engine was built against; every engine is held to them.
func hostileInputNeverErrors(t *testing.T, h Harness) {
	put(t, h, corpus(IndexA))

	hostile := []string{
		"a & | b", "!!!", "((((", `"unclosed`, "'; DROP TABLE users; --",
		"&&&", "<>", "a <-> b", ":*", "|&!", "-", "~", "''", "\\",
		strings.Repeat("x", 500),
		strings.Repeat("a & ", 200),
		"\x00", "\xff\xfe", "ਅ & ।",
	}

	for _, text := range hostile {
		for _, prefix := range []bool{false, true} {
			_, err := search.NewFinder(h.Engine).Search(context.Background(),
				search.Query{Text: text, Prefix: prefix}, h.Source(IndexA, false))
			if err != nil {
				t.Errorf("Search(%q, prefix=%v) errored: %v\n"+
					"No query text may ever produce an error: whatever somebody "+
					"types into a box has to come back as results or as none.",
					text, prefix, err)
			}
		}
	}
}

func visibilityIsEnforced(t *testing.T, h Harness) {
	put(t, h, corpus(IndexA))

	// The control first, so the case cannot pass by finding nothing at all.
	all := find(t, h, search.Query{Text: "postgres", Limit: 50}, h.Source(IndexA, false))
	if !contains(keysOf(all.Groups[0]), "5") {
		t.Fatalf("the draft was not findable through a permissive rule, so this "+
			"case proves nothing: got %v", keysOf(all.Groups[0]))
	}

	visible := find(t, h, search.Query{Text: "postgres", Limit: 50}, h.Source(IndexA, true))
	if contains(keysOf(visible.Groups[0]), "5") {
		t.Errorf("the draft was returned through a rule admitting only "+
			"published documents: got %v", keysOf(visible.Groups[0]))
	}
	if !contains(keysOf(visible.Groups[0]), "1") {
		t.Errorf("a published document was not returned: got %v", keysOf(visible.Groups[0]))
	}
}

func unenforceableIsRefused(t *testing.T, h Harness) {
	if h.Unenforceable == nil {
		t.Skipf("every rule the %s engine is offered is one it can enforce", h.Engine.Name())
	}

	_, err := search.NewFinder(h.Engine).Search(context.Background(),
		search.Query{Text: "postgres"}, h.Unenforceable(IndexA))

	// Either sentinel is a refusal, and both are correct. An engine may
	// conclude it cannot enforce the rule from the rule itself, or from the
	// descriptor being one it does not read at all — which is the real
	// migration case: a Table written for the postgres engine, handed to an
	// engine that reads filters, because somebody moved a source and did not
	// translate it.
	//
	// What is forbidden is searching anyway. Zero results would look like an
	// empty index, and rows would look like working software.
	if !errors.Is(err, search.ErrVisibilityUnenforceable) &&
		!errors.Is(err, search.ErrSourceMismatch) {
		t.Fatalf("got %v, want a refusal wrapping ErrVisibilityUnenforceable "+
			"or ErrSourceMismatch.\n"+
			"An engine that cannot evaluate a source's visibility rule must "+
			"refuse the source rather than searching rows it has no way to "+
			"filter.", err)
	}
}

func limitIsHonouredAndCapped(t *testing.T, h Harness) {
	put(t, h, corpus(IndexA))

	got := find(t, h, search.Query{Text: "postgres", Limit: 1}, h.Source(IndexA, false))
	if n := len(got.Groups[0].Hits); n != 1 {
		t.Errorf("asked for 1 hit, got %d", n)
	}

	got = find(t, h, search.Query{Text: "postgres", Limit: 10_000}, h.Source(IndexA, false))
	if n := len(got.Groups[0].Hits); n > search.MaxPerSource {
		t.Errorf("asked for 10,000 hits and got %d; the cap is %d and a limit "+
			"arriving from a query string is the normal case", n, search.MaxPerSource)
	}
}

func pagingDoesNotRepeatOrSkip(t *testing.T, h Harness) {
	if !h.Caps.OffsetPaging {
		t.Skipf("the %s engine does not page by offset", h.Engine.Name())
	}
	put(t, h, corpus(IndexA))

	seen := map[string]int{}
	for offset := 0; offset < 6; offset += 2 {
		got := find(t, h, search.Query{Text: "postgres OR quince OR damson",
			Limit: 2, Offset: offset}, h.Source(IndexA, false))
		for _, key := range keysOf(got.Groups[0]) {
			seen[key]++
		}
	}
	for key, n := range seen {
		if n > 1 {
			t.Errorf("document %s appeared on %d pages; without a stable "+
				"tiebreak a page boundary repeats a row and hides another", key, n)
		}
	}
}

func totalsAreExact(t *testing.T, h Harness) {
	if !h.Caps.ExactTotals {
		t.Skipf("the %s engine reports estimated totals", h.Engine.Name())
	}
	put(t, h, corpus(IndexA))

	got := find(t, h, search.Query{Text: "postgres", Limit: 1}, h.Source(IndexA, false))
	group := got.Groups[0]
	if group.Estimated {
		t.Error("Estimated is set on an engine claiming ExactTotals")
	}
	if group.Total != 3 {
		t.Errorf("Total = %d, want 3 — the count of matching documents, not "+
			"the %d returned", group.Total, len(group.Hits))
	}
	if !group.More {
		t.Error("More is false although two matches were not returned")
	}
}

func totalsAreAtLeast(t *testing.T, h Harness) {
	if h.Caps.ExactTotals {
		t.Skipf("the %s engine reports exact totals", h.Engine.Name())
	}
	put(t, h, corpus(IndexA))

	got := find(t, h, search.Query{Text: "postgres", Limit: 50}, h.Source(IndexA, false))
	group := got.Groups[0]
	if !group.Estimated {
		t.Error("Estimated is not set on an engine that does not claim ExactTotals; " +
			"a page rendering the total as a count would be asserting something " +
			"it was not told")
	}
	if group.Total < len(group.Hits) {
		t.Errorf("Total %d is below the %d hits returned", group.Total, len(group.Hits))
	}
	if group.More && group.Total <= len(group.Hits) {
		t.Error("More is set although every match was returned, so a Next " +
			"button leads to an empty page")
	}
}

func aKeyRoundTrips(t *testing.T, h Harness) {
	keys := []string{"42", "43", "44"}
	if h.StringKeys {
		keys = []string{"42", "a-b_c", "01HQ3GZ8XK9PQRSTUVWXYZ0123", "A1"}
	}

	docs := make([]search.Document, 0, len(keys))
	for i, key := range keys {
		docs = append(docs, doc(IndexA, key, "Kumquat marmalade",
			"A note about kumquats.", "published", 1, int64(i+1)))
	}
	put(t, h, docs)

	got := find(t, h, search.Query{Text: "kumquat", Limit: 50}, h.Source(IndexA, false))
	found := keysOf(got.Groups[0])
	for _, want := range keys {
		if !contains(found, want) {
			t.Errorf("key %q did not round-trip: got %v", want, found)
		}
	}
	for _, hit := range got.Groups[0].Hits {
		if hit.Key == "" {
			t.Error("a hit came back with an empty Key; it is the field a " +
				"caller uses and the only one a string-keyed engine can fill")
		}
	}
}

func snippetIsEscapedThenMarked(t *testing.T, h Harness) {
	if !h.Caps.Highlighting {
		t.Skipf("the %s engine does not highlight", h.Engine.Name())
	}

	put(t, h, []search.Document{doc(IndexA, "1", "Mulberry",
		`Where a < b & c, I like mulberries, "quoted" and all`,
		"published", 1, 1)})

	got := find(t, h, search.Query{Text: "mulberries"}, h.Source(IndexA, false))
	if len(got.Groups[0].Hits) == 0 {
		t.Fatal("nothing matched, so this case proves nothing")
	}

	snippet := string(got.Groups[0].Hits[0].Snippet)
	if !strings.Contains(snippet, "<mark>") {
		t.Fatalf("the match was not marked: %q", snippet)
	}

	bare := strings.ReplaceAll(strings.ReplaceAll(snippet, "<mark>", ""), "</mark>", "")
	if strings.ContainsAny(bare, "<>") {
		t.Errorf("markup other than the highlight survived into the snippet: %q\n"+
			"The document is escaped first and the <mark> elements introduced "+
			"afterwards; the reverse order publishes whatever is in somebody's "+
			"prose.", snippet)
	}
	if !strings.Contains(snippet, "&lt;") || !strings.Contains(snippet, "&amp;") {
		t.Errorf("the document was not escaped: %q", snippet)
	}
}

func snippetWithoutAMatchIsEmpty(t *testing.T, h Harness) {
	if !h.Caps.Highlighting {
		t.Skipf("the %s engine does not highlight", h.Engine.Name())
	}

	// The term is in the title, which is not a snippet field, so there is
	// nothing to highlight in the body.
	put(t, h, []search.Document{doc(IndexA, "1", "Loganberry",
		"This prose does not mention the fruit at all.", "published", 1, 1)})

	got := find(t, h, search.Query{Text: "loganberry"}, h.Source(IndexA, false))
	if len(got.Groups[0].Hits) == 0 {
		t.Skip("the engine does not match on the title, so there is no such case here")
	}
	if s := got.Groups[0].Hits[0].Snippet; s != "" {
		t.Errorf("Snippet = %q for a hit with no match in the snippet fields; "+
			"an unhighlighted lead fragment presented as evidence of a match is "+
			"worse than no evidence", s)
	}
}

func upsertReplaces(t *testing.T, h Harness) {
	idx := indexer(t, h)
	put(t, h, []search.Document{doc(IndexA, "1", "Greengage",
		"The first body.", "published", 1, 1)})

	if err := idx.Index(context.Background(), []search.Document{
		doc(IndexA, "1", "Greengage", "The second body mentions apricots.", "published", 1, 1),
	}); err != nil {
		t.Fatalf("re-indexing: %v", err)
	}
	settle(t, h)

	got := find(t, h, search.Query{Text: "greengage", Limit: 50}, h.Source(IndexA, false))
	if n := len(got.Groups[0].Hits); n != 1 {
		t.Fatalf("got %d hits for one key indexed twice, want 1", n)
	}

	apricots := find(t, h, search.Query{Text: "apricots", Limit: 50}, h.Source(IndexA, false))
	if len(apricots.Groups[0].Hits) != 1 {
		t.Error("the replacement body is not searchable, so Index did not replace")
	}
}

func indexReplacesWholly(t *testing.T, h Harness) {
	idx := indexer(t, h)
	put(t, h, []search.Document{doc(IndexA, "1", "Nectarine",
		"A body mentioning persimmons.", "published", 1, 1)})

	// The same key with the body replaced by something else entirely.
	if err := idx.Index(context.Background(), []search.Document{
		doc(IndexA, "1", "Nectarine", "A body mentioning nothing else.", "published", 1, 1),
	}); err != nil {
		t.Fatalf("re-indexing: %v", err)
	}
	settle(t, h)

	got := find(t, h, search.Query{Text: "persimmons", Limit: 50}, h.Source(IndexA, false))
	if len(got.Groups[0].Hits) != 0 {
		t.Error("the old body is still searchable, so Index merged rather than " +
			"replaced. A field a model stops setting must stop being findable.")
	}
}

func unindexRemoves(t *testing.T, h Harness) {
	idx := indexer(t, h)
	put(t, h, corpus(IndexA))

	if err := idx.Unindex(context.Background(), IndexA, []string{"3"}); err != nil {
		t.Fatalf("unindexing: %v", err)
	}
	settle(t, h)

	got := find(t, h, search.Query{Text: "quince", Limit: 50}, h.Source(IndexA, false))
	if contains(keysOf(got.Groups[0]), "3") {
		t.Error("the unindexed document is still findable")
	}

	// And its neighbours survive.
	others := find(t, h, search.Query{Text: "damson", Limit: 50}, h.Source(IndexA, false))
	if !contains(keysOf(others.Groups[0]), "4") {
		t.Error("unindexing one document removed another")
	}
}

// unindexAbsentIsNotAnError is load-bearing rather than lenient.
//
// It is what lets Sync answer "this is no longer searchable" with an
// unconditional Unindex and no before-state — which is how a model that stops
// being searchable has its stale document removed without the two extra hooks
// Scout needs for it.
func unindexAbsentIsNotAnError(t *testing.T, h Harness) {
	idx := indexer(t, h)
	put(t, h, corpus(IndexA))

	if err := idx.Unindex(context.Background(), IndexA, []string{"nosuchkey"}); err != nil {
		t.Errorf("unindexing an absent key errored: %v\n"+
			"Sync removes the document of a model that says it should not be "+
			"searchable, without first asking whether one was ever written. "+
			"That is only correct if this is a no-op.", err)
	}

	// Twice, so the idempotence is explicit.
	if err := idx.Unindex(context.Background(), IndexA, []string{"3"}); err != nil {
		t.Fatalf("first unindex: %v", err)
	}
	if err := idx.Unindex(context.Background(), IndexA, []string{"3"}); err != nil {
		t.Errorf("unindexing twice errored the second time: %v", err)
	}
}

func flushEmptiesOneIndex(t *testing.T, h Harness) {
	idx := indexer(t, h)
	put(t, h, corpus(IndexA))
	put(t, h, corpus(IndexB))

	if err := idx.Flush(context.Background(), IndexA); err != nil {
		t.Fatalf("flushing: %v", err)
	}
	settle(t, h)

	emptied := find(t, h, search.Query{Text: "postgres", Limit: 50}, h.Source(IndexA, false))
	if n := len(emptied.Groups[0].Hits); n != 0 {
		t.Errorf("the flushed index still answers with %d hits", n)
	}

	survivor := find(t, h, search.Query{Text: "postgres", Limit: 50}, h.Source(IndexB, false))
	if len(survivor.Groups[0].Hits) == 0 {
		t.Error("flushing one index emptied the other")
	}

	// And the index is still usable, which is what separates emptying it from
	// deleting it — deleting drops the configuration that made it searchable.
	put(t, h, corpus(IndexA))
	refilled := find(t, h, search.Query{Text: "postgres", Limit: 50}, h.Source(IndexA, false))
	if len(refilled.Groups[0].Hits) == 0 {
		t.Error("the index could not be written to after a flush, so Flush " +
			"removed the index rather than emptying it")
	}
}

// capabilitiesAgree guards the guard.
//
// A capability that nothing checks is decorative, and an engine claiming it
// cannot enforce relational visibility while happily accepting such a source
// would make the refusal above meaningless.
func capabilitiesAgree(t *testing.T, h Harness) {
	caps := h.Engine.Capabilities()

	if caps.RelationalVisibility && h.Unenforceable != nil {
		t.Error("the engine claims RelationalVisibility but the harness offers " +
			"a rule it cannot enforce; one of the two is wrong")
	}

	if _, isIndexer := h.Engine.(search.Indexer); !isIndexer {
		put(t, h, corpus(IndexA))
		// An engine with no index of its own must still answer, because its
		// index is the application's rows.
		got := find(t, h, search.Query{Text: "postgres"}, h.Source(IndexA, false))
		if len(got.Groups[0].Hits) == 0 {
			t.Error("the engine implements no Indexer and found nothing, so " +
				"there is no way for anything to become searchable")
		}
	}

	if !caps.Filters {
		// The contract is that an engine reporting false refuses a request
		// carrying filters rather than returning unfiltered rows.
		_, err := h.Engine.MultiSearch(context.Background(), []search.SearchRequest{{
			Source:  h.Source(IndexA, false),
			Text:    "postgres",
			Limit:   5,
			Filters: []search.Filter{{Field: "owner", Op: search.OpEq, Values: []any{int64(1)}}},
		}})
		if err == nil {
			t.Error("the engine reports Filters false but accepted a request " +
				"carrying one; returning unfiltered rows is the dangerous half " +
				"of that mistake")
		}
		if err != nil && !errors.Is(err, search.ErrUnsupported) {
			t.Errorf("refusing filters gave %v, want it to wrap ErrUnsupported", err)
		}
	}

	// The other half — an engine claiming Filters and silently ignoring one —
	// is checked by aFilterRestrictsTheResults rather than here, because
	// proving a filter was applied needs a corpus and an assertion about
	// which documents came back, not a yes-or-no answer.

	if caps.Asynchronous {
		if _, ok := h.Engine.(search.Settler); !ok {
			t.Error("Asynchronous without a Settler")
		}
	}
}

// --- filters ----------------------------------------------------------------

// row is a model type for the builder's sake. The filter cases call Raw, so
// nothing is ever loaded — but For is generic over the model, which is what
// makes the typed path typed, and a conformance suite has no models.
type row struct{ key string }

func (r row) SearchIndex() string            { return "" }
func (r row) SearchKey() string              { return r.key }
func (r row) SearchDocument() map[string]any { return nil }

func requireFilters(t *testing.T, h Harness) {
	t.Helper()
	if !h.Caps.Filters {
		t.Skipf("the %s engine reports Filters false, so a filtered request is "+
			"refused rather than applied — which capabilitiesAgree checks",
			h.Engine.Name())
	}
}

// query starts a filtered search over one source.
func query(h Harness, src search.Source, text string) *search.Builder[row] {
	// Take(MaxPerSource) because these cases assert on the whole matched set
	// and the default page is five.
	return search.For[row](h.Engine, src, text).Take(search.MaxPerSource)
}

func keysFrom(t *testing.T, b *search.Builder[row]) []string {
	t.Helper()
	group, err := b.Raw(context.Background())
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	return keysOf(group)
}

func aFilterRestrictsTheResults(t *testing.T, h Harness) {
	requireFilters(t, h)
	put(t, h, corpus(IndexA))
	src := h.Source(IndexA, false)

	// Checked first, so the case cannot pass because the query matched
	// nothing in the first place — which is how a filter test goes vacuous.
	all := keysFrom(t, query(h, src, "postgres"))
	if !contains(all, "5") {
		t.Fatalf("searching for postgres found %v, want the draft among them — "+
			"without it there is nothing for the filter to remove", all)
	}

	got := keysFrom(t, query(h, src, "postgres").Where("status", "published"))
	if contains(got, "5") {
		t.Errorf("status = published returned %v, which includes the draft", got)
	}
	if !contains(got, "1") || !contains(got, "2") {
		t.Errorf("status = published returned %v, want the two published matches", got)
	}
}

func aNegativeFilterExcludes(t *testing.T, h Harness) {
	requireFilters(t, h)
	put(t, h, corpus(IndexA))
	src := h.Source(IndexA, false)

	got := keysFrom(t, query(h, src, "postgres").WhereNot("status", "draft"))
	if contains(got, "5") {
		t.Errorf("status != draft returned %v, which includes the draft", got)
	}
	if len(got) == 0 {
		t.Error("status != draft returned nothing, so it excluded everything " +
			"rather than the drafts")
	}
}

func anInFilterMatchesAnyValue(t *testing.T, h Harness) {
	requireFilters(t, h)
	put(t, h, corpus(IndexA))
	src := h.Source(IndexA, false)

	// Both values, so the case fails for an engine that honours only the
	// first — which is what a filter rendered as an equality would do.
	got := keysFrom(t, query(h, src, "databases").WhereIn("owner", int64(1), int64(2)))
	if !contains(got, "3") || !contains(got, "4") {
		t.Errorf("owner IN (1, 2) returned %v, want both owners' matches", got)
	}

	one := keysFrom(t, query(h, src, "postgres").WhereIn("status", "draft"))
	if len(one) != 1 || one[0] != "5" {
		t.Errorf("status IN (draft) returned %v, want just the draft", one)
	}
}

// aRangeFilterComparesNumerically, which is the case a filter value rendered
// or bound as text passes every other time.
//
// 10 against 9 is the smallest pair that tells the two apart: numerically 10
// is greater, lexicographically it is not. Every other number in the corpus
// is a single digit, where the two orderings agree — so without these two
// documents a text comparison would pass the whole suite.
func aRangeFilterComparesNumerically(t *testing.T, h Harness) {
	requireFilters(t, h)
	put(t, h, append(corpus(IndexA),
		doc(IndexA, "9", "Postgres nine", "The ninth note.", "published", 1, 9),
		doc(IndexA, "10", "Postgres ten", "The tenth note.", "published", 1, 10),
	))
	src := h.Source(IndexA, false)

	got := keysFrom(t, query(h, src, "postgres").WhereGt("rank_hint", int64(9)))
	if len(got) != 1 || got[0] != "10" {
		t.Fatalf("rank_hint > 9 returned %v, want just the one with 10 — "+
			"nothing back means the comparison was made on text", got)
	}

	below := keysFrom(t, query(h, src, "postgres").WhereLte("rank_hint", int64(2)))
	if !contains(below, "1") || !contains(below, "2") || contains(below, "10") {
		t.Errorf("rank_hint <= 2 returned %v", below)
	}
}

// aFilterCannotReplaceVisibility is the one case here that is about safety
// rather than about correctness.
//
// A filter and a visibility rule are both predicates, and an engine that
// composed them by replacement rather than by conjunction would let any
// caller ask for rows the rule exists to hide — by asking for them. So the
// filter here names exactly what the rule forbids, and the only acceptable
// answer is nothing.
func aFilterCannotReplaceVisibility(t *testing.T, h Harness) {
	requireFilters(t, h)
	put(t, h, corpus(IndexA))

	visible := h.Source(IndexA, true)
	got := keysFrom(t, query(h, visible, "postgres").Where("status", "draft"))
	if len(got) != 0 {
		t.Fatalf("a filter for status = draft against a source that admits only "+
			"published rows returned %v, so the filter displaced the "+
			"visibility rule", got)
	}
}

// aBadFilterFieldIsRefused, before any query runs.
//
// A field name cannot be a bind parameter, so every engine here interpolates
// it. That makes it the one part of a filter that is not escaped on the way
// through, and refusing anything that is not a plain name is what makes the
// interpolation safe — in all three engines, from one rule.
func aBadFilterFieldIsRefused(t *testing.T, h Harness) {
	requireFilters(t, h)
	put(t, h, corpus(IndexA))
	src := h.Source(IndexA, false)

	for _, field := range []string{
		"status; DROP TABLE enginetest_a",
		"status) OR (1=1",
		"enginetest_a.status",
		"",
	} {
		if _, err := query(h, src, "postgres").Where(field, "published").
			Raw(context.Background()); !errors.Is(err, search.ErrBadField) {
			t.Errorf("a filter on %q returned %v, want ErrBadField", field, err)
		}
	}
}
