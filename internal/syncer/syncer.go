// Package syncer holds the half of an ORM sync adapter that is the same for
// every ORM.
//
// Internal, because it is not a contract anybody should depend on: the three
// adapters differ in their hook signature and in how they name an operation,
// and what is left over is the error policy, the deferral seam and the
// decision to ignore a model that is not indexable. Three copies of that
// would drift, and exporting it would invite a fourth adapter to depend on
// shape rather than on search.Sync.
package syncer

import (
	"context"
	"log/slog"

	"github.com/lemmego/search"
)

// Syncer applies one row's change to an engine.
type Syncer struct {
	Engine search.Engine

	// OnError is told about every failure. Never nil once New has run.
	//
	// An ORM hook cannot return an error to the caller — or rather, in two of
	// the three it can and doing so would fail the write, which is the wrong
	// trade: a search index that is briefly stale is a worse page, and a
	// refused write is lost data. So failures are reported sideways, and the
	// default logs rather than swallowing, because an index that quietly
	// stopped updating is the hardest kind of outage to account for.
	OnError func(ctx context.Context, err error)

	// Defer, when set, is handed the work instead of it being done now.
	//
	// It exists for the two adapters whose hooks fire *inside* the
	// transaction. An application with its own after-commit mechanism routes
	// the closure through it; one without leaves this nil and accepts that a
	// rolled-back write leaves the index describing a row that does not
	// exist.
	Defer func(ctx context.Context, apply func(context.Context) error)
}

// Index syncs the models that are indexable and ignores the rest.
func (s Syncer) Index(ctx context.Context, models ...any) {
	s.run(ctx, func(ctx context.Context) error {
		return search.Sync(ctx, s.Engine, models...)
	}, models)
}

// Remove unindexes the models that are indexable and ignores the rest.
func (s Syncer) Remove(ctx context.Context, models ...any) {
	s.run(ctx, func(ctx context.Context) error {
		return search.Forget(ctx, s.Engine, models...)
	}, models)
}

// run applies or defers, and never calls the engine for a batch holding
// nothing indexable.
//
// That last check is not an optimisation. A global ORM observer sees every
// write in an application and most models are not searchable, so without it
// every unrelated INSERT would reach the engine with an empty batch — and
// against an engine with no index at all, every one would report
// ErrUnsupported through OnError and bury the real failures.
func (s Syncer) run(ctx context.Context, apply func(context.Context) error, models []any) {
	if !anyIndexable(models) {
		return
	}
	if s.Defer != nil {
		s.Defer(ctx, apply)
		return
	}
	if err := apply(ctx); err != nil {
		s.OnError(ctx, err)
	}
}

func anyIndexable(models []any) bool {
	for _, m := range models {
		if _, ok := m.(search.Indexable); ok {
			return true
		}
	}
	return false
}

// LogErrors is the default OnError.
func LogErrors(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "search: keeping the index in step with the database failed",
		"error", err)
}
