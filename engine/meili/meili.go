// Package meili is a Meilisearch engine for lemmego/search.
//
// Hand-rolled over net/http, with no SDK, for the reasons pay/stripe gives
// about the Stripe API: the surface used here is nine endpoints; an SDK pins
// behaviours in code that are easier to read as requests; its types model
// everything rather than the handful of fields this needs; and a hand-rolled
// client is assertable against an httptest server, so the task polling, the
// highlight tags and the composed filter are checked rather than assumed.
//
// It also keeps the whole module standard-library only, which search's
// deps_test.go holds as a line.
//
// # What this engine cannot do
//
// It reports RelationalVisibility false, and that is the important sentence
// in this package. Meilisearch filters compare declared attributes; they
// cannot evaluate an EXISTS against another table, because the index holds a
// copy of the documents and no access to the rows. An application whose rule
// is "visible if the viewer follows the author, unless the post is in a
// private circle the viewer is not a member of" cannot enforce it here, and
// this engine refuses such a source rather than searching rows it cannot
// filter. Keep those sources on the postgres engine, or denormalise the
// audience into the document and express the rule as a filter.
package meili

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lemmego/search"
)

// Name is the name this engine registers under.
const Name = "meilisearch"

// KeyAttribute is the attribute a document's key is stored under.
//
// Named "key" and not "id" deliberately: a model's own id is usually among
// its fields for display, and Meilisearch infers the primary key from the
// first document it is given when the index was created implicitly — so an
// index keyed on the wrong attribute, with no way to change it short of
// dropping the index, is one careless write away. CreateIndex names it
// explicitly for the same reason.
const KeyAttribute = "key"

// Engine searches a Meilisearch instance.
type Engine struct {
	host   string
	apiKey string
	prefix string
	client *http.Client

	// mu guards lastTask, the high-water mark Settle waits on.
	//
	// Task uids are monotonic, so one number is enough: waiting for the last
	// task this engine issued is waiting for every earlier one, because
	// Meilisearch processes them in the order they were enqueued. That is
	// what makes Settle terminate because a task finished rather than because
	// a timer expired.
	mu       sync.Mutex
	lastTask int64
}

// New returns an engine over an instance.
func New(cfg search.MeiliConfig, prefix string) *Engine {
	timeout := cfg.Timeout
	if timeout == 0 {
		// http.Client's own default is no timeout at all, so a hung instance
		// would hold a request goroutine until it gave up, which it never
		// would.
		timeout = 10 * time.Second
	}
	return &Engine{
		host:   strings.TrimRight(cfg.Host, "/"),
		apiKey: cfg.APIKey,
		prefix: prefix,
		client: &http.Client{Timeout: timeout},
	}
}

func init() {
	search.Register(Name, func(ctx context.Context, cfg *search.Config) (search.Engine, error) {
		if cfg.Meili.Host == "" {
			return nil, fmt.Errorf("the meilisearch engine needs a host in Config.Meili.Host")
		}
		engine := New(cfg.Meili, cfg.Prefix)
		// Reachability checked here rather than on the first search, so a
		// wrong address stops a deployment instead of degrading it into an
		// application that finds nothing.
		if err := engine.Ping(ctx); err != nil {
			return nil, fmt.Errorf("the meilisearch engine cannot reach %s: %w",
				cfg.Meili.Host, err)
		}
		return engine, nil
	})
}

// Name reports the engine's name.
func (e *Engine) Name() string { return Name }

// Capabilities reports what this engine can do.
func (e *Engine) Capabilities() search.Capabilities {
	return search.Capabilities{
		RelationalVisibility: false,
		FilterVisibility:     true,
		// False although this engine asks for exhaustive totals.
		//
		// Meilisearch has two paging modes: offset/limit answers with
		// estimatedTotalHits, which is explicitly non-exhaustive and can jump
		// between pages, and hitsPerPage/page answers with an exhaustive
		// totalHits — capped by the index's maxTotalHits, default 1000. This
		// engine uses the second, so the number is right up to the cap.
		//
		// "Exact up to 1000" is not exact. A capability that holds 99% of the
		// time is worse than one that is false, because a caller who believes
		// it builds "page 300 of 412" and is wrong precisely on the corpora
		// where it matters.
		ExactTotals:  false,
		OffsetPaging: true,
		Prefix:       true,
		Highlighting: true,
		Filters:      true,
		Asynchronous: true,
	}
}

// Ping reports whether the instance is reachable.
func (e *Engine) Ping(ctx context.Context) error {
	var health struct{ Status string }
	return e.do(ctx, http.MethodGet, "/health", nil, &health)
}

