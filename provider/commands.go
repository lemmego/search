package provider

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lemmego/api/app"
	"github.com/spf13/cobra"

	"github.com/lemmego/search"
)

// AddCommands contributes search's commands.
//
// Both are registered unconditionally and both refuse at run time against an
// engine with no index of its own, rather than being hidden. A command that
// is absent reads as a version problem; one that refuses and says why names
// the real reason — the postgres engine searches the application's rows where
// they already are, so there is nothing to import into and nothing to empty.
func (p *Provider) AddCommands() []app.Command {
	return []app.Command{p.importCommand, p.flushCommand}
}

// indexer returns the engine's write half, or a refusal naming the engine.
func (p *Provider) indexer() (search.Indexer, error) {
	if p.engine == nil {
		return nil, fmt.Errorf("search: no engine; the provider has not booted")
	}
	indexer, ok := p.engine.(search.Indexer)
	if !ok {
		return nil, fmt.Errorf("%w: the %s engine keeps no index of its own, so "+
			"there is nothing to write to — it searches rows where they already "+
			"are, and a migration is what makes them searchable",
			search.ErrUnsupported, p.engine.Name())
	}
	return indexer, nil
}

// importCommand walks every Importable and writes its documents.
//
// Keyset rather than offset, for Cursor's reason: an import runs for minutes
// against a table that is being written to, and under exactly that traffic an
// offset page indexes one row twice and misses another.
func (p *Provider) importCommand(a app.App) *cobra.Command {
	var (
		only   string
		fresh  bool
		batch  int
		after  string
		settle bool
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "search:import",
		Short: "Index everything the application declares importable",
		Long: "Walks each Importable in batches and writes its documents.\n\n" +
			"Resumable: --after takes the last key a previous run reported, so an\n" +
			"interrupted import continues rather than starting over.\n\n" +
			"There is no --prune. Removing documents whose rows are gone would\n" +
			"need to list what is in the index, which the Engine contract has no\n" +
			"operation for — so --fresh is the answer, and the cost is that the\n" +
			"index is empty while the walk runs. A model that stops being\n" +
			"searchable is handled where it belongs instead: Conditional plus a\n" +
			"sync adapter unindexes it at the write.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			indexer, err := p.indexer()
			if err != nil {
				return err
			}
			if len(p.Importables) == 0 {
				return fmt.Errorf("search: the provider lists no Importables, so " +
					"there is nothing to import. They are listed by the " +
					"application rather than discovered — see Provider.Importables")
			}

			chosen, err := p.choose(only)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			started := time.Now()
			var total int

			for _, importable := range chosen {
				index := importable.SearchIndex()

				if fresh && !dryRun {
					// Before the walk, so the window where the index is
					// incomplete is as short as the walk and not longer.
					if err := indexer.Flush(ctx, index); err != nil {
						return fmt.Errorf("emptying %s: %w", index, err)
					}
					cmd.Printf("search: emptied %s\n", index)
				}

				count, last, err := p.walk(ctx, cmd, indexer, importable,
					search.Cursor{AfterKey: after, Limit: batch}, dryRun)
				total += count
				if err != nil {
					// The last key is reported even on failure, because it is
					// what --after takes and the run is otherwise lost.
					cmd.Printf("search: %s stopped after %d documents; "+
						"resume with --index=%s --after=%s\n", index, count, index, last)
					return err
				}
				cmd.Printf("search: %s — %d documents\n", index, count)
			}

			if dryRun {
				cmd.Printf("search: %d documents would be indexed (nothing was written)\n", total)
				return nil
			}

			// Settled by default, so the command's own answer is "indexed"
			// rather than "accepted". An asynchronous engine returns a task
			// id and processes writes one at a time, so a run that finished
			// without settling has not finished.
			if settle {
				if s, ok := p.engine.(search.Settler); ok {
					if err := s.Settle(ctx); err != nil {
						return fmt.Errorf("search: %d documents were accepted but "+
							"settling failed, so they may not be searchable yet: %w",
							total, err)
					}
				}
			}

			cmd.Printf("search: %d documents indexed in %s\n",
				total, time.Since(started).Round(time.Millisecond))
			return nil
		},
	}

	cmd.Flags().StringVar(&only, "index", "", "import only this index")
	cmd.Flags().BoolVar(&fresh, "fresh", false,
		"empty each index before importing; it is not searchable until the walk finishes")
	cmd.Flags().IntVar(&batch, "batch", search.DefaultPageSize,
		fmt.Sprintf("documents per batch, up to %d", search.MaxPageSize))
	cmd.Flags().StringVar(&after, "after", "",
		"resume after this document key, as a previous run reported it")
	cmd.Flags().BoolVar(&settle, "settle", true,
		"wait until the writes are searchable before reporting success")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"count what would be indexed without writing anything")
	return cmd
}

