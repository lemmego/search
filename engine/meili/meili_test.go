package meili

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lemmego/search"
)

// recorded is one request the engine made.
type recorded struct {
	method string
	path   string
	query  string
	body   map[string]any
	raw    string
}

// server records every request and answers with a task or a search result.
//
// The whole reason this package is hand-rolled rather than built on the Go
// SDK is that the requests are the contract: POST against PUT, documents
// against indexes, which tags are asked for. An SDK would hide exactly the
// decisions worth asserting.
func server(t *testing.T, answer func(r *http.Request) any) (*Engine, *[]recorded) {
	t.Helper()

	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := new(strings.Builder)
		body := map[string]any{}
		if r.Body != nil {
			decoded, _ := readAll(r)
			raw.WriteString(decoded)
			_ = json.Unmarshal([]byte(decoded), &body)
		}
		got = append(got, recorded{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			body: body, raw: raw.String(),
		})

		w.Header().Set("Content-Type", "application/json")
		payload := any(map[string]any{"taskUid": 7, "status": "succeeded"})
		if answer != nil {
			if custom := answer(r); custom != nil {
				payload = custom
			}
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)

	return New(search.MeiliConfig{Host: srv.URL}, ""), &got
}

func readAll(r *http.Request) (string, error) {
	payload, err := io.ReadAll(r.Body)
	return string(payload), err
}

func find(t *testing.T, got []recorded, method, pathContains string) recorded {
	t.Helper()
	for _, r := range got {
		if r.method == method && strings.Contains(r.path, pathContains) {
			return r
		}
	}
	t.Fatalf("no %s request to a path containing %q; made %v", method, pathContains, paths(got))
	return recorded{}
}

func paths(got []recorded) []string {
	out := make([]string, 0, len(got))
	for _, r := range got {
		out = append(out, r.method+" "+r.path)
	}
	return out
}

