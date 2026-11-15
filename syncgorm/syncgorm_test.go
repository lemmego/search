package syncgorm_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/lemmego/search"
	"github.com/lemmego/search/engine/memory"
	"github.com/lemmego/search/syncgorm"
)

type Post struct {
	ID     uint `gorm:"primarykey"`
	Title  string
	Status string
}

func (p *Post) SearchIndex() string { return "posts" }
func (p *Post) SearchKey() string   { return strconv.FormatUint(uint64(p.ID), 10) }
func (p *Post) SearchDocument() map[string]any {
	return map[string]any{"title": p.Title, "status": p.Status}
}
func (p *Post) ShouldBeSearchable() bool { return p.Status == "published" }

type Note struct {
	ID   uint `gorm:"primarykey"`
	Body string
}

func open(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(filepath.Join(t.TempDir(), "sync.db")),
		&gorm.Config{Logger: logger.Discard},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Post{}, &Note{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func watched(t *testing.T, options ...syncgorm.Option) (*gorm.DB, *memory.Engine) {
	t.Helper()
	db, engine := open(t), memory.New()
	if err := syncgorm.Watch(db, engine, options...); err != nil {
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

func TestAGormWriteReachesTheIndex(t *testing.T) {
	db, engine := watched(t)

	post := &Post{Title: "Postgres full text search", Status: "published"}
	if err := db.Create(post).Error; err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 1 || got[0] != post.SearchKey() {
		t.Fatalf("the index holds %v, want just %q", got, post.SearchKey())
	}
}

// TestABatchCreateIndexesEveryRow, which is why the statement's Dest is
// flattened rather than asserted to one model.
func TestABatchCreateIndexesEveryRow(t *testing.T) {
	db, engine := watched(t)

	posts := []*Post{
		{Title: "Postgres one", Status: "published"},
		{Title: "Postgres two", Status: "published"},
		{Title: "Postgres three", Status: "published"},
	}
	if err := db.Create(&posts).Error; err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 3 {
		t.Fatalf("a batch of three indexed %v", got)
	}
}

// TestABatchOfValuesIsIndexedToo — a []Post rather than a []*Post, where the
// methods are on the pointer, so each element has to be addressed.
func TestABatchOfValuesIsIndexedToo(t *testing.T) {
	db, engine := watched(t)

	posts := []Post{
		{Title: "Postgres alpha", Status: "published"},
		{Title: "Postgres beta", Status: "published"},
	}
	if err := db.Create(&posts).Error; err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 2 {
		t.Fatalf("a batch of values indexed %v — the elements were probably not "+
			"addressed, so *Post's methods were invisible", got)
	}
}

func TestAGormDeleteRemovesTheDocument(t *testing.T) {
	db, engine := watched(t)

	post := &Post{Title: "Postgres", Status: "published"}
	if err := db.Create(post).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(post).Error; err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a deleted row is still findable as %v", got)
	}
}

func TestAnUnpublishedPostStopsBeingFindable(t *testing.T) {
	db, engine := watched(t)

	post := &Post{Title: "Postgres", Status: "published"}
	if err := db.Create(post).Error; err != nil {
		t.Fatal(err)
	}
	post.Status = "draft"
	if err := db.Save(post).Error; err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("an unpublished post is still findable as %v", got)
	}
}

// TestARolledBackGormWriteLeavesTheIndexDirty.
//
// This asserts the known-dirty state rather than hiding it. GORM's callbacks
// run as part of the statement, so there is no point at which this adapter
// can learn the transaction committed — the document for the rolled-back row
// is in the index and nothing says so.
//
// Written as a test, and not only as a sentence in the package doc, for two
// reasons: a reader checking whether the limitation is real gets an answer,
// and the day GORM grows an after-commit hook this test fails and names the
// thing to change.
func TestARolledBackGormWriteLeavesTheIndexDirty(t *testing.T) {
	db, engine := watched(t)

	refused := errors.New("no")
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&Post{Title: "Postgres doomed", Status: "published"}).Error; err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("got %v", err)
	}

	// The row is gone.
	var rows int64
	if err := db.Model(&Post{}).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("the transaction did not roll back; %d rows survived", rows)
	}

	// The document is not.
	if got := keys(t, engine, "postgres"); len(got) != 1 {
		t.Fatalf("the index holds %v. If this now holds nothing, GORM has "+
			"gained a hook that fires after the commit — change the adapter to "+
			"use it and delete this test", got)
	}
}

