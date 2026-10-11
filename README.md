# lemmego/search

Full-text search over Postgres `tsvector` columns an application already has.

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

## What it is, and is not

**Not an indexing engine.** An application that added generated `tsvector`
columns and GIN indexes alongside the migrations that created the tables —
which is cheap up front and an expensive retrofit later — already has an
index. What it lacks is a safe way to turn what somebody typed into a query
against it, and a shape for the answer.

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

## Costs, and where they are

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

## Deliberate omissions

No synonyms, no spelling correction, no faceting, no aggregations, no
incremental indexing, and no second engine. `Searcher` is an interface so a
Meilisearch implementation can replace this one; the `Table` descriptors are
Postgres-shaped, which is the honest seam — that implementation would take its
own descriptors, and `Query`, `Hit`, `Group` and `Results` would carry over
unchanged.

## Testing

```bash
go test ./...                                      # query building and escaping
SEARCH_POSTGRES_DSN="host=localhost dbname=postgres sslmode=disable" \
  go test ./...                                    # and the SQL
```

The Postgres tests **skip** without a DSN rather than fail. They are not
optional for a change to this package: the unit tests never execute the
generated SQL, so a syntax error in a statement passes every one of them.
