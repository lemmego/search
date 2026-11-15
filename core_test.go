package search

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeEngine is an engine the core's invariants can be checked against
// without a database.
type fakeEngine struct {
	caps     Capabilities
	refuse   error
	results  []SearchResult
	dropOne  bool
	seen     []SearchRequest
	indexed  []Document
	unindexd map[string][]string
}

func (f *fakeEngine) Name() string               { return "fake" }
func (f *fakeEngine) Capabilities() Capabilities { return f.caps }
func (f *fakeEngine) Supports(src Source) error  { return f.refuse }

func (f *fakeEngine) MultiSearch(_ context.Context, reqs []SearchRequest) ([]SearchResult, error) {
	f.seen = append(f.seen, reqs...)
	out := f.results
	if out == nil {
		out = make([]SearchResult, len(reqs))
	}
	if f.dropOne && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, nil
}

func (f *fakeEngine) Index(_ context.Context, docs []Document) error {
	f.indexed = append(f.indexed, docs...)
	return nil
}

func (f *fakeEngine) Unindex(_ context.Context, index string, keys []string) error {
	if f.unindexd == nil {
		f.unindexd = map[string][]string{}
	}
	f.unindexd[index] = append(f.unindexd[index], keys...)
	return nil
}

func (f *fakeEngine) Flush(context.Context, string) error { return nil }

// fakeSource is a descriptor the fake engine accepts.
type fakeSource struct {
	name, lbl string
	vis       Visibility
}

func (s fakeSource) SourceName() string     { return s.name }
func (s fakeSource) SourceLabel() string    { return s.lbl }
func (s fakeSource) Visibility() Visibility { return s.vis }

func source(name string) fakeSource {
	return fakeSource{name: name, vis: Visibility{Filter: "published = true"}}
}

// TestEveryGroupIsPresentInTheOrderAsked is the invariant the core exists to
// own rather than trust an engine with.
//
// A consumer infers "one source was asked for" from len(Groups) and reads
// groups[0].More for its Next button, so a result set that dropped an empty
// group would shift every label by one and silently change a page's meaning.
func TestEveryGroupIsPresentInTheOrderAsked(t *testing.T) {
	engine := &fakeEngine{}
	sources := []Source{source("people"), source("posts"), source("jobs")}

	got, err := search(context.Background(), engine, Query{Text: "postgres"}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 3 {
		t.Fatalf("got %d groups, want one per source", len(got.Groups))
	}
	for i, want := range []string{"people", "posts", "jobs"} {
		if got.Groups[i].Source != want {
			t.Errorf("group %d is %q, want %q", i, got.Groups[i].Source, want)
		}
	}
}

// TestAnEngineThatAnswersTheWrongNumberOfRequestsIsRefused.
//
// Matching results to sources by position is the only thing that can work, so
// a mismatch cannot be papered over: the alternative is a hit rendered under
// another source's label, which looks like working software.
func TestAnEngineThatAnswersTheWrongNumberOfRequestsIsRefused(t *testing.T) {
	engine := &fakeEngine{dropOne: true}
	sources := []Source{source("people"), source("posts")}

	_, err := search(context.Background(), engine, Query{Text: "postgres"}, sources)
	if !errors.Is(err, ErrEngineContract) {
		t.Fatalf("got %v, want ErrEngineContract", err)
	}
	if !strings.Contains(err.Error(), "fake") {
		t.Errorf("the error does not name the engine: %v", err)
	}
}

// TestARefusedSourceFailsTheWholeSearch rather than being dropped.
//
// Dropping it would break the one-group-per-source invariant above and
// produce a page quietly missing a section. A caller that wants degradation
// asks for it with errors.Is.
func TestARefusedSourceFailsTheWholeSearch(t *testing.T) {
	engine := &fakeEngine{refuse: ErrVisibilityUnenforceable}

	_, err := search(context.Background(), engine, Query{Text: "x"},
		[]Source{source("people")})
	if !errors.Is(err, ErrVisibilityUnenforceable) {
		t.Fatalf("got %v, want ErrVisibilityUnenforceable", err)
	}
	if !strings.Contains(err.Error(), "people") {
		t.Errorf("the error does not name the source: %v", err)
	}
}

// TestAnEmptyQueryReachesNoEngine, and still answers with the shape.
func TestAnEmptyQueryReachesNoEngine(t *testing.T) {
	engine := &fakeEngine{}

	got, err := search(context.Background(), engine, Query{Text: "   ???  "},
		[]Source{source("people"), source("posts")})
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.seen) != 0 {
		t.Errorf("the engine was asked %d questions for an unsearchable query", len(engine.seen))
	}
	if !got.Empty {
		t.Error("Empty is false; a page cannot tell 'type something' from 'no results'")
	}
	if len(got.Groups) != 2 {
		t.Fatalf("got %d groups, want the shape anyway", len(got.Groups))
	}
	for _, g := range got.Groups {
		if g.Hits == nil {
			t.Error("Hits is nil, so it marshals as null rather than []")
		}
	}
}

