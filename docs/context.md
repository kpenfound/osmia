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
| Decisions | `workstreams/<id>/questions/<q>/rulings.jsonl` | The latest revision of each ruling in the workstream, or in every workstream of the project without one, with its record ID, revision and question. An owner ruling shows what the owner said; one the chief of staff has not relayed shows `answer: waiting for the chief of staff to relay the ruling`. An empty set is valid. |
| Notices | `workstreams/<id>/questions/<q>/rulings.jsonl` | Every owner ruling of the project that the chief of staff relayed with scope `notify`, from every workstream, whatever the bundle's workstream and entity scope: its source, record ID, revision, workstream, the owner's exact response and the chief of staff's relay. A ruling relayed to a batch of questions is one notice, sourced from the batch's first ruling in decision order. A ruling relayed with scope `local` is no notice. An empty set is valid. |
| Charter notices | `workstreams/<id>/questions/<q>/charter.json` | Every [charter proposal](trace.md#charter-proposals) of the project that the owner ratified and the charter records (`chartered`), from every workstream, whatever the bundle's scope: its source, record ID, revision, workstream, the rule, the number it took, the `charter.md` revision that holds it, the ruling it came from and the owner's words in it. A proposal waiting, declined, or ratified but not yet in the charter is no notice. An empty set is valid. |

Knowledge-base files are ordered by subsystem name, entities by ID, and decisions
and notices by workstream, time and record ID, and charter notices by when the
charter recorded them. The same files and records always produce the
same bundle.

A symlinked or otherwise irregular `kb/<subsystem>.md` entry the bundle would
include, damaged trace history or an unreadable charter fails assembly, with or
without a scope.

## Rendering

`Bundle.Render` returns the text form a turn request carries. An owner message
to the chief of staff carries it in the request's system prompt, and so does
every [event turn](service.md#event-delivery); see
[conversation](service.md#conversation). Each
section header names the path and record it came from, so an agent can cite
`charter#2`, `kb/internal.md` or, by its record ID, a ruling as
`ruling#<record>`; the [answer tool](trace.md#questions) checks such
citations. Knowledge-base
prose is included verbatim between a
`### kb/<subsystem>.md (subsystem <subsystem>)` line and a
`### end of kb/<subsystem>.md` line; a newline is added when the prose does not
end with one.

Each notice uses the same attributed envelope as an answer turn: an
`owner_response` section copied by Osmia and a `returned_answer` section
attributed to the chief of staff. Every content line is prefixed with `| `,
and the header records the original byte count. Backslashes and carriage
returns in content are escaped. Content that resembles a delimiter or an
attribution line remains inside its section. Charter notices follow the
ruling notices under the same heading, each with the owner's words as
`owner_response` and the ratified rule as a `charter_rule` section attributed
to the chief of staff's proposal, ratified by the owner. `No project-wide
notices.` stands for neither kind.

## Status

`GET /v1/runtime` lists each active project with `context_mode`, and
`osmia status` prints it as `Context: <project> context_mode=file`. Each
[workstream status](service.md#workstream-status) carries the same
`context_mode`.
