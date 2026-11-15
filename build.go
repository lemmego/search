package search

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Factory builds an engine from a resolved configuration.
type Factory func(ctx context.Context, cfg *Config) (Engine, error)

var (
	engineMu sync.RWMutex
	engines  = map[string]Factory{}
)

// Register makes an engine available to Build under name.
//
// Engines register themselves from an init function and are selected by a
// blank import, the way database/sql drivers and cache/store/* are. The
// indirection is what lets an engine depend on this package — it needs
// Engine, Query and RawHit — without this package depending on it, so an
// application that searches Postgres does not compile a Meilisearch client
// into its binary.
//
// Importing github.com/lemmego/search/engines registers every engine that is
// not built in. The postgres engine needs no import: it is in this package,
// because it needs nothing but database/sql and so costs a consumer nothing.
func Register(name string, factory Factory) {
	if name == "" || factory == nil {
		panic("search: Register needs a name and a factory")
	}
	engineMu.Lock()
	defer engineMu.Unlock()
	if _, taken := engines[name]; taken {
		panic("search: engine " + name + " is registered twice")
	}
	engines[name] = factory
}

// SupportedEngines lists the registered engine names.
func SupportedEngines() []string {
	engineMu.RLock()
	defer engineMu.RUnlock()

	names := make([]string, 0, len(engines))
	for name := range engines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Build creates the engine a Config describes.
//
// A backend that cannot be reached is reported here rather than on first use,
// so a wrong address fails the boot instead of surfacing later as a failed
// search. There is deliberately no lenient mode: a cache that keeps nothing
// is slow but still correct, whereas a search that finds nothing looks like
// an empty database and is the kind of outage somebody notices a week later.
func Build(ctx context.Context, cfg *Config) (Engine, error) {
	if cfg == nil {
		return nil, fmt.Errorf("search: Build needs a configuration")
	}
	if err := cfg.normalizeAndValidate(); err != nil {
		return nil, err
	}

	engineMu.RLock()
	factory, ok := engines[cfg.Engine]
	engineMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf(
			"search: no engine registered for %q (registered: %v); "+
				"import github.com/lemmego/search/engines to register the ones "+
				"that are not built in",
			cfg.Engine, SupportedEngines())
	}

	engine, err := factory(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return engine, nil
}