// TestIndexingAddsOrReplacesRatherThanMerging.
//
// POST is add-or-replace; PUT is add-or-update, a field-wise merge. One
// letter, and with the wrong one a field a model stops setting stays in the
// document and stays findable forever.
func TestIndexingAddsOrReplacesRatherThanMerging(t *testing.T) {
	engine, got := server(t, nil)

	err := engine.Index(context.Background(), []search.Document{
		{Index: "posts", Key: "1", Fields: map[string]any{"title": "Quince"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := find(t, *got, http.MethodPost, "/documents")
	if req.method != http.MethodPost {
		t.Errorf("documents were written with %s, want POST — PUT merges", req.method)
	}
	if !strings.Contains(req.query, "primaryKey="+KeyAttribute) {
		t.Errorf("the write did not name the primary key: %q", req.query)
	}
	if !strings.Contains(req.raw, `"`+KeyAttribute+`":"1"`) {
		t.Errorf("the document does not carry its key under %q: %s", KeyAttribute, req.raw)
	}
}

// TestFlushEmptiesTheDocumentsAndKeepsTheIndex.
//
// DELETE /indexes/{uid} would take the settings with it — the primary key and
// the searchable and filterable attributes — so the next write would
// recreate the index unconfigured and infer a key from whatever arrived
// first.
func TestFlushEmptiesTheDocumentsAndKeepsTheIndex(t *testing.T) {
	engine, got := server(t, nil)

	if err := engine.Flush(context.Background(), "posts"); err != nil {
		t.Fatal(err)
	}

	req := find(t, *got, http.MethodDelete, "/documents")
	if !strings.HasSuffix(req.path, "/indexes/posts/documents") {
		t.Errorf("flush went to %q, want the documents collection", req.path)
	}
	for _, r := range *got {
		if r.method == http.MethodDelete && strings.HasSuffix(r.path, "/indexes/posts") {
			t.Error("flush deleted the index itself, which drops its settings")
		}
	}
}

// TestAnIndexIsCreatedWithAnExplicitPrimaryKey.
//
// Meilisearch creates an index on the first document write and infers the
// primary key from that document, so an index keyed on the wrong attribute is
// one careless write away — and it can only be fixed by dropping the index.
func TestAnIndexIsCreatedWithAnExplicitPrimaryKey(t *testing.T) {
	engine, got := server(t, nil)

	err := engine.CreateIndex(context.Background(), "posts", search.IndexOptions{
		Searchable: []string{"title", "body"},
		Filterable: []string{"status", "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}

	create := find(t, *got, http.MethodPost, "/indexes")
	if create.body["primaryKey"] != KeyAttribute {
		t.Errorf("the index was created with primaryKey %v, want %q",
			create.body["primaryKey"], KeyAttribute)
	}

	settings := find(t, *got, http.MethodPatch, "/settings")
	if settings.body["filterableAttributes"] == nil {
		t.Error("the filterable attributes were not declared; a filter naming " +
			"an undeclared attribute fails at search time")
	}
}

// TestASearchAsksForTheSentinelsSnippetExpects, so search.Snippet works on
// the result unchanged and the escaping is not reimplemented here.
func TestASearchAsksForTheSentinelsSnippetExpects(t *testing.T) {
	engine, got := server(t, func(r *http.Request) any {
		if strings.Contains(r.URL.Path, "multi-search") {
			return map[string]any{"results": []map[string]any{{
				"hits": []map[string]any{}, "totalHits": 0, "page": 1, "totalPages": 0,
			}}}
		}
		return nil
	})

	src := Source{Index: "posts", Title: "title", Snippet: []string{"body"},
		PublicAttribute: "is_public"}
	_, err := engine.MultiSearch(context.Background(), []search.SearchRequest{
		{Source: src, Text: "quince", Limit: 5},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := find(t, *got, http.MethodPost, "multi-search")
	queries, _ := req.body["queries"].([]any)
	if len(queries) != 1 {
		t.Fatalf("sent %d queries, want 1", len(queries))
	}
	query, _ := queries[0].(map[string]any)

	if query["highlightPreTag"] != search.HighlightPreTag ||
		query["highlightPostTag"] != search.HighlightPostTag {
		t.Errorf("the highlight tags are %q/%q, want the sentinels search.Snippet "+
			"looks for — asking for <mark> directly would mix real markup into "+
			"somebody's prose with no way to escape one without the other",
			query["highlightPreTag"], query["highlightPostTag"])
	}

	// hitsPerPage/page, not offset/limit: only the former answers with an
	// exhaustive totalHits.
	if query["hitsPerPage"] == nil || query["page"] == nil {
		t.Errorf("the query pages with %v, want hitsPerPage and page", query)
	}
	if query["offset"] != nil {
		t.Error("the query uses offset, whose totals are estimates that jump between pages")
	}
}

// TestTheViewerIsNeverPastedIntoAFilter.
//
// The only place a value somebody chose is written into a filter expression,
// so the only place that can get it wrong. An unescaped quote ends the
// literal and hands the rest of the expression to whoever picked the key.
func TestTheViewerIsNeverPastedIntoAFilter(t *testing.T) {
	src := Source{Index: "posts", OwnerAttribute: "owner", PublicAttribute: "is_public"}

	hostile := `" OR is_public = true OR "`
	filter, err := src.filter(hostile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filter, `" OR is_public = true OR "`) {
		t.Fatalf("the viewer broke out of its literal: %s", filter)
	}
	if !strings.Contains(filter, `\"`) {
		t.Errorf("the quote was not escaped: %s", filter)
	}

	// A backslash must not escape the closing quote either.
	filter, err = src.filter(`ada\`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filter, `ada\\`) {
		t.Errorf("a trailing backslash was not escaped: %s", filter)
	}

	if _, err := src.filter(1.5, nil); !errors.Is(err, search.ErrBadViewer) {
		t.Errorf("a float viewer gave %v, want ErrBadViewer", err)
	}
}

// TestAnAnonymousViewerSeesOnlyWhatIsPublic.
func TestAnAnonymousViewerSeesOnlyWhatIsPublic(t *testing.T) {
	src := Source{Index: "posts", OwnerAttribute: "owner", PublicAttribute: "is_public"}

	filter, err := src.filter(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filter, "owner") {
		t.Errorf("an anonymous filter mentions the owner: %s", filter)
	}
	if !strings.Contains(filter, "is_public = true") {
		t.Errorf("an anonymous filter does not restrict to public: %s", filter)
	}

	// And with no public attribute at all, nothing is visible rather than
	// everything — the clause is a contradiction rather than an omission.
	ownerOnly := Source{Index: "posts", OwnerAttribute: "owner"}
	filter, err = ownerOnly.filter(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filter, "NOT EXISTS") {
		t.Errorf("an owner-only rule with no viewer composed %q; omitting the "+
			"clause would widen the rule to the whole index", filter)
	}
}

// TestASourceDeclaringNoRuleIsRefused.
func TestASourceDeclaringNoRuleIsRefused(t *testing.T) {
	engine, _ := server(t, nil)

	if err := engine.Supports(Source{Index: "posts"}); !errors.Is(err, search.ErrNoVisibility) {
		t.Errorf("got %v, want ErrNoVisibility", err)
	}
}

// TestASQLOnlyRuleIsRefusedWithAReason.
func TestASQLOnlyRuleIsRefusedWithAReason(t *testing.T) {
	engine, _ := server(t, nil)

	err := engine.Supports(search.Table{
		Name: "posts", From: "posts", Vector: "search_vector",
		ID: "id", Title: "title", Visible: "deleted_at IS NULL",
	})
	if !errors.Is(err, search.ErrSourceMismatch) {
		t.Errorf("got %v, want ErrSourceMismatch for a Table", err)
	}
}

// TestSettleWaitsForTheTaskRatherThanATimer.
func TestSettleWaitsForTheTaskRatherThanATimer(t *testing.T) {
	var polls int
	engine, got := server(t, func(r *http.Request) any {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			polls++
			if polls < 3 {
				return map[string]any{"status": "processing"}
			}
			return map[string]any{"status": "succeeded"}
		}
		return nil
	})

	if err := engine.Index(context.Background(), []search.Document{
		{Index: "posts", Key: "1", Fields: map[string]any{"title": "Quince"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Settle(context.Background()); err != nil {
		t.Fatal(err)
	}

	if polls < 3 {
		t.Errorf("Settle polled %d times, so it did not wait for the task", polls)
	}
	find(t, *got, http.MethodGet, "/tasks/7")
}

// TestSettleReportsAFailedTask rather than returning as though the write
// landed.
func TestSettleReportsAFailedTask(t *testing.T) {
	engine, _ := server(t, func(r *http.Request) any {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			return map[string]any{"status": "failed", "error": map[string]any{
				"message": "index not found", "code": "index_not_found",
			}}
		}
		return nil
	})

	if err := engine.Index(context.Background(), []search.Document{
		{Index: "posts", Key: "1", Fields: map[string]any{"title": "Q"}},
	}); err != nil {
		t.Fatal(err)
	}
	err := engine.Settle(context.Background())
	if err == nil {
		t.Fatal("Settle returned nil for a failed task, so an import would " +
			"report documents as indexed that are not")
	}
	if !strings.Contains(err.Error(), "index_not_found") {
		t.Errorf("the error does not carry the cause: %v", err)
	}
}

// TestNothingToSettleIsNotAPoll, so Settle on a fresh engine is free.
func TestNothingToSettleIsNotAPoll(t *testing.T) {
	engine, got := server(t, nil)

	if err := engine.Settle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Errorf("Settle made %v with nothing outstanding", paths(*got))
	}
}

// TestAnIndexNameCarriesThePrefix, which is what bounds Flush.
func TestAnIndexNameCarriesThePrefix(t *testing.T) {
	engine := New(search.MeiliConfig{Host: "http://x"}, "staging")
	if got := engine.name("posts"); got != "staging_posts" {
		t.Errorf("name = %q, want the prefix applied — without it a flush from "+
			"a laptop empties production", got)
	}
}

// TestABadKeyIsReportedAsOne, mapped onto this module's sentinel.
func TestABadKeyIsReportedAsOne(t *testing.T) {
	engine, _ := server(t, nil)

	err := engine.Index(context.Background(), []search.Document{
		{Index: "posts", Key: "user:7", Fields: map[string]any{}},
	})
	if !errors.Is(err, search.ErrBadKey) {
		t.Errorf("got %v, want ErrBadKey — Meilisearch accepts only "+
			"alphanumerics, hyphen and underscore in a document id", err)
	}
}

// TestUnindexingUsesTheBatchEndpoint.
func TestUnindexingUsesTheBatchEndpoint(t *testing.T) {
	engine, got := server(t, nil)

	if err := engine.Unindex(context.Background(), "posts", []string{"1", "2"}); err != nil {
		t.Fatal(err)
	}
	find(t, *got, http.MethodPost, "/documents/delete-batch")
}

// TestUnindexingNothingMakesNoRequest.
func TestUnindexingNothingMakesNoRequest(t *testing.T) {
	engine, got := server(t, nil)

	if err := engine.Unindex(context.Background(), "posts", nil); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Errorf("made %v for an empty key list", paths(*got))
	}
}
