# lemmego/search

Full-text search with a pluggable engine: Postgres `tsvector` columns an
application already has, Meilisearch, or an engine you write.

```go
import "github.com/lemmego/search"

profiles := search.Table{
    Name: "profiles", Label: "People",
    From: "users", Vector: "search_vector",
    ID: "id", Title: "name", Subtitle: "headline",
    URL: "'/@' || handle",
    Snippet: []string{"headline", "bio"},
    Visible: "deleted_at IS NULL AND profile_published",
}

results, err := search.New(db).Search(ctx,
    search.Query{Text: "postgres", Viewer: userID}, profiles, launches)
```

## Two layers, and the seam between them

**The core owns the shape of an answer.** One group per source in the order
asked, an empty query answered without touching the engine, every snippet
escaped before it is marked up, the page size clamped once. An engine cannot
opt out of any of it, because an engine never builds a `Group` or a `Hit` — it
returns `RawHit`s and the core turns them into results.

**An engine owns how documents are matched**, and declares its own descriptor
for them. The Postgres engine takes a `Table` of SQL fragments; Meilisearch
takes an index name, a filter expression and a list of filterable attributes.
`Query`, `Hit`, `Group` and `Results` carry over unchanged, which is what
makes changing engine a configuration change rather than a rewrite.

```
Finder ──> Engine ──> Source          one group per source, snippets escaped here
   │          │
   │          ├── search.Postgres     the application's own tsvector columns
   │          ├── meili.Engine        Meilisearch over net/http, no SDK
   │          └── memory.Engine       in-process, for tests and the contract
   │
   └── For[T] ──> Builder[T]          one source, filtered, hydrated into models
```

## When to use which

| | `search.Table` + the Postgres engine | `search.Indexable` + an engine with an index |
|---|---|---|
| Where the index lives | your own tables, as a generated `tsvector` column | the engine's storage, as a copy of your rows |
| What makes a row findable | writing the row | `search.Sync`, or a sync adapter on your ORM |
| Visibility rules | an arbitrary SQL predicate, joins included | a filter expression over indexed fields |
| Infrastructure | none | a Meilisearch instance that must be up at boot |

These are not alternatives you pick once. A `Table` is right for the sources
whose visibility is a live join against membership tables — the rule that no
filter dialect can express — and an `Indexable` is right for the ones that
want typo tolerance, prefix matching and ranking a database does not do. One
application can run both, because a `Source` is per engine and the finder is
not.

## Install

```bash
go get github.com/lemmego/search
```

No provider, no registration. `New` takes anything with `QueryContext` and
`QueryRowContext`, so `*sql.DB`, `*sql.Tx` and `*sql.Conn` all work and the
module depends on no ORM.

```go
search.New(app.Get[*orm.DB](a).SQLDB())   // lemmego/orm
search.New(gormDB.DB())                    // GORM
```

The root package is **the standard library and nothing else** — a dependency
test asserts it, both ways. An engine that needs a client library lives in its
own package, and `search/provider` is where the `api/app` dependency lives.
The root reaches 126 packages; the provider reaches 259.

## Four things here are not conveniences

### The query never throws

`to_tsquery` raises a **syntax error** on input like `a & | b`, so a search
box wired to it answers 500 for a string somebody can type by accident.
Everything goes through `websearch_to_tsquery`, the only one of Postgres's
four conversion functions designed for untrusted input — and it brings quoted
phrases, `or`, and `-exclusion` with it.

There is a test that throws fourteen hostile strings at a real database,
including `'; DROP TABLE x; --`, and asserts none of them errors.

### A snippet is escaped before it is marked up

`ts_headline` returns **the document** — somebody's bio, a comment, a job
description — with delimiters around the matches. Interpolating that into a
page publishes user content as markup by construction.

So the delimiters asked for are `\x02` and `\x03`, two ASCII control
characters. Neither is an HTML metacharacter, so the whole string is escaped
first and the `<mark>` elements are introduced afterwards. That ordering is
the only one that works: asking Postgres for `<mark>` directly means the
markup and the bio arrive mixed, and then there is no way to escape one
without the other.