// TestTheCoreEscapesTheSnippetAndTheEngineCannot.
//
// An engine returns text; only the core mints a template.HTML. This is the
// property that makes a new engine unable to publish somebody's bio as
// markup, and it is asserted here rather than left to each engine's own
// tests.
func TestTheCoreEscapesTheSnippetAndTheEngineCannot(t *testing.T) {
	engine := &fakeEngine{results: []SearchResult{{
		Hits: []RawHit{{
			Key:      "1",
			Headline: "a " + HighlightPreTag + "<script>" + HighlightPostTag + " b & c",
		}},
		Total: 1,
	}}}

	got, err := search(context.Background(), engine, Query{Text: "script"},
		[]Source{source("people")})
	if err != nil {
		t.Fatal(err)
	}

	snippet := string(got.Groups[0].Hits[0].Snippet)
	if !strings.Contains(snippet, "<mark>") {
		t.Fatalf("the match was not marked: %q", snippet)
	}
	if strings.Contains(snippet, "<script>") {
		t.Fatalf("a script element survived into the markup: %q", snippet)
	}
	if !strings.Contains(snippet, "&lt;script&gt;") || !strings.Contains(snippet, "&amp;") {
		t.Fatalf("the document was not escaped: %q", snippet)
	}
}

// TestTheCoreOwnsHitSource, so an engine that labels a hit wrongly is
// corrected rather than believed.
func TestTheCoreOwnsHitSource(t *testing.T) {
	engine := &fakeEngine{results: []SearchResult{{
		Hits:  []RawHit{{Key: "1"}},
		Total: 1,
	}}}

	got, _ := search(context.Background(), engine, Query{Text: "x"},
		[]Source{source("people")})
	if got.Groups[0].Hits[0].Source != "people" {
		t.Errorf("Hit.Source = %q, want the source the core asked about",
			got.Groups[0].Hits[0].Source)
	}
}

// TestAViewerOfAnUnbindableTypeIsRefusedBeforeAnyQuery.
//
// Query.Viewer is `any` so a uuid works, and this is the check that buys back
// what widening it cost: a wrong type is refused here rather than surfacing
// from inside a driver as "unsupported type" halfway through a page.
func TestAViewerOfAnUnbindableTypeIsRefusedBeforeAnyQuery(t *testing.T) {
	engine := &fakeEngine{}

	_, err := search(context.Background(), engine,
		Query{Text: "x", Viewer: 1.5}, []Source{source("people")})
	if !errors.Is(err, ErrBadViewer) {
		t.Fatalf("got %v, want ErrBadViewer", err)
	}
	if len(engine.seen) != 0 {
		t.Error("the engine was queried with an unbindable viewer")
	}

	for _, ok := range []any{nil, "abc", []byte("abc"), 7, int64(7), uint64(7)} {
		if _, err := search(context.Background(), engine,
			Query{Text: "x", Viewer: ok}, []Source{source("people")}); err != nil {
			t.Errorf("a viewer of type %T was refused: %v", ok, err)
		}
	}
}

// TestThePageSizeIsResolvedOnceByTheCore, so no engine carries its own
// forgotten clamp.
func TestThePageSizeIsResolvedOnceByTheCore(t *testing.T) {
	engine := &fakeEngine{}

	_, err := search(context.Background(), engine,
		Query{Text: "x", Limit: 10_000}, []Source{source("people")})
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.seen[0].Limit; got != MaxPerSource {
		t.Errorf("the engine was handed Limit %d, want it clamped to %d", got, MaxPerSource)
	}

	engine.seen = nil
	if _, err := search(context.Background(), engine,
		Query{Text: "x"}, []Source{source("people")}); err != nil {
		t.Fatal(err)
	}
	if got := engine.seen[0].Limit; got != DefaultPerSource {
		t.Errorf("the engine was handed Limit %d for an unset one, want %d",
			got, DefaultPerSource)
	}
}