// Supports reports whether this engine can serve a source.
func (e *Engine) Supports(src search.Source) error {
	source, ok := src.(Source)
	if !ok {
		return fmt.Errorf("%w: the meilisearch engine takes a meili.Source, got %T",
			search.ErrSourceMismatch, src)
	}
	if source.Index == "" {
		return fmt.Errorf("%w: a source must name an index", search.ErrBadTable)
	}

	vis := source.Visibility()
	if !vis.Declared() {
		return fmt.Errorf("%w: %q", search.ErrNoVisibility, source.Index)
	}
	if vis.Filter == "" && !vis.AtIndexTime {
		return fmt.Errorf("%w: %q declares only a SQL predicate, which only an "+
			"engine reading the rows themselves can evaluate. Give it a Filter, "+
			"enforce it at index time with a Conditional model, or keep this "+
			"source on the postgres engine.",
			search.ErrVisibilityUnenforceable, source.Index)
	}
	return nil
}

// Settle blocks until every write this engine has accepted is searchable.
func (e *Engine) Settle(ctx context.Context) error {
	e.mu.Lock()
	target := e.lastTask
	e.mu.Unlock()

	if target == 0 {
		return nil
	}

	// Polled rather than slept: the loop ends because the task reached a
	// terminal status, so it is as fast as the instance and cannot pass
	// early. A ticker is the only timing here, and it bounds how often the
	// instance is asked rather than how long the caller waits.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		var task struct {
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := e.do(ctx, http.MethodGet, "/tasks/"+strconv.FormatInt(target, 10), nil, &task); err != nil {
			return err
		}

		switch task.Status {
		case "succeeded":
			return nil
		case "failed", "canceled":
			return fmt.Errorf("meilisearch task %d %s: %s (%s)",
				target, task.Status, task.Error.Message, task.Error.Code)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// CreateIndex creates and configures an index.
//
// Explicitly, and before anything is written to it. Meilisearch creates an
// index on the first document write and *infers* the primary key from that
// document, so a document carrying its own "id" yields an index keyed on the
// wrong attribute that can only be fixed by dropping it.
func (e *Engine) CreateIndex(ctx context.Context, index string, opts search.IndexOptions) error {
	body := map[string]any{"uid": e.name(index), "primaryKey": KeyAttribute}
	if err := e.do(ctx, http.MethodPost, "/indexes", body, nil); err != nil {
		// An index that already exists is the normal case on a second boot.
		if !strings.Contains(err.Error(), "index_already_exists") {
			return err
		}
	}

	settings := map[string]any{}
	if len(opts.Searchable) > 0 {
		settings["searchableAttributes"] = opts.Searchable
	}
	if len(opts.Filterable) > 0 {
		// Declared, because a filter naming an undeclared attribute is a
		// Meilisearch error at search time — which is the good failure, and
		// the reason the visibility attributes have to be listed here.
		settings["filterableAttributes"] = opts.Filterable
	}
	if len(opts.Sortable) > 0 {
		settings["sortableAttributes"] = opts.Sortable
	}
	if len(settings) == 0 {
		return nil
	}
	return e.enqueue(ctx, http.MethodPatch, "/indexes/"+e.name(index)+"/settings", settings)
}

// DeleteIndex removes an index and everything in it.
func (e *Engine) DeleteIndex(ctx context.Context, index string) error {
	return e.enqueue(ctx, http.MethodDelete, "/indexes/"+e.name(index), nil)
}

// Index writes documents, replacing each wholly.
func (e *Engine) Index(ctx context.Context, docs []search.Document) error {
	byIndex := map[string][]map[string]any{}

	for _, doc := range docs {
		if err := search.ValidKey(doc.Key); err != nil {
			return err
		}
		payload := make(map[string]any, len(doc.Fields)+1)
		for k, v := range doc.Fields {
			payload[k] = v
		}
		payload[KeyAttribute] = doc.Key
		byIndex[doc.Index] = append(byIndex[doc.Index], payload)
	}

	for index, payload := range byIndex {
		// POST, which is add-or-replace. PUT is add-or-*update*, a field-wise
		// merge — so a field a model stops setting would stay in the document
		// and stay findable forever. One letter, and it is the difference
		// between a withdrawn value disappearing and never disappearing.
		path := "/indexes/" + e.name(index) + "/documents?primaryKey=" + KeyAttribute
		if err := e.enqueue(ctx, http.MethodPost, path, payload); err != nil {
			return err
		}
	}
	return nil
}

// Unindex removes documents by key.
func (e *Engine) Unindex(ctx context.Context, index string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	// delete-batch rather than one request per key, and it reports success
	// for a key that was not there — which is what lets search.Sync remove
	// the document of a model that is no longer searchable without first
	// asking whether one was ever written.
	return e.enqueue(ctx, http.MethodPost,
		"/indexes/"+e.name(index)+"/documents/delete-batch", keys)
}

// Flush empties an index without removing it.
func (e *Engine) Flush(ctx context.Context, index string) error {
	// DELETE on the documents collection, not on the index. Deleting the
	// index would take its settings with it — the primary key, the searchable
	// and filterable attributes — so the next write would recreate it
	// unconfigured and infer a key from whatever arrived first.
	return e.enqueue(ctx, http.MethodDelete,
		"/indexes/"+e.name(index)+"/documents", nil)
}

// MultiSearch answers each request.
func (e *Engine) MultiSearch(ctx context.Context, reqs []search.SearchRequest) ([]search.SearchResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}

	queries := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		source, ok := req.Source.(Source)
		if !ok {
			return nil, fmt.Errorf("%w: got %T", search.ErrSourceMismatch, req.Source)
		}
		filter, err := source.filter(req.Viewer, req.Filters)
		if err != nil {
			return nil, err
		}

		// hitsPerPage/page rather than offset/limit: see Capabilities.
		page := 1
		if req.Limit > 0 {
			page = req.Offset/req.Limit + 1
		}

		query := map[string]any{
			"indexUid":    e.name(source.Index),
			"q":           req.Text,
			"hitsPerPage": req.Limit,
			"page":        page,
			// The sentinels search.Snippet already looks for. Not "<mark>":
			// the string coming back would then contain real markup mixed
			// into somebody's prose, and there would be no way to escape the
			// prose without also escaping the markup.
			"highlightPreTag":  search.HighlightPreTag,
			"highlightPostTag": search.HighlightPostTag,
		}
		if filter != "" {
			query["filter"] = filter
		}
		if len(source.Snippet) > 0 {
			query["attributesToHighlight"] = source.Snippet
			query["attributesToCrop"] = source.Snippet
			// Matched to ts_headline's MaxWords and FragmentDelimiter, so the
			// two engines produce snippets of the same shape.
			query["cropLength"] = 18
			query["cropMarker"] = " … "
		}
		queries = append(queries, query)
	}

	var answer struct {
		Results []struct {
			Hits        []map[string]any `json:"hits"`
			TotalHits   int              `json:"totalHits"`
			TotalPages  int              `json:"totalPages"`
			Page        int              `json:"page"`
			HitsPerPage int              `json:"hitsPerPage"`
		} `json:"results"`
	}
	if err := e.do(ctx, http.MethodPost, "/multi-search",
		map[string]any{"queries": queries}, &answer); err != nil {
		return nil, err
	}
	if len(answer.Results) != len(reqs) {
		return nil, fmt.Errorf("%w: asked %d queries and got %d answers",
			search.ErrEngineContract, len(reqs), len(answer.Results))
	}

	out := make([]search.SearchResult, 0, len(reqs))
	for i, result := range answer.Results {
		source := reqs[i].Source.(Source)

		res := search.SearchResult{
			Hits:      make([]search.RawHit, 0, len(result.Hits)),
			Total:     result.TotalHits,
			Estimated: true,
			More:      result.Page < result.TotalPages,
		}
		for _, hit := range result.Hits {
			res.Hits = append(res.Hits, source.rawHit(hit))
		}
		out = append(out, res)
	}
	return out, nil
}