A document that somehow contains a control character can produce at worst a
stray `<mark>`, never a tag of its author's choosing — and an unbalanced pair
is dropped so the element does not stay open across the page.

`Snippet` returns `template.HTML`. That promise is kept by that one function
and nowhere else: `Title` and `Subtitle` stay plain strings for the renderer
to escape like any other value.

**A headline with no match in it returns empty.** Postgres returns an opening
fragment when the term matched a column the snippet is not drawn from — a
title, usually — and presenting that as the matched text says "this is why it
matched" about text that is not why it matched.

### Results are grouped by source, never blended

`ts_rank` is a number about one document against one query. It is **not
comparable across documents of different shapes**: a four-word product
tagline and a three-paragraph bio that match equally well get ranks an order
of magnitude apart. Merging them produces an ordering that looks
authoritative and is arbitrary.

So `Results.Groups` is in the order the tables were passed, and a caller shows
them as sections. `Hit.Rank` is exposed because re-sorting *within* one source
is legitimate.

### Visibility is required

A `Table` with no `Visible` predicate is refused with `ErrNoVisibility` rather
than quietly searching every row.

Search is where a forgotten visibility rule surfaces, because it is the one
read path that touches every row in a table rather than the ones somebody
navigated to. The safe default is unknowable from here; the unsafe one is
"everything".

```go
Visible: "deleted_at IS NULL AND published_at IS NOT NULL",
Visible: "(is_public OR owner_id = " + search.ViewerPlaceholder + ")",
```

`{{viewer}}` (`ViewerPlaceholder`) is replaced with the right placeholder and
bound to `Query.Viewer`. A named marker rather than a fixed `$1` because
Postgres **rejects a statement that binds a parameter it never references** —
so with a fixed `$1` every viewer-independent predicate had to carry a no-op
comparison to be bindable.

It is a string rather than a structured filter because the rules that matter
are not column comparisons. "A post in a circle, unless the circle is private
and you are not in it" is an `EXISTS`, and a search that reimplemented it as
something simpler would be a second copy of a rule that is hard to get right
once.

## `Table`

| Field | |
|---|---|
| `Name`, `Label` | identify and caption the group |
| `From` | the relation, optionally with a join: `"job_posts j JOIN companies c ON …"` |
| `Vector` | the tsvector column — see below |
| `Config` | the text search configuration, default `"english"` |
| `ID`, `Title`, `Subtitle` | column expressions selected into the `Hit` |
| `URL` | an expression for the address: `"'/launch/' || slug"` |
| `Snippet` | columns `ts_headline` draws from, concatenated |
| `Visible` | **required** |
| `Tiebreak` | orders equal ranks, default `ID DESC` |

Every one of these is written by the application and interpolated into the
statement, because an identifier cannot be a bind parameter. **What somebody
typed is always a parameter.** Keeping those apart is why this is a
declarative descriptor and not a builder taking strings at request time.

**The predicate is `<Vector> @@ <query>` against the bare column**, because
that is the form the GIN index answers. Wrapping it in anything — a
`coalesce`, a cast, a `to_tsvector` of it — silently turns every search into a
sequential scan.

**A column built from more than one configuration** — `simple` for a handle
and `english` for a bio, which is reasonable — can only be queried through
one of them, so a term that stems differently in the two will not match the
half it was not parsed for. That is inherent to mixing them in one vector and
is worth knowing rather than working around.

## Prefix matching

```go
search.Query{Text: "postg", Prefix: true}   // matches "postgres"
```

Off by default, because it changes what the query means: with it, `go` matches
`golang` and `gorilla`, which is right while somebody is typing and wrong once
they have pressed enter.

Prefix needs the `:*` operator, which only `to_tsquery` understands — the one
that throws. So it is never handed anything a person typed: every rune that is
not a letter or a digit becomes a separator, and the expression is assembled
from terms this package produced, each still **bound** as a parameter. The
`:*` and the `&` are ours; the words are theirs.

## Performance

