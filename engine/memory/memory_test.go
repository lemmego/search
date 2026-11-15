package memory_test

import (
	"context"
	"testing"

	"github.com/lemmego/search"
	"github.com/lemmego/search/engine/memory"
	"github.com/lemmego/search/enginetest"
)

// TestMemoryConformsToTheEngineContract runs the whole suite, including the
// five cases the Postgres engine skips.
//
// That is the reason this engine exists. Index, Unindex and Flush are
// contract that the first engine cannot implement, and contract with no
// implementation behind it is contract nobody has checked.
func TestMemoryConformsToTheEngineContract(t *testing.T) {
	enginetest.Run(t, func(t *testing.T) enginetest.Harness {
		engine := memory.New()

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
			// A real relational rule, in this engine's own descriptor, so
			// the refusal case tests the visibility refusal rather than a
			// descriptor-type mismatch. The memory engine compares a field
			// to a value and nothing else, so an EXISTS is exactly what it
			// has to refuse.
			Unenforceable: func(index string) search.Source {
				return memory.Source{
					Index: index, Label: index,
					Title:   "title",
					Visible: "EXISTS (SELECT 1 FROM follows WHERE follower_id = " + search.ViewerPlaceholder + ")",
				}
			},
			StringKeys: true,
			Caps:       engine.Capabilities(),
		}
	})
}
