package search

import (
	"fmt"
	"strings"
	"time"
)

// Engine names the built-in engines, so a configuration string is not a
// literal at every call site.
const (
	EnginePostgres = "postgres"
	EngineMeili    = "meilisearch"
)

// Config is how an engine is selected and configured.
//
// Per-engine blocks are inline rather than in the engine packages, so that an
// engine subpackage needs no config type of its own and this package needs no
// import of it — cache's shape, for cache's reason.
type Config struct {
	// Engine selects the backend. Empty defaults to postgres, because that is
	// the engine that needs no infrastructure an application does not already
	// have.
	Engine string

	// Prefix namespaces every index name an engine writes or reads.
	//
	// It is also what bounds Flush, which is the reason an engine sharing an
	// instance between environments insists on one: without it, a
	// `search:flush posts` run from a laptop empties production's posts
	// index. Meaningless to the postgres engine, which has no index of its
	// own to name.
	Prefix string

	Postgres PostgresConfig
	Meili    MeiliConfig
}

// PostgresConfig carries the handle, not a DSN.
//
// The engine searches the application's own tables, so it must use the
// application's own pool — one pool, one set of limits — rather than opening a
// second one whose size nobody configured.
type PostgresConfig struct {
	DB Querier
}

// MeiliConfig configures the Meilisearch engine.
type MeiliConfig struct {
	// Host is the base URL, such as http://127.0.0.1:7700.
	Host string

	// APIKey should be a key scoped to the actions this application performs,
	// not the master key. The master key can create and drop every index on
	// the instance, which is more authority than a web process needs to
	// answer a search box.
	APIKey string

	// Timeout bounds every request. Zero means a sensible default, which is
	// not http.Client's own: that one has no timeout at all, so a hung
	// instance would hold a request goroutine until the client gave up,
	// which it never would.
	Timeout time.Duration
}

func (c *Config) normalizeAndValidate() error {
	c.Engine = strings.TrimSpace(c.Engine)
	if c.Engine == "" {
		c.Engine = EnginePostgres
	}
	c.Prefix = strings.TrimSpace(c.Prefix)

	if c.Prefix != "" {
		if err := ValidKey(c.Prefix); err != nil {
			return fmt.Errorf("search: the index prefix %q is not usable as part "+
				"of an index name: %w", c.Prefix, err)
		}
	}
	return nil
}
