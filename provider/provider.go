// Package provider wires search into a Lemmego application.
//
// It is a subpackage rather than part of search, for the reason lemmego/log's
// 289 modules recorded: importing api/app pulls in api's whole dependency
// closure, so a root package that offered the provider would charge every
// consumer for it. search's root is the standard library and nothing else,
// and deps_test.go asserts both halves of that — this package does reach
// api/app, and the root does not.
//
//	bootstrap/providers.go
//
//	&searchprovider.Provider{
//	    Importables: []search.Importable{repos.Posts(db), repos.People(db)},
//	}
//
// With no Engine and no EngineFor, the configuration published as
// internal/configs/search.go selects and configures one — and the Postgres
// handle comes from the container, because the engine searches the
// application's own tables and so must use the application's own pool.
package provider

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lemmego/api/app"
	"github.com/lemmego/api/config"

	"github.com/lemmego/search"
)

// Provider builds the engine, registers the finder, and contributes the
// import and flush commands.
type Provider struct {
	// Engine is an engine the application built itself.
	//
	// Injection rather than a name in a configuration file, because the
	// Postgres engine needs a *sql.DB that no configuration string can
	// express — which is why this module does not follow cache's registry
	// shape all the way. The registry is still there, under Config, for the
	// engines that can be described in text.
	Engine search.Engine

	// EngineFor resolves the engine from the container.
	//
	// A function rather than a value for the reason auth's SocialStore
	// records: a provider list is written before the container exists. It is
	// called once during Provide and an error fails the boot, which is the
	// point — an engine that was asked for and cannot be built must not
	// quietly become a different one.
	//
	// Takes precedence over Engine when both are set.
	EngineFor func(a app.App) (search.Engine, error)

	// Config configures the engine when neither Engine nor EngineFor is set.
	//
	// Layered under the application's own `search` configuration block, which
	// is where the published stub writes the engine name, the prefix and the
	// Meilisearch address — so a deployment changes engine by changing an
	// environment variable and not by editing Go.
	Config *search.Config

	// DB supplies the pool for the postgres engine.
	//
	// Needed only when the application's database handle is not in the
	// container as a *sql.DB or a search.Querier, which is the case for an
	// ORM that keeps its pool to itself.
	DB func(a app.App) (search.Querier, error)

	// Importables is what search:import walks, one per index.
	//
	// Listed by the application rather than discovered. Go has no equivalent
	// of Laravel's model discovery, and a reflective registry that silently
	// missed a model would be worse than a list: the symptom is one
	// collection quietly absent from search, which nothing fails on.
	Importables []search.Importable

	engine search.Engine
	finder *search.Finder
}

var (
	_ app.Provider            = (*Provider)(nil)
	_ app.CommandProvider     = (*Provider)(nil)
	_ app.PublishableProvider = (*Provider)(nil)
	_ app.ShutdownProvider    = (*Provider)(nil)
)

// Provide builds the engine and registers it.
//
// A backend that was configured and cannot be reached fails the boot. There
// is deliberately no lenient mode, for Build's reason: a cache that keeps
// nothing is slow but correct, whereas a search that finds nothing looks like
// an empty database and is the kind of outage somebody notices a week later.
func (p *Provider) Provide(a app.App) error {
	engine, err := p.resolve(a)
	if err != nil {
		return err
	}

	// Reachability checked here rather than on the first search, so a wrong
	// address is a failed boot instead of a search box that answers nothing.
	if pinger, ok := engine.(interface{ Ping(context.Context) error }); ok {
		if err := pinger.Ping(context.Background()); err != nil {
			return fmt.Errorf("search: the %s engine is configured but "+
				"unreachable: %w", engine.Name(), err)
		}
	}

	p.engine = engine
	p.finder = search.NewFinder(engine)

	// Both, under the concrete type a handler resolves and under the
	// interface another module would depend on. Registering only the
	// concrete type would make this module's own seam unreachable from
	// anything that does not import it.
	a.Services().Register[*search.Finder](p.finder)
	a.Services().Register[search.Engine](engine)
	return nil
}

