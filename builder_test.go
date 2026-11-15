package search

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// post is the model the builder hydrates into.
type post struct {
	key   string
	title string
}

func (p *post) SearchIndex() string            { return "posts" }
func (p *post) SearchKey() string              { return p.key }
func (p *post) SearchDocument() map[string]any { return map[string]any{"title": p.title} }

// loader hydrates from a fixed corpus, and deliberately answers in its own
// order rather than the order it was asked in — which is what a database
// does with `WHERE key IN (...)` and what Get has to correct for.
type loader struct {
	have    map[string]*post
	askedIn []string
}

func (l *loader) Load(_ context.Context, keys []string) ([]*post, error) {
	l.askedIn = append([]string{}, keys...)
	sorted := append([]string{}, keys...)
	sort.Strings(sorted)

	out := make([]*post, 0, len(sorted))
	for _, k := range sorted {
		if p, ok := l.have[k]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func corpus(keys ...string) *loader {
	have := map[string]*post{}
	for _, k := range keys {
		have[k] = &post{key: k, title: "post " + k}
	}
	return &loader{have: have}
}

// hits builds one engine answer in a given order.
func hits(more bool, keys ...string) SearchResult {
	res := SearchResult{Total: len(keys), More: more}
	for i, k := range keys {
		res.Hits = append(res.Hits, RawHit{Key: k, Rank: float64(len(keys) - i)})
	}
	return res
}

// offsetEngine answers from a corpus by offset, so a walk can be observed
// rather than stubbed.
type offsetEngine struct {
	keys  []string
	calls []SearchRequest
}

func (o *offsetEngine) Name() string { return "offset" }
func (o *offsetEngine) Capabilities() Capabilities {
	return Capabilities{OffsetPaging: true, ExactTotals: true}
}
func (o *offsetEngine) Supports(Source) error { return nil }

func (o *offsetEngine) MultiSearch(_ context.Context, reqs []SearchRequest) ([]SearchResult, error) {
	o.calls = append(o.calls, reqs...)
	req := reqs[0]
	start := min(req.Offset, len(o.keys))
	end := min(start+req.Limit, len(o.keys))
	res := hits(end < len(o.keys), o.keys[start:end]...)
	res.Total = len(o.keys)
	return []SearchResult{res}, nil
}

// TestGetRestoresTheEnginesOrderAfterHydration.
//
// This is the whole reason Get exists rather than two lines at a call site. A
// hydrator loads by key, a database returns those rows in whatever order its
// plan produces, and hydrating without re-sorting discards the ranking the
// search was for — silently, because every row is correct and only the order
// is wrong. The loader here answers in key order, which is exactly the
// plausible wrong answer.
func TestGetRestoresTheEnginesOrderAfterHydration(t *testing.T) {
	engine := &fakeEngine{results: []SearchResult{hits(false, "c", "a", "b")}}
	load := corpus("a", "b", "c")

	got, err := For[*post](engine, source("posts"), "postgres").Hydrate(load).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	for _, p := range got {
		order = append(order, p.key)
	}
	if strings.Join(order, ",") != "c,a,b" {
		t.Errorf("got %v, want the engine's order c,a,b — the hydrator answered %v",
			order, []string{"a", "b", "c"})
	}
	// And the hydrator was asked once, for every key, rather than per hit.
	if len(load.askedIn) != 3 {
		t.Errorf("the hydrator was asked for %v, want all three keys in one call", load.askedIn)
	}
}

// TestAKeyWithNoModelIsDroppedRatherThanReturnedEmpty.
//
// A document can outlive its row — briefly after a delete, indefinitely after
// one that was never synced. A zero value in the middle of a result list is a
// nil dereference at the call site; a shorter list is a missing entry.
func TestAKeyWithNoModelIsDroppedRatherThanReturnedEmpty(t *testing.T) {
	engine := &fakeEngine{results: []SearchResult{hits(false, "a", "gone", "b")}}

	got, err := For[*post](engine, source("posts"), "postgres").
		Hydrate(corpus("a", "b")).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2", len(got))
	}
	for _, p := range got {
		if p == nil {
			t.Fatal("a nil model reached the caller")
		}
	}
	if got[0].key != "a" || got[1].key != "b" {
		t.Errorf("got %q then %q, want the surviving two in engine order", got[0].key, got[1].key)
	}
}

// TestGetWithoutAHydratorIsRefused, rather than returning nothing found.
func TestGetWithoutAHydratorIsRefused(t *testing.T) {
	engine := &fakeEngine{results: []SearchResult{hits(false, "a")}}

	if _, err := For[*post](engine, source("posts"), "postgres").Get(context.Background()); !errors.Is(err, ErrNoHydrator) {
		t.Fatalf("got %v, want ErrNoHydrator", err)
	}
	err := For[*post](engine, source("posts"), "postgres").Each(context.Background(), func(*post) error { return nil })
	if !errors.Is(err, ErrNoHydrator) {
		t.Fatalf("Each: got %v, want ErrNoHydrator", err)
	}
}

// TestAFilterIsRefusedByAnEngineThatReadsNone.
//
// The dangerous outcome is not an error: it is a page of results from a query
// the caller did not ask for, because the restriction was dropped on the way
// in. So the builder checks the capability before any query runs and names
// the engine.
func TestAFilterIsRefusedByAnEngineThatReadsNone(t *testing.T) {
	engine := &fakeEngine{caps: Capabilities{Filters: false}}

	_, err := For[*post](engine, source("posts"), "postgres").
		Where("status", "published").Keys(context.Background())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "fake") {
		t.Errorf("the error does not name the engine: %v", err)
	}
	if len(engine.seen) != 0 {
		t.Error("a query ran for a filter the engine would have ignored")
	}
}

