# Knowledge base

`internal/kb` owns the local knowledge base of a project trace: the entity map
in `kb/entities.json`, the map from how people talk about the code to where it
is, and the prose the librarian writes per subsystem in `kb/<subsystem>.md`.
Footprints resolve through the entity map alone, with or without Hearsay. The
[extraction](#extraction) pass writes both.

## Schema

```json
{
  "version": 1,
  "entities": [
    {
      "id": "internal.trace",
      "name": "internal/trace",
      "aliases": ["trace"],
      "paths": ["internal/trace"],
      "owners": ["@org/core"],
      "part_of": ["internal"]
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `version` | Always `1`. |
| `id` | Stable identifier: lowercase letters, digits, `.` and `-`. |
| `name` | One-line display name. |
| `aliases` | Other names for the entity. |
| `paths` | Repository-relative glob patterns. The first is the primary path. |
| `owners` | Owners as CODEOWNERS writes them (`@user`, `@org/team`, email). |
| `part_of` | IDs of the entities this one belongs to. |

`kb.Parse` and `kb.LoadFile` read the file. Empty content, `{}` and a missing
file are an empty version-1 map. Unknown fields are rejected.

### Path patterns

A pattern is a list of `/`-separated segments. Each segment is a
[`path.Match`](https://pkg.go.dev/path#Match) pattern, or `**` for any number
of segments. A pattern matches a path when it matches the path itself or one of
its parent directories, so `internal/trace` covers `internal/trace/git.go`.
Patterns must not be empty, absolute, or contain empty, `.` or `..` segments.

### Validation

`kb.Validate` returns every problem at once, each as a `kb.Problem` naming the
entity:

- a malformed or duplicate ID;
- a missing name, or a blank alias or owner;
- an alias that matches another entity's ID or alias, ignoring case;
- a `part_of` target that does not exist;
- a `part_of` cycle, reported at its smallest ID;
- a pattern that is invalid, absolute or escapes the repository.

`kb.Encode` refuses an invalid map. Otherwise it writes the canonical form:
fields in schema order, entities sorted by ID, `aliases`, `owners` and `part_of`
sorted, absent lists as `[]`, two-space indentation and a trailing newline.
`paths` keeps its order because the first entry is the primary path.

## Seeding

`kb.Seed(clone)` builds a map from the clone, without any agent. The clone is
opened through `os.Root` and only read. Symlinks are never followed out of the
clone.

1. **CODEOWNERS.** The first regular file among `.github/CODEOWNERS`,
   `CODEOWNERS` and `docs/CODEOWNERS` is read, the order GitHub uses. Each rule
   becomes an entity path pattern. A pattern with a leading or inner `/` is
   anchored at the root; any other pattern gets a `**/` prefix. A trailing `/`
   or `/**` is dropped. A rule whose pattern cannot be expressed is skipped.
   Because a pattern covers everything below what it matches, `docs/*` also
   covers files in subdirectories of `docs`. For a footprint, that errs on the
   wide side.
2. **Structure.** Every visible top-level directory becomes an entity. So does
   every visible directory directly below one of the containers `apps`, `cmd`,
   `crates`, `internal`, `libs`, `packages`, `pkg` and `services`. Hidden
   directories (including `.git`), symlinks and names containing glob
   characters are skipped.
3. **Literal rules.** An anchored CODEOWNERS rule (one with a leading or inner
   `/`) without wildcards that names an existing, visible path adds that path
   as an entity. Unanchored rules only set owners.

For each entity:

- `name` and the only `paths` entry are its path.
- `owners` come from the last CODEOWNERS rule that matches the path, as on
  GitHub. A matching rule without owners leaves the entity unowned.
- `part_of` is the nearest entity whose path is a parent directory.
- A nested entity gets its last path segment as an alias. The alias is dropped
  when another nested entity has the same last segment (ignoring case) or when
  it equals an ID.

The same files always produce byte-identical `kb.Encode` output.

### ID rule

An entity's ID is derived from its primary path: lowercase it, turn `/` into
`.`, and replace every other character outside `[a-z0-9.-]` with `-`. For
example, `internal/trace` becomes `internal.trace` and `Docs/API_v2` becomes
`docs.api-v2`. When two paths derive the same ID, the one that sorts first
keeps it and the others get `-2`, `-3` and so on, in path order.

A path whose derived ID would not start with a letter or digit, such as
`_site`, `@types` or `élan`, gets no entity in the seed, and paths below it
resolve as unresolved until someone adds an entity for it by hand. A nested
path such as `packages/@types` is kept, because its ID (`packages.-types`)
starts with the parent's name.

`kb.Merge(existing, seed)` regenerates a map without changing identities.

- An existing entity whose primary path the seed also produces is kept exactly
  as it is, including a hand-edited ID, name, aliases, owners and `part_of`.
- A seeded entity with a new primary path is added. If its ID is already in use
  it gets the next free `-N` suffix. Its `part_of` edges point at the IDs the
  merged map uses. Any alias that would collide is dropped.
- Existing entities the seed no longer produces are kept.

An entity that still exists therefore keeps its ID across regenerations. Seed
suffixes depend on which colliding paths exist, so regenerate through `Merge`
rather than by replacing the map with a fresh seed.

## Resolution

`Map.ResolveEntities(names)` accepts IDs or aliases, ignoring case. It returns
the named entities, every entity that is transitively `part_of` one of them, and
all their path patterns, sorted.

`Map.ResolvePaths(paths)` returns, for each repository path, the entities whose
patterns match it most specifically. Specificity is the length of a pattern's
literal prefix (the text before its first `*`, `?`, `[` or `\`), and an entity
scores its most specific matching pattern. Every entity with the top score is
returned.

`PatternsOverlap(a, b)` reports whether two valid patterns can cover the same
path, including a file below a matched directory or a path admitted by both
globs. The plan's start decision uses it after resolving unit footprints.

Both calls return an explicit `Unresolved` list, in input order, and never drop
an input. It holds:

- names that match no entity, or that resolve to no path pattern at all;
- paths that are empty, `.`, absolute, unclean or escape the repository, or
  that no pattern matches.

## Storage

The map is a project `Document` in the trace with record ID `kb-entities` and
path `kb/entities.json` (`trace.EntitiesDocument` and `trace.EntitiesPath`).

- `trace.Create` writes `{}` and records no revision.
- `trace.CreateSeeded` records the given map as revision 1 in the trace's first
  commit.
- `kb.Store` validates and encodes a map before anything is written, then
  appends it as the next revision.
- `kb.Load` returns the latest recorded revision, or an empty map when none
  exists. `kb.LoadRevision` also returns that revision number, 0 when none
  exists.

Subsystem prose lives beside the map as `kb/<subsystem>.md`.
`trace.Repository.Prose` reads one file as it is on disk and
`trace.Repository.Subsystems` lists them; [context bundles](context.md) use both.

`osmia project add` seeds the map from a temporary copy of the clone's tracked
files, the same input the librarian's `seed/entities.json` comes from, and
creates the trace with `CreateSeeded`. A registration that is interrupted before the trace's first
commit seeds again when it is finished.

## Extraction

The librarian's extraction pass writes the knowledge base from the clone:
prose per subsystem and a refined entity map. `osmia project add` requests
the first pass; `osmia project extract <project-id>` (API:
`POST /v1/projects/extract`) requests another. Each pass is a durable
`kb-extract` operation in the project's librarian workstream, a workstream
whose ID is derived from the project ID and which holds the `agent_librarian`
thread. That workstream carries no feature: workstream status does not list
it, and `osmia status <workstream-id>` does not find it. The service's
reconciliation loop runs the operation; the request returns as soon as it is
recorded, and `osmia status` follows it.

### The turn

A pass is one turn of the librarian thread, run through the thread runner and
the [turn isolation](isolation.md) path. The service stages a workspace under
`<root>/librarian/<project-id>/<turn>/workspace` and gives the turn a private
copy of it:

| Path in the view | Content |
| --- | --- |
| `repo/` | The clone's tracked regular files, listed with `git ls-files`. Untracked files, symlinks and `.git` are never copied, and the clone is never written. Changes the librarian makes there are discarded. |
| `kb/` | The current knowledge base: the latest recorded `entities.json` and every recorded `<subsystem>.md`. |
| `seed/entities.json` | `kb.Seed` of `repo/`, canonically encoded: untracked directories of the clone seed nothing here. |
| `output/` | Empty. The librarian writes the complete knowledge base here. |

The librarian gets `file_read`, `file_write`, `notes_read` and `notes_write`
and nothing else: no execute, network or VCS capability. The prompt asks for
`output/kb/<subsystem>.md` per subsystem (how it is built, the tests that
matter, what breaks when you touch what, and the decisions behind it) and
`output/kb/entities.json`, the entity map refined from the seed with stable
IDs, and says that `CLAUDE.md`, `AGENTS.md` and `CONTRIBUTING.md` are inputs
it must not repeat. The turn's profile is the librarian's effective binding at
the time the turn is accepted; its sandbox settings come from the role's
configuration and must name a `container`.

### Output and recording

When the turn ends, the service copies `output/` from the view to
`<root>/librarian/<project-id>/<turn>/output` and validates it with
`kb.ReadOutput`. Subsystem names match `^[a-z0-9][a-z0-9-]*$` and are at most
64 characters, every prose file lies directly in `output/kb/`, prose is not
empty, `output/kb/entities.json` is present and passes `kb.Validate`, and
nothing else is in `output/`. Every problem is reported at once.

Valid output is recorded with `trace.RecordDocuments` as one commit of
librarian-authored `Document` revisions (actor `agent`/`agent_librarian`,
cause the operation ID): a revision of `subsystem-<name>` at
`kb/<subsystem>.md` for every produced subsystem, an empty revision for every
recorded subsystem the pass no longer produced, which deletes its file, and a
revision of `kb-entities` in canonical form. Either every file lands or none
does, and the files under `kb/` match the latest pass while history keeps
every earlier revision. The write is keyed by the operation ID: a retry after
an interruption finds the recorded revisions and records nothing again.

Invalid output, a failed turn and a service without a librarian runner are
recorded as a failed extraction with the reason; the previous knowledge base
stays in place. A service stop during the turn leaves it interrupted; the next
start abandons that turn and starts another, up to three per extraction, after
which the extraction fails with the count. Storage failures leave the
operation pending for the reconciliation loop to retry.

### Status and re-runs

`osmia status` shows the latest extraction: its number, `pending`, `running`,
`succeeded` or `failed`, the time of its last recorded activity and the reason
when it failed or is waiting to retry. A new extraction is refused while the
latest is pending or running. Each re-run is a new turn of the same librarian
thread, so it continues the thread's owned log.

## Refresh after landing

A mason's `done` report may include `learnings`, an array of project facts it
found while building its unit. The report is recorded with the unit candidate.
After a successful landing records `units/<unit>/landing.json` and moves the
unit to `merged`, the service requests one `kb-refresh` operation for that
landing. A pending or refused landing supplies no refresh. The request names
the exact unit, mason report revision, landing revision and commit.

The librarian receives the current knowledge base, the files of the exact
landed commit under `repo/`, and the report and landing under `source/`. It
writes a complete `output/kb/` using the same schema and validation as an
extraction pass. The service records changed prose and entity-map revisions
atomically. `kb/sources.json` links each accepted prose revision to its
workstream, unit, report, landing, commit and refresh operation. A refresh
that changes no prose still records its source ledger revision as a durable
completion marker. A failed turn or invalid output leaves the previous KB
untouched.

The operation and librarian turn IDs derive from the landing commit. A retry
finds the already recorded revisions and does not add them again. The next
unit waits while a refresh is pending; its file-based bundle then reads the
newest local prose without Hearsay.
