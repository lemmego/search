package search

import (
	"context"
	"fmt"
)

// Finder is the engine-agnostic API, and the concrete type an application
// resolves from the container.
//
// It owns every guarantee this module makes about the shape of an answer: one
// group per source in the order they were passed, the empty-query answer with
// no queries run, the escaping of every snippet, the page-size clamp. An
// engine cannot opt out of any of them, because an engine never builds a
// Group or a Hit.
type Finder struct {
	engine Engine
}

// NewFinder returns a Finder over an engine.
func NewFinder(e Engine) *Finder { return &Finder{engine: e} }

// Engine returns the underlying engine, for the rare caller that needs a
// capability this type does not expose.
func (f *Finder) Engine() Engine { return f.engine }

// Capabilities reports what the underlying engine can do.
func (f *Finder) Capabilities() Capabilities { return f.engine.Capabilities() }

// Search answers a query over the given sources.
func (f *Finder) Search(ctx context.Context, q Query, sources ...Source) (*Results, error) {
	return search(ctx, f.engine, q, sources)
}

// SourceSearcher is Finder's method as an interface, for a handler that wants
// to depend on the contract rather than the type. Searcher is the
// Table-shaped spelling of the same thing, kept for the callers that have it.
type SourceSearcher interface {
	Search(ctx context.Context, q Query, sources ...Source) (*Results, error)
}

// AsSources widens a slice of descriptors, because Go has no covariance:
// []Table is not []Source even though every Table is a Source.
func AsSources[T Source](in []T) []Source {
	out := make([]Source, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}

// search is the one place the invariants live. Both Finder.Search and
// (*Postgres).Search go through it, so there is no second copy to drift.
func search(ctx context.Context, e Engine, q Query, sources []Source) (*Results, error) {
	if len(sources) == 0 {
		return nil, ErrNoTable
	}
	if err := validateViewer(q.Viewer); err != nil {
		return nil, err
	}

	// Every source checked before any query runs, so a wiring mistake is one
	// error rather than a page that quietly lacks a section. A source the
	// engine cannot enforce fails the whole search rather than being dropped:
	// dropping it would break the one-group-per-source invariant, and a
	// caller that genuinely wants degradation can ask for it with
	// errors.Is(err, ErrVisibilityUnenforceable).
	for _, src := range sources {
		if err := e.Supports(src); err != nil {
			return nil, fmt.Errorf("source %s: %w", src.SourceName(), err)
		}
	}

	results := &Results{Text: q.Text, Groups: make([]Group, 0, len(sources))}

	// Nothing searchable: answer with the shape and no queries. Eight index
	// scans for an empty box is work nobody asked for, and the page needs to
	// know the difference between "type something" and "no results".
	if !Searchable(q.Text) {
		results.Empty = true
		for _, src := range sources {
			results.Groups = append(results.Groups, Group{
				Source: src.SourceName(),
				Label:  label(src),
				Hits:   []Hit{},
			})
		}
		return results, nil
	}

	reqs := make([]SearchRequest, 0, len(sources))
	for _, src := range sources {
		reqs = append(reqs, SearchRequest{
			Source: src,
			Text:   normalise(q.Text),
			Prefix: q.Prefix,
			Viewer: q.Viewer,
			Limit:  ClampPerSource(q.Limit),
			Offset: q.Offset,
		})
	}

	out, err := e.MultiSearch(ctx, reqs)
	if err != nil {
		return nil, err
	}

	// Cannot be papered over. Matching results to sources by guessing is how
	// a hit gets the wrong label, and a MultiSearch that dropped an empty
	// result would shift every label by one.
	if len(out) != len(reqs) {
		return nil, fmt.Errorf("%w: the %s engine answered %d of %d requests",
			ErrEngineContract, e.Name(), len(out), len(reqs))
	}

	for i, res := range out {
		group := groupOf(sources[i], res)
		results.Groups = append(results.Groups, group)
		results.Total += group.Total
	}

	return results, nil
}

func label(src Source) string {
	if l := src.SourceLabel(); l != "" {
		return l
	}
	return src.SourceName()
}

// groupOf turns one engine answer into one group.
//
// The one place a RawHit becomes a Hit, which is what keeps the escaping
// promise in Hit.Snippet checkable: every path into this module's result
// types — a grouped Search and the typed Builder alike — comes through here.
func groupOf(src Source, res SearchResult) Group {
	group := Group{
		Source:    src.SourceName(),
		Label:     label(src),
		Hits:      make([]Hit, 0, len(res.Hits)),
		Total:     res.Total,
		Estimated: res.Estimated,
		More:      res.More,
	}
	for _, raw := range res.Hits {
		group.Hits = append(group.Hits, Hit{
			// Set here and not by the engine. An engine that filled this in
			// differently would be silently corrected, which is the intent —
			// the core knows which request this result answers and the engine
			// only thinks it does.
			Source:   src.SourceName(),
			Key:      raw.Key,
			ID:       raw.ID,
			Title:    raw.Title,
			Subtitle: raw.Subtitle,
			URL:      raw.URL,
			// The one place a template.HTML is minted in this module. Engines
			// return text; this escapes it and then introduces the <mark>
			// elements. See Snippet.
			Snippet: Snippet(raw.Headline),
			Rank:    raw.Rank,
		})
	}
	return group
}

// validateViewer refuses a viewer no engine can bind.
//
// Query.Viewer is `any` so that a uuid key works and a filter expression can
// have it as a string, and the cost of that is a type error moving from
// compile time to here. So it is checked once, before any query, rather than
// surfacing from deep inside a driver as "unsupported type".
func validateViewer(v any) error {
	switch v.(type) {
	case nil,
		string, []byte,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return nil
	}
	return fmt.Errorf("%w: got %T", ErrBadViewer, v)
}
