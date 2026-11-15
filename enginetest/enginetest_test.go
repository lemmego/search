package enginetest_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lemmego/search"
	"github.com/lemmego/search/engine/memory"
	"github.com/lemmego/search/enginetest"
)

// defectEnv names the defect the child process should introduce.
const defectEnv = "ENGINETEST_DEFECT"

// TestTheSuiteFailsForEachDefect is the guard on the guard.
//
// A conformance suite that cannot fail is the most expensive kind of test:
// every engine passes it, so it reads as evidence and is none. This project
// has found that class of failure repeatedly — a leak test that passed
// because the page echoed the query back, a channel test that went through a
// code path forcing the answer — so the rule here is that a probe is not
// trusted until it has been seen to fail.
//
// Each defect below is a plausible mistake rather than a contrived one, and
// each names the case that must catch it. The suite is run in a child
// process, because a failing subtest fails its parent and there is no way to
// un-fail one: the child's non-zero exit is the assertion.
func TestTheSuiteFailsForEachDefect(t *testing.T) {
	// The child: introduce the defect and run the suite against it. Its
	// failure is the point, so nothing here asserts anything.
	if defect := os.Getenv(defectEnv); defect != "" {
		enginetest.Run(t, brokenHarness(defect))
		return
	}

	defects := []struct {
		defect   string
		wantCase string
		why      string
	}{
		{
			defect:   "blend",
			wantCase: "results_are_grouped_and_never_blended",
			why: "ranks are not comparable across sources, so merging them " +
				"produces an ordering that looks authoritative and means nothing",
		},
		{
			defect:   "merge",
			wantCase: "indexing_replaces_the_whole_document",
			why: "a PUT where a POST was meant: a field a model stops setting " +
				"stays findable forever",
		},
		{
			defect:   "permissive",
			wantCase: "a_visibility_rule_the_engine_cannot_enforce_is_refused",
			why:      "searching rows it has no way to filter, which is the disclosure",
		},
		{
			defect:   "throws",
			wantCase: "hostile_input_never_errors",
			why:      "a 500 the first time somebody types `a & | b` into a search box",
		},
		{
			defect:   "nodelete",
			wantCase: "unindexing_removes_from_results",
			why:      "a withdrawn document that stays findable",
		},
		{
			defect:   "nofilter",
			wantCase: "a_filter_restricts_the_results",
			why: "claiming Filters and then ignoring one, which answers a page " +
				"of results from a query nobody asked for",
		},
		{
			defect:   "filterwins",
			wantCase: "a_filter_cannot_replace_the_visibility_rule",
			why: "composing a filter and a visibility rule by replacement, so " +
				"any caller can ask for the rows the rule exists to hide",
		},
	}

	for _, d := range defects {
		t.Run(d.defect, func(t *testing.T) {
			cmd := exec.Command(os.Args[0],
				"-test.run", "TestTheSuiteFailsForEachDefect", "-test.v")
			cmd.Env = append(os.Environ(), defectEnv+"="+d.defect)
			out, err := cmd.CombinedOutput()

			if err == nil {
				t.Fatalf("the suite PASSED against an engine that %s.\n"+
					"So %s is not actually checking anything.\n%s",
					d.why, d.wantCase, out)
			}
			if !strings.Contains(string(out), "FAIL: "+parent+"/"+d.wantCase) {
				t.Errorf("the suite failed, but not at %s — so that case is not "+
					"the one catching this defect, and the defect may be caught "+
					"only by accident.\n%s", d.wantCase, out)
			}
		})
	}
}

const parent = "TestTheSuiteFailsForEachDefect"

