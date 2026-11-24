package syncbun_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	_ "github.com/glebarez/go-sqlite"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	"github.com/lemmego/search"
	"github.com/lemmego/search/engine/memory"
	"github.com/lemmego/search/syncbun"
)

type Post struct {
	bun.BaseModel `bun:"table:posts"`

	ID     int64  `bun:"id,pk,autoincrement"`
	Title  string `bun:"title"`
	Status string `bun:"status"`
}

func (p *Post) SearchIndex() string { return "posts" }
func (p *Post) SearchKey() string   { return strconv.FormatInt(p.ID, 10) }
func (p *Post) SearchDocument() map[string]any {
	return map[string]any{"title": p.Title, "status": p.Status}
}
func (p *Post) ShouldBeSearchable() bool { return p.Status == "published" }

type Note struct {
	bun.BaseModel `bun:"table:notes"`

	ID   int64  `bun:"id,pk,autoincrement"`
	Body string `bun:"body"`
}

// Plain is indexable and declares no Conditional, which is what makes the
// zero-model guard observable: with a ShouldBeSearchable on the model, a
// zero struct answers false and search.Sync takes the withdraw path instead
// of indexing it — so the guard is shadowed and the case cannot fail.
type Plain struct {
	bun.BaseModel `bun:"table:plains"`

	ID    int64  `bun:"id,pk,autoincrement"`
	Title string `bun:"title"`
}

func (p *Plain) SearchIndex() string { return "plains" }
func (p *Plain) SearchKey() string   { return strconv.FormatInt(p.ID, 10) }

// SearchDocument carries a constant alongside the title, and that is what
// makes a stray document observable at all: a zero model's title is empty,
// so a document written from one matches no search for its own content. The
// constant is a field every document in this index has, so the search below
// counts documents rather than hoping an empty one turns up.
func (p *Plain) SearchDocument() map[string]any {
	return map[string]any{"title": p.Title, "kind": "plainrecord"}
}

func open(t *testing.T) *bun.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	db := bun.NewDB(sqlDB, sqlitedialect.New())
	ctx := context.Background()
	for _, model := range []any{(*Post)(nil), (*Note)(nil), (*Plain)(nil)} {
		if _, err := db.NewCreateTable().Model(model).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func watched(t *testing.T, options ...syncbun.Option) (*bun.DB, *memory.Engine) {
	t.Helper()
	db, engine := open(t), memory.New()
	if err := syncbun.Watch(db, engine, options...); err != nil {
		t.Fatal(err)
	}
	return db, engine
}

func keys(t *testing.T, engine search.Engine, text string) []string {
	t.Helper()
	source := memory.Source{Index: "posts", Title: "title", Visible: "true"}
	got, err := search.NewFinder(engine).Search(context.Background(),
		search.Query{Text: text, Limit: search.MaxPerSource}, source)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(got.Groups[0].Hits))
	for _, hit := range got.Groups[0].Hits {
		out = append(out, hit.Key)
	}
	return out
}

func TestABunWriteReachesTheIndex(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres full text search", Status: "published"}
	if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 1 || got[0] != post.SearchKey() {
		t.Fatalf("the index holds %v, want just %q", got, post.SearchKey())
	}
}

func TestABunBatchInsertIndexesEveryRow(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	posts := []*Post{
		{Title: "Postgres one", Status: "published"},
		{Title: "Postgres two", Status: "published"},
	}
	if _, err := db.NewInsert().Model(&posts).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 2 {
		t.Fatalf("a batch of two indexed %v", got)
	}
}

func TestABunDeleteRemovesTheDocument(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres", Status: "published"}
	if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewDelete().Model(post).WherePK().Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a deleted row is still findable as %v", got)
	}
}

func TestAnUnpublishedPostStopsBeingFindable(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres", Status: "published"}
	if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	post.Status = "draft"
	if _, err := db.NewUpdate().Model(post).WherePK().Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("an unpublished post is still findable as %v", got)
	}
}

// TestAReadNeverWritesToTheIndex.
//
// Bun's hook sees every query, SELECTs included, and a query event carries
// the model a read was scanned into — so an adapter that did not switch on
// the operation would reindex on every page view.
func TestAReadNeverWritesToTheIndex(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	post := &Post{Title: "Postgres", Status: "published"}
	if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// Unindex behind the adapter's back, then read the row. If the read
	// reindexed it, the document comes back.
	if err := engine.Unindex(ctx, "posts", []string{post.SearchKey()}); err != nil {
		t.Fatal(err)
	}

	var read Post
	if err := db.NewSelect().Model(&read).Where("id = ?", post.ID).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a SELECT wrote %v to the index", got)
	}
}

// TestAConditionalDeleteWithNoLoadedRowIsIgnored.
//
// `Model((*Post)(nil)).Where(...)` is how Bun is told the table for a delete
// by condition, and the event then carries a *Post pointing at a zero struct
// rather than nil. Its SearchKey() renders "0", which search.ValidKey
// accepts, so acting on it would unindex whatever document happens to be
// keyed "0".
//
// The index is therefore seeded with a document keyed "0" directly. Without
// that the case passes whatever the adapter does, because the row Bun deletes
// is keyed 1 and a stray unindex of "0" hits nothing — which is how the
// first version of this went vacuous.
//
// What is left is a hole: the row goes and its document stays. It is the
// same hole a bulk mutation has everywhere else, and the repair is a
// reimport.
func TestAConditionalDeleteWithNoLoadedRowIsIgnored(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	if err := engine.Index(ctx, []search.Document{{
		Index: "posts", Key: "0",
		Fields: map[string]any{"title": "Postgres bystander", "status": "published"},
	}}); err != nil {
		t.Fatal(err)
	}

	post := &Post{Title: "Postgres doomed", Status: "published"}
	if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 2 {
		t.Fatalf("the seed plus the row is %v, want two documents", got)
	}

	if _, err := db.NewDelete().Model((*Post)(nil)).
		Where("status = ?", "published").Exec(ctx); err != nil {
		t.Fatal(err)
	}

	count, err := db.NewSelect().Model((*Post)(nil)).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("the conditional delete removed nothing; %d rows survived", count)
	}

	// Both documents are still there: the deleted row's, which is the
	// documented hole, and the bystander keyed 0, which is the bug.
	got := keys(t, engine, "postgres")
	if len(got) != 2 {
		t.Fatalf("the index holds %v. If the document keyed 0 is the missing "+
			"one, the zero model was acted on and a conditional delete now "+
			"removes an unrelated document.", got)
	}
}