**One statement per table, plus a count.** Not a `UNION ALL`: the ranks are
not comparable so there is nothing to gain by merging in SQL, and a union of
eight differently-shaped selects has to coerce every column to a common type.
Not concurrent either — each query is an index scan answering in single-digit
milliseconds, and fanning eight of them over a pool trades a connection per
source under load, which is the resource a search page is most likely to
exhaust.

**`ts_headline` is the expensive part** — it re-parses the document. It sits
in the select list of a query that `LIMIT`s, so it runs once per returned row
and never over the matched set. Putting it in a subquery that is then ordered
and limited would compute a snippet for every match in the table to show five.

**The count is a separate query**, not a window function. `COUNT(*)` on the
same predicate is answered from the index; a window function makes every page
of a paginated search recompute the total.

**Input is bounded**: 200 bytes of query, 6 prefix terms, `MaxPerSource` of
100. A long query is always a mistake or an attack.

## `Searchable` tells two answers apart

```go
if !search.Searchable(text) { /* "type something" */ }
```

Punctuation alone parses to an empty `tsquery`, which matches nothing — and a
page that says "no results for ???" is answering a question nobody asked.
`Search` sets `Results.Empty` and runs **no queries at all** for such input,
because eight index scans for an empty box is work nobody asked for.

## Engines

```go
import _ "github.com/lemmego/search/engines"  // registers the rest

engine, err := search.Build(ctx, &search.Config{
    Engine: search.EngineMeili,
    Prefix: "myapp_prod",
    Meili:  search.MeiliConfig{Host: "http://127.0.0.1:7700", APIKey: key},
})
```

| | `postgres` | `meilisearch` | `memory` |
|---|---|---|---|
| Relational visibility | ✓ | | |
| Filter visibility | | ✓ | ✓ |
| Exact totals | ✓ | | ✓ |
| Offset paging | ✓ | ✓ | ✓ |
| Prefix | ✓ | ✓ | ✓ |
| Highlighting | ✓ | ✓ | ✓ |
| Filters | ✓ | ✓ | ✓ |
| Asynchronous | | ✓ | |
| Keeps an index | | ✓ | ✓ |

Read these from `Engine.Capabilities()` rather than from this table. The
contract is that an engine reporting a capability false **refuses** a request
that needs it: a lock that pretends to have been acquired is worse than one
that refuses, and a search that silently dropped a visibility rule is worse
than a search that failed. The conformance suite checks both directions, so a
capability cannot be decorative.

The **postgres engine is in the root package**, because it needs nothing but
`database/sql` and so costs a consumer nothing to link. Everything that needs
a network client registers itself from its own package, the way `database/sql`
drivers and `cache/store/*` do.

`Prefix` namespaces every index name an engine writes or reads, and it is what
bounds a flush. Without one, `search:flush posts` run from a laptop empties
production's posts index.

## The typed builder

The grouped API answers "what is on this site about X" across every source at
once, which is what a search box needs. `For` answers "which of these does X"
about one collection, hydrated into the model you already have.

```go
posts, err := search.For[*Post](engine, postsIndex, "postgres").
    Where("status", "published").
    WhereGte("published_at", cutoff).
    Take(20).
    Hydrate(search.HydratorFunc[*Post](repo.LoadByKeys)).
    Get(ctx)
```

Terminals: `Keys` (no models loaded), `Raw` (the group, snippets and totals,
straight from the index), `Get`, and `Each`, which pages by whichever
mechanism the engine supports so you write no paging.

**`Get` restores the engine's order after hydration, and that is the whole
reason it exists rather than two lines at a call site.** A hydrator loading by
key issues something shaped like `WHERE key IN (...)`, and a database returns
those rows in whatever order its plan produces — so hydrating without
re-sorting discards the ranking the search was for. It is a quiet bug: every
row is correct, only the order is wrong, and nothing fails. A key with no row
is dropped rather than returned as a zero value, because a document can
outlive its row.

Filters are **ANDed, with no grouping, no OR and no nesting**, and that
refusal is written down rather than left as a gap. An engine-portable boolean
tree is a filter DSL, and the rules that need one — "a post in a circle,
unless the circle is private and you are not in it" — are exactly the rules no
DSL can express. Those belong in `Visibility`, where each engine gets the
dialect it can actually evaluate.

