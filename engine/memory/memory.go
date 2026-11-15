// Package memory is a search engine that keeps its documents in this process.
//
// It exists for three jobs, and is honest about not being a fourth.
//
// It is what the conformance suite runs against, which matters more than it
// sounds: the Postgres engine keeps no index of its own, so it skips every
// case about writing one. Without a second engine that *does*, Index, Unindex
// and Flush would be contract with no implementation behind it — and a
// contract nobody implements is a contract nobody has checked.
//
// It is what an application's own handler tests can run against, with no
// database and no container.
//
// And it is the reference an engine author reads, because it is small enough
// to read.
//
// It is **not** for production. Matching is by token prefix with no stemming,
// no stop words and no ranking worth the name, and everything is lost when
// the process exits. An application wanting an index of its own wants
// Meilisearch; an application happy searching its rows where they are wants
// the Postgres engine.
package memory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/lemmego/search"
)

// Name is the name this engine registers under.
const Name = "memory"

// Source describes one index to this engine.
type Source struct {
	// Index is the index to search.
	Index string

	// Label is the human caption.
	Label string

	// Title, Subtitle and URL name the fields a hit is built from.
	Title, Subtitle, URL string

	// Snippet names the fields a snippet is drawn from.
	Snippet []string

	// Visible is a rule this engine can evaluate: conjuncts of
	// `field = value`, joined by AND, with search.ViewerPlaceholder standing
	// in for the viewer.
	//
	// Deliberately not a general expression language. A filter DSL is the
	// thing this module refuses to grow — see search.Filter — and for the one
	// engine whose job is to be readable, a parser would be the largest and
	// least interesting part of it.
	Visible string

	// AtIndexTime declares the rule is enforced by what was indexed.
	AtIndexTime bool
}

func (s Source) SourceName() string  { return s.Index }
func (s Source) SourceLabel() string { return s.Label }

func (s Source) Visibility() search.Visibility {
	return search.Visibility{Filter: s.Visible, AtIndexTime: s.AtIndexTime}
}

// Engine is an in-process search engine.
type Engine struct {
	mu      sync.RWMutex
	indexes map[string]map[string]search.Document
}

// New returns an empty engine.
func New() *Engine {
	return &Engine{indexes: map[string]map[string]search.Document{}}
}

func init() {
	search.Register(Name, func(context.Context, *search.Config) (search.Engine, error) {
		return New(), nil
	})
}

// Name reports the engine's name.
func (e *Engine) Name() string { return Name }

// Capabilities reports what this engine can do.
//
// FilterVisibility is true and RelationalVisibility false, which is the
// position every engine holding its own documents is in: it has a copy of the
// fields and no access to the rows, so it can compare a field and cannot
// evaluate an EXISTS over a follow graph. The suite's refusal case exercises
// exactly that.
func (e *Engine) Capabilities() search.Capabilities {
	return search.Capabilities{
		RelationalVisibility: false,
		FilterVisibility:     true,
		ExactTotals:          true,
		OffsetPaging:         true,
		Prefix:               true,
		Highlighting:         true,
		Filters:              true,
		Asynchronous:         false,
	}
}

// Supports reports whether this engine can serve a source.
func (e *Engine) Supports(src search.Source) error {
	source, ok := src.(Source)
	if !ok {
		return fmt.Errorf("%w: the memory engine takes a memory.Source, got %T",
			search.ErrSourceMismatch, src)
	}

	vis := source.Visibility()
	if !vis.Declared() {
		return fmt.Errorf("%w: %q", search.ErrNoVisibility, source.Index)
	}
	if vis.SQL != "" && vis.Filter == "" && !vis.AtIndexTime {
		return fmt.Errorf("%w: %q declares only a SQL predicate, which only an "+
			"engine reading the rows themselves can evaluate. Give it a Filter, "+
			"or enforce it at index time, or keep this source on the postgres "+
			"engine.", search.ErrVisibilityUnenforceable, source.Index)
	}
	// A rule this engine cannot parse is a rule it cannot enforce, and it
	// says so with that sentinel rather than a bespoke error: the caller's
	// question is "will you refuse this source", and the answer must be
	// recognisable whatever the reason. Refused at Supports, which runs
	// before any query, so an unparseable rule is a wiring error at boot
	// rather than an empty predicate matching the whole index at request
	// time — which is the one failure a search engine must not have.
	if _, err := parseFilter(source.Visible, nil); err != nil {
		return fmt.Errorf("%w: %q: %w", search.ErrVisibilityUnenforceable, source.Index, err)
	}
	return nil
}

