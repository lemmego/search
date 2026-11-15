package search

import (
	"html"
	"html/template"
	"strings"
)

// The delimiters ts_headline is asked to wrap matches in.
//
// STX and ETX, two ASCII control characters, and the choice is the whole
// safety argument. The alternative — asking Postgres for "<mark>" and
// "</mark>" directly — means the string coming back out of the database
// contains real HTML mixed into somebody's bio, and then there is no way to
// escape the bio without also escaping the markup. Separating them is the
// only ordering that works: escape everything, then introduce the elements.
//
// Neither character is an HTML metacharacter, so html.EscapeString leaves
// both untouched and they survive to be replaced. And a document that
// somehow contains one — they cannot be typed, but an API client could send
// one — can produce at worst a stray <mark> or </mark>, never a tag of the
// attacker's choosing, because escaping has already happened by then.
// They are exported because an engine author is now the reader who needs
// them: a backend that highlights its own matches must be configured to emit
// exactly these two, and then Snippet works on its output unchanged. The
// Meilisearch engine sets highlightPreTag and highlightPostTag to them.
const (
	// HighlightPreTag goes before a match.
	HighlightPreTag = "\x02"

	// HighlightPostTag goes after one.
	HighlightPostTag = "\x03"
)

// The unexported spellings this file was written against, kept so the body
// below reads as it did and a reviewer comparing against v0.1.0 sees no
// change where none was intended.
const (
	startSel = HighlightPreTag
	stopSel  = HighlightPostTag
)

// headlineOptions is the ts_headline configuration.
//
// MaxFragments=2 with a delimiter gives two short windows around the matches
// rather than one long one, which is what reads usefully for a bio that
// mentions the term twice in different places. HighlightAll=FALSE matters:
// with it true, ts_headline returns the entire document, so a snippet of a
// long description would be the whole description.
const headlineOptions = "StartSel=\x02,StopSel=\x03," +
	"MaxFragments=2,FragmentDelimiter= … ,MaxWords=18,MinWords=5,HighlightAll=FALSE"

// Snippet turns one ts_headline result into safe markup.
//
// Escape first, mark second. The reverse order — or a template that trusts
// the string — publishes whatever is in the document, and the documents here
// are bios, comments and job descriptions written by anybody with an account.
//
// Returns template.HTML, which is a promise. It is kept by this function and
// nowhere else in the package: Title and Subtitle stay plain strings and are
// escaped by the renderer like any other value.
//
// Returns empty for a headline with no match in it. See the note below.
func Snippet(headline string) template.HTML {
	if headline == "" {
		return ""
	}

	// No delimiters means ts_headline found nothing to highlight in these
	// columns, which happens whenever the term matched a column the snippet
	// is not drawn from — a title, typically. Postgres still returns an
	// opening fragment of the document, and returning that would be a
	// snippet that shows why nothing matched.
	//
	// So it is dropped. The field is documented as the matched text, a
	// caller has the subtitle for plain context, and an unhighlighted lead
	// paragraph presented as evidence of a match is worse than no evidence.
	//
	// The reasoning above is about ts_headline, but the rule is not
	// Postgres-specific and must not be reimplemented per engine: a
	// Meilisearch hit that matched only the title comes back with an
	// unhighlighted crop of the body, which is the same situation and wants
	// the same answer.
	if !strings.Contains(headline, startSel) {
		return ""
	}

	escaped := html.EscapeString(headline)
	escaped = strings.ReplaceAll(escaped, startSel, "<mark>")
	escaped = strings.ReplaceAll(escaped, stopSel, "</mark>")

	// An unbalanced pair can only come from a document that contained a
	// control character itself. Dropping the orphan keeps the output
	// well-formed rather than leaving an element open across the rest of the
	// page.
	if strings.Count(escaped, "<mark>") != strings.Count(escaped, "</mark>") {
		escaped = strings.ReplaceAll(escaped, "<mark>", "")
		escaped = strings.ReplaceAll(escaped, "</mark>", "")
	}

	return template.HTML(escaped)
}

// PlainSnippet is Snippet without the markup, for a JSON API or a plain-text
// context such as an email or a meta description.
func PlainSnippet(headline string) string {
	cleaned := strings.ReplaceAll(headline, startSel, "")
	return strings.ReplaceAll(cleaned, stopSel, "")
}