// walk pages through one Importable, returning how many documents it wrote
// and the last key it saw.
func (p *Provider) walk(ctx context.Context, cmd *cobra.Command, indexer search.Indexer,
	importable search.Importable, cursor search.Cursor, dryRun bool) (int, string, error) {

	index := importable.SearchIndex()
	cursor.Limit = search.ClampLimit(cursor.Limit)

	var (
		count int
		last  = cursor.AfterKey
	)
	for {
		docs, next, err := importable.SearchBatch(ctx, cursor)
		if err != nil {
			return count, last, fmt.Errorf("reading %s: %w", index, err)
		}
		// An empty batch ends the walk. There is no sentinel cursor and no
		// Done flag, which is Cursor's own decision: an empty AfterKey
		// already means "start", so it cannot also mean "finished".
		if len(docs) == 0 {
			return count, last, nil
		}

		for i := range docs {
			// Set here rather than trusted, so an Importable that forgot it
			// does not write every collection into one index. The key is
			// validated too: an engine would refuse it, but refusing here
			// names the collection and the key rather than the batch.
			if docs[i].Index == "" {
				docs[i].Index = index
			}
			if err := search.ValidKey(docs[i].Key); err != nil {
				return count, last, fmt.Errorf("%s: %w", index, err)
			}
			last = docs[i].Key
		}

		if !dryRun {
			if err := indexer.Index(ctx, docs); err != nil {
				return count, last, fmt.Errorf("indexing %s: %w", index, err)
			}
		}
		count += len(docs)
		cmd.Printf("search: %s — %d so far (through %s)\n", index, count, last)

		// A cursor the Importable did not advance would loop forever on the
		// same batch, which is a mistake in the application's walk rather
		// than here — and one that is invisible except as an import that
		// never ends.
		if next.AfterKey == cursor.AfterKey {
			return count, last, fmt.Errorf("search: %s returned the same cursor "+
				"twice (%q), so the walk would not end. SearchBatch must advance "+
				"its cursor past the batch it returned", index, next.AfterKey)
		}
		next.Limit = cursor.Limit
		cursor = next
	}
}

// choose narrows the Importables to one index, or returns all of them.
func (p *Provider) choose(only string) ([]search.Importable, error) {
	if only == "" {
		return p.Importables, nil
	}
	for _, importable := range p.Importables {
		if importable.SearchIndex() == only {
			return []search.Importable{importable}, nil
		}
	}
	return nil, fmt.Errorf("search: no Importable declares the index %q (declared: %v)",
		only, p.indexes())
}

func (p *Provider) indexes() []string {
	names := make([]string, 0, len(p.Importables))
	for _, importable := range p.Importables {
		names = append(names, importable.SearchIndex())
	}
	sort.Strings(names)
	return names
}

// flushCommand empties an index.
//
// It takes the index as an argument rather than defaulting to all of them,
// and asks before doing it. Emptying an index is not a reversible operation
// without a reimport, and the thing that makes it dangerous is that a search
// box over an empty index looks exactly like a search box over a database
// with nothing in it.
func (p *Provider) flushCommand(a app.App) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "search:flush <index>",
		Short: "Empty one search index without deleting it",
		Long: "Removes every document from an index and leaves the index and its\n" +
			"settings in place, so the next import does not have to recreate them.\n\n" +
			"The index is unsearchable until something reimports it, and a search\n" +
			"over an empty index is indistinguishable from a search over an empty\n" +
			"database — which is why this asks.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			indexer, err := p.indexer()
			if err != nil {
				return err
			}
			index := args[0]

			if !force {
				ok, err := confirm(cmd, fmt.Sprintf(
					"Empty the %s index on the %s engine? Nothing will be "+
						"findable in it until it is reimported.", index, p.engine.Name()))
				if err != nil {
					return err
				}
				if !ok {
					cmd.Println("search: nothing was emptied")
					return nil
				}
			}

			if err := indexer.Flush(cmd.Context(), index); err != nil {
				return fmt.Errorf("emptying %s: %w", index, err)
			}
			if s, ok := p.engine.(search.Settler); ok {
				if err := s.Settle(cmd.Context()); err != nil {
					return fmt.Errorf("search: %s was accepted for emptying but "+
						"settling failed: %w", index, err)
				}
			}

			cmd.Printf("search: %s is empty. Reimport with "+
				"`lemmego run search:import --index=%s`\n", index, index)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "do not ask")
	return cmd
}

// confirm asks on the terminal.
//
// Anything other than "y" is no, including an unreadable stdin: a command
// piped from a script with no input must not read EOF as consent.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	cmd.Printf("%s [y/N]: ", question)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(line), "y"), nil
}
