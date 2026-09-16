# Command line

Build with `go build -o osmia ./cmd/osmia`, or install with
`go install ./cmd/osmia`. Prepare the [configuration](configuration.md) first;
a top-level `config.toml` with profiles and no project is enough to start.

```sh
osmia serve --root ~/.osmia
# In another terminal:
osmia status --root ~/.osmia
osmia project add dagger --upstream dagger/dagger --fork kpenfound/dagger --clone ~/github.com/dagger/dagger
osmia project extract p_0123456789abcdef0123456789abcdef
osmia project remove p_0123456789abcdef0123456789abcdef
osmia handin p_0123456789abcdef0123456789abcdef design.md
osmia pause all --reason "Away for the weekend"
osmia resume all
osmia profiles
osmia profiles set mason default
osmia profiles clear mason
osmia status --json
```

## Commands and flags

- `serve [--root PATH]` validates and starts the foreground service. SIGINT and
  SIGTERM drain requests and release ownership and the socket. A live owner is
  refused; a provably stale socket is recovered automatically.
- `status` shows health, loaded configuration digest/root, the active project
  and its trace path (or that none is configured), its charter state (ready or
  empty, rule count, recorded revision and numbering diagnostics), its latest
  knowledge-base extraction (number, `pending`, `running`, `succeeded` or
  `failed`, the time of its last activity and the reason when it failed or
  waits to retry), diagnostics, effective runtime controls and the project's
  context mode (`file`, a normal mode; see [context](context.md)). Reading the
  charter records any edit you made to it; see [charter](charter.md).
- `project add <name> --upstream OWNER/REPO --fork OWNER/REPO --clone PATH
  [--base-branch NAME]` registers a project with the running service: it
  validates the request, generates the project ID, writes
  `projects/<id>/config.toml`, creates the trace repository with a charter
  template and an entity map seeded from the clone, lists the ID in
  `active_projects`, requests the librarian's first knowledge-base extraction
  and activates the project without a restart. The clone path is made absolute by the client and must be an
  existing local Git repository outside the Osmia root; nothing is written to
  it. The output names the project ID, the trace path and the next step:
  writing the charter in `<trace>/charter.md`. The extraction runs in the
  service after the command returns; `status` shows its state, and the project
  is usable whether it succeeds or fails. Operation stays single-project:
  adding another project while one is active is refused and the error names
  the active project. Repeating the active project's exact registration
  returns it again.
- `project extract <project-id>` starts a new
  [knowledge-base extraction](knowledge-base.md#extraction) of the active
  project: a librarian turn that rewrites `kb/<subsystem>.md` and
  `kb/entities.json` from the clone. The command returns once the extraction
  is recorded as pending; follow it with `status`. A project ID that is not the
  active project fails with `not_found` (exit 4). While an extraction is still
  pending or running, another is refused with `conflict` (exit 5) naming the
  extraction to wait for.
- `project remove <project-id>` takes the active project out of
  `active_projects` and closes its runtime state. The trace directory and the
  clone are kept. Adding the same upstream again afterwards creates a new
  project ID and a new trace; see [configuration](configuration.md#project-registration).
- `handin <project-id> [path...]` hands work to the active project. Paths are
  made absolute by the client. It first checks the charter: with no rules it
  fails with `charter_empty` (exit 4), naming the project and the path to its
  `charter.md`. A project ID that is not the active project fails with
  `not_found` (exit 4). With a charter that has rules it currently fails with
  `unsupported` (exit 5): the hand-in itself is not implemented yet, and no
  workstream is created.
- `pause <all|project-id|workstream-id> [--hard] [--reason TEXT]` stores an
  operator pause; the default mode is soft.
- `resume <all|project-id|workstream-id>` clears that scope's pause. Parent pauses
  still apply; all effective pauses are displayed.
- `priority set <workstream-id>...` stores the active project's ordered list.
  `priority clear` removes that preference. Both, and `pause`/`resume` of a
  workstream, need an active project and say so otherwise.
- `profiles` displays effective bindings and runtime diagnostics.
  `profiles set <role> <profile>` overrides a binding.
  `profiles clear <role>` restores its configured default.

Every command accepts `--root PATH`, defaulting to `~/.osmia`. Flags may occur
before or after positional arguments; value flags also accept `--flag=value`.
Duplicate and unknown flags are errors. `--help` prints the supported syntax.

Clients connect to `osmia.sock` under the root. If configuration specifies another
socket name, pass `--socket PATH` to each client command; relative paths resolve
against the root. For example, with `listen.socket = "local.sock"`:

```sh
osmia status --root ~/.osmia --socket local.sock --json
```

Clients never load configuration, runtime, trace or project files. They obtain
the active project and effective values over HTTP using the service client.
Changing socket configuration requires restarting the service and updating the
client's `--socket` argument.

Project and workstream arguments are persistent IDs (`p_` or `w_` followed by
32 lowercase hexadecimal digits), not display names. Workstream IDs must be known
to the service's record repository. The standalone entry point does not
discover workstreams. The runtime commands store controls; scheduler effects
are unavailable before M4.

## Output and exit codes

All client commands accept `--json`. Status returns an object with `health`,
`configuration` and `runtime` API responses; profiles returns the runtime
response. Mutations return `mutation` (the API acknowledgement) and `runtime`
(the subsequent effective-state response). Project commands return the API's
project response: the project view (ID, name, upstream, fork, clone, base
branch, trace and charter paths) and `next_step`; `project extract` returns
the project view and the pending extraction. Output is one JSON value plus
newline, without progress text. Profile map keys are sorted in human output and
JSON. The responses are separate API requests, not an atomic snapshot.

Human mutation output identifies the affected scope and prints effective controls,
including defaults after clearing overrides. If the mutation succeeds but reading
the effective state fails, stderr says it was acknowledged; inspect status before
retrying. Failures leave stdout empty and write actionable diagnostics to stderr.
Raw configuration/parser and server error text is omitted from failure messages.
Project and hand-in command failures print the service's message, which names
the field, project ID or path at fault and never raw file contents.

| Exit | Meaning |
| --- | --- |
| 0 | Success, help, or clean foreground cancellation |
| 1 | Invalid API response, local output or unexpected client failure |
| 2 | Invalid command, flags, arguments or root |
| 3 | Missing socket, connection failure or unavailable service |
| 4 | API malformed-input or validation rejection, no project is configured, unknown project, or empty charter on hand-in |
| 5 | API conflict (including an extraction already running), project already active, unsupported operation, restart required or internal failure |
| 6 | Foreground startup/service failure, including ownership conflict |

For exit 3, start the service and verify matching root/socket and permissions.
For exit 6, check configuration and runtime validity and permissions, stop any
existing owner, and ensure the socket path is unused or stale. Do not delete a
live-owned socket. Unsupported responses identify the M1 limit; restart-required
responses instruct the operator to stop and start the service.

Detached management, install/upgrade commands, completion, web/tailnet,
hand-in past the charter check, inbox, conversation, ratification,
answer, reload and trace navigation are unavailable. The command examples in
the design describe the eventual product; this reference lists the implemented
surface.