// Index writes documents, replacing each wholly.
func (e *Engine) Index(_ context.Context, docs []search.Document) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, doc := range docs {
		if err := search.ValidKey(doc.Key); err != nil {
			return err
		}
		if e.indexes[doc.Index] == nil {
			e.indexes[doc.Index] = map[string]search.Document{}
		}
		// Replace, never merge. A field a model stops setting must stop
		// being findable.
		copied := make(map[string]any, len(doc.Fields))
		for k, v := range doc.Fields {
			copied[k] = v
		}
		e.indexes[doc.Index][doc.Key] = search.Document{
			Index: doc.Index, Key: doc.Key, Fields: copied,
		}
	}
	return nil
}

// Unindex removes documents by key. Removing an absent key is not an error.
func (e *Engine) Unindex(_ context.Context, index string, keys []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, key := range keys {
		delete(e.indexes[index], key)
	}
	return nil
}

// Flush empties an index without removing it.
func (e *Engine) Flush(_ context.Context, index string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// An empty map rather than a delete, so the index still exists and can be
	// written to — which is what separates emptying it from dropping it.
	e.indexes[index] = map[string]search.Document{}
	return nil
}

// MultiSearch answers each request.
func (e *Engine) MultiSearch(_ context.Context, reqs []search.SearchRequest) ([]search.SearchResult, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]search.SearchResult, 0, len(reqs))
	for _, req := range reqs {
		source, ok := req.Source.(Source)
		if !ok {
			return nil, fmt.Errorf("%w: got %T", search.ErrSourceMismatch, req.Source)
		}
		if err := search.ValidateFilters(req.Filters); err != nil {
			return nil, err
		}

		res, err := e.searchOne(req, source)
		if err != nil {
			return nil, fmt.Errorf("search %s: %w", source.Index, err)
		}
		out = append(out, res)
	}
	return out, nil
}

func (e *Engine) searchOne(req search.SearchRequest, source Source) (search.SearchResult, error) {
	res := search.SearchResult{Hits: []search.RawHit{}}

	predicate, err := parseFilter(source.Visible, req.Viewer)
	if err != nil {
		return res, err
	}

	terms := tokens(req.Text)
	if len(terms) == 0 {
		return res, nil
	}

	type scored struct {
		doc  search.Document
		rank float64
	}
	var matched []scored

	for _, doc := range e.indexes[source.Index] {
		if !predicate(doc) {
			continue
		}
		if !matchFilters(doc, req.Filters) {
			continue
		}
		rank := score(doc, terms, req.Prefix)
		if rank == 0 {
			continue
		}
		matched = append(matched, scored{doc: doc, rank: rank})
	}

	// Rank descending, then by key ascending. The tiebreak is not decoration:
	// without a total order, two pages of the same query can repeat a
	// document and hide another, which the conformance suite checks.
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].rank != matched[j].rank {
			return matched[i].rank > matched[j].rank
		}
		return matched[i].doc.Key < matched[j].doc.Key
	})

	res.Total = len(matched)

	start := req.Offset
	if start > len(matched) {
		start = len(matched)
	}
	end := start + req.Limit
	if end > len(matched) {
		end = len(matched)
	}

	for _, m := range matched[start:end] {
		res.Hits = append(res.Hits, search.RawHit{
			Key:      m.doc.Key,
			ID:       asInt(m.doc.Key),
			Title:    field(m.doc, source.Title),
			Subtitle: field(m.doc, source.Subtitle),
			URL:      field(m.doc, source.URL),
			Headline: headline(m.doc, source.Snippet, terms, req.Prefix),
			Rank:     m.rank,
		})
	}
	res.More = res.Total > req.Offset+len(res.Hits)
	return res, nil
}