// TestAConditionalUpdateDoesNotIndexAZeroDocument.
//
// The worse half of the placeholder problem, and the one a nil check alone
// does not catch: `Model(&Plain{})` is a non-nil pointer to a zero struct,
// so without the zero guard a conditional UPDATE writes a document keyed "0"
// with every field empty — polluting the index rather than merely failing to
// update it.
//
// It uses Plain and not Post deliberately. Post declares
// ShouldBeSearchable, and a zero Post answers false, so search.Sync takes
// the withdraw path and the stray document is never written — the guard is
// shadowed and the case passes whatever the adapter does. A model with no
// Conditional, which is the ordinary case, is where this is visible.
func TestAConditionalUpdateDoesNotIndexAZeroDocument(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	if _, err := db.NewInsert().Model(&Plain{Title: "Postgres real"}).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	source := memory.Source{Index: "plains", Title: "title",
		Snippet: []string{"kind"}, Visible: "true"}
	found := func() []string {
		got, err := search.NewFinder(engine).Search(ctx,
			search.Query{Text: "plainrecord", Limit: search.MaxPerSource}, source)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(got.Groups[0].Hits))
		for _, hit := range got.Groups[0].Hits {
			out = append(out, hit.Key)
		}
		return out
	}

	if before := found(); len(before) != 1 {
		t.Fatalf("the seed is %v, want one document", before)
	}

	if _, err := db.NewUpdate().Model(&Plain{}).
		Set("title = ?", "nothing").Where("1 = 0").Exec(ctx); err != nil {
		t.Fatal(err)
	}

	for _, key := range found() {
		if key == "0" {
			t.Fatal("a conditional update indexed a zero model as document 0")
		}
	}
}

// TestARolledBackBunWriteLeavesTheIndexDirty, asserted rather than hidden.
// Bun's query hooks fire inside the transaction, exactly as GORM's do.
func TestARolledBackBunWriteLeavesTheIndexDirty(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	refused := errors.New("no")
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().
			Model(&Post{Title: "Postgres doomed", Status: "published"}).
			Exec(ctx); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("got %v", err)
	}

	count, err := db.NewSelect().Model((*Post)(nil)).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("the transaction did not roll back; %d rows survived", count)
	}
	if got := keys(t, engine, "postgres"); len(got) != 1 {
		t.Fatalf("the index holds %v. If this now holds nothing, Bun has gained "+
			"a hook that fires after the commit — use it and delete this test", got)
	}
}

// TestWithDeferMakesTheRollbackTheApplicationsProblem.
func TestWithDeferMakesTheRollbackTheApplicationsProblem(t *testing.T) {
	var pending []func(context.Context) error
	db, engine := watched(t, syncbun.WithDefer(
		func(_ context.Context, apply func(context.Context) error) {
			pending = append(pending, apply)
		}))
	ctx := context.Background()

	refused := errors.New("no")
	_ = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().
			Model(&Post{Title: "Postgres doomed", Status: "published"}).
			Exec(ctx); err != nil {
			return err
		}
		return refused
	})

	if len(pending) != 1 {
		t.Fatalf("the adapter offered %d writes, want 1", len(pending))
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a deferred write reached the index anyway: %v", got)
	}

	pending = nil
	if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewInsert().
			Model(&Post{Title: "Postgres kept", Status: "published"}).Exec(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, apply := range pending {
		if err := apply(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := keys(t, engine, "postgres"); len(got) != 1 {
		t.Fatalf("running the deferred write after the commit left %v", got)
	}
}

func TestANonIndexableModelIsIgnoredSilently(t *testing.T) {
	var failures []error
	db, _ := watched(t, syncbun.WithErrorHandler(
		func(_ context.Context, err error) { failures = append(failures, err) }))

	if _, err := db.NewInsert().Model(&Note{Body: "not searchable"}).
		Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("writing an unsearchable model was reported: %v", failures)
	}
}

func TestAFailedStatementNeverReachesTheIndex(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	if _, err := db.NewInsert().
		Model(&Post{ID: 1, Title: "Postgres", Status: "published"}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewInsert().
		Model(&Post{ID: 1, Title: "Meilisearch clash", Status: "published"}).
		Exec(ctx); err == nil {
		t.Fatal("the duplicate insert succeeded, so there is nothing to assert")
	}
	if got := keys(t, engine, "meilisearch"); len(got) != 0 {
		t.Fatalf("a failed statement reached the index as %v", got)
	}
}

func TestWatchingAnEngineWithNoIndexIsRefusedAtBoot(t *testing.T) {
	if err := syncbun.Watch(open(t), indexless{}); !errors.Is(err, search.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

type indexless struct{}

func (indexless) Name() string                      { return "indexless" }
func (indexless) Capabilities() search.Capabilities { return search.Capabilities{} }
func (indexless) Supports(search.Source) error      { return nil }
func (indexless) MultiSearch(context.Context, []search.SearchRequest) ([]search.SearchResult, error) {
	return nil, nil
}
