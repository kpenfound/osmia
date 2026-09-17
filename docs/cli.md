# Command line

Build with `go build -o osmia ./cmd/osmia`, or install with
`go install ./cmd/osmia`. Prepare the [configuration](configuration.md) first;
a top-level `config.toml` with profiles and no project is enough to start.

```sh
osmia serve --root ~/.osmia
# In another terminal:
osmia status --root ~/.osmia
osmia status w_0123456789abcdef0123456789abcdef
osmia project add dagger --upstream dagger/dagger --fork kpenfound/dagger --clone ~/github.com/dagger/dagger
osmia project extract p_0123456789abcdef0123456789abcdef
osmia project remove p_0123456789abcdef0123456789abcdef
osmia handin p_0123456789abcdef0123456789abcdef design.md
osmia send w_0123456789abcdef0123456789abcdef "Start with the upload API."
osmia conversation w_0123456789abcdef0123456789abcdef
osmia abandon w_0123456789abcdef0123456789abcdef "Superseded by the new upload design."
osmia pause all --reason "Away for the weekend"
osmia resume all
osmia profiles
osmia profiles set mason default
osmia profiles clear mason
osmia status --json
```

The [onboarding walkthrough](m2-onboarding.md) runs `project add`, `handin`,
`status` and `project remove` in order for a new project.

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
  context mode (`file`, a normal mode; see [context](context.md)), then each
  workstream with its state, open question count and context mode, and the
  chief of staff's goal and attention (`Attention: none` when nothing needs
  you), or `no status yet`. Reading the charter records any edit you made to
  it; see [charter](charter.md).
