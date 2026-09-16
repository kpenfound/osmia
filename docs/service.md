# M1 local service API

`internal/service.Run` loads the M1 configuration and runtime store, then serves
HTTP/JSON over the configured Unix socket until its context is cancelled.
`RunSignals` also handles SIGINT and SIGTERM. Both run in the foreground and wait
for cleanup. Embedders can use `Start`, `Socket`, `Wait` and `Close`; a successful
`Start` means the stores are loaded and the listener is bound. No models,
TCP listeners or authentication service are started. Agent turns run only when
an embedder supplies a turn reconciler (see below).

The root and its top-level configuration must already exist; a project is not
required, and one is registered through the API (see below). The service
acquires an exclusive advisory lock on `<root>/.service.lock` before loading
state or touching the socket. Root aliases resolve to the same lock. The lock file remains on disk:
closing its descriptor releases ownership, including after process termination;
unlinking it would allow competing owners to lock different inodes. Operators must
not remove it while the service is running. A second owner fails with an actionable
startup error and leaves the first owner's state alone.

The root is restricted to mode 0700 before binding and the socket to mode 0600.
A live socket is never removed, even if its listener does not hold the Osmia lock.
A socket is considered stale only when connecting returns connection-refused;
other probe failures are not proof of staleness. Replacement happens under root
ownership. Regular files and symlinks are not deleted to make room for a socket.
Shutdown stops accepting work and gives accepted requests five seconds to finish
(configurable for embedding); it then closes remaining connections, joins handlers,
closes the store and releases the lock. Cleanup removes only the socket inode it
created. An acknowledged mutation is durable; an interrupted client must read the
runtime view to reconcile whether its request committed.

## Contract

All paths start with `/v1`. Shared request, response and error types and a Unix-only
`Client` live in `internal/service`. `NewClient(socket)` accepts an explicit socket
path; it never reads configuration or runtime files. `Do` supports all operations,
and `Health`, `Configuration` and `Runtime` provide typed read helpers. Close the
client to release idle connections. API version 1 uses snake_case JSON fields.

| Method | Path after `/v1` | Input / response |
| --- | --- | --- |
| GET | `/health` | Readiness, service name, API version, supplied build version and commit |
| GET | `/config` | Resolved root, loaded effective-config SHA-256 digest, effective validated configuration, project view (null without a project), diagnostics |
| GET | `/runtime` | Effective runtime state, each active project's `context_mode` (`file`; see [context](context.md)), and diagnostics |
| GET | `/status` | `StatusResponse`: every workstream's status and facts in the active project, and diagnostics |
| GET | `/status/<workstream-id>` | `WorkstreamStatus` for one workstream of the active project |
| POST | `/projects` | `ProjectAddRequest`: name, upstream, fork, clone, optional base_branch; returns `ProjectResponse` |
| DELETE | `/projects` | `ProjectRemoveRequest`: project; returns `ProjectResponse` |
| POST | `/handin` | `HandInRequest`: project, paths; checks the charter, then returns `unsupported` |
| PUT | `/runtime/pause` | `PauseRequest`: target, mode, reason, source |
| DELETE | `/runtime/pause` | `ClearPauseRequest`: scope, project, workstream |
| PUT | `/runtime/priority` | `PriorityRequest`: project, workstreams |
| DELETE | `/runtime/priority` | `ClearPriorityRequest`: project |
| PUT | `/runtime/profile` | `ProfileRequest`: role, profile |
| DELETE | `/runtime/profile` | `ClearProfileRequest`: role |

Mutation bodies are one JSON object, at most 1 MiB; unknown or duplicate fields and
trailing values are rejected. Successful mutations return `{"applied":true}` only
after the runtime store acknowledges persistence. DELETE requests carry JSON bodies.
See [runtime overrides](runtime.md) for target, mode and reference rules. The service
accepts workstream IDs from the active trace repository’s validated manifests.
Without an initialized trace, embedding callers may supply known IDs; the default
list is empty. The service does not create workstream identities.

The configuration digest hashes the canonical JSON of the effective loaded
configuration, including defaults and the resolved socket/project paths, not TOML
comments or formatting. Configuration reads validate current disk input for
comparison but never apply it. Invalid/unreadable configuration yields a diagnostic
and retains the loaded digest and view. Valid changes report `restart_required`.
Runtime reads report external edits or unreadable input while retaining the last
acknowledged view; further mutations reject changed disk state. Restore the exact
runtime file or restart with valid state to reconcile it. No partial configuration
or runtime load is applied. Startup rejects invalid files because no previous valid
view exists. Stale runtime references remain diagnostic and are excluded from the
effective view according to the runtime store's rules.

Errors use `{"error":{"code":"validation","message":"..."}}`. Codes are stable;
messages are fixed operator guidance, never raw filesystem errors, parser input,
credentials or arbitrary file contents. Configuration responses contain only
validated schema fields. Diagnostics identify the affected field and a stable code.

| Code | HTTP status | Meaning |
| --- | --- | --- |
| `malformed_input` | 400 | Malformed, ambiguous, unknown-field or oversized JSON |
| `validation` | 422 | Invalid override or unavailable reference |
| `conflict` | 409 | Runtime file changed outside the store |
| `unsupported` | 501 | Unknown path/method or later-milestone operation, including POST `/reload` |
| `restart_required` | 409 | PUT `/config/root` or `/config/listen` |
| `unavailable` | 503 | Service shutting down; also the client's code for transport failure |
| `internal` | 500 | Storage or other internal failure, including an interrupted project registration |
| `no_project` | 409 | The operation needs an active project and none is configured |
| `project_active` | 409 | A project is active and single-project operation refuses another |
| `not_found` | 404 | The project ID is not the active project, or the workstream is not in it |
| `charter_empty` | 409 | Hand-in refused: the project's charter has no rules |

