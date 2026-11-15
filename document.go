package search

import (
	"context"
	"fmt"
)

// Document is one row as an engine stores it.
type Document struct {
	// Index is unprefixed, as SearchIndex returns it: "posts", not
	// "myapp_prod_posts". The engine applies its configured prefix, which is
	// what stops a search:flush run from a laptop emptying production.
	Index string

	// Key is the document id, kept out of Fields on purpose: an engine needs
	// to know which attribute is the primary key, and inferring it from a
	// field called "id" mis-keys every model that uses "id" for something
	// else. The engine writes Key into the index under whatever name it
	// requires.
	Key string

	// Fields is what is searchable, filterable and displayable.
	//
	// Values must be JSON-encodable: the engines here speak JSON over HTTP,
	// and a value that will not marshal fails the whole batch rather than one
	// document.
	Fields map[string]any
}

// Indexable is implemented by a model whose rows belong in an engine's own
// index. It is Scout's toSearchableArray, searchableAs and getScoutKey, with
// types.
//
// Not named Searchable, which is unavailable: this package already exports
// Searchable(text string) bool, it is in the README and in callers' handlers,
// and Go will not let one package hold both. Indexable is the better name for
// this side anyway — it is about being put into an index, not about being
// queryable text.
//
// Three methods rather than one returning a Document, because Forget needs
// the index and the key without paying to build the fields.
type Indexable interface {
	// SearchIndex is the index these documents live in, unprefixed.
	SearchIndex() string

	// SearchKey is this row's document id, and must be stable for the life of
	// the row: it is what Unindex is given and what a second Index call
	// replaces rather than duplicates.
	//
	// A string because an engine stores what it is given, an integer
	// round-trips through one losslessly, and a uuid does not round-trip
	// through an int64. See ValidKey for what may be in it.
	SearchKey() string

	// SearchDocument is what gets copied into the index.
	//
	// Only what search needs. Every field here leaves the database, lands in
	// another system's storage, and comes back to whoever can reach that
	// system — so a document carrying a password hash or an email address
	// because the struct had one is a disclosure with no request behind it.
	SearchDocument() map[string]any
}

// Conditional is implemented by a model that is not always indexable. It is
// Scout's shouldBeSearchable.
//
// Pure, and documented as such: no context, no error, no I/O. An
// error-returning predicate has a third outcome nobody can specify — does a
// transient database failure mean index, skip, or remove? — and whichever
// answer is chosen becomes a silent corpus-wide bug in one direction. The
// fields it usually reads, a published_at or a deleted_at, are already on the
// struct that was just written; anything else should be computed into a field
// before the save.
type Conditional interface {
	ShouldBeSearchable() bool
}

// Importable is a collection search:import can walk.
//
// Batched with a keyset Cursor rather than a page number, for the reason
// Cursor gives. The closure is written by the application because this module
// has no ORM and never will, which is what lets an application on GORM, on
// Bun, on lemmego/orm or on none of them use it.
type Importable interface {
	SearchIndex() string

	// SearchBatch returns the next batch and the cursor to continue from. An
	// empty batch ends the walk.
	SearchBatch(ctx context.Context, after Cursor) (docs []Document, next Cursor, err error)
}

// Hydrator loads models by document key.
//
// This module cannot load them itself: it has no ORM, which is the property
// that makes it usable from anything. Load may return them in any order and
// may return fewer than it was asked for — a document can outlive its row —
// and the caller restores the engine's order afterwards using SearchKey.
type Hydrator[T Indexable] interface {
	Load(ctx context.Context, keys []string) ([]T, error)
}

// HydratorFunc adapts a function to Hydrator.
type HydratorFunc[T Indexable] func(ctx context.Context, keys []string) ([]T, error)

// Load calls f.
func (f HydratorFunc[T]) Load(ctx context.Context, keys []string) ([]T, error) {
	return f(ctx, keys)
}

// ValidKey reports whether a key can be stored by every engine here.
//
// Alphanumerics, hyphen and underscore. That is Meilisearch's document-id
// charset, so the weakest engine sets the rule for all of them — including
// the Postgres engine, which has no such restriction.
//
// Making the Postgres engine stricter than it needs to be is the deliberate
// part. In exchange, engine portability becomes a property a test can check
// rather than a hope, and a model keyed "tenant:42/post:7" finds out on the
// first search:import instead of on the day somebody tries to move engines —
// by which time changing SearchKey means reindexing every document.
//
// It validates and never transforms. A transparently re-encoded key would
// make Hit.Key differ from SearchKey(), and then a caller holding a hit can no
// longer look the row up.
func ValidKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: a key may not be empty", ErrBadKey)
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return fmt.Errorf("%w: %q contains %q", ErrBadKey, key, r)
		}
	}
	return nil
}

// DocumentOf builds a Document from a model.
func DocumentOf(m Indexable) (Document, error) {
	key := m.SearchKey()
	if err := ValidKey(key); err != nil {
		return Document{}, fmt.Errorf("%T: %w", m, err)
	}
	return Document{Index: m.SearchIndex(), Key: key, Fields: m.SearchDocument()}, nil
}
