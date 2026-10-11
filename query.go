package search

import (
	"strings"
	"unicode"
)

// MaxQueryBytes bounds what is sent to the parser.
//
// websearch_to_tsquery will happily parse a megabyte, and then the resulting
// tsquery is matched against every row the index offers. The limit is on the
// input rather than on the output because a long query is always a mistake or
// an attack, and truncating one at a sensible length produces a search that
// still works.
const MaxQueryBytes = 200

// MaxPrefixTerms caps how many words a prefix query is built from.
//
// Each term becomes an index scan, and prefix terms are the expensive kind —
// a one-letter prefix matches a large fraction of the lexemes in the table.
const MaxPrefixTerms = 6

// Searchable reports whether there is anything to search for.
//
// Punctuation alone is not: "???" parses to an empty tsquery, which matches
// nothing, and a page that says "no results for ???" is worse than one that
// says "type something". Callers use this to tell those two apart.
func Searchable(text string) bool { return normalise(text) != "" }

// normalise trims and bounds the input.
func normalise(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > MaxQueryBytes {
		trimmed = trimmed[:MaxQueryBytes]
		// Truncating mid-rune would send invalid UTF-8 to the parser.
		for len(trimmed) > 0 && !isValidTail(trimmed) {
			trimmed = trimmed[:len(trimmed)-1]
		}
	}
	if !hasSearchableRune(trimmed) {
		return ""
	}
	return trimmed
}

func isValidTail(s string) bool {
	return strings.ToValidUTF8(s, "") == s
}

func hasSearchableRune(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// prefixTerms turns input into words safe to interpolate into a to_tsquery
// expression.
//
// Prefix matching needs the ":*" operator, which only to_tsquery
// understands — and to_tsquery raises a syntax error on anything it does not
// like, which is the failure websearch_to_tsquery exists to avoid. The way
// out is to never hand it anything it could object to: every rune that is not
// a letter or a digit becomes a separator, so each term is alphanumeric and
// the expression is built from terms this function produced rather than from
// anything a person typed.
//
// The terms are still passed as bind parameters. This returns them, it does
// not return SQL.
func prefixTerms(text string) []string {
	normalised := normalise(text)
	if normalised == "" {
		return nil
	}

	fields := strings.FieldsFunc(strings.ToLower(normalised), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})

	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		if field == "" {
			continue
		}
		terms = append(terms, field)
		if len(terms) == MaxPrefixTerms {
			break
		}
	}
	return terms
}