// parseFilter turns a Visible rule into a predicate.
//
// The grammar is conjuncts of `field = value` joined by AND, which is all this
// engine claims. Anything else is a declaration error reported from Supports,
// at boot, rather than a rule that silently matches everything — an empty
// predicate returns the whole index, which is the one failure a search
// engine must not have.
func parseFilter(rule string, viewer any) (func(search.Document) bool, error) {
	rule = strings.TrimSpace(rule)
	if rule == "" || strings.EqualFold(rule, "true") {
		return func(search.Document) bool { return true }, nil
	}

	type clause struct{ field, value string }
	var clauses []clause

	for _, part := range splitAnd(rule) {
		field, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("the memory engine understands conjuncts of "+
				"`field = value` joined by AND, not %q", part)
		}
		field = strings.TrimSpace(field)
		value = strings.Trim(strings.TrimSpace(value), "'\"")

		// The field has to be a plain identifier, and checking it is not
		// decoration. Without this, Cut finds the `=` inside
		// `EXISTS (SELECT 1 ... WHERE follower_id = x)` and the rule parses
		// as a comparison on a field named "EXISTS (SELECT 1 ...". Nothing
		// has that field, so the predicate matches nothing — the whole index
		// silently filtered out by a rule this engine cannot evaluate, which
		// is precisely the case it is supposed to refuse. Found by the
		// conformance suite.
		if !identifier(field) {
			return nil, fmt.Errorf("%q is not a field name; this engine "+
				"understands conjuncts of `field = value` joined by AND, and "+
				"nothing else", field)
		}
		if value == search.ViewerPlaceholder {
			value = fmt.Sprint(viewer)
		}
		clauses = append(clauses, clause{field: field, value: value})
	}

	return func(doc search.Document) bool {
		for _, c := range clauses {
			if fmt.Sprint(doc.Fields[c.field]) != c.value {
				return false
			}
		}
		return true
	}, nil
}

// identifier reports a plain unqualified column name.
//
// The core's rule rather than a second copy of it, so a field name this
// engine accepts in a visibility rule is one every engine accepts in a
// filter.
func identifier(s string) bool { return search.ValidField(s) == nil }

// matchFilters reports whether a document satisfies every filter.
//
// Filters are ANDed, which is the whole of the contract — see search.Filter
// on why there is no OR.
//
// A document that does not have the field satisfies no filter, including a
// negative one. SQL answers that case with three-valued logic, where
// `status != 'draft'` is unknown rather than true for a missing status and
// so excludes the row — and a filter that admitted documents by virtue of
// lacking the field would quietly widen every restriction written with
// WhereNot. One rule, applied to every operator.
func matchFilters(doc search.Document, filters []search.Filter) bool {
	for _, f := range filters {
		have, present := doc.Fields[f.Field]
		if !present {
			return false
		}
		if !matchOne(have, f) {
			return false
		}
	}
	return true
}

func matchOne(have any, f search.Filter) bool {
	switch f.Op {
	case search.OpIn, search.OpNotIn:
		var found bool
		for _, want := range f.Values {
			if cmp, ok := compare(have, want); ok && cmp == 0 {
				found = true
				break
			}
		}
		return found == (f.Op == search.OpIn)
	}

	cmp, ok := compare(have, f.Values[0])
	if !ok {
		// Two values of kinds that do not compare — a number against a
		// string. Not an error from here: the filter was already validated,
		// so this is a document whose field holds something else, and the
		// honest answer about one document is that it does not match.
		return false
	}
	switch f.Op {
	case search.OpEq:
		return cmp == 0
	case search.OpNe:
		return cmp != 0
	case search.OpLt:
		return cmp < 0
	case search.OpLte:
		return cmp <= 0
	case search.OpGt:
		return cmp > 0
	case search.OpGte:
		return cmp >= 0
	}
	return false
}

