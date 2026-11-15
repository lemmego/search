package meili_test

import (
	"context"
	"os"
	"testing"

	"github.com/lemmego/search"
	"github.com/lemmego/search/engine/meili"
	"github.com/lemmego/search/enginetest"
)

// URLEnv names a Meilisearch instance. Without it these are skipped.
//
// The httptest tests in this package prove what requests are made; only this
// proves the filter syntax, the highlight round trip and the key charset are
// what Meilisearch actually accepts. Those are not optional for a change
// here.
//
//	docker run -p 7700:7700 getmeili/meilisearch:v1.11
//	SEARCH_MEILISEARCH_URL=http://127.0.0.1:7700 go test ./...
const URLEnv = "SEARCH_MEILISEARCH_URL"

// TestMeilisearchConformsToTheEngineContract runs the whole suite against a
// real instance.
//
// This is the engine the contract was written for without existing yet, so
// its passing is the evidence that the contract describes something general
// rather than the first engine's habits.
func TestMeilisearchConformsToTheEngineContract(t *testing.T) {
	host := os.Getenv(URLEnv)
	if host == "" {
		t.Skipf("set %s to run the conformance suite against Meilisearch", URLEnv)
	}

	enginetest.Run(t, func(t *testing.T) enginetest.Harness {
		// A prefix per test run, so two runs — or a run against a shared
		// instance — cannot flush each other's indexes.
		engine := meili.New(search.MeiliConfig{
			Host:   host,
			APIKey: os.Getenv("SEARCH_MEILISEARCH_KEY"),
		}, "enginetest")

		ctx := context.Background()
		if err := engine.Ping(ctx); err != nil {
			t.Skipf("%s is set but unreachable: %v", URLEnv, err)
		}

		opts := search.IndexOptions{
			Searchable: []string{"title", "body"},
			Filterable: []string{"status", "owner", "rank_hint"},
			Sortable:   []string{"rank_hint"},
		}
		for _, index := range []string{enginetest.IndexA, enginetest.IndexB} {
			if err := engine.CreateIndex(ctx, index, opts); err != nil {
				t.Fatalf("creating %s: %v", index, err)
			}
			// Emptied rather than dropped, so each case starts clean without
			// losing the settings just written.
			if err := engine.Flush(ctx, index); err != nil {
				t.Fatalf("flushing %s: %v", index, err)
			}
		}
		if err := engine.Settle(ctx); err != nil {
			t.Fatalf("settling the setup: %v", err)
		}

		return enginetest.Harness{
			Engine: engine,
			Put: func(ctx context.Context, docs []search.Document) error {
				if err := engine.Index(ctx, docs); err != nil {
					return err
				}
				// Settled here, because the suite never sleeps and never
				// retries an assertion: a write is not searchable until the
				// task that carried it has run.
				return engine.Settle(ctx)
			},
			Source: func(index string, visibleOnly bool) search.Source {
				src := meili.Source{
					Index: index, Label: index,
					Title: "title", Subtitle: "status",
					Snippet: []string{"body"},
					// Still a declared rule when it admits everything: every
					// document has a status, so this is the permissive form
					// of a real filter rather than an absent one.
					Filter:     []string{"status EXISTS"},
					Filterable: opts.Filterable,
				}
				if visibleOnly {
					src.Filter = []string{"status = published"}
				}
				return src
			},
			// A Table, which is the real case: a source written for the
			// postgres engine and handed to this one unchanged.
			Unenforceable: func(index string) search.Source {
				return search.Table{
					Name: index, From: index, Vector: "search_vector",
					ID: "id", Title: "title",
					Visible: "deleted_at IS NULL AND EXISTS (" +
						"SELECT 1 FROM follows f WHERE f.follower_id = " +
						search.ViewerPlaceholder + ")",
				}
			},
			StringKeys: true,
			Caps:       engine.Capabilities(),
		}
	})
}