// --- transport --------------------------------------------------------------

// enqueue performs a write and records the task it produced, so Settle knows
// what to wait for.
func (e *Engine) enqueue(ctx context.Context, method, path string, body any) error {
	var task struct {
		TaskUID int64 `json:"taskUid"`
	}
	if err := e.do(ctx, method, path, body, &task); err != nil {
		return err
	}

	e.mu.Lock()
	if task.TaskUID > e.lastTask {
		e.lastTask = task.TaskUID
	}
	e.mu.Unlock()
	return nil
}

func (e *Engine) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding the request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, e.host+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Bounded, as pay/stripe reads its error bodies: an error body is not a
	// reason to read an unbounded amount from a server that may be confused
	// about what it is.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading the response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return e.failure(resp.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decoding the response: %w", err)
	}
	return nil
}

func (e *Engine) failure(status int, payload []byte) error {
	var body struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	}
	_ = json.Unmarshal(payload, &body)

	if body.Message == "" {
		body.Message = strings.TrimSpace(string(payload))
	}

	err := fmt.Errorf("meilisearch answered %d: %s (%s)", status, body.Message, body.Code)

	// Mapped onto this module's sentinels where there is one, so a caller can
	// tell a bad key from an unreachable instance without matching strings.
	switch body.Code {
	case "invalid_document_id":
		return fmt.Errorf("%w: %v", search.ErrBadKey, err)
	case "index_not_found":
		return fmt.Errorf("%w: %v", search.ErrNoTable, err)
	case "invalid_search_filter", "invalid_search_filterable_attributes":
		return fmt.Errorf("%w: %v", search.ErrVisibilityUnenforceable, err)
	}
	return err
}

// name applies the configured prefix.
//
// It is what bounds Flush: without a prefix, a `search:flush posts` run from
// a laptop against a shared instance empties production's posts index.
func (e *Engine) name(index string) string {
	if e.prefix == "" {
		return index
	}
	return e.prefix + "_" + index
}

var (
	_ search.Engine       = (*Engine)(nil)
	_ search.Indexer      = (*Engine)(nil)
	_ search.IndexManager = (*Engine)(nil)
	_ search.Settler      = (*Engine)(nil)
)