A filter is checked against the engine's capabilities **before any query
runs**, so a restriction an engine would have ignored is an error naming the
engine rather than a page of results from a query nobody asked for. A field
name goes through `search.ValidField` because an identifier cannot be a bind
parameter: it is the one part of a filter that is interpolated, and refusing
anything that is not a plain name is what makes that safe.

## Indexing

An engine with its own index needs documents put into it.

```go
func (p *Post) SearchIndex() string  { return "posts" }
func (p *Post) SearchKey() string    { return strconv.FormatInt(p.ID, 10) }
func (p *Post) SearchDocument() map[string]any {
    return map[string]any{"title": p.Title, "body": p.Body, "status": p.Status}
}

// Optional: indexed only sometimes.
func (p *Post) ShouldBeSearchable() bool { return p.Status == "published" }

err := search.Sync(ctx, engine, post)     // after the transaction commits
err = search.Forget(ctx, engine, post)
```

Only what search needs goes in `SearchDocument`. Every field there leaves the
database, lands in another system's storage, and comes back to whoever can
reach that system — so a document carrying a password hash because the struct
had one is a disclosure with no request behind it.

**A `ShouldBeSearchable` returning false triggers an unindex, not a skip.**
That is one rule where Scout needs three — it has `shouldBeSearchable`,
`wasSearchableBeforeUpdate` and `wasSearchableBeforeDelete` — and it is
correct for the create case too, because unindexing an absent document is not
an error. Without it, a post that is unpublished stays findable forever.

**Call `Sync` after the transaction commits, never inside it.** Inside, an
index write lands before the row is visible, and if the transaction then rolls
back the document describes a row that does not exist. No ordering guarantee
inside `Sync` can save a caller from that — which is also why the three ORM
adapters below are not all equivalent.

A document key is **alphanumerics, hyphen and underscore** — Meilisearch's
charset, enforced by every engine including Postgres, which has no such
restriction. Making one engine stricter than it needs to be is the deliberate
part: portability becomes a property a test can check rather than a hope, and
a model keyed `tenant:42/post:7` finds out on the first import instead of on
the day somebody tries to move engines. It validates and never transforms,
because a rewritten key makes `Hit.Key` differ from `SearchKey()` and then a
caller holding a hit cannot look the row up.

## Staying in sync with an ORM

Three adapters, one per ORM, each in its own package so a consumer links only
the one it uses — `./syncorm` reaches 136 packages, `./syncgorm` 158,
`./syncbun` 156, and the root still 126. A dependency test asserts each
carries exactly one ORM and that the root carries none.

```go
syncorm.Watch(db, engine)     // lemmego/orm
syncgorm.Watch(gormDB, engine)
syncbun.Watch(bunDB, engine)
```

That is the whole wiring. From then on every write to a model implementing
`search.Indexable` keeps its document current, and every write to a model
that does not is ignored — which matters, because a global hook sees every
write in an application and most models are not searchable.

**`syncorm` is the only one of the three with nothing to apologise for.**
`orm.Observe` fires *after* the transaction commits, so a rolled-back write
announces nothing and a subscriber told to refetch can see the row. GORM's
callbacks and Bun's query hooks both fire *inside* the transaction, and
neither has an after-commit seam — so for those two:

- a rolled-back write has already been indexed, and the document then
  describes a row that does not exist;
- `WithDefer` hands each index write to the application's own after-commit
  mechanism, which turns the adapter into a change detector and makes the
  problem solvable;
- or call `search.Sync` from the service layer after your own commit, which
  needs no adapter at all and is the path recommended above.

Each of those adapters has a test asserting the **known-dirty state** rather
than a comment claiming it. The day GORM or Bun grows an after-commit hook,
that test fails and names the thing to change.

Three further limits, each with a test rather than a sentence: a conditional
statement (`Where(...).Delete(&Post{})`, `Model((*Post)(nil))`) carries no
identifiable row and is ignored — acting on the zero model would unindex
whatever document is keyed `"0"`, or write an empty one; a failed statement
never reaches the index, because the hooks are registered after rather than
before; and a `SELECT` never writes to it, which is not automatic for Bun —
its hook sees every query and carries the model a read was scanned into.