// TestAPrefixSearchIsRefusedByAnEngineWithoutIt, for the same reason: an
// as-you-type box against an engine that matches whole words only answers
// nothing until the last keystroke, which reads as a broken index.
func TestAPrefixSearchIsRefusedByAnEngineWithoutIt(t *testing.T) {
	engine := &fakeEngine{caps: Capabilities{Prefix: false}}

	_, err := For[*post](engine, source("posts"), "post").Prefix().Keys(context.Background())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

// TestEachPagingMechanismIsRefusedByTheEngineThatDoesNotUseIt.
//
// A cursor an offset engine discards is a walk that returns page one forever,
// and an offset a cursor engine discards is a page of results that is not the
// page asked for. Neither can be detected by the caller, so neither is
// accepted.
func TestEachPagingMechanismIsRefusedByTheEngineThatDoesNotUseIt(t *testing.T) {
	offset := &fakeEngine{caps: Capabilities{OffsetPaging: true}}
	if _, err := For[*post](offset, source("posts"), "postgres").After("a").Keys(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("After against an offset engine: got %v, want ErrUnsupported", err)
	}

	cursor := &fakeEngine{caps: Capabilities{OffsetPaging: false}}
	if _, err := For[*post](cursor, source("posts"), "postgres").Skip(10).Keys(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Skip against a cursor engine: got %v, want ErrUnsupported", err)
	}
}

// TestSkipAndAfterTogetherAreRefused, because they are two answers to one
// question and silently preferring either returns a different page than the
// one asked for.
func TestSkipAndAfterTogetherAreRefused(t *testing.T) {
	engine := &fakeEngine{caps: Capabilities{OffsetPaging: true}}

	_, err := For[*post](engine, source("posts"), "postgres").Skip(10).After("a").Keys(context.Background())
	if !errors.Is(err, ErrPagingConflict) {
		t.Fatalf("got %v, want ErrPagingConflict", err)
	}
}

// TestWhereInWithNoValuesIsRefused.
//
// An empty set almost always means a slice that happened to be empty, so
// reading it as "match nothing" answers an empty page that looks like a
// corpus problem rather than a bug at the call site.
func TestWhereInWithNoValuesIsRefused(t *testing.T) {
	engine := &fakeEngine{caps: Capabilities{Filters: true}}

	_, err := For[*post](engine, source("posts"), "postgres").
		WhereIn("status").Keys(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no values") {
		t.Fatalf("got %v, want a refusal naming the empty set", err)
	}
}

// TestTheFirstMisuseIsTheOneReported, since a chain cannot return an error
// from every link and the second complaint is usually a consequence of the
// first.
func TestTheFirstMisuseIsTheOneReported(t *testing.T) {
	engine := &fakeEngine{caps: Capabilities{Filters: true}}

	_, err := For[*post](engine, source("posts"), "postgres").
		WhereIn("status").WhereIn("kind").Keys(context.Background())
	if err == nil || strings.Count(err.Error(), "no values") != 1 {
		t.Fatalf("got %v, want exactly one complaint", err)
	}
	if !strings.Contains(err.Error(), "status") {
		t.Errorf("the second misuse was reported instead of the first: %v", err)
	}
}

// TestAnEmptyQueryReachesNoEngineThroughTheBuilder, matching the grouped
// path. Eight index scans for an empty box is work nobody asked for.
func TestAnEmptyQueryReachesNoEngineThroughTheBuilder(t *testing.T) {
	engine := &fakeEngine{caps: Capabilities{OffsetPaging: true}}
	load := corpus("a")

	keys, err := For[*post](engine, source("posts"), "   ").Hydrate(load).Keys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("got %v, want nothing", keys)
	}

	var walked int
	if err := For[*post](engine, source("posts"), "").Hydrate(load).
		Each(context.Background(), func(*post) error { walked++; return nil }); err != nil {
		t.Fatal(err)
	}
	if walked != 0 {
		t.Errorf("Each called its function %d times for an empty query", walked)
	}
	if len(engine.seen) != 0 {
		t.Errorf("%d queries ran for an unsearchable query", len(engine.seen))
	}
}

