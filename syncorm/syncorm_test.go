package syncorm_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lemmego/orm"

	"github.com/lemmego/search"
	"github.com/lemmego/search/engine/memory"
	"github.com/lemmego/search/syncorm"
)

// Post is the searchable model.
type Post struct {
	ID        int `orm:"primaryKey;autoIncrement"`
	Title     string
	Status    string
	DeletedAt *time.Time
}

func (p *Post) SearchIndex() string { return "posts" }
func (p *Post) SearchKey() string   { return strconv.Itoa(p.ID) }
func (p *Post) SearchDocument() map[string]any {
	return map[string]any{"title": p.Title, "status": p.Status}
}

// ShouldBeSearchable makes a draft unsearchable, so the withdraw path is
// exercised rather than described.
func (p *Post) ShouldBeSearchable() bool { return p.Status == "published" }

// Note is a model that is not indexable at all, which is most of an
// application's models as far as a global observer is concerned.
type Note struct {
	ID   int `orm:"primaryKey;autoIncrement"`
	Body string
}

const schema = `
CREATE TABLE posts (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	title      TEXT,
	status     TEXT,
	deleted_at DATETIME
);
CREATE TABLE notes (
	id   INTEGER PRIMARY KEY AUTOINCREMENT,
	body TEXT
);
`

func open(t *testing.T) *orm.DB {
	t.Helper()
	// File-backed rather than ":memory:", because every connection to the
	// latter gets its own empty database.
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return orm.Open(sqlDB, orm.SQLite())
}

func watched(t *testing.T, options ...syncorm.Option) (*orm.DB, *memory.Engine) {
	t.Helper()
	db, engine := open(t), memory.New()
	if err := syncorm.Watch(db, engine, options...); err != nil {
		t.Fatal(err)
	}
	return db, engine
}

// source describes the index to the memory engine.
func source() memory.Source {
	return memory.Source{Index: "posts", Title: "title", Visible: "true"}
}

// keys is what the index holds for a query.
func keys(t *testing.T, engine search.Engine, text string) []string {
	t.Helper()
	got, err := search.NewFinder(engine).Search(context.Background(),
		search.Query{Text: text, Limit: search.MaxPerSource}, source())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(got.Groups[0].Hits))
	for _, hit := range got.Groups[0].Hits {
		out = append(out, hit.Key)
	}
	return out
}

// TestAWriteReachesTheIndexWithNothingAtTheCallSite.
func TestAWriteReachesTheIndexWithNothingAtTheCallSite(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres full text search", Status: "published"}
	if _, err := db.Model[Post]().Create(ctx, post); err != nil {
		t.Fatal(err)
	}

	if got := keys(t, engine, "postgres"); len(got) != 1 || got[0] != post.SearchKey() {
		t.Fatalf("the index holds %v, want just %q", got, post.SearchKey())
	}
}

// TestAnUpdateReplacesTheDocumentRatherThanAddingOne.
func TestAnUpdateReplacesTheDocumentRatherThanAddingOne(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres", Status: "published"}
	if _, err := db.Model[Post]().Create(ctx, post); err != nil {
		t.Fatal(err)
	}
	post.Title = "Meilisearch"
	if _, err := db.Model[Post]().Update(ctx, post); err != nil {
		t.Fatal(err)
	}

	if got := keys(t, engine, "meilisearch"); len(got) != 1 {
		t.Fatalf("searching for the new title found %v", got)
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("the old title is still findable as %v, so the write added a "+
			"document rather than replacing one", got)
	}
}

// TestADeleteRemovesTheDocument.
func TestADeleteRemovesTheDocument(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres", Status: "published"}
	if _, err := db.Model[Post]().Create(ctx, post); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Post]().ForceDelete(ctx, post); err != nil {
		t.Fatal(err)
	}

	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a deleted row is still findable as %v", got)
	}
}

// TestASoftDeleteRemovesTheDocument, which is the case a naive adapter gets
// wrong: the statement is an UPDATE, so an adapter switching on the SQL would
// reindex the row it should have removed.
func TestASoftDeleteRemovesTheDocument(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres", Status: "published"}
	if _, err := db.Model[Post]().Create(ctx, post); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Post]().Delete(ctx, post); err != nil {
		t.Fatal(err)
	}

	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a soft-deleted row is still findable as %v", got)
	}
}