## The provider and its commands

```go
// bootstrap/providers.go
&searchprovider.Provider{
    Importables: []search.Importable{repos.Posts(db), repos.People(db)},
}
```

```bash
lemmego run publish --tags=search-config
lemmego run search:import --batch 500
lemmego run search:import --index posts --after key0004210   # resume
lemmego run search:flush posts
```

The engine is taken by **injection**, not by name alone, and that is forced
rather than stylistic: the Postgres engine needs a `*sql.DB` that no
configuration string can express. So `Engine`, `EngineFor` and `Config` are
three ways in, in that order of priority, and the Postgres handle comes from
the container because the engine searches the application's own tables and
must use the application's own pool.

A configured backend that cannot be reached **fails the boot**. There is no
lenient mode: a cache that keeps nothing is slow but still correct, whereas a
search that finds nothing looks like an empty database and is the kind of
outage somebody notices a week later.

`Importables` are listed by the application rather than discovered. Go has no
equivalent of Laravel's model discovery, and a reflective registry that
silently missed a model would be worse than a list — the symptom is one
collection quietly absent from search, which nothing fails on.

Both commands refuse against an engine with no index of its own, naming the
engine and why, rather than being hidden: a command that is absent reads as a
version problem.

**There is no `--prune`.** Removing documents whose rows are gone needs an
operation to list what is in the index, which the `Engine` contract does not
have. `--fresh` is the answer, and the cost is that the index is empty while
the walk runs. A model that stops being searchable is handled where it
belongs instead — `ShouldBeSearchable` plus a sync adapter unindexes it at the
write.

## Writing an engine

`search/enginetest` is the contract, exported so an engine written outside
this module proves itself against the same one the built-in engines are held
to.

```go
func TestMyEngineConformsToTheEngineContract(t *testing.T) {
    enginetest.Run(t, func(t *testing.T) enginetest.Harness {
        return enginetest.Harness{Engine: …, Put: …, Source: …, Caps: …}
    })
}
```

Twenty-six cases over a fixed corpus, capability-gated so the ones that do not
apply skip rather than fail. It was written **before** the Meilisearch engine,
deliberately: a seam fitted against one implementation is fitted by accident,
and the contract passing against an engine that did not exist when it was
written is the evidence that it describes something general.

The suite's own tests introduce seven plausible defects and assert the suite
fails at the named case for each, because a conformance suite that cannot fail
is the most expensive kind of test — every engine passes it, so it reads as
evidence and is none.

## What this does not do

No synonyms, no spelling correction, no faceting, no aggregations. No queued
indexing: `search.Sync` from a service is already off the request path when
the caller wants it to be. No soft-delete index flag — the Postgres engine
gets that free from `Visible`. No `OR` in filters, for the reason above. No
Algolia or Typesense driver: the contract has been fitted against two real
engines, which is the bar, and a third proves nothing new until somebody
deploys it.

## Testing

```bash
go test ./...                                      # query building and escaping
SEARCH_POSTGRES_DSN="host=localhost dbname=postgres sslmode=disable" \
SEARCH_MEILISEARCH_URL="http://127.0.0.1:7700" \
  go test ./...                                    # and both real engines
```

```bash
docker run -p 7700:7700 getmeili/meilisearch:v1.11
```

`syncorm` needs `lemmego/orm` at a version carrying `orm.Observe`, so
`GOWORK=off go build ./syncorm` fails against anything older — which is the
honest signal that the release order is forced: `orm` first, then this. The
other packages build standalone either way.

Both engines' tests **skip** without their service rather than fail, and that
is a trap worth naming: a suite that skips exits 0, which is indistinguishable
from one that passed. The giveaway is the runtime — about a second against
about fifteen — and CI asserts that neither conformance suite was skipped
rather than trusting the exit code. They are not optional for a change here:
the unit tests never execute the generated SQL or reach a real instance, so a
syntax error in a statement and a wrong filter dialect both pass every one of
them.
