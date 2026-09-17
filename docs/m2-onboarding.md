# M2 onboarding demonstration

Onboarding takes a project from a local clone to a ready charter and a local
knowledge base. `TestM2ProjectOnboarding` in `internal/cli/onboarding_test.go`
runs every step through the CLI entry point and the API client against a real
service on its Unix socket. The librarian is a scripted fake isolation engine
and MCP transport. The test makes no provider or GitHub calls and pushes
nothing. It runs in a temporary root with a local Git repository as the clone.

## Running it

```sh
DAGGER_X_RELEASE=v1.0.0-beta.13 dagger check
```

The test runs as part of the `internal/cli` package tests. It removes its
temporary root when it finishes.

## The walkthrough

The commands below are the ones the test runs. `p_…` stands for the project ID
that `project add` prints.

### 1. Start with no project

```sh
osmia serve --root ~/.osmia
osmia status --root ~/.osmia
```

The root holds a top-level `config.toml` with profiles and
`active_projects = []`. Status reports `Project: none configured`.

### 2. Add the project

```sh
osmia project add demo --upstream owner/demo --fork fork/demo --clone ~/src/demo
```

The fixture clone has a `CODEOWNERS` file and the subsystems `cmd/demo`,
`internal/service` and `internal/trace`. The service creates the trace
repository at `<root>/projects/<project-id>/`, outside the clone. The trace
starts with the charter template in `charter.md` and an entity map in
`kb/entities.json` seeded from the clone. The service then requests the
librarian's first [extraction](knowledge-base.md#extraction). The fake
librarian writes:

- `kb/trace.md`, `kb/service.md` and `kb/demo.md`,
- a refined `kb/entities.json` whose entities `trace`, `service`, `demo` and
  `internal` carry names, aliases and owners.

Each file becomes a revision in the trace's `documents.jsonl` with actor
`agent`/`agent_librarian` and the extraction operation as its cause. The seed
revision before it has actor `owner`/`local`, the owner who ran
`project add`. Status shows `Knowledge base: extraction 1 succeeded`.

The test hashes every path, mode and file content in the clone, `.git`
included, before registration and again after extraction and removal. The
hashes match: onboarding never writes to the clone.

### 3. Hand-in waits for the charter

```sh
osmia handin p_…
```

The command fails with `charter_empty` (exit 4). The message names the
project and the path of its `charter.md`.

### 4. Write the charter

The owner edits `<root>/projects/<project-id>/charter.md`:

```markdown
# Charter

1. Keep the trace append-only.
2. Every change ships with a test.
```

The next `osmia status` records the edit as charter revision 2, with actor
`owner`/`local` and cause `owner-edit`, and shows:

```text
Charter: ready (2 rules, revision 2) <root>/projects/<project-id>/charter.md
Context: p_… context_mode=file
```

See [charter](charter.md) for the rule format and how edits are recorded.

### 5. Resolve context locally

Resolution and bundle assembly are what turns receive. No CLI command or API
endpoint prints them, so the test calls them directly:

- `kb.LoadFile` reads `kb/entities.json` from the trace, and nothing else.
  `ResolveEntities(["history", "internal"])` returns the entities `internal`,
  `service` and `trace` and their paths. `ResolvePaths` maps
  `internal/trace/git.go` to `trace` and `cmd/demo/main.go` to `demo`, and
  reports `docs/unknown.md` as unresolved. See
  [resolution](knowledge-base.md#resolution).
- The running service's context provider (`Service.Context()`) assembles a
  bundle for the footprint `history`, the alias of `trace`. The bundle is in
  `file` mode. It contains the charter rules `charter#1` and `charter#2` from
  revision 2, the prose of `kb/trace.md` and not that of other subsystems,
  the entity `trace`, and an empty set of decisions, because no rulings are
  recorded. See [turn context bundles](context.md).

### 6. Restart during extraction

The fake librarian's first turn waits until it is cancelled. While status
shows extraction 1 as `running`, the test stops the service and starts it
again. A second librarian turn runs, and extraction 1 succeeds. The trace
then holds exactly one charter revision, one revision per subsystem file and
two entity map revisions (the seed and the librarian's). The restart recorded
nothing twice.

### 7. Remove the project

```sh
osmia project remove p_…
```

The project leaves `active_projects` and status shows no project. The trace
repository keeps its HEAD commit and the owner's charter, and the clone's hash
is unchanged.

## What is faked

The test passes a scripted `service.Options.Librarian`: an isolation engine
that runs no model and an in-memory MCP transport. `osmia serve` supplies no
librarian runner, so there every extraction fails with a recorded reason and
`project extract` can retry it later. The project stays usable.