Project operations compose their messages from the request's fields and
identities: a validation failure names the field at fault, `project_active`
names the active project, and an incomplete registration names the project ID
to finish. They never include raw file contents or parser output.

Health readiness means the loaded stores can serve requests; disk diagnostics do
not discard that valid view. Reload application, lifecycle endpoints, streaming,
web/tailnet access and scheduling effects are outside M1.


The service opens the active project's existing trace and starts the
[local operation reconciliation loop](trace.md#durable-local-operations).
Startup scans durable intent even without wakeups. Missing reconciliation
adapters leave work pending; corrupt or locked traces prevent startup. Shutdown
cancels and joins the loop before releasing trace ownership.

## Projects

`POST /v1/projects` registers a project and activates it in the running
service, opening its new trace and reconciliation loop exactly as startup does.
`DELETE /v1/projects` removes the active project from configuration, stops its
loop and releases its trace; the trace and the clone stay on disk. Both edit
`config.toml` as text and replace the loaded configuration's project only, so
`/config` keeps matching the disk. Validation, recovery after an interrupted
registration and the single-project rule are described in
[configuration](configuration.md#project-registration). Without a project,
`/config` and `/runtime` carry a `no_project` diagnostic, project-scoped
overrides are rejected as validation failures, and `DELETE /v1/projects`
returns `no_project`. A registration interrupted by a service stop is finished
at the next start; if that fails, the service starts with the configuration as
loaded (without a project unless the registration had already listed it) and
reports an `internal` diagnostic on `projects` until `POST /v1/projects` finishes
it. A journal naming a project other than the active one is never finished:
startup reports it, and `POST /v1/projects` refuses with the two IDs until the
active project is removed or the journal is inspected.

`/config` reports the active project's `charter_state`: `ready`, the number of
`rules`, the recorded `revision` and numbering `diagnostics`. Reading it records
any owner edit to the charter first; if the charter cannot be read or recorded,
`charter_state` is absent and a `charter` diagnostic with code `internal` names
the file. A configured project with no trace repository has no charter state
and no charter diagnostic. The project view returned by `POST` and `DELETE /v1/projects` carries
no charter state.

`POST /v1/handin` takes a project ID and a list of paths. The service does not
check the paths; `osmia handin` sends them as absolute paths. A malformed
project ID returns `validation`. An ID that is not the active project returns
`not_found`; an active project with no trace repository returns `internal`.
The charter gate runs next: an empty charter returns `charter_empty` with a
message naming the project and its `charter.md`. With rules, hand-in returns
`unsupported`, naming the project and its rule count. See [charter](charter.md).

## Workstream status

`GET /v1/status` lists each workstream of the active project in trace manifest
order, and `GET /v1/status/<workstream-id>` returns one. Each
`WorkstreamStatus` carries the chief of staff's latest
[status](trace.md#workstream-status) next to the facts the service owns:

| Field | Value |
| --- | --- |
| `workstream`, `project` | The workstream and its project |
| `state` | The feature workflow state, or `null` before one is recorded |
| `open_questions` | Questions in the workstream without a ruling |
| `context_mode` | The project's context mode, as in `/runtime`: `file` for [file-based context](context.md) |
| `status` | `null` until the chief of staff writes one; otherwise `goal`, `attention` (empty when nothing needs the owner), `note`, `agents`, `revision` and `updated_at` |

Without an active project or its trace, the list is empty. If the trace
cannot be read, the list is empty and carries a `workstreams` diagnostic with
code `internal`. For one workstream, a malformed ID returns `validation`, no
configured project returns `no_project`, a workstream the active trace does not
hold (or no trace at all) returns `not_found`, and an unreadable trace returns
`internal`; these messages name the workstream or project.

`Options.Threads` binds a runner-boundary reconciler to the trace the service
opened, each time a project's trace opens: at startup and when a project is
added. It replaces any runner adapter in `Options.Reconciliation`. The [thread dispatcher](trace.md#turn-dispatch) is the
intended binding; it receives the service-owned repository handle, which callers
must not close. The [M1 demonstration](m1-demonstration.md) uses this path with
fake engines.

With `Options.Threads` set, the service also runs queued workstream turns on its
own. At the start of every reconciliation pass, `internal/scheduler` reads each
workstream's threads and turn operations. For every thread with no turn in
flight, it publishes a `thread-turn` operation for the oldest unfinished turn,
and the same pass delivers it through the thread dispatcher. A turn is in flight
while its thread holds a claim, or while the turn has an operation and has not
completed. A thread therefore never has two turns in flight, and a message
queued mid-turn runs as the thread's next turn once the current one completes.
Turns already covered by an operation, including one an embedder published, are
left alone. The pass makes no model call.

Each dispatch is a transition on the thread's workflow subject
`scheduler.Subject(agent)` (`dispatch_` and the first 40 hex digits of the
SHA-256 of the agent ID). Its ID derives from the agent and turn IDs, its cause
and depth are the request's, its actor is `service`/`scheduler`, and its target
state is the turn ID. After a restart, an in-flight turn is recovered through its
existing operation, as the [turn dispatch](trace.md#turn-dispatch) table
describes: it is neither dispatched again nor lost. `scheduler.Options.Admit` is
the single dispatch gate; the service admits every candidate.