// TestARefusedSourceFailsTheBuilder, rather than being searched anyway.
func TestARefusedSourceFailsTheBuilder(t *testing.T) {
	engine := &fakeEngine{refuse: ErrVisibilityUnenforceable}

	_, err := For[*post](engine, source("posts"), "postgres").Keys(context.Background())
	if !errors.Is(err, ErrVisibilityUnenforceable) {
		t.Fatalf("got %v, want the engine's refusal", err)
	}
	if len(engine.seen) != 0 {
		t.Error("the source was searched after being refused")
	}
}

// TestEachWalksEveryDocumentExactlyOnce over a corpus larger than one page.
func TestEachWalksEveryDocumentExactlyOnce(t *testing.T) {
	var keys []string
	for i := range 23 {
		keys = append(keys, fmt.Sprintf("k%02d", i))
	}
	engine := &offsetEngine{keys: keys}

	var seen []string
	err := For[*post](engine, source("posts"), "postgres").
		Take(5).Hydrate(corpus(keys...)).
		Each(context.Background(), func(p *post) error { seen = append(seen, p.key); return nil })
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(seen, ",") != strings.Join(keys, ",") {
		t.Fatalf("walked %d of %d documents: %v", len(seen), len(keys), seen)
	}
	// Five pages of five and one of three: the walk stops on the engine's own
	// More rather than on a count comparison, which is the only thing correct
	// for an estimated total.
	if len(engine.calls) != 5 {
		t.Errorf("the walk took %d pages, want 5", len(engine.calls))
	}
}

// TestEachStopsAtTheFirstErrorItsFunctionReturns, and reports it unchanged so
// a caller can use a sentinel to break out.
func TestEachStopsAtTheFirstErrorItsFunctionReturns(t *testing.T) {
	var keys []string
	for i := range 12 {
		keys = append(keys, fmt.Sprintf("k%02d", i))
	}
	engine := &offsetEngine{keys: keys}
	stop := errors.New("enough")

	var seen int
	err := For[*post](engine, source("posts"), "postgres").
		Take(5).Hydrate(corpus(keys...)).
		Each(context.Background(), func(*post) error {
			seen++
			if seen == 3 {
				return stop
			}
			return nil
		})
	if !errors.Is(err, stop) {
		t.Fatalf("got %v, want the function's own error", err)
	}
	if seen != 3 {
		t.Errorf("the function was called %d times after returning an error", seen)
	}
	if len(engine.calls) != 1 {
		t.Errorf("the walk fetched %d pages after being told to stop", len(engine.calls))
	}
}