- `status <workstream-id>` shows one workstream of the active project: the same
  facts, then the full status (goal, attention, note, one line per active
  agent, and when it was written), or `Status: none yet` before the chief of
  staff writes one. A workstream the project does not hold fails with
  `not_found` (exit 4); with no project configured it fails with `no_project`
  (exit 4). See [workstream status](service.md#workstream-status).
- `send <workstream-id> <message>` sends a message to the workstream's chief
  of staff. The message is one argument; quote it. The service records it
  before answering, and the output names the message's turn ID and its state
  (`queued`). The chief of staff answers it as its next turn, after any turn
  already running. A message to an abandoned workstream is recorded but never
  answered; the next service start completes it as cancelled and
  `conversation` lists it as `failed`. An empty message, or a workstream the active project does
  not hold, fails with `validation` (exit 4); with no project configured it
  fails with `no_project` (exit 4). See [conversation](service.md#conversation).
- `conversation <workstream-id>` lists the owner's messages to that
  workstream's chief of staff and its final responses, oldest first. Each
  entry shows when it was sent or answered, who wrote it, its turn ID and the
  turn's state (`queued`, `running`, `done` or `failed`), then its text
  indented. It fails like `send`.
- `abandon <workstream-id> <reason>` abandons a workstream of the active
  project that is neither delivered nor abandoned. The reason is one argument;
  quote it. The service records the move to `abandoned` with you as actor and
  your reason, cancels the workstream's running turns (their recorded work is
  kept), completes its queued turns as cancelled, and runs no turn of the
  workstream again, including after a restart. Nothing is deleted: the trace,
  `handed/` and any branch stay. `status` then shows the state `abandoned`. An
  empty reason, or a workstream the active project does not hold, fails with
  `validation` (exit 4); a delivered or already abandoned workstream fails
  with `conflict` (exit 5). See [abandoning](service.md#abandoning).
- `project add <name> --upstream OWNER/REPO --fork OWNER/REPO --clone PATH
  [--base-branch NAME]` registers a project with the running service: it
  validates the request, generates the project ID, writes
  `projects/<id>/config.toml`, creates the trace repository with a charter
  template and an entity map seeded from the clone's tracked files, lists the ID in
  `active_projects`, requests the librarian's first knowledge-base extraction
  and activates the project without a restart. The clone path is made
  absolute by the client and must be an existing local Git repository outside
  the Osmia root; nothing is written to it. The output names the project ID,
  the trace path and the next step: writing the charter in
  `<trace>/charter.md`. The extraction runs in the service after the command
  returns; `status` shows its state, and the project is usable whether it
  succeeds or fails. `osmia serve` has no librarian runner, so there the
  extraction fails with the recorded reason "this service has no agent runner
  for the librarian"; `osmia project extract` starts a new attempt. Operation
  stays single-project: adding another project while one is active is refused
  and the error names the active project. Repeating the active project's exact registration
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
- `handin <project-id> <path|issue-url|->` hands one input to the active
  project and creates a workstream in state `handed`. The input is a file
  (made absolute by the client and read by the service), a GitHub issue URL
  (`https://github.com/OWNER/REPO/issues/NUMBER`, fetched by the service), or
  `-` for stdin (read by the client, at most 512 KiB of UTF-8 text; the client
  refuses larger or non-UTF-8 stdin itself with exit 4, before sending a
  request, and that message has no `validation:` prefix). The output names the
  workstream ID, its state, the path of the copy under the trace's
  `workstreams/<id>/handed/` and the recorded source. The client sends a new
  idempotency key with each command, so a request the API retries creates one
  workstream, and running the command again creates another. A project whose
  charter has no rules fails with `charter_empty` (exit 4), naming the project
  and the path to its `charter.md`. A project ID that is not the active project
  fails with `not_found` (exit 4). An unreadable, empty, non-UTF-8 or oversized
  input, or a URL of another shape, fails with `validation` (exit 4); an issue
  the service cannot fetch fails with `internal` (exit 5). A refused hand-in
  creates nothing. See [service](service.md#hand-in) for what is
  recorded. The service then asks the architect for the spec and plan; see
  [architect drafting](service.md#architect-drafting). `osmia serve` has no
  architect runner, so there the workstream stays `handed` and is not
  drafted.
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
discover workstreams. The runtime commands store controls. A pause holds new
worker turns in its scope while chief-of-staff turns still run; priority has no
scheduler effect before M4.

## Output and exit codes

All client commands accept `--json`. Status returns an object with `health`,
`configuration`, `runtime` and `status` API responses, where `status` lists
every workstream except the librarian's with its full status (`null` before
the first); `status <workstream-id>` returns that workstream's status
response; profiles returns the runtime
response. `abandon` returns the API's abandon response: `project`,
`workstream`, `state` and `reason`. `send` returns the accepted message entry and `conversation` the
API's conversation response (see [conversation](service.md#conversation)).
Mutations return `mutation` (the API acknowledgement) and `runtime`
(the subsequent effective-state response). Project commands return the API's
project response: the project view (ID, name, upstream, fork, clone, base
branch, trace and charter paths) and `next_step`; `project extract` returns
the project view and the pending extraction. `handin` returns the API's
hand-in response: `project`, `workstream`, `state`, `handed` (the absolute
path of the copy) and `source`. Output is one JSON value plus
newline, without progress text. Profile map keys are sorted in human output and
JSON. The responses are separate API requests, not an atomic snapshot.

Human mutation output identifies the affected scope and prints effective controls,
including defaults after clearing overrides. If the mutation succeeds but reading
the effective state fails, stderr says it was acknowledged; inspect status before
retrying. Failures leave stdout empty and write actionable diagnostics to stderr.
Raw configuration/parser and server error text is omitted from failure messages.
Project, hand-in, abandon, single-workstream status, send and conversation failures print the service's
message, which names the field, project or workstream ID or path at fault and
never raw file contents.

| Exit | Meaning |
| --- | --- |
| 0 | Success, help, or clean foreground cancellation |
| 1 | Invalid API response, local output or unexpected client failure |
| 2 | Invalid command, flags, arguments or root |
| 3 | Missing socket, connection failure or unavailable service |
| 4 | API malformed-input or validation rejection, no project is configured, unknown project or workstream, empty charter on hand-in, or stdin over the hand-in limit or not UTF-8 |
| 5 | API conflict (including an extraction already running or abandoning a delivered or abandoned workstream), project already active, unsupported operation, restart required or internal failure |
| 6 | Foreground startup/service failure, including ownership conflict |

For exit 3, start the service and verify matching root/socket and permissions.
For exit 6, check configuration and runtime validity and permissions, stop any
existing owner, and ensure the socket path is unused or stale. Do not delete a
live-owned socket. Unsupported responses identify the M1 limit; restart-required
responses instruct the operator to stop and start the service.

Detached management, install/upgrade commands, completion, web/tailnet,
inbox, ratification,
answer, reload and trace navigation are unavailable. The command examples in
the design describe the eventual product; this reference lists the implemented
surface.
