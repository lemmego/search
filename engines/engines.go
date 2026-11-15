// Package engines registers every search engine that is not built in.
//
// Import it for its side effects when an application selects its engine from
// configuration:
//
//	import _ "github.com/lemmego/search/engines"
//
// The postgres engine is deliberately not here: it lives in the root package
// and registers itself, because it needs nothing but database/sql and so
// costs a consumer nothing to link. Everything that needs a network client
// or an SDK registers itself from its own package, the way database/sql
// drivers and cache/store/* do — and today the memory and meilisearch
// engines are standard library too, so this aggregator is about where a
// decision is declared rather than about binary size. The day an engine
// wants a client library, this is already the shape that keeps it out of
// everybody else's binary.
package engines

import (
	_ "github.com/lemmego/search/engine/meili"
	_ "github.com/lemmego/search/engine/memory"
)