// TestWithDeferMakesTheRollbackTheApplicationsProblem, which is the seam's
// whole purpose: the adapter becomes a change detector and the application
// decides when the write happens.
func TestWithDeferMakesTheRollbackTheApplicationsProblem(t *testing.T) {
	var pending []func(context.Context) error
	db, engine := watched(t, syncgorm.WithDefer(
		func(_ context.Context, apply func(context.Context) error) {
			pending = append(pending, apply)
		}))

	refused := errors.New("no")
	_ = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&Post{Title: "Postgres doomed", Status: "published"}).Error; err != nil {
			return err
		}
		return refused
	})

	// The closure was offered and the application did not run it, because the
	// transaction did not commit. Nothing reached the index.
	if len(pending) != 1 {
		t.Fatalf("the adapter offered %d writes, want 1", len(pending))
	}
	if got := keys(t, engine, "postgres"); len(got) != 0 {
		t.Fatalf("a deferred write reached the index anyway: %v", got)
	}

	// And the same closure, run after a commit, does index.
	pending = nil
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&Post{Title: "Postgres kept", Status: "published"}).Error
	}); err != nil {
		t.Fatal(err)
	}
	for _, apply := range pending {
		if err := apply(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := keys(t, engine, "postgres"); len(got) != 1 {
		t.Fatalf("running the deferred write after the commit left %v", got)
	}
}

func TestANonIndexableModelIsIgnoredSilently(t *testing.T) {
	var failures []error
	db, _ := watched(t, syncgorm.WithErrorHandler(
		func(_ context.Context, err error) { failures = append(failures, err) }))

	if err := db.Create(&Note{Body: "not searchable"}).Error; err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("writing an unsearchable model was reported: %v", failures)
	}
}

// TestAFailedStatementNeverReachesTheIndex, which is the one half of the
// transaction problem the adapter can solve by registering After rather than
// Before.
func TestAFailedStatementNeverReachesTheIndex(t *testing.T) {
	db, engine := watched(t)

	post := &Post{ID: 1, Title: "Postgres", Status: "published"}
	if err := db.Create(post).Error; err != nil {
		t.Fatal(err)
	}
	// A second insert of the same primary key fails.
	clash := &Post{ID: 1, Title: "Meilisearch clash", Status: "published"}
	if err := db.Create(clash).Error; err == nil {
		t.Fatal("the duplicate insert succeeded, so there is nothing to assert")
	}
	if got := keys(t, engine, "meilisearch"); len(got) != 0 {
		t.Fatalf("a failed statement reached the index as %v", got)
	}
}

func TestWatchingAnEngineWithNoIndexIsRefusedAtBoot(t *testing.T) {
	if err := syncgorm.Watch(open(t), indexless{}); !errors.Is(err, search.ErrUnsupported) {
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

// TestAConditionalDeleteIsIgnoredRatherThanUnindexingTheZeroKey.
//
// `Where(...).Delete(&Post{})` leaves a zero struct in the statement, whose
// SearchKey() renders "0" — a key search.ValidKey accepts. Acting on it would
// unindex whatever document happens to be keyed "0", which is somebody
// else's row rather than nothing.
//
// So the index is seeded with a document keyed "0" directly, behind the
// adapter's back. Seeding it through GORM would not work: an autoincrement
// column assigns 1 rather than keeping the 0 it was given, and a test whose
// index holds no "0" passes whatever the adapter does — which is how the
// first version of this went vacuous.
func TestAConditionalDeleteIsIgnoredRatherThanUnindexingTheZeroKey(t *testing.T) {
	db, engine := watched(t)
	ctx := context.Background()

	if err := engine.Index(ctx, []search.Document{{
		Index: "posts", Key: "0",
		Fields: map[string]any{"title": "Postgres bystander", "status": "published"},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, engine, "postgres"); len(got) != 1 || got[0] != "0" {
		t.Fatalf("the seed is %v, want the one document keyed 0", got)
	}

	if err := db.Where("status = ?", "nothing-matches-this").Delete(&Post{}).Error; err != nil {
		t.Fatal(err)
	}

	if got := keys(t, engine, "postgres"); len(got) != 1 {
		t.Fatalf("a conditional delete that matched no rows unindexed the "+
			"document keyed 0; the index now holds %v", got)
	}
}
