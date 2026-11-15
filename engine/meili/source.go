package meili

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lemmego/search"
)

// Source describes one index to this engine.
//
// Every field naming an attribute is written by the application. Nothing here
// is built from a request, and that is structural rather than advisory: the
// one piece of request data a visibility rule needs — who is asking — is not
// a field on this struct at all. The engine composes that clause itself from
// OwnerAttribute and PublicAttribute, so an application never writes a filter
// expression containing a value somebody chose, and there is nothing for it
// to forget to quote.
type Source struct {
	// Index is the index to search, unprefixed.
	Index string

	// Label is the human caption, falling back to Index.
	Label string

	// Title, Subtitle and URL name the attributes a hit is built from.
	Title, Subtitle, URL string

	// Snippet names the attributes a snippet is drawn from. They are
	// highlighted and cropped, and joined with the same " … " the postgres
	// engine's concat_ws uses.
	Snippet []string

	// Filter is static conjuncts in Meilisearch's filter syntax, ANDed.
	//
	// Written by the application and never by a request. A rule that needs to
	// know who is asking uses the two attributes below.
	Filter []string

	// OwnerAttribute names the attribute holding the document's owner, and
	// PublicAttribute the boolean saying it is visible to everybody.
	//
	// Given either or both, the engine composes the viewer clause: with a
	// viewer, "public OR owner = them"; without one, "public" alone. That is
	// the whole of what this engine can express about per-viewer visibility,
	// and it is deliberately not a string the application writes — see the
	// type comment.
	OwnerAttribute, PublicAttribute string

	// Filterable are the attributes CreateIndex declares.
	//
	// Meilisearch errors on a filter naming an undeclared attribute, which is
	// the good failure — so every attribute used above has to be listed here
	// or the search fails loudly at request time.
	Filterable []string

	// AtIndexTime declares the rule is enforced by what was indexed: a
	// Conditional model keeps the unreadable documents out.
	//
	// Only correct when every indexed document is visible to every viewer. A
	// published-or-not rule qualifies; a per-viewer rule never does.
	AtIndexTime bool
}

// SourceName and SourceLabel make a Source a search.Source.
func (s Source) SourceName() string { return s.Index }

// SourceLabel is the caption, falling back to the index name.
func (s Source) SourceLabel() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Index
}

// Visibility reports the rule in the form this engine speaks.
//
// The expression is composed for an anonymous viewer, which makes it a real
// filter somebody can read in a log or an error rather than a description of
// one. It is what Declared and Supports consult; the per-request expression
// comes from filter.
func (s Source) Visibility() search.Visibility {
	rule, err := s.filter(nil, nil)
	if err != nil {
		// Nothing declared. Reported as an absent rule, which is what
		// Supports refuses.
		rule = ""
	}
	return search.Visibility{Filter: rule, AtIndexTime: s.AtIndexTime}
}

// filter composes the expression for one request.
func (s Source) filter(viewer any, requested []search.Filter) (string, error) {
	conjuncts := make([]string, 0, len(s.Filter)+len(requested)+1)
	for _, f := range s.Filter {
		if trimmed := strings.TrimSpace(f); trimmed != "" {
			conjuncts = append(conjuncts, "("+trimmed+")")
		}
	}

	clause, err := s.viewerClause(viewer)
	if err != nil {
		return "", err
	}
	if clause != "" {
		conjuncts = append(conjuncts, clause)
	}

	// An empty expression returns the whole index, which is the one answer a
	// visibility rule must never produce. Supports has already refused a
	// source declaring nothing, so reaching here with nothing is a bug in
	// this engine rather than in the configuration — and it is better to say
	// so than to search everything.
	if len(conjuncts) == 0 && !s.AtIndexTime {
		return "", fmt.Errorf("%w: %q composed an empty filter",
			search.ErrVisibilityUnenforceable, s.Index)
	}

	// The caller's own filters go on last, so the visibility rule is never
	// the thing a parenthesis mismatch would detach. They are ANDed with it
	// and with each other, and there is no syntax a caller can supply that
	// reaches this expression — a field name is validated against
	// search.ValidField and a value is rendered by value(), never
	// interpolated.
	for _, f := range requested {
		clause, err := s.clause(f)
		if err != nil {
			return "", err
		}
		conjuncts = append(conjuncts, clause)
	}

	return strings.Join(conjuncts, " AND "), nil
}

