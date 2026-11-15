package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/lemmego/search"
	"github.com/lemmego/search/engine/memory"
)

// TestTheConfigStubIsValidGoAndFormatted.
//
// It is published into somebody's project and compiled there, so "it parses"
// is the floor and gofmt-clean is what stops the first `go fmt ./...` after
// an install showing up as a diff the developer did not write.
func TestTheConfigStubIsValidGoAndFormatted(t *testing.T) {
	if _, err := parser.ParseFile(token.NewFileSet(), "search.go", ConfigStub, parser.AllErrors); err != nil {
		t.Fatalf("the published config does not parse: %v", err)
	}
	formatted, err := format.Source([]byte(ConfigStub))
	if err != nil {
		t.Fatalf("formatting: %v", err)
	}
	if string(formatted) != ConfigStub {
		t.Error("the published config is not gofmt-clean, so installing it " +
			"leaves a diff nobody wrote")
	}
}

// TestTheConfigStubSetsEveryKeyTheProviderReads.
//
// The stub and resolveConfig are two halves of one contract written in
// different places, so a key renamed on one side is silently ignored on the
// other — and the symptom is a configured engine that is not the one running.
func TestTheConfigStubSetsEveryKeyTheProviderReads(t *testing.T) {
	for _, key := range []string{`"engine"`, `"prefix"`, `"meilisearch"`, `"host"`, `"key"`} {
		if !strings.Contains(ConfigStub, key) {
			t.Errorf("the published config never sets %s, which resolveConfig reads", key)
		}
	}
	if !strings.Contains(ConfigStub, `config.Set("search"`) {
		t.Error(`the published config does not register under "search"`)
	}
	// Nothing in it may name the Postgres handle: it cannot come from a
	// configuration file, which is this module's reason for taking the engine
	// by injection rather than purely by name.
	if strings.Contains(ConfigStub, "postgres") && !strings.Contains(ConfigStub, `"postgres"`) {
		t.Error("the published config appears to configure the postgres engine, " +
			"which takes the application's own pool and cannot be described in text")
	}
}

// TestThePublishTagIsNamespaced, because --tags is a hard selector and a bare
// "config" would sweep in every other module that publishes one.
func TestThePublishTagIsNamespaced(t *testing.T) {
	if !strings.HasPrefix(TagConfig, "search-") {
		t.Errorf("TagConfig is %q, want it prefixed with the module name", TagConfig)
	}
	p := &Provider{}
	published := p.AddPublishables()
	if len(published) != 1 {
		t.Fatalf("got %d publishables, want 1", len(published))
	}
	if published[0].Tag != TagConfig {
		t.Errorf("published under %q, want %q", published[0].Tag, TagConfig)
	}
	if !strings.HasSuffix(published[0].FilePath, "search.go") {
		t.Errorf("published to %q", published[0].FilePath)
	}
	if !strings.Contains(InstallInstructions, TagConfig) {
		t.Error("the install instructions name a tag other than the one that works")
	}
}

// indexless is an engine with no index of its own — the postgres engine's
// position, without needing a database to assert it.
//
// It implements search.Engine and nothing more. It deliberately does not
// embed the memory engine: embedding would promote Index, Unindex and Flush,
// the assertion in indexer() would succeed, and the test would pass against
// an engine that is not the one it describes.
type indexless struct{}

func (indexless) Name() string                      { return "indexless" }
func (indexless) Capabilities() search.Capabilities { return search.Capabilities{} }
func (indexless) Supports(search.Source) error      { return nil }
func (indexless) MultiSearch(context.Context, []search.SearchRequest) ([]search.SearchResult, error) {
	return nil, nil
}

var _ search.Engine = indexless{}

// TestBothCommandsRefuseAnEngineWithNoIndex, rather than reporting success
// having written nothing.
//
// The refusal names the engine and says why, because the alternative — a
// command that is simply absent — reads as a version problem rather than as
// the real reason, which is that the engine searches rows where they already
// are.
func TestBothCommandsRefuseAnEngineWithNoIndex(t *testing.T) {
	p := &Provider{engine: indexless{}}
	p.Importables = []search.Importable{&fixture{index: "posts"}}

	for _, cmd := range []string{"search:import", "search:flush"} {
		_, err := run(t, p, cmd, "posts")
		if !errors.Is(err, search.ErrUnsupported) {
			t.Errorf("%s returned %v, want ErrUnsupported", cmd, err)
		}
		if err != nil && !strings.Contains(err.Error(), "indexless") {
			t.Errorf("%s did not name the engine: %v", cmd, err)
		}
	}
}

// TestImportWalksEveryBatch over a collection larger than one batch.
func TestImportWalksEveryBatch(t *testing.T) {
	engine := memory.New()
	source := &fixture{index: "posts", count: 23}
	p := &Provider{engine: engine, Importables: []search.Importable{source}}

	out, err := run(t, p, "search:import", "--batch", "5")
	if err != nil {
		t.Fatal(err)
	}
	// Six and not five: four full batches, one of three, and one more that
	// comes back empty. The contract is that an *empty* batch ends the walk,
	// not a short one — an Importable that filters within a batch returns a
	// short one legitimately, and stopping there would silently truncate the
	// import.
	if source.batches != 6 {
		t.Errorf("the walk took %d batches, want 6", source.batches)
	}
	if !strings.Contains(out, "23 documents indexed") {
		t.Errorf("the command reported:\n%s", out)
	}
}