// brokenHarness returns a harness over an engine with one defect.
func brokenHarness(defect string) enginetest.Factory {
	return func(t *testing.T) enginetest.Harness {
		engine := &broken{Engine: memory.New(), defect: defect}

		return enginetest.Harness{
			Engine: engine,
			Put: func(ctx context.Context, docs []search.Document) error {
				return engine.Index(ctx, docs)
			},
			Source: func(index string, visibleOnly bool) search.Source {
				visible := "true"
				if visibleOnly {
					visible = "status = published"
				}
				return memory.Source{
					Index: index, Label: index,
					Title: "title", Subtitle: "status",
					Snippet: []string{"body"},
					Visible: visible,
				}
			},
			Unenforceable: func(index string) search.Source {
				return memory.Source{
					Index: index, Label: index, Title: "title",
					Visible: "EXISTS (SELECT 1 FROM follows)",
				}
			},
			StringKeys: true,
			Caps:       engine.Capabilities(),
		}
	}
}

// broken is the memory engine with one thing wrong.
type broken struct {
	*memory.Engine
	defect string

	// seen shadows what has been indexed, so the merge defect can do what a
	// merge really does. The memory engine exposes no way to read a document
	// back, and giving it one so a test could misbehave would be the test
	// shaping the thing it tests.
	seen map[string]search.Document
}

func (b *broken) Supports(src search.Source) error {
	if b.defect == "permissive" {
		// Accepts anything, including a rule it cannot evaluate.
		return nil
	}
	return b.Engine.Supports(src)
}

func (b *broken) Index(ctx context.Context, docs []search.Document) error {
	if b.seen == nil {
		b.seen = map[string]search.Document{}
	}

	if b.defect == "merge" {
		for i, doc := range docs {
			if existing, ok := b.seen[doc.Index+"\x00"+doc.Key]; ok {
				merged := map[string]any{}
				for k, v := range existing.Fields {
					merged[k] = v
				}
				for k, v := range doc.Fields {
					merged[k] = v
				}
				// The old text kept alongside the new, which is what a merge
				// amounts to for a searchable field.
				if old, had := existing.Fields["body"]; had {
					if fresh, has := doc.Fields["body"]; has {
						merged["body"] = fmt.Sprint(old) + " " + fmt.Sprint(fresh)
					}
				}
				docs[i].Fields = merged
			}
		}
	}

	for _, doc := range docs {
		b.seen[doc.Index+"\x00"+doc.Key] = doc
	}
	return b.Engine.Index(ctx, docs)
}

func (b *broken) Unindex(ctx context.Context, index string, keys []string) error {
	if b.defect == "nodelete" {
		return nil
	}
	return b.Engine.Unindex(ctx, index, keys)
}

func (b *broken) MultiSearch(ctx context.Context, reqs []search.SearchRequest) ([]search.SearchResult, error) {
	switch b.defect {
	case "nofilter":
		// Dropped on the way in, which is the shape of an engine that
		// reports the capability and has not implemented it.
		for i := range reqs {
			reqs[i].Filters = nil
		}
	case "filterwins":
		// "The caller named the subset they want, so the rule is redundant."
		// It is not: the rule says which rows this viewer may see at all.
		for i := range reqs {
			if len(reqs[i].Filters) == 0 {
				continue
			}
			if src, ok := reqs[i].Source.(memory.Source); ok {
				src.Visible = "true"
				reqs[i].Source = src
			}
		}
	}

	if b.defect == "throws" {
		for _, req := range reqs {
			if strings.ContainsAny(req.Text, "&|!:*()") {
				return nil, fmt.Errorf("syntax error in tsquery: %q", req.Text)
			}
		}
	}

	out, err := b.Engine.MultiSearch(ctx, reqs)
	if err != nil {
		return nil, err
	}

	switch b.defect {
	case "blend":
		// Every hit returned under the first source, which is what a UNION
		// ALL across differently-shaped sources amounts to.
		var all []search.RawHit
		for _, res := range out {
			all = append(all, res.Hits...)
		}
		for i := range out {
			out[i].Hits = all
		}

		// There is deliberately no defect for the empty-snippet rule. An engine
		// cannot break it: the core calls Snippet on whatever headline it is
		// given, and Snippet returns empty for any string with no sentinel in it.
		// The property is structural rather than conformance, which is a better
		// place for it to live — and worth recording, because the obvious defect
		// here passes and would otherwise read as a hole in the suite.
	}
	return out, nil
}
