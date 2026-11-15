package search_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/lemmego/search"
	"github.com/lemmego/search/enginetest"
)

// TestPostgresConformsToTheEngineContract runs the conformance suite against
// the engine that existed before the contract did.
//
// That ordering is the point. The suite was written from the contract rather
// than from this engine's behaviour, so the cases it passes are evidence the
// contract describes something real — and the cases it skips are an honest
// record of what an engine whose index is the application's own rows cannot
// do: it keeps no documents, so there is nothing to upsert, unindex or flush.
func TestPostgresConformsToTheEngineContract(t *testing.T) {
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run the conformance suite against Postgres", DSNEnv)
	}

	enginetest.Run(t, func(t *testing.T) enginetest.Harness {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("opening: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		if err := db.Ping(); err != nil {
			t.Skipf("%s is set but unreachable: %v", DSNEnv, err)
		}

		// One table per index the suite uses, since for this engine an index
		// is a relation.
		for _, index := range []string{enginetest.IndexA, enginetest.IndexB} {
			conformanceTable(t, db, index)
		}

		engine, err := search.Build(context.Background(), &search.Config{
			Engine:   search.EnginePostgres,
			Postgres: search.PostgresConfig{DB: db},
		})
		if err != nil {
			t.Fatalf("building the engine: %v", err)
		}

		return enginetest.Harness{
			Engine: engine,
			Put:    func(ctx context.Context, docs []search.Document) error { return conformancePut(ctx, db, docs) },
			Source: func(index string, visibleOnly bool) search.Source {
				return conformanceSource(index, visibleOnly)
			},
			// Every rule this engine is offered is SQL, which is the one form
			// it can evaluate — so there is no unenforceable case for it.
			Unenforceable: nil,
			StringKeys:    false,
			Caps:          engine.Capabilities(),
		}
	})
}

// conformanceTable creates the suite's fixed schema as a real table.
func conformanceTable(t *testing.T, db *sql.DB, name string) {
	t.Helper()

	for _, statement := range []string{
		fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name),
		fmt.Sprintf(`CREATE TABLE %s (
			id        bigint PRIMARY KEY,
			title     text NOT NULL,
			body      text NOT NULL,
			status    text NOT NULL,
			owner     bigint NOT NULL,
			rank_hint bigint NOT NULL,
			search_vector tsvector GENERATED ALWAYS AS (
				setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
				setweight(to_tsvector('english', coalesce(body,  '')), 'B')
			) STORED
		)`, name),
		fmt.Sprintf(`CREATE INDEX %s_search_gin ON %s USING GIN (search_vector)`, name, name),
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("creating the conformance table: %v\n%s", err, statement)
		}
	}
	t.Cleanup(func() { db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)) })
}

// conformancePut is this engine's Put: an upsert, because its index is the
// row and writing the row is what makes a document searchable.
func conformancePut(ctx context.Context, db *sql.DB, docs []search.Document) error {
	for _, doc := range docs {
		_, err := db.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %s (id, title, body, status, owner, rank_hint)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO UPDATE SET
				title = excluded.title, body = excluded.body,
				status = excluded.status, owner = excluded.owner,
				rank_hint = excluded.rank_hint`, doc.Index),
			doc.Key,
			doc.Fields["title"], doc.Fields["body"], doc.Fields["status"],
			doc.Fields["owner"], doc.Fields["rank_hint"])
		if err != nil {
			return fmt.Errorf("putting %s/%s: %w", doc.Index, doc.Key, err)
		}
	}
	return nil
}

func conformanceSource(index string, visibleOnly bool) search.Source {
	// Still a rule when it admits everything: a source declaring none is
	// refused, which is the whole point of requiring one.
	visible := "true"
	if visibleOnly {
		visible = "status = 'published'"
	}
	return search.Table{
		Name:     index,
		Label:    index,
		From:     index,
		Vector:   "search_vector",
		ID:       "id",
		Title:    "title",
		Subtitle: "status",
		Snippet:  []string{"body"},
		Visible:  visible,
		Tiebreak: "rank_hint ASC",
	}
}