// resolve settles which engine to use, in order of how explicit the answer is.
func (p *Provider) resolve(a app.App) (search.Engine, error) {
	if p.EngineFor != nil {
		engine, err := p.EngineFor(a)
		if err != nil {
			return nil, fmt.Errorf("search: EngineFor: %w", err)
		}
		if engine == nil {
			return nil, fmt.Errorf("search: EngineFor returned no engine and no error")
		}
		return engine, nil
	}
	if p.Engine != nil {
		return p.Engine, nil
	}

	cfg := p.resolveConfig(a)

	// The handle is fetched for the postgres engine and only for it, so an
	// application running Meilisearch is not required to have a SQL pool in
	// the container to boot.
	if cfg.Engine == "" || cfg.Engine == search.EnginePostgres {
		if cfg.Postgres.DB == nil {
			db, err := p.querier(a)
			if err != nil {
				return nil, err
			}
			cfg.Postgres.DB = db
		}
	}

	return search.Build(context.Background(), cfg)
}

// resolveConfig layers the provider's own Config over the application's
// `search` block, in that order of increasing priority.
func (p *Provider) resolveConfig(a app.App) *search.Config {
	cfg := &search.Config{}
	if p.Config != nil {
		copied := *p.Config
		cfg = &copied
	}

	m := a.Config().Get("search")
	block, ok := m.(config.M)
	if !ok {
		return cfg
	}

	// Each key read only when present, so a provider field is not overwritten
	// by a configuration file that does not mention it — and read directly
	// rather than through config.MustEnv, which treats a present-and-empty
	// variable as a decision and panics on a value it cannot parse. Both of
	// those have bitten this framework: api/logger and mail each worked
	// around them, and `SEARCH_ENGINE=` choosing nothing would be a boot
	// failure explained by nothing.
	if v, ok := block["engine"].(string); ok && v != "" {
		cfg.Engine = v
	}
	if v, ok := block["prefix"].(string); ok && v != "" {
		cfg.Prefix = v
	}
	if meili, ok := block["meilisearch"].(config.M); ok {
		if v, ok := meili["host"].(string); ok && v != "" {
			cfg.Meili.Host = v
		}
		if v, ok := meili["key"].(string); ok && v != "" {
			cfg.Meili.APIKey = v
		}
	}
	return cfg
}

// querier finds the application's database handle.
func (p *Provider) querier(a app.App) (search.Querier, error) {
	if p.DB != nil {
		db, err := p.DB(a)
		if err != nil {
			return nil, fmt.Errorf("search: DB: %w", err)
		}
		if db == nil {
			return nil, fmt.Errorf("search: DB returned no handle and no error")
		}
		return db, nil
	}
	if db, ok := app.Lookup[*sql.DB](a); ok {
		return db, nil
	}
	if db, ok := app.Lookup[search.Querier](a); ok {
		return db, nil
	}
	return nil, fmt.Errorf("search: the postgres engine needs the application's " +
		"own pool and nothing in the container supplies one. Register a *sql.DB, " +
		"or give the provider a DB function — for lemmego/orm that is " +
		"func(a app.App) (search.Querier, error) { return app.Get[*orm.DB](a).SQLDB(), nil }")
}

// Finder returns the finder this provider built, or nil before Provide runs.
//
// There is deliberately no Engine accessor: the field of that name is the
// injection point, and Finder().Engine() already reaches the built engine.
func (p *Provider) Finder() *search.Finder { return p.finder }

// FromApp resolves the finder from the service container.
//
// Panics when nothing is registered, which is app.Get's behaviour and the
// right one here: a page that searches cannot render without it, so the
// alternative is a handler returning no results and looking like an empty
// database. A caller that genuinely wants to degrade uses app.Lookup.
func FromApp(a app.App) *search.Finder { return app.Get[*search.Finder](a) }

// Shutdown releases whatever the engine holds.
//
// The assertion rather than a Close on the Engine contract, because most
// engines have nothing to close: the postgres engine borrows the
// application's pool and closing it here would take the whole application's
// database with it.
func (p *Provider) Shutdown(context.Context) error {
	if closer, ok := p.engine.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}
