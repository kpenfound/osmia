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
`Close` unlocks it before closing its descriptor, so ownership is released at
once even while a child process still shares the descriptor, and process
termination releases it too;
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
| POST | `/conversation/<workstream-id>` | `SendRequest`: text; returns the accepted `ConversationEntry` |
| GET | `/conversation/<workstream-id>` | `ConversationResponse`: the workstream's conversation with its chief of staff |
| GET | `/inbox` | `InboxResponse`: the escalations of the active project that wait for the owner's ruling |
| POST | `/inbox/<number>` | `AnswerRequest`: text; records the owner's ruling and returns `AnswerResponse` |
| POST | `/projects` | `ProjectAddRequest`: name, upstream, fork, clone, optional base_branch; returns `ProjectResponse` |
| DELETE | `/projects` | `ProjectRemoveRequest`: project; returns `ProjectResponse` |
| POST | `/projects/extract` | `ProjectExtractRequest`: project; returns `ExtractionResponse` |
| POST | `/abandon/<workstream-id>` | `AbandonRequest`: reason; returns `AbandonResponse` |
| POST | `/handin` | `HandInRequest`: project, key, and one of path, url and stdin; returns `HandInResponse` |
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
| `conflict` | 409 | Runtime file changed outside the store, an extraction requested while one is pending or running, or a hand-in key reused for other input |
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
and web/tailnet access are outside M1. Pauses hold queued turns, capacity
bounds dispatch, and a `waiting` turn parks its thread, as described with the
queued-turn scheduler below.


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