// compare orders two filter values, reporting whether they are comparable at
// all.
//
// Numbers compare numerically across their Go types and everything else as
// text. That is not tidiness: a document field set from an int and a filter
// value arriving from a JSON request body as a float64 are the same number,
// and comparing their fmt.Sprint forms makes "7" and "7.0" different
// documents.
func compare(a, b any) (int, bool) {
	an, aok := asFloat(a)
	bn, bok := asFloat(b)
	if aok && bok {
		switch {
		case an < bn:
			return -1, true
		case an > bn:
			return 1, true
		}
		return 0, true
	}
	if aok != bok {
		return 0, false
	}

	ab, aok := asText(a)
	bb, bok := asText(b)
	if !aok || !bok {
		return 0, false
	}
	return strings.Compare(ab, bb), true
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func asText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case []byte:
		return string(t), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

func splitAnd(rule string) []string {
	parts := strings.Split(rule, " AND ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func tokens(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// "or" is dropped rather than honoured: this engine ANDs nothing and
		// ORs nothing, it scores by how many terms appear, so a query word
		// that is really an operator elsewhere would otherwise be searched
		// for literally.
		if f == "or" || f == "and" {
			continue
		}
		out = append(out, f)
	}
	return out
}

func score(doc search.Document, terms []string, prefix bool) float64 {
	var total float64
	for _, value := range doc.Fields {
		text, ok := value.(string)
		if !ok {
			continue
		}
		for _, word := range tokens(text) {
			for _, term := range terms {
				if word == term || (prefix && strings.HasPrefix(word, term)) {
					total++
				}
			}
		}
	}
	return total
}

func headline(doc search.Document, fields, terms []string, prefix bool) string {
	var parts []string
	for _, name := range fields {
		text, _ := doc.Fields[name].(string)
		if text == "" {
			continue
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return ""
	}

	joined := strings.Join(parts, " … ")
	marked, any := mark(joined, terms, prefix)
	if !any {
		// Nothing matched in these fields, so there is no snippet. Returning
		// the lead fragment would present unhighlighted prose as evidence of
		// a match.
		return ""
	}
	return marked
}

// mark wraps each matching word in the sentinels search.Snippet looks for.
//
// It never introduces markup: the two sentinels are control characters, and
// the core escapes the whole string before turning them into <mark>. That
// ordering is the only one that is safe, and it is the core's to perform —
// an engine that returned "<mark>" here would publish whatever else is in the
// document.
func mark(text string, terms []string, prefix bool) (string, bool) {
	var out strings.Builder
	var marked bool

	word := strings.Builder{}
	flush := func() {
		if word.Len() == 0 {
			return
		}
		w := word.String()
		lower := strings.ToLower(w)
		hit := false
		for _, term := range terms {
			if lower == term || (prefix && strings.HasPrefix(lower, term)) {
				hit = true
				break
			}
		}
		if hit {
			out.WriteString(search.HighlightPreTag)
			out.WriteString(w)
			out.WriteString(search.HighlightPostTag)
			marked = true
		} else {
			out.WriteString(w)
		}
		word.Reset()
	}

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word.WriteRune(r)
			continue
		}
		flush()
		out.WriteRune(r)
	}
	flush()

	return out.String(), marked
}

func field(doc search.Document, name string) string {
	if name == "" {
		return ""
	}
	if v, ok := doc.Fields[name]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func asInt(key string) int64 {
	n, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

var (
	_ search.Engine  = (*Engine)(nil)
	_ search.Indexer = (*Engine)(nil)
)
