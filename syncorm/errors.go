package syncorm

import (
	"errors"
	"fmt"

	"github.com/lemmego/search"
)

var errNilArgument = errors.New("syncorm: Watch needs a database and an engine")

func indexlessError(engine search.Engine) error {
	return fmt.Errorf("%w: the %s engine keeps no index of its own, so there is "+
		"nothing to keep in step — it searches your rows where they are, and a "+
		"migration is what makes them searchable",
		search.ErrUnsupported, engine.Name())
}
