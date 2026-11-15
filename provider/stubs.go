package provider

// ConfigStub is the search configuration published into a project.
//
// The Postgres engine is deliberately absent from it: it takes the
// application's own pool, which no configuration string can name, and the
// provider fetches it from the container. So there is nothing to configure
// for the default engine, which is the point — an application searching its
// own tsvector columns needs no infrastructure and no configuration beyond
// the engine name.
const ConfigStub = `package configs

import "github.com/lemmego/api/config"

func init() {
	config.Set("search", config.M{
		// postgres searches the tsvector columns this application already
		// has, and needs nothing else running. meilisearch keeps its own
		// index and has to be reachable at boot — a configured engine that
		// cannot be reached fails the boot rather than answering nothing.
		"engine": config.MustEnv("SEARCH_ENGINE", "postgres"),

		// Namespaces every index name, and bounds what a flush can empty.
		// Set it per environment: without one, ` + "`lemmego run search:flush posts`" + `
		// from a laptop empties production's posts index. Meaningless to the
		// postgres engine, which has no index of its own to name.
		"prefix": config.MustEnv("SEARCH_PREFIX", ""),

		"meilisearch": config.M{
			"host": config.MustEnv("SEARCH_MEILISEARCH_URL", "http://127.0.0.1:7700"),

			// A key scoped to the actions this application performs, not the
			// master key — that one can create and drop every index on the
			// instance, which is more authority than answering a search box
			// needs.
			"key": config.MustEnv("SEARCH_MEILISEARCH_KEY", ""),
		},
	})
}
`
