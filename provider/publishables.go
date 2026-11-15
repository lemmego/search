package provider

import (
	"path/filepath"

	"github.com/lemmego/api/app"
	"github.com/lemmego/api/utils"
)

// TagConfig is what `lemmego run publish --tags` selects this module's
// configuration with.
//
// Namespaced, because --tags is a hard selector: `lemmego run publish
// --tags=config` would sweep in every other module that ever publishes one,
// and api/app/publish.go errors on a tag nothing offers — so a bare name is
// both too wide and unverifiable.
const TagConfig = "search-config"

// AddPublishables offers the configuration file to the project.
//
// Published rather than read from this package, because the engine and its
// address are the application's decision and belong somewhere an operator can
// read, edit and diff. The path comes from the project's configured
// directories rather than a hardcoded "internal/configs", so a project that
// moved them gets the file where it asked for it — which cache and queue both
// get wrong.
func (p *Provider) AddPublishables() []*app.Publishable {
	return []*app.Publishable{
		{
			FilePath: filepath.Join(utils.ConfigPath(), "search.go"),
			Content:  []byte(ConfigStub),
			Tag:      TagConfig,
		},
	}
}

// InstallInstructions is the sequence after publishing.
//
// Interpolated from the tag constant so it cannot drift from what publish
// actually accepts.
var InstallInstructions = `
Next:

  1. lemmego run publish --tags=` + TagConfig + `
  2. add &searchprovider.Provider{} to LoadProviders, after the database connector
  3. set SEARCH_ENGINE in .env — postgres needs no infrastructure you do not
     already have; meilisearch needs SEARCH_MEILISEARCH_URL and a scoped key
`
