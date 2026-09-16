# Turn context bundles

`internal/bundle` assembles the project context a turn receives. Callers depend
on the `bundle.Provider` interface:

```go
type Provider interface {
	Mode(config.ProjectID) Mode
	Assemble(context.Context, config.ProjectID, Scope) (Bundle, error)
}
```

`bundle.Files` is the only provider. It reads the project's trace directory and
nothing else, and reports mode `file`. File-based context is a supported
operating mode, not a degraded one. `Service.Context()` returns a provider bound
to the active project's open trace; other projects are refused.

## Scope

`Scope.Entities` names footprint entities by ID or alias. An empty list means the
whole project. `Scope.Workstream` limits decisions to that workstream; an unknown
workstream is an error.

## Contents

Assembly reads everything at call time and keeps nothing between calls, so an
edit to the charter or the knowledge base is used by the next bundle without a
reload.

| Section | Source | Contents |
| --- | --- | --- |
| Charter | `charter.md`, record `charter` | Latest recorded revision, its rules as `charter#<n>` with their headings, and numbering diagnostics. The charter is read through `trace.Repository.Charter`, which first records an unrecorded owner edit. |
| Knowledge base | `kb/<subsystem>.md` | Without a scope, every `kb/*.md` file. With a scope, the files named after each resolved entity or one of its `part_of` ancestors. |
| Missing prose | | Each named scope entity for which no file exists along its own lineage, with the paths looked for. A missing file is not an error. |
| Entities | `kb/entities.json`, record `kb-entities` | Latest recorded map revision. Without a scope, every entity; with a scope, the entities `ResolveEntities` returns, including their parts, with their path patterns. Scope names that resolve to nothing are listed as unresolved. |
| Decisions | `workstreams/<id>/questions/<q>/rulings.jsonl` | The latest revision of each ruling in the workstream, or in every workstream of the project without one, with its record ID, revision and question. An empty set is valid. |

Knowledge-base files are ordered by subsystem name, entities by ID, and decisions
by workstream, time and record ID. The same files and records always produce the
same bundle.

A symlinked or otherwise irregular knowledge-base file, damaged trace history or
an unreadable charter fails assembly.

## Rendering

`Bundle.Render` returns the text form placed in a turn request's prompt. Each
section header names the path and record it came from, so an agent can cite
`charter#2`, `kb/internal.md` or a ruling's record and revision. Knowledge-base
prose is included verbatim between `### kb/<subsystem>.md` and
`### end of kb/<subsystem>.md` lines.

## Status

`GET /v1/runtime` lists each active project with `context_mode`, and
`osmia status` prints it as `Context: <project> context_mode=file`.
