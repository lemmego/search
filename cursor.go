package search

// Paging bounds for walking a whole index — an import, a reindex, a prune.
//
// These are not DefaultPerSource and MaxPerSource, and the pair of
// vocabularies is deliberate rather than an oversight. Those two page a
// grouped search, where five is a product decision about a page of short
// lists; these page a backfill, where the number is about batch size and
// round trips. Each doc comment names the other so nobody unifies them by
// accident.
const (
	DefaultPageSize = 100
	MaxPageSize     = 1000
)

// ClampLimit applies DefaultPageSize and MaxPageSize.
//
// Exported because a limit that arrives from a command-line flag is the
// normal case, and because every Importable needs the same clamping.
func ClampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageSize
	case limit > MaxPageSize:
		return MaxPageSize
	default:
		return limit
	}
}

// Cursor pages by key rather than by offset.
//
// An import runs for minutes against a table that is being written to, and
// under exactly that traffic an offset page indexes one row twice and misses
// another — and OFFSET on a large table is quadratic besides, because the
// database fetches and discards every row before it.
type Cursor struct {
	// AfterKey returns rows ordered after this key. Empty starts at the
	// beginning.
	AfterKey string

	// Limit is how many to return. Zero means DefaultPageSize.
	Limit int
}

// A walk ends when a batch comes back empty, and there is deliberately no
// Done method or sentinel cursor to say so. An empty AfterKey already means
// "start", so it cannot also mean "finished" without the one value meaning
// two things — and a separate flag is a third return value every caller has
// to remember to check. An empty batch is unambiguous and impossible to
// ignore.