`/config` also reports the project's latest
[knowledge-base extraction](knowledge-base.md#extraction) as `extraction`:
its number, its `state` (`pending`, `running`, `succeeded` or `failed`), the
time of its last recorded activity and, when it failed or is waiting to
retry, the `reason`. It is absent for a project whose trace has no librarian
workstream, and an unreadable state adds an `extraction` diagnostic with code
`internal`. Registration requests extraction 1; `POST /v1/projects/extract`
requests the next one and returns it as `pending`. A malformed project ID
returns `validation`, an ID that is not the active project `not_found`, a
project without a trace `internal`, and a request while an extraction is
pending or running `conflict`, naming the extraction to wait for.

## Hand-in

`POST /v1/handin` creates a workstream from one input. The request carries the
project ID, a `key` that identifies the request, and exactly one of `path` (a
clean absolute path to a regular file the service reads), `url` (a GitHub issue,
`https://github.com/OWNER/REPO/issues/NUMBER`) and `stdin` (the input text).
Checks run in this order, and a refused request writes nothing:

1. A malformed project ID returns `validation`. An ID that is not the active
   project, including a removed one, returns `not_found`; an active project
   with no trace repository returns `internal`.
2. The charter gate: an empty charter returns `charter_empty` with a message
   naming the project and its `charter.md`. See [charter](charter.md).
3. A `key` that is not 1 to 128 letters, digits, `_` or `-` (starting with a
   letter or digit), no input or more than one, a path that is not clean and
   absolute, or a URL of another shape returns `validation`.
4. The input is read. A missing or unreadable file, a file that is not regular,
   and input that is empty, not UTF-8 or larger than 512 KiB return
   `validation`. An issue the service cannot fetch returns `internal`.

The service fetches issues itself, with the GitHub REST API and the
`GITHUB_TOKEN` of its own environment when set. The token never reaches a
session or the trace. An issue is stored as Markdown: its title as a heading,
then its body.

The workstream ID is derived from the project and the key. The service creates
the workstream with its chief-of-staff thread, copies the input byte for byte
to `workstreams/<id>/handed/<name>` with its source (see
[trace](trace.md#files-and-records)), and records the feature transition `-> handed` in
`events.jsonl` with the owner as actor and a reason naming the copy and its
source. The same transaction queues a notice for the chief of staff. `<name>`
is the file's base name, or `input` when the trace does not accept that name,
`issue-<number>.md` for an issue and `stdin` for stdin.

The response is a `HandInResponse`: `project`, `workstream`, `state`
(`handed`), `handed` (the absolute path of the copy) and `source`. A request
repeating a key returns the same response without reading the input again or
writing anything; a hand-in interrupted part way is finished by the retry. The
same key with another source, or other stdin text, returns `conflict` naming
the key and the workstream. A storage failure returns `internal` and names the
workstream; retry with the same key. The next reconciliation pass asks the
architect for the workstream's spec and plan; see
[architect drafting](#architect-drafting).

## Architect drafting

The architect controller runs at the start of every reconciliation pass,
before event delivery and the scheduler, with or without `Options.Threads`.
For every workstream in feature state `handed`, except the librarian's, it
keeps the workflow subject `draft`, whose transitions are recorded in
`events.jsonl` with the actor `service`/`architect-drafting`:

| `draft` state | Meaning |
| --- | --- |
| `drafting-<n>` | Draft `n` is requested: transition `draft-<n>` published an `architect-draft` operation, which the reconciliation loop runs. |
| `invalid-<n>` | Draft `n` was recorded and failed validation. Transition `draft-<n>-invalid` lists every problem in its reason, and the operation's result carries the same text. |
| `failed-<n>` | Draft `n` ran no valid turn: the architect's turn failed, service stops interrupted it three times, the workstream was abandoned, or the workstream left `handed` before the draft was presented. Transition `draft-<n>-failed` holds the reason. |
| `exhausted` | Three drafts were not accepted. Transition `draft-exhausted` records it (`none of the architect's 3 drafts of the spec and plan was accepted; ...`) with a notice for the chief of staff naming the count and quoting the last draft's outcome, and nothing more is requested. |

Draft 1 is requested as soon as the workstream is handed, with the hand-in
transition as cause; after `invalid-<n>` or `failed-<n>` with `n` below three,
draft `n+1` is requested with that outcome's transition as cause. A draft in
progress, an exhausted workstream and a workstream in any other feature state
need nothing. The bound is the librarian's: three drafts per workstream and
three turns per draft.

### The turn

Each draft is one turn of the workstream's `agent_architect` thread (role
`architect`, thread `thread_architect`, created with the first request), run
through the thread runner and the [turn isolation](isolation.md) path by the
service's own reconciler, never the scheduler: the scheduler's gate declines
every architect thread. Turn `draft-<n>-<attempt>` carries the operation ID
as cause; a service stop mid-turn leaves it interrupted, and the next start
abandons it and starts the next attempt, up to three per draft. The turn's
profile is the architect's effective binding when the turn is accepted.

The service stages the view under
`<root>/architect/<project-id>/<workstream-id>/<turn>/workspace` and gives the
turn a read-only private copy of it:

| Path in the view | Content |
| --- | --- |
| `handed/<name>` | The handed input, unchanged. |
| `charter.md` | The charter as recorded, after any owner edit is recorded. |
| `context.md` | The rendered [context bundle](context.md) for the whole project, with the workstream's decisions. |
| `draft/spec.md`, `draft/plan.json` | The latest recorded draft, once one is recorded. |

The architect gets `file_read` and `draft_write` and nothing else: no notes
(they are the project's, not the workstream's), no write, execute, network or
VCS capability, and no other context source. `draft_write` takes `path` (`spec.md` or `plan.json`) and
`content` (UTF-8 text of at most 512 KiB) and stores the file in the turn's
service-owned directory; it is a memory tool, so the read-only role holds it.
The prompt names the view, asks for `spec.md` with the intended behaviour,
what the feature must not do and a `## Acceptance criteria` section holding
a numbered list, and for `plan.json` in the [plan format](trace.md#planjson)
with every criterion addressed by a unit with a named proof, acyclic
dependencies and footprints naming entities of the bundle. It states the
validation rules and leaves how finely the work is cut to the architect. The
prompt of draft `n+1` opens with `Draft <n> was not accepted:` and the reason
of draft `n`'s transition, and points at `draft/` when a draft is recorded or
says no file of that draft was recorded.

### Recording and validation

When the turn completes, the delivered files are recorded with
`RecordDocuments` in one commit as revisions of the workstream's `spec` and
`plan` documents (actor `agent`/`agent_architect`, cause the operation ID). A
file that was not delivered, or is not UTF-8 text, is a problem with the draft
and records no revision. The recorded draft is then validated with
`plan.ParseSpec`, `plan.Parse` and `plan.Validate` against the latest recorded
entity map; see [validation](trace.md#validation).

A valid draft moves the workstream `handed -> sketched` in one transaction:
transition `sketched`, actor `service`/`architect-drafting`, cause the
operation ID, timestamped with the draft's revisions, and a reason naming the
draft, its revisions and their criteria and unit counts. Its notice tells the
chief of staff the draft exists. An invalid draft records `draft-<n>-invalid`
instead and the workstream stays `handed`.

Recovery keys on the trace: a restart during the turn finds it interrupted;
one between recording and the transition finds the revisions by their cause
and records nothing again; one after the transition finds it and repeats
nothing. A recorded outcome completes the operation without running the
architect.

[Abandoning](#abandoning) the workstream cancels the architect's running turn,
which is recorded as interrupted. The draft of an abandoned workstream starts
no turn, completes a queued one as cancelled, and records `draft-<n>-failed`
with the reason `draft <n> failed: the workstream was abandoned, so the
architect runs no turn for it`; nothing more is requested.

`Options.Architect` supplies the execution engine and MCP host factory the
architect's turns run in. Without it the controller requests nothing, so a
handed workstream stays `handed` until a service with a runner starts and
drafts it. A draft already requested stays pending: applying it returns
`this service has no agent runner for the architect` wherever it would start or
run a turn, the operation is retried, and no draft is spent. A captured turn is
still completed and its draft recorded, since that runs no architect.

## The shed: committee rounds

The committee controller runs in every reconciliation pass after the
architect controller, before event delivery and the scheduler. It moves a
`sketched` workstream into the shed and asks its committee for round 1; the
committee's contributions to a round are validated as they are made and
recorded once the round ends. The package `internal/shed` holds the record,
the tools and the open-dissent computation.

### Entering the shed

For every workstream in feature state `sketched`, except the librarian's, the
controller creates the workstream's committee and moves it `sketched ->
in-shed`: transition `in-shed`, actor `service`/`shed`, cause `sketched`, with
the reason `spec.md revision <s> and plan.json revision <p> enter the shed with
a committee of <N>` and the usual state notice for the chief of staff.

The committee is a fixed set of durable threads of the workstream:
`agent_committee_1` to `agent_committee_<N>` (role `committee`, threads
`thread_committee_<i>`), where `N` is
[`capacity.committee`](configuration.md) when the workstream enters the shed.
Later rounds run the threads that exist; a changed configuration does not
resize a committee.

The workflow subject `shed` tracks the rounds, with transitions by
`service`/`shed` in `events.jsonl`:

| `shed` state | Meaning |
| --- | --- |
| `round-<n>` | Round `n` is requested: transition `shed-round-<n>` published a `shed-round` operation whose input pins the round to one revision of `spec.md` and one of `plan.json`. Round 1 pins the latest revisions when it is requested. |
| `heard-<n>` | Every member's turn of round `n` has ended and its record is committed. Transition `shed-round-<n>-heard` and the operation's result carry the same summary: members heard, objections, concessions, failed turns and how many objections stand. |
| `failed-<n>` | The round ended without a record because the workstream was abandoned. Transition `shed-round-<n>-failed` holds the reason. |

### A member's turn

The round operation runs one turn per member, all at the same time, through
the thread runner and the [turn isolation](isolation.md) path, by the
service's own reconciler: the scheduler's gate declines every committee
thread. Turn `shed-<n>-<agent>-<attempt>` carries the operation ID as cause
and the committee's effective profile. The operation waits for every member
before it records anything.

Each turn gets a read-only private copy of a view staged under
`<root>/shed/<project-id>/<workstream-id>/<turn>/workspace`:

| Path in the view | Content |
| --- | --- |
| `spec.md`, `plan.json` | The pinned revisions, whatever was recorded since. |
| `handed/<name>` | The handed input, unchanged. |
| `charter.md` | The charter as recorded, after any owner edit is recorded. |
| `context.md` | The rendered [context bundle](context.md) for the whole project, with the workstream's decisions. |
| `repo/` | The tracked files of the owner's clone. |
| `shed/round-<k>/` | The records of the earlier rounds. |

A member gets `file_read`, `object` and `concede` and nothing else: no notes,
no write, execute, network or VCS capability. `object` and `concede` are
memory tools that only the `committee` role can hold. The prompt names the
round, the pinned revision, the view, the two tests and the judgement, and the
citation forms; from round 2 on it lists the member's own objections that
still stand, with their IDs.

`object` takes `kind`, `part`, `argument` and `citations`:

| `kind` | Meaning | `part` |
| --- | --- | --- |
| `charter` | The part violates a charter rule: a veto. It must cite the rule as `charter#<n>`. | `spec#<n>`, `plan#<unit>`, `spec` or `plan` |
| `fit` | The plan does not realise the handed design, or works against a recorded decision: advice. | `spec#<n>`, `plan#<unit>`, `spec` or `plan` |
| `size` | A unit addresses too much and must be split. | `plan#<unit>` |
| `proof` | The plan names no proof that can show a criterion holds. | `spec#<n>` |

`spec` and `plan` name a whole document. A `part` that names a criterion or a
unit must exist in the pinned revision. Every objection needs a non-empty
argument and at least one citation, and every citation must exist:

| Citation | Must name |
| --- | --- |
| `charter#<n>` | A rule of the latest charter, numbered exactly once. |
| `spec#<n>` | An acceptance criterion of the pinned `spec.md` revision. |
| `plan#<unit>` | A unit of the pinned `plan.json` revision. |
| `kb/<subsystem>.md` | An existing knowledge-base file. |
| `kb/entities.json#<entity>` | An entity of the recorded entity map, by ID or alias. |

An accepted objection returns `{"recorded":true,"objection":"<id>"}` with the
ID `<agent>-r<round>-<k>`. `concede` takes `objection` and `reason` and
withdraws or settles one of the member's own objections that still stands,
from an earlier round or from this turn. A contribution that is invalid is an
ordinary result, `{"recorded":false,"reason":...}`, so the member reads why
and can correct it within the turn; it takes no ID and is not kept.

### The record

The tools keep a turn's contributions in the turn's service-owned directory.
Once every member's turn has ended, the operation records one document per
member with `RecordDocuments`, all in one commit:
`shed/round-<n>/<agent>.json`, record ID `shed-round-<n>-<agent>`, actor
`agent`/`<agent>`, cause the operation ID. The file holds the round, the
member, the pinned `revision` (`spec` and `plan`), the turn, the `objections`
and the `concessions`. A member whose turn ended without `object` or `concede`
has a file with neither: no new dissent. A member whose turn failed, or was
interrupted by three service stops, has a file with the `failure` and whatever
the failed turn contributed before it; contributions of an interrupted attempt
that was retried are dropped.

`shed.OpenDissent` computes the dissent that stands from the records alone. An
objection stands until its member concedes it, or until its member accepts a
later revision of the documents: a turn against the later revision that ends
normally without a new objection. A silent turn against the revision the
objection was made on settles nothing. A failed turn accepts nothing. A new
objection against the later revision accepts nothing either, so the member's
earlier objections stand until it concedes them.

Recovery keys on the trace: a restart during the round finds the turns that
ended in their threads and runs only the members that had not finished, each
up to three attempts; one between the record and the transition finds the
round's files and records nothing again; a recorded outcome completes the
operation without running a member.

[Abandoning](#abandoning) the workstream cancels the members' running turns.
The round of an abandoned workstream records no file and ends `failed-<n>`
with the reason `round <n> failed: the workstream was abandoned, so the
committee is not heard`.

`Options.Committee` supplies the execution engine and MCP host factory the
committee's turns run in. Without it no workstream enters the shed, so a
sketched workstream stays `sketched` until a service with a runner starts. A
round already requested stays pending: applying it returns `this service has
no agent runner for the committee` wherever it would start or run a turn, the
operation is retried, and no member's attempt is spent.

## Abandoning

`POST /v1/abandon/<workstream-id>` abandons a workstream of the active project
for the owner. The body is `{"reason": "..."}`. A workstream whose feature state
is neither `delivered` nor `abandoned` moves to `abandoned` in one recorded
transition whose actor is the owner (`owner`/`local`) and whose reason is the
owner's, with a notice for the chief of staff in the same commit. The service
then cancels the turn operations it is applying for the workstream, the
[architect's draft](#architect-drafting) included, so a
running turn stops and records the partial result it has, and completes every
other unfinished turn of the workstream as cancelled (actor
`service`/`abandon`). A cancelled turn holds no capacity.

The scheduler admits no turn of an abandoned workstream, and a turn operation
of one completes its turn as cancelled instead of running it. When the service
opens a trace it completes the unfinished turns of every abandoned workstream,
which finishes an abandonment a stop interrupted. Turns queued afterwards, such
as the chief of staff's turn for the notice or an owner message, stay queued
and never run. Nothing is deleted: the trace, `handed/`, the documents and any
branch stay. Workstream status reports the state `abandoned`.

The response is an `AbandonResponse`: `project`, `workstream`, `state`
(`abandoned`) and `reason`. A malformed workstream ID, a workstream the active
trace does not hold (or no trace at all), the librarian's workstream, or an
empty reason returns `validation`; no configured project returns `no_project`;
a delivered or abandoned workstream, or one whose state changes during the
request, returns `conflict`; a trace that cannot be read or written returns
`internal`. These messages name the workstream, except the empty reason
(`reason must not be empty`) and `no_project` (`no project is configured; add
one with osmia project add`). If the transition was recorded but the turns
could not be cancelled, the `internal` message says the workstream is
abandoned and that its queued turns are cancelled at the next start.

## Workstream status

`GET /v1/status` lists each workstream of the active project in trace manifest
order, except the librarian's, which carries no feature (see
[extraction](knowledge-base.md#extraction)), and
`GET /v1/status/<workstream-id>` returns one. Each
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

## Inbox and rulings

`GET /v1/inbox` returns an `InboxResponse`: `entries`, every escalation of the
active project whose questions are still `escalated`, ordered by inbox number.
Questions the chief of staff escalated as one batch are one entry. An
escalation of an abandoned workstream is left out. Without a configured
project, or without a trace, `entries` is empty. A trace that cannot be read
returns `internal`.

| Field | Meaning |
| --- | --- |
| `number` | The inbox number `POST /v1/inbox/<number>` accepts. The trace assigns it when the questions are escalated, counting the project's escalations from 1, and never reuses it |
| `workstream`, `batch` | The workstream and the escalation's batch ID in it |
| `question` | The chief of staff's rephrasing for the owner |
| `blocked` | What waits on the ruling |
| `options` | The choices, possibly none |
| `recommendation` | What the chief of staff would decide |
| `escalated_at` | When the questions were escalated |
| `asked` | Each question of the batch in the order it was escalated: `id`, `asked_by` (the asking agent), `unit` when the asking turn had one, and `question` as asked |

`POST /v1/inbox/<number>` takes an `AnswerRequest`, `text`, and records it as
the owner's ruling on that entry. One commit holds revision 1 of the ruling of
every question in the batch, each question's move from `escalated` to `ruled`
and one notice event for the workstream's chief of staff, so the ruling is
durable before anything acts on it and a failed write leaves neither a ruling
nor an event; see [the trace reference](trace.md#the-owners-ruling). It
returns an `AnswerResponse`: `number`, `workstream`, `batch`, the `questions`
the ruling covers, the `ruling` as given and `at`.

| Case | Error |
| --- | --- |
| `<number>` is not a positive decimal integer | `validation`: `inbox entry must be a number from osmia inbox` |
| No configured project | `no_project` |
| No entry carries the number, or the project has no trace | `validation`: `there is no inbox entry <n>; list the entries with osmia inbox` |
| Empty text | `validation`: `text must not be empty` |
| The entry already has a ruling, relayed or not | `conflict`: `inbox entry <n> is already answered` |
| The entry belongs to an abandoned workstream | `conflict`: `inbox entry <n> belongs to abandoned workstream <id> and takes no ruling` |
| The trace cannot be read or written | `internal` |

A refused request records nothing. The abandoned-workstream check and the
write hold the trace repository's lock together, so an abandonment that commits
first always refuses the ruling. Rulings are accepted without a turn
reconciler, but only a service with `Options.Threads` delivers the event to
the chief of staff and the relayed ruling to the askers, as described under
[questions](#questions).

## Conversation

`POST /v1/conversation/<workstream-id>` sends the owner's message to the
workstream's chief-of-staff thread, and to no other. The body is
`{"text": "..."}`. The service queues the message on that thread with
`EnqueueTurn` and answers only once the request is in the trace, so an
acknowledged message survives a restart and runs at most once. It runs as the
thread's next turn; a message sent while a turn is in flight waits for it. A
message to an abandoned workstream is accepted but never runs: the next
service start completes it as cancelled (see [abandoning](#abandoning)).

The accepted request fixes, at acceptance:

- the profile the `chief_of_staff` role is bound to, including a runtime
  override;
- the prompt, which is the message text as sent;
- the system prompt, which names the workstream and carries the workstream's
  [context bundle](context.md) rendered at acceptance.

Its actor is the owner (`owner`/`local`) and its turn ID is `message_`
followed by 32 random hexadecimal digits.

`GET /v1/conversation/<workstream-id>` returns a `ConversationResponse`:
`workstream` and `entries`, oldest first. It is read from the chief-of-staff
thread's turn log in the trace, never from a backend transcript. Each owner
message is an entry of kind `message`. Once its turn has completed with a
final response, an entry of kind `response` follows it. Turns on the thread
that the owner did not send are not listed. Each entry has:

| Field | Value |
| --- | --- |
| `turn` | The turn that answers the message |
| `kind` | `message` or `response` |
| `text` | The message as sent, or the chief of staff's final response |
| `at` | When the message was accepted, or the response captured |
| `state` | The turn's state: `queued` until claimed, `running` until completed, then `failed` for a failed or cancelled turn and `done` otherwise. A turn a restart interrupted before its result was captured is never retried and lists as `failed` |

`POST` returns the message's entry, whose state is `queued`. A malformed
workstream ID, a workstream the active trace does not hold (or no trace at
all), the librarian's workstream (which status leaves out too, and whose chief
of staff never gets a turn), or empty text returns `validation`. No configured project returns
`no_project`. A trace that cannot be read or written, a `chief_of_staff` profile that cannot be used, or a
bundle that cannot be assembled, returns `internal`.
These messages name the workstream or project. A rejected message is not
recorded. Messages are accepted without a turn reconciler, but only a service
with `Options.Threads` runs them.

## Running turns

`osmia serve` starts the service with `service.Enforce(opts,
service.CoreEnforcement())`, which sets `Options.Librarian`,
`Options.Architect`, `Options.Committee` and `Options.Threads`, so a served
project runs real role turns: extraction after `project add`, drafting after a
hand-in, committee rounds once a draft is sketched, and every queued
chief-of-staff turn. All four use one `Enforcement`:

| Part | Production value |
|---|---|
| `Engine` | `coreadapter.CoreEngine`: each role's enforcer for its configured `sandbox` and `image` |
| `Hosts` | `coreadapter.MCPHost` serving host turns with `CoreTransport` and container turns with `ContainerTransport` |

`Options.Threads` binds the thread dispatcher to isolated turns that grant only
the chief of staff, with `set_status`, `answer`, `escalate`, `relay_ruling`,
`route_amendment` and `propose_charter`. A thread turn of any other role fails with the recorded
reason `role has no service grant`. The chief of staff's workspace is an empty
directory, `workspaces/<project-id>/<workstream-id>` under the root; its context
is in the prompt. Its session directories are under
`threads/<project-id>/<workstream-id>/<agent-id>/<turn-id>`. Its sandbox, image
and the root come from the configuration the service loaded at startup, like
every other role's; an edited configuration on disk takes effect on restart.
Thread turns record UTC times.

A role whose sandbox the platform cannot enforce, such as `claude` on Linux,
fails each of its turns with core's reason, recorded like any failed turn. The
service keeps running and the other roles' turns still run.

`Options.Threads` binds a runner-boundary reconciler to the trace the service
opened and the configuration it loaded, each time a project's trace opens: at startup and when a project is
added. It replaces any runner adapter in `Options.Reconciliation` for every
runner operation except the librarian's `kb-extract` action and the
architect's `architect-draft` action, which the service reconciles itself. The [thread dispatcher](trace.md#turn-dispatch) is the
intended binding; it receives the service-owned repository handle, which callers
must not close. The [M1 demonstration](m1-demonstration.md) uses this path with
fake engines.

`Options.Librarian` supplies the execution engine and MCP host factory the
librarian's extraction turns run in. Without it every extraction fails with a
recorded reason, so a service without an execution engine still registers
projects and reports the failure in status.

With `Options.Threads` set, the service also runs queued workstream turns on its
own and delivers outbox events to each chief of staff (see
[event delivery](#event-delivery)). Event delivery followed by the scheduler
replaces any `Schedule` hook in `Options.Reconciliation`; without
`Options.Threads` that hook runs. In both cases the
[architect controller](#architect-drafting) and then the
[committee controller](#the-shed-committee-rounds) run first.
At the start of every reconciliation pass, `internal/scheduler` reads each
workstream's threads and turn operations. For every thread with no turn in
flight, it publishes a `thread-turn` operation for the oldest unfinished turn,
and the same pass delivers it through the thread dispatcher. A turn is in flight
while its thread holds a claim, or while the turn has an operation and has not
completed. A thread therefore never has two turns in flight, and a message
queued mid-turn runs as the thread's next turn once the current one completes.
Turns already covered by a turn operation that the dispatcher accepts, including
one an embedder published, are left alone; an operation whose input the
dispatcher refuses covers no turn and is retried by the controller. The pass
makes no model call.

While the service runs, embedders accept turns with `EnqueueTurn` alone and let
the scheduler publish the operation. An embedder that publishes its own turn
operation must do so before the service opens the trace, or while an earlier
turn of the same thread is in flight. Otherwise the scheduler can publish first
and the turn gets two operations and two transitions; the dispatcher still runs
it once.

Each dispatch is a transition on the thread's workflow subject
`scheduler.Subject(agent)` (`dispatch_` and the first 40 hex digits of the
SHA-256 of the agent ID). Its ID derives from the agent and turn IDs, its cause
and depth are the request's, its actor is `service`/`scheduler`, and its target
state is the turn ID. After a restart, an in-flight turn is recovered through its
existing operation, as the [turn dispatch](trace.md#turn-dispatch) table
describes: it is neither dispatched again nor lost. `scheduler.Options.Admit` is
the dispatch gate after capacity.

The service's gate declines every turn of the librarian's workstream and of
every `architect` and `committee` thread: the service's own `kb-extract`,
`architect-draft` and `shed-round` reconcilers run those, staged in their own
views, and such a turn the scheduler found queued gets no turn operation.
The gate holds a turn that a pause in `runtime.Effective` covers:
a `factory` pause, a `project` pause on the active project, or a `workstream`
pause on the turn's workstream. Chief-of-staff turns are never held, so the
chief of staff stays reachable while everything is paused. A held turn gets no
operation and stays queued; a turn already in flight is not interrupted, in
either pause mode. The gate reads the runtime store on every pass, so after a
pause is cleared the loop's next periodic pass runs the held turns with no new
message or operation.

The scheduler also dispatches within the configured `[capacity]`. Mason,
reviewer and committee turns share `capacity.masons`, `capacity.reviewers` and
`capacity.committee` across workstreams. Every other role runs one turn at a
time per workstream. Each workstream runs at most the project's
`capacity.per_workstream` turns at once. Chief-of-staff turns take no slot and
run even when every slot is taken. A turn holds its slots while it is in
flight, so they are free again once it completes, whether it succeeded, failed,
is waiting or was cancelled. A claim a restart interrupted holds no slot,
although its thread stays reserved. A parked thread holds no slot. The capacity
check runs before the service's gate, so the gate sees only candidates with a
free slot. A candidate without a free slot stays queued and is offered again on
a later pass, in workstream and agent ID order.

A turn whose outcome is `waiting` parks its thread (`trace.Thread.Parked`). A
parked thread has no unfinished turn, so the scheduler offers it to no gate and
dispatches nothing, and recovery has nothing to run: its turn is complete. The
state is derived from the trace, so it survives a restart. Queuing a new turn
for the thread unparks it, and the next pass runs that turn.

### Event delivery

A workflow transition can carry notification events for its workstream's chief
of staff, and no other thread. The transition and its events commit together,
so a failed transaction leaves no event. `trace.Notice` builds such an event,
and `Repository.SetFeatureState` records a feature state change with one.
`internal/events` delivers them.

At the start of every reconciliation pass, after the architect and committee
controllers and before the scheduler, the deliverer
reads each workstream's ready notification events (events without an
operation). It waits until the oldest has been ready for `events.window` (see
[configuration](configuration.md#top-level-configtoml)), then delivers every
ready event as one chief-of-staff turn. The loop's periodic tick runs the pass
that closes a window. The turn's prompt starts with `events.Preamble`, which
frames the events as information: they grant no permission, trigger no
transition and do not change the chief of staff's tools. It then lists one line
per event with its transition's timestamp, kind and body, oldest first. The
turn's actor is `service`/`events`, its cause is the oldest event's transition,
and its profile is the chief of staff's effective profile: the runtime
override when one is set, which may name any configured profile, otherwise the
role binding. The turn is queued
with `EnqueueTurn`, so a turn in flight on the chief-of-staff thread finishes
first, and the scheduler dispatches it in the same pass otherwise.
The turn's system prompt names the workstream, carries
`questions.Guidance` (what to do with an open question and with the owner's ruling) and the workstream's
[context bundle](context.md) assembled from the local files and trace when the
turn is queued, which is the chief of staff's whole context for a question.

Delivery claims every event with one new attempt token, queues the turn
`events.TurnID(token)`, then acknowledges the events. A crash or restart at any
point neither loses nor repeats an event: an event with a claim whose turn is
already on the chief-of-staff thread is acknowledged without another turn, and
any other unacknowledged event is delivered in the next window. A claim whose
lease ran out before its acknowledgement is settled the same way. A
failing store call, chief-of-staff profile lookup or bundle assembly stops the loop, as the
scheduler's errors do; an event another claim holds is skipped and retried on a
later pass.

### Questions

A role asks with the `ask` tool, and the chief of staff chooses with `answer`
or `escalate`; the [trace reference](trace.md#questions) describes the
records, the tools and the citation checks. An embedder offers the tools of
`questions.Tools` through its turn runner's scoped tools and wraps that runner
in `questions.Turns`, which ends an asking turn with the outcome `waiting`.
The asker's thread then parks as described above: it holds no slot, its
workspace stays, the workstream keeps its feature state, and nothing times
the question out.

`ask` raises one notice event, so the question reaches the chief of staff
through [event delivery](#event-delivery), together with any other event of
the same window. Several questions in one window arrive as one turn, which
lets the chief of staff escalate them as one batch.

In every reconciliation pass, after event delivery and before the scheduler,
`questions.Deliverer` queues each answered question's answer as the asker's
next turn on its original thread, which unparks it; the scheduler dispatches
it in the same pass when a slot is free. The turn's profile is the asker's
role binding, or its runtime override, at delivery. An abandoned workstream
keeps its answers undelivered. A failing trace read or profile lookup stops
the loop, as the scheduler's errors do.

An escalated question waits in the [inbox](#inbox-and-rulings). The owner's
ruling raises one notice event in the question's workstream, so it reaches the
chief of staff as its next event turn. There the chief of staff calls
`relay_ruling`, which records what goes back and its scope and moves every
question of the batch to `answered`; the same delivery pass then queues the
relayed ruling on each asker's original thread.

Questions, choices and deliveries are derived from the trace on every pass. A
restart with an open question delivers its event once the window closes and
asks nothing again. A restart between a recorded answer and its delivery
queues the answer turn once. An escalated question stays escalated and its
asker stays parked until the owner rules. A restart after the owner's ruling
delivers its event once the window closes; a restart after the relay queues
each asker's answer turn once; a restart with an answer turn queued runs it
once.