// clause renders one request filter in Meilisearch's filter syntax.
func (s Source) clause(f search.Filter) (string, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}

	// Meilisearch answers invalid_search_filter for an attribute it was not
	// told to make filterable, which arrives as a failed search rather than
	// as the configuration error it is. So it is caught here, where the
	// message can name the attribute and the list it is missing from.
	if !slices.Contains(s.Filterable, f.Field) {
		return "", fmt.Errorf("%w: %q is not in %q's Filterable %v, and "+
			"meilisearch filters only on attributes it was told about",
			search.ErrBadFilter, f.Field, s.Index, s.Filterable)
	}

	if f.Op == search.OpIn || f.Op == search.OpNotIn {
		rendered := make([]string, 0, len(f.Values))
		for _, v := range f.Values {
			text, err := value(v)
			if err != nil {
				return "", err
			}
			rendered = append(rendered, text)
		}
		op := "IN"
		if f.Op == search.OpNotIn {
			op = "NOT IN"
		}
		return fmt.Sprintf("(%s %s [%s])", f.Field, op, strings.Join(rendered, ", ")), nil
	}

	text, err := value(f.Values[0])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(%s %s %s)", f.Field, f.Op, text), nil
}

// value renders a filter value.
//
// Numbers and booleans go in bare, because quoting them makes Meilisearch
// compare them as strings and a numeric range stops working. Everything else
// is quoted by the same function the viewer goes through, which is what keeps
// a value containing a quote from ending the expression and widening it.
func value(v any) (string, error) {
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t), nil
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	}
	// literal handles every integer type and quotes a string or a []byte. A
	// filter value and a viewer are the same problem, so they are the same
	// code: there is one place that can get the quoting wrong.
	return literal(v)
}

// viewerClause is the part of the rule that depends on who is asking.
func (s Source) viewerClause(viewer any) (string, error) {
	if s.OwnerAttribute == "" && s.PublicAttribute == "" {
		return "", nil
	}

	public := ""
	if s.PublicAttribute != "" {
		public = s.PublicAttribute + " = true"
	}

	if viewer == nil {
		if public != "" {
			return "(" + public + ")", nil
		}
		// An owner-only rule and nobody asking: nothing is visible. A
		// contradiction rather than an omitted clause, because omitting it
		// would widen the rule to everything — and rather than a reserved
		// sentinel value, which a document could legitimately hold.
		return fmt.Sprintf("(%s EXISTS AND %s NOT EXISTS)",
			s.OwnerAttribute, s.OwnerAttribute), nil
	}

	if s.OwnerAttribute == "" {
		return "(" + public + ")", nil
	}

	value, err := literal(viewer)
	if err != nil {
		return "", err
	}
	owned := s.OwnerAttribute + " = " + value

	if public == "" {
		return "(" + owned + ")", nil
	}
	return "(" + public + " OR " + owned + ")", nil
}

// literal renders a viewer as a filter value.
//
// This is the only place a value chosen by somebody else is written into a
// filter expression, so it is the only place that can get it wrong. An
// unescaped string key containing a quote ends the literal and hands the rest
// of the expression to whoever picked the key — the filter-expression form of
// SQL injection, in the one expression whose job is keeping rows apart.
//
// So integers go in bare, strings are quoted with the quote and the backslash
// escaped, and anything else is refused rather than guessed at.
func literal(viewer any) (string, error) {
	switch v := viewer.(type) {
	case int:
		return strconv.Itoa(v), nil
	case int8:
		return strconv.FormatInt(int64(v), 10), nil
	case int16:
		return strconv.FormatInt(int64(v), 10), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case uint:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case string:
		return quote(v), nil
	case []byte:
		return quote(string(v)), nil
	}
	return "", fmt.Errorf("%w: got %T", search.ErrBadViewer, viewer)
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// rawHit builds a hit from one Meilisearch document.
func (s Source) rawHit(hit map[string]any) search.RawHit {
	raw := search.RawHit{
		Key:      text(hit[KeyAttribute]),
		Title:    text(hit[s.Title]),
		Subtitle: text(hit[s.Subtitle]),
		URL:      text(hit[s.URL]),
	}
	if n, err := strconv.ParseInt(raw.Key, 10, 64); err == nil {
		raw.ID = n
	}
	if score, ok := hit["_rankingScore"].(float64); ok {
		raw.Rank = score
	}

	// The highlighted, cropped copies live under _formatted. Joined the way
	// the postgres engine's concat_ws joins its snippet columns, so the two
	// produce the same shape and search.Snippet sees one string either way.
	formatted, _ := hit["_formatted"].(map[string]any)
	if formatted == nil || len(s.Snippet) == 0 {
		return raw
	}

	parts := make([]string, 0, len(s.Snippet))
	for _, field := range s.Snippet {
		if value := text(formatted[field]); value != "" {
			parts = append(parts, value)
		}
	}
	raw.Headline = strings.Join(parts, " … ")
	return raw
}

func text(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	}
	return fmt.Sprint(v)
}