// TestImportIsResumable, which is the whole reason the walk reports the last
// key it saw: an import runs for minutes and an interrupted one must not
// start over.
func TestImportIsResumable(t *testing.T) {
	engine := memory.New()
	source := &fixture{index: "posts", count: 10}
	p := &Provider{engine: engine, Importables: []search.Importable{source}}

	out, err := run(t, p, "search:import", "--batch", "5", "--after", "key05")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "5 documents indexed") {
		t.Errorf("resuming after key05 indexed something other than the "+
			"remaining five:\n%s", out)
	}
}

// TestADryRunWritesNothing, so a figure can be checked before an index is
// emptied on the strength of it.
func TestADryRunWritesNothing(t *testing.T) {
	engine := memory.New()
	source := &fixture{index: "posts", count: 7}
	p := &Provider{engine: engine, Importables: []search.Importable{source}}

	out, err := run(t, p, "search:import", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing was written") {
		t.Errorf("a dry run reported:\n%s", out)
	}

	src := memory.Source{Index: "posts", Title: "title", Visible: "true"}
	got, err := search.NewFinder(engine).Search(context.Background(),
		search.Query{Text: "post"}, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups[0].Hits) != 0 {
		t.Errorf("a dry run indexed %d documents", len(got.Groups[0].Hits))
	}
}

// TestAnImportableThatDoesNotAdvanceItsCursorIsRefused.
//
// The failure it prevents is an import that never ends, which looks like a
// slow import for as long as anybody is willing to wait. Reporting it names
// the collection and the cursor instead.
func TestAnImportableThatDoesNotAdvanceItsCursorIsRefused(t *testing.T) {
	p := &Provider{
		engine:      memory.New(),
		Importables: []search.Importable{&fixture{index: "posts", count: 10, stuck: true}},
	}

	_, err := run(t, p, "search:import", "--batch", "5")
	if err == nil || !strings.Contains(err.Error(), "same cursor") {
		t.Fatalf("got %v, want a refusal naming the cursor", err)
	}
}

// TestImportingAnIndexNothingDeclaresNamesWhatThereIs.
func TestImportingAnIndexNothingDeclaresNamesWhatThereIs(t *testing.T) {
	p := &Provider{
		engine: memory.New(),
		Importables: []search.Importable{
			&fixture{index: "posts"}, &fixture{index: "people"},
		},
	}

	_, err := run(t, p, "search:import", "--index", "launches")
	if err == nil {
		t.Fatal("importing an index nothing declares succeeded")
	}
	if !strings.Contains(err.Error(), "people") || !strings.Contains(err.Error(), "posts") {
		t.Errorf("the error does not name what is declared: %v", err)
	}
}

// TestImportWithNoImportablesSaysSo rather than reporting a successful import
// of nothing, which is the shape of a wiring mistake nobody notices.
func TestImportWithNoImportablesSaysSo(t *testing.T) {
	p := &Provider{engine: memory.New()}

	_, err := run(t, p, "search:import")
	if err == nil || !strings.Contains(err.Error(), "Importables") {
		t.Fatalf("got %v, want a refusal naming Importables", err)
	}
}

// TestFlushEmptiesTheIndexAndLeavesItUsable.
func TestFlushEmptiesTheIndexAndLeavesItUsable(t *testing.T) {
	engine := memory.New()
	source := &fixture{index: "posts", count: 3}
	p := &Provider{engine: engine, Importables: []search.Importable{source}}

	if _, err := run(t, p, "search:import"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, p, "search:flush", "posts", "--force"); err != nil {
		t.Fatal(err)
	}

	src := memory.Source{Index: "posts", Title: "title", Visible: "true"}
	got, err := search.NewFinder(engine).Search(context.Background(),
		search.Query{Text: "post"}, src)
	if err != nil {
		t.Fatalf("searching a flushed index: %v — an empty index must still be "+
			"searchable, or a flush has deleted the index rather than its "+
			"documents", err)
	}
	if len(got.Groups[0].Hits) != 0 {
		t.Errorf("%d documents survived the flush", len(got.Groups[0].Hits))
	}
}

// run executes one command and returns everything it printed.
func run(t *testing.T, p *Provider, name string, args ...string) (string, error) {
	t.Helper()

	var cmd = p.importCommand(nil)
	if name == "search:flush" {
		cmd = p.flushCommand(nil)
	}

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true

	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// fixture is an Importable over a synthetic collection.
type fixture struct {
	index   string
	count   int
	stuck   bool
	batches int
}

func (f *fixture) SearchIndex() string { return f.index }

func (f *fixture) SearchBatch(_ context.Context, after search.Cursor) ([]search.Document, search.Cursor, error) {
	f.batches++

	// Keys are fixed width so that ordering by key is ordering by number,
	// which is what a keyset walk needs — the same reason the plan's
	// materialised paths are zero-padded.
	start := 0
	if after.AfterKey != "" {
		if _, err := fmt.Sscanf(after.AfterKey, "key%02d", &start); err != nil {
			return nil, search.Cursor{}, fmt.Errorf("unreadable cursor %q", after.AfterKey)
		}
	}

	var docs []search.Document
	for i := start; i < f.count && len(docs) < after.Limit; i++ {
		key := fmt.Sprintf("key%02d", i+1)
		docs = append(docs, search.Document{
			Index: f.index,
			Key:   key,
			Fields: map[string]any{
				"title": "post " + key,
				"body":  "a document about posts",
			},
		})
	}
	if len(docs) == 0 {
		return nil, search.Cursor{}, nil
	}

	next := search.Cursor{AfterKey: docs[len(docs)-1].Key}
	if f.stuck {
		next.AfterKey = after.AfterKey
	}
	return docs, next, nil
}