// TestRawEscapesTheSnippetTheSameWayTheGroupedPathDoes.
//
// Both terminals go through groupOf, which is the one place a RawHit becomes
// a Hit — so the escaping promise in Hit.Snippet is kept once rather than
// once per entry point.
func TestRawEscapesTheSnippetTheSameWayTheGroupedPathDoes(t *testing.T) {
	res := hits(false, "a")
	res.Hits[0].Headline = HighlightPreTag + "<script>" + HighlightPostTag + " b & c"
	engine := &fakeEngine{results: []SearchResult{res}}

	group, err := For[*post](engine, source("posts"), "postgres").Raw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snippet := string(group.Hits[0].Snippet)
	if strings.Contains(snippet, "<script>") {
		t.Fatalf("the engine's text reached the page unescaped: %q", snippet)
	}
	if !strings.Contains(snippet, "<mark>") || !strings.Contains(snippet, "&amp;") {
		t.Fatalf("got %q, want the match marked and the rest escaped", snippet)
	}
}

// TestRawCarriesTheSourceAndTheTotals, so a list can be rendered from the
// index alone without loading a model.
func TestRawCarriesTheSourceAndTheTotals(t *testing.T) {
	res := hits(true, "a", "b")
	res.Total, res.Estimated = 97, true
	engine := &fakeEngine{results: []SearchResult{res}}

	group, err := For[*post](engine, fakeSource{name: "posts", lbl: "Posts",
		vis: Visibility{Filter: "published = true"}}, "postgres").Raw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if group.Source != "posts" || group.Label != "Posts" {
		t.Errorf("got source %q label %q", group.Source, group.Label)
	}
	if group.Total != 97 || !group.Estimated || !group.More {
		t.Errorf("got total %d estimated %v more %v", group.Total, group.Estimated, group.More)
	}
	for _, hit := range group.Hits {
		if hit.Source != "posts" {
			t.Errorf("hit source is %q, want the source the core asked for", hit.Source)
		}
	}
}

// TestThePageSizeIsClampedOnTheBuilderPathToo, so an engine never has to
// carry a default of its own.
func TestThePageSizeIsClampedOnTheBuilderPathToo(t *testing.T) {
	engine := &fakeEngine{caps: Capabilities{OffsetPaging: true}}

	if _, err := For[*post](engine, source("posts"), "postgres").Keys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := engine.seen[0].Limit; got != DefaultPerSource {
		t.Errorf("an unasked limit reached the engine as %d, want %d", got, DefaultPerSource)
	}

	engine.seen = nil
	if _, err := For[*post](engine, source("posts"), "postgres").
		Take(MaxPerSource * 10).Keys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := engine.seen[0].Limit; got != MaxPerSource {
		t.Errorf("an oversized limit reached the engine as %d, want %d", got, MaxPerSource)
	}
}

// TestAViewerOfAnUnbindableTypeIsRefusedByTheBuilder, at the call that set it
// rather than from inside a driver.
func TestAViewerOfAnUnbindableTypeIsRefusedByTheBuilder(t *testing.T) {
	engine := &fakeEngine{caps: Capabilities{OffsetPaging: true}}

	_, err := For[*post](engine, source("posts"), "postgres").
		Viewer(struct{ ID int }{7}).Keys(context.Background())
	if !errors.Is(err, ErrBadViewer) {
		t.Fatalf("got %v, want ErrBadViewer", err)
	}
	if len(engine.seen) != 0 {
		t.Error("a query ran with a viewer no engine can bind")
	}
}