// TestAModelThatStopsBeingSearchableStopsBeingFindable.
//
// This is the Scout bug the design avoids: unpublishing a post is an update,
// and an adapter that skipped a non-searchable model would have discarded the
// one event that could have removed it.
func TestAModelThatStopsBeingSearchableStopsBeingFindable(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres", Status: "published"}
	if _, err := db.Model[Post]().Create(ctx, post); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 1 {
		t.Fatalf("the published post was not indexed: %v", got)
	}

	post.Status = "draft"
	if _, err := db.Model[Post]().Update(ctx, post); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("an unpublished post is still findable as %v", got)
	}
}

// TestADraftIsNeverIndexedInTheFirstPlace, and the unindex of a key that was
// never there is not an error.
func TestADraftIsNeverIndexedInTheFirstPlace(t *testing.T) {
	var failures []error
	db, engine := watched(t, syncorm.WithErrorHandler(
		func(_ context.Context, err error) { failures = append(failures, err) }))
	ctx := context.Background()

	if _, err := db.Model[Post]().Create(ctx,
		&Post{Title: "Postgres", Status: "draft"}); err != nil {
		t.Fatal(err)
	}

	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a draft was indexed as %v", got)
	}
	if len(failures) != 0 {
		t.Fatalf("unindexing a key that was never there was reported as a "+
			"failure, which would make every draft save an error line: %v", failures)
	}
}

// TestARolledBackWriteIndexesNothing.
//
// The reason this adapter is the one with nothing to apologise for.
// orm.Observe fires after the commit, so there is no window in which the
// index describes a row that does not exist — which is exactly what the GORM
// and Bun adapters cannot promise, and their tests say so.
func TestARolledBackWriteIndexesNothing(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	refused := errors.New("no")
	err := db.Transaction(ctx, func(tx *orm.Tx) error {
		if _, err := tx.Model[Post]().Create(ctx,
			&Post{Title: "Postgres", Status: "published"}); err != nil {
			return err
		}
		if got := keys(t, engine, "postgres"); len(got) != 0 {
			t.Errorf("the index was written inside the transaction: %v", got)
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("got %v", err)
	}

	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a rolled-back write left %v in the index, describing a row "+
			"that does not exist", got)
	}
}

// TestACommittedTransactionDoesIndex, so the test above cannot pass against
// an adapter that simply never indexes.
func TestACommittedTransactionDoesIndex(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	if err := db.Transaction(ctx, func(tx *orm.Tx) error {
		_, err := tx.Model[Post]().Create(ctx,
			&Post{Title: "Postgres", Status: "published"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if got := keys(t, engine, "postgres"); len(got) != 1 {
		t.Fatalf("a committed write left the index holding %v", got)
	}
}

// TestAModelThatIsNotIndexableIsIgnoredSilently.
//
// A global observer sees every write in an application and most models are
// not searchable, so this must cost nothing and report nothing — an error
// line per unrelated INSERT would bury the real failures.
func TestAModelThatIsNotIndexableIsIgnoredSilently(t *testing.T) {
	var failures []error
	db, _ := watched(t, syncorm.WithErrorHandler(
		func(_ context.Context, err error) { failures = append(failures, err) }))

	if _, err := db.Model[Note]().Create(context.Background(),
		&Note{Body: "not searchable"}); err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("writing an unsearchable model was reported: %v", failures)
	}
}

// TestWatchingAnEngineWithNoIndexIsRefusedAtBoot rather than once per write.
func TestWatchingAnEngineWithNoIndexIsRefusedAtBoot(t *testing.T) {
	err := syncorm.Watch(open(t), indexless{})
	if !errors.Is(err, search.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "indexless") {
		t.Errorf("the error does not name the engine: %v", err)
	}
}

// TestWatchNeedsBothArguments.
func TestWatchNeedsBothArguments(t *testing.T) {
	if err := syncorm.Watch(nil, memory.New()); err == nil {
		t.Error("watching a nil database succeeded")
	}
	if err := syncorm.Watch(open(t), nil); err == nil {
		t.Error("watching with a nil engine succeeded")
	}
}

// indexless implements search.Engine and nothing more.
type indexless struct{}

func (indexless) Name() string                      { return "indexless" }
func (indexless) Capabilities() search.Capabilities { return search.Capabilities{} }
func (indexless) Supports(search.Source) error      { return nil }
func (indexless) MultiSearch(context.Context, []search.SearchRequest) ([]search.SearchResult, error) {
	return nil, nil
}
