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
Client calls have a 15-second response budget, including connection and response
reading. `AddProject` uses a 60-second budget for clone traversal and trace
seeding. A caller's shorter context deadline takes precedence. The service's
read-header, read and write timeouts default to 5, 10 and 10 seconds and can be
set through `Options` by embedders.

| Method | Path after `/v1` | Input / response |
| --- | --- | --- |
| GET | `/health` | Readiness, service name, API version, supplied build version and commit |
| GET | `/config` | Resolved root, loaded effective-config SHA-256 digest, effective validated configuration, project view (null without a project), diagnostics |
| GET | `/runtime` | Effective runtime state, each active project's `context_mode` (`file`; see [context](context.md)), and diagnostics |
| GET | `/status` | `StatusResponse`: every workstream's status and facts in the active project, and diagnostics |
| GET | `/status/<workstream-id>` | `WorkstreamStatus` for one workstream of the active project |
| GET | `/trace/<workstream-id>` | `TraceSummary`: sealed revisions, criteria, unit walks, delivery and explicit gaps |
| GET | `/trace/<workstream-id>/unit/<unit-id>` | `UnitTrace`: document revisions, reports, reviews, rulings, landings, turns, costs, history and gaps |
| GET | `/trace/<workstream-id>/criterion/<spec#n>` | `CriterionTrace`: sealed criterion, assigned unit evidence, rulings, final account, delivery and gaps |
| GET | `/trace/<workstream-id>/commit/<sha>` | `CommitTrace`: records naming the full commit ID, linked landings and their reviewed evidence, delivery and gaps |
| POST | `/conversation/<workstream-id>` | `SendRequest`: text; returns the accepted `ConversationEntry` |
| GET | `/conversation/<workstream-id>` | `ConversationResponse`: the workstream's conversation with its chief of staff |
| GET | `/inbox` | `InboxResponse`: the escalations of the active project that wait for the owner's ruling |
| POST | `/inbox/<number>` | `AnswerRequest`: text; records the owner's ruling and returns `AnswerResponse` |
| POST | `/contested/<workstream-id>/<unit-id>` | `ContestedRulingRequest`: decision (`review` or `revise`) and note; records the owner's direction and returns `ContestedRulingResponse` |
| GET | `/delivery/<workstream-id>` | Current final report, trace-based draft description, any matching approval and the latest publication record |
| POST | `/delivery/<workstream-id>` | Final review number and report revision, commit, draft hash and optional edited description; records the owner's approval |
| POST | `/projects` | `ProjectAddRequest`: name, upstream, fork, clone, optional base_branch; returns `ProjectResponse` |
| DELETE | `/projects` | `ProjectRemoveRequest`: project; returns `ProjectResponse` |
| POST | `/projects/extract` | `ProjectExtractRequest`: project; returns `ExtractionResponse` |
| POST | `/abandon/<workstream-id>` | `AbandonRequest`: reason; returns `AbandonResponse` |
| POST | `/handin` | `HandInRequest`: project, key, one of path, url and stdin, optional skip_debate; returns `HandInResponse` |
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
list is empty. The service does not create workstream identities: activating a
project re-resolves the list from its trace, and a hand-in that creates a
workstream re-resolves it again, so the new workstream's ID takes an override
without a restart.

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
Client transport errors use `unavailable`: a timeout reports `no response within
15s` (or the applicable call budget), a caller context deadline reports `no
response before caller's context deadline`, and an unreachable or closed socket
reports `cannot reach Osmia Unix socket`.

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
Its response write deadline extends to 60 seconds so registration can finish
beyond the server's default 10-second write timeout.
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
`https://github.com/OWNER/REPO/issues/NUMBER`) and `stdin` (the input text),
and optionally `skip_debate`. Checks run in this order, and a refused request writes nothing:

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

With `skip_debate` true the hand-in also records the owner's
[skip of debate](#skip-debate) before the handed state: the `shed-owner`
transition `shed-owner-skip` to `skipped`, with the owner as actor, cause
`handin`, the handed document's timestamp and the reason `the owner skipped
debate at hand-in; the workstream still needs the owner's ratification of the
spec and the plan`, which the [packet](#the-packet) carries as its conclusion.
The reason of the handed transition then ends with `and skipped debate`. The
architect still drafts; the shed controller then
[enters the shed](#entering-the-shed) without a committee, and no round runs.

The response is a `HandInResponse`: `project`, `workstream`, `state`
(`handed`), `handed` (the absolute path of the copy), `source`, and
`skip_debate` when the hand-in skipped debate. A request
repeating a key returns the same response without reading the input again or
writing anything; a hand-in interrupted part way is finished by the retry. The
same key with another source, or other stdin text, returns `conflict` naming
the key and the workstream, and so does the same key with another
`skip_debate` than the hand-in recorded. A storage failure returns `internal` and names the
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
| `drafting-<n>` | Draft `n` is requested: transition `draft-<n>` published an `architect-draft` operation, which the reconciliation loop runs. A draft resumed after the architect's question is in this state again, requested by transition `draft-<n>-resume-<k>`. |
| `waiting-<n>` | Draft `n` is parked: the architect's turn asked a question, and the draft waits for the answer. Transition `draft-<n>-waiting-<k>` and the operation's result, outcome `waiting`, name the question. |
| `invalid-<n>` | Draft `n` was recorded and failed validation. Transition `draft-<n>-invalid` lists every problem in its reason, and the operation's result carries the same text. |
| `failed-<n>` | Draft `n` ran no valid turn: the architect's turn failed, service stops interrupted it three times, the workstream was abandoned, or the workstream left `handed` before the draft was presented. Transition `draft-<n>-failed` holds the reason. |
| `exhausted` | Three drafts were not accepted. Transition `draft-exhausted` records it (`none of the architect's 3 drafts of the spec and plan was accepted; ...`) with a notice for the chief of staff naming the count and quoting the last draft's outcome, and nothing more is requested. |

Draft 1 is requested as soon as the workstream is handed, with the hand-in
transition as cause; after `invalid-<n>` or `failed-<n>` with `n` below three,
draft `n+1` is requested with that outcome's transition as cause. After
`waiting-<n>`, draft `n` is requested again once the answer is queued; see
[the architect's question](#the-architects-question). A draft in progress, a
parked draft whose answer is not queued, an exhausted workstream and a
workstream in any other feature state need nothing. The bound is the
librarian's: three drafts per workstream and three attempts per draft. The
turn that delivers the answer to the architect's question continues its
attempt and spends none.

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

The architect gets `file_read`, `draft_write` and `ask` and nothing else: no notes
(they are the project's, not the workstream's), no write, execute, network or
VCS capability, and no other context source. `draft_write` takes `path` (`spec.md` or `plan.json`) and
`content` (UTF-8 text of at most 512 KiB) and stores the file in the
service-owned directory of the attempt the turn belongs to; it is a memory
tool, so the read-only role holds it.
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
architect's turns run in: its drafts, its replies to shed rounds and the
redrafts the owner asks for. Without it the controller requests nothing, so a
handed workstream stays `handed` until a service with a runner starts and
drafts it. A draft already requested stays pending: applying it returns
`this service has no agent runner for the architect` wherever it would start or
run a turn, the operation is retried, and no draft is spent. A captured turn is
still completed and its draft recorded, since that runs no architect.

### The architect's question

An architect that calls `ask`, while drafting or while [replying or
redrafting](#the-architects-reply) in the shed, ends its turn with the outcome
`waiting`, like any other asker: the question is recorded with a notice for
the chief of staff, the thread parks and the architect's slot is free. What
the asking turn ended with does not matter: a turn that asked and then failed,
or that a service stop interrupted, waits for its answer all the same.

A draft that asks is parked: transition `draft-<n>-waiting-<k>` moves `draft`
to `waiting-<n>` with the reason `draft <n> is parked: the architect waits for
the answer to question <q>`, and the operation ends with the outcome `waiting`
and that reason as evidence. `k` counts the draft's parks from 1. Nothing is
recorded and the workstream stays `handed`, however long the answer takes: a
question has no timeout, and a restart finds the draft parked and leaves it
so. The [answer](trace.md#tools-and-delivery) is queued as turn `answer_<q>`
on the architect's thread; once it is, the controller requests the draft
again: transition `draft-<n>-resume-<k>`, caused by the park it follows, moves
`draft` back to `drafting-<n>` with the reason `draft <n> resumes: the
architect's question is answered` and publishes an `architect-draft` operation
whose input carries `resume: <k>`. The draft number does not advance, and
neither the park nor the answer turn spends one of the three drafts or one of
the three attempts.

The answer turn continues the attempt that asked: it runs through the same
turn path with the same tools, and what it delivers with `draft_write` joins
what the asking turn delivered. The files the attempt delivered are recorded
once its last turn ends normally. An architect that asks again in its answer
turn parks the draft again, with the next `k`. A service stop that interrupts
an answer turn is recovered like one that interrupts an attempt: the draft's
next attempt, while attempts remain, carries the answers the architect
received in the draft under `The answers to the questions you asked in this
draft:`.

A reply or redraft that asks parks the same way, in the shed: transition
`shed-reply-<n>-waiting-<k>` or `shed-redraft-<n>-waiting-<k>` moves the shed
to `asked-<n>` with the reason `the reply to round <n> is parked: the
architect waits for the answer to question <q>` (or `the redraft after round
<n> ...`), and the operation ends `waiting`. Nothing is recorded, and the
debate neither concludes nor starts another round while the shed is
`asked-<n>`. Once the answer is queued, transition `shed-reply-<n>-resume-<k>`
or `shed-redraft-<n>-resume-<k>` moves the shed back to `reply-<n>` or
`redraft-<n>` with the reason `the reply to round <n> resumes: the architect's
question is answered` and publishes the operation again, pinned to the
revision the parked one was, with `resume: <k>`. The answer turn keeps the
answers given before asking, may answer again, and delivers into the same
attempt's redraft; the reply names the last turn that ended. The owner's
actions in the shed keep working while the architect's question is open, and
[skipping debate](#skip-debate) is allowed, since a parked reply holds no
turn; a skipped reply never resumes.

Abandoning the workstream while a draft or reply is parked cancels nothing,
since nothing runs: the answer is not delivered, and `draft` stays
`waiting-<n>`, or the shed `asked-<n>`. A turn that asked while the workstream
was abandoned fails the draft or the reply as any turn of an abandoned
workstream does.

## The shed: debate

The shed controller runs in every reconciliation pass after the architect
controller, before the sealing and building controllers, event delivery and the scheduler. It moves a `sketched`
workstream into the shed and runs its debate to a conclusion: a round of the
committee, the architect's one reply to it, and the next round against what
the architect redrafted, until no dissent stands or the debate reaches its
[round limit](#concluding-the-debate). The committee's contributions to a round are validated as they are
made and recorded once the round ends. The package `internal/shed` holds the
records, the tools and the dissent computation.

The controller is level-triggered. Every pass derives the next step of each
workstream in the shed from the `shed` state and the recorded rounds, and
keeps nothing in memory between passes, so a restart resumes the debate where
the trace says it is:

| `shed` state | Open dissent | Next step |
| --- | --- | --- |
| none | | Round 1, against the latest revisions. |
| `waiting-<n>` | | Round `n` [again](#a-members-question), against its own revision, once every member that asked has its answer queued; nothing while one still waits. |
| `asked-<n>` | | The architect's parked reply or redraft after round `n` [again](#the-architects-question), against its own revision, once its answer is queued; nothing before. |
| `heard-<n>` | none | [Conclude](#concluding-the-debate) by consensus. |
| `heard-<n>` | some | Ask the architect for [its reply](#the-architects-reply) to round `n`. |
| `replied-<n>` | some, `n` below the round limit | Round `n+1`, against the latest revisions. |
| `replied-<n>` | some, `n` at the round limit or above | Conclude at the cap. |
| `concluded-<n>` | | The [redraft the owner asked for](#redraft) after round `n`, then round `n+1`; round `n+1` alone where they asked for [further rounds](#more-debate) and the limit allows it; nothing otherwise. |
| `redrafted-<n>` | | Round `n+1`, against the revisions the redraft wrote. |
| `round-<n>`, `reply-<n>`, `redraft-<n>`, `failed-<n>` | | Nothing. |

Open dissent here is the dissent record without what the owner dismissed or
overruled. The round limit is `shed.max_rounds` until the owner asks for
[further rounds](#more-debate) or for a redraft, and the last round they asked
for from then on. Only a workstream in feature state `in-shed` takes a step, so an abandoned
workstream's debate stays where it stopped. A debate the owner skipped takes no
step at all, except that a `sketched` workstream whose debate was skipped at
[hand-in](#hand-in) enters the shed without a committee. A step that runs turns waits for
the runner of those turns: entering the shed with a committee and every round
for `Options.Committee`, the reply and the redraft for `Options.Architect`.
Concluding runs no turn and waits for neither, so a service with an architect
runner and no committee runner still answers a heard round and still concludes
a debate.

Before any of that, and for a skipped debate too, every pass records the
owner's [edits](#the-owner-in-the-shed) to `spec.md` and `plan.json`, so no
turn ever reads an unrecorded edit. After it, the pass presents the
[ratification packet](#the-ratification-gate) of a workstream whose debate has
concluded or been skipped.

### Entering the shed

For every workstream in feature state `sketched`, except the librarian's, the
controller creates the workstream's committee and moves it `sketched ->
in-shed`: transition `in-shed`, actor `service`/`shed`, cause `sketched`, with
the reason `spec.md revision <s> and plan.json revision <p> enter the shed with
a committee of <N>` and the usual state notice for the chief of staff.

A `sketched` workstream whose debate the owner skipped at hand-in gets no
committee and runs no round. The controller moves it `sketched -> in-shed`
with transition `in-shed`, actor `service`/`shed`, cause `shed-owner-skip`,
the reason `spec.md revision <s> and plan.json revision <p> enter the shed
without a committee: the owner skipped debate`, and a notice that asks the
chief of staff to present the [packet](#the-packet), as the notice of
`osmia shed skip` does. A skip through `osmia shed skip` moves the workstream
itself.

The committee is a fixed set of durable threads of the workstream:
`agent_committee_1` to `agent_committee_<N>` (role `committee`, threads
`thread_committee_<i>`), where `N` is
[`capacity.committee`](configuration.md) when the workstream enters the shed.
Every round runs the threads that exist, and the transition's reason counts
them; a changed configuration does not resize a committee.

The workflow subject `shed` tracks the debate, with transitions by
`service`/`shed` in `events.jsonl`:

| `shed` state | Meaning |
| --- | --- |
| `round-<n>` | Round `n` is requested: transition `shed-round-<n>` published a `shed-round` operation whose input pins the round to one revision of `spec.md` and one of `plan.json`. Every round pins the latest revisions when it is requested. A round resumed after a member's question is in this state again, requested by transition `shed-round-<n>-resume-<k>`. |
| `waiting-<n>` | Round `n` is parked: a member's turn asked a question, and the round waits for the answer. Transition `shed-round-<n>-waiting-<k>` and the operation's result, outcome `waiting`, name the members that wait and their questions. |
| `heard-<n>` | Every member's turn of round `n` has ended and its record is committed. Transition `shed-round-<n>-heard` and the operation's result carry the same summary: members heard, objections, concessions, failed turns and how many objections stand. |
| `reply-<n>` | The architect's reply to round `n` is requested: transition `shed-reply-<n>`, caused by `shed-round-<n>-heard`, published a `shed-reply` operation pinned to the revision the round debated. Its reason counts the objections that stand. A reply resumed after the architect's question is in this state again, requested by transition `shed-reply-<n>-resume-<k>`. |
| `asked-<n>` | The architect's reply to round `n`, or its redraft after it, is parked on the architect's question. Transition `shed-reply-<n>-waiting-<k>` or `shed-redraft-<n>-waiting-<k>` and the operation's result, outcome `waiting`, name the question. |
| `replied-<n>` | The architect's reply to round `n` is recorded. Transition `shed-reply-<n>-replied` and the operation's result carry the same summary: how many objections it answered, and whether it redrafted, left the revision as it is, gave up an invalid redraft or failed. |
| `concluded-<n>` | Debate ended after round `n`. Transition `shed-concluded-<n>` holds why. |
| `failed-<n>` | Round `n` ended without a record, or its reply without one, because the workstream was abandoned. Transition `shed-round-<n>-failed` or `shed-reply-<n>-failed` holds the reason. |

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
| `shed/round-<k>/` | The records of the earlier rounds, the architect's reply to each as `reply.json`, and the owner's own files of the round. |

A member gets `file_read`, `object`, `concede` and `ask` and nothing else: no
notes, no write, execute, network or VCS capability. `object` and `concede`
are memory tools that only the `committee` role can hold; `ask` is the
[question tool](trace.md#tools-and-delivery) every role but the chief of staff
holds. The prompt names the round, the pinned revision, the view, the two
tests and the judgement, the citation forms and when to ask; from round 2 on
it lists the member's own objections that still stand, with their IDs.

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

### A member's question

A member that calls `ask` ends its turn with the outcome `waiting`, like any
other asker: the question is recorded with a notice for the chief of staff,
the thread parks and the member's slot is free. The round does not conclude
without the member. Once every member's turn has ended and one of them
asked, the operation parks the round instead of recording it: transition
`shed-round-<n>-waiting-<k>` moves the shed to `waiting-<n>` with the reason
`round <n> against <revision> is parked: <agent> waits for the answer to
question <q>` (one clause per waiting member, joined by `; `), and the
operation ends with the outcome `waiting` and that reason as evidence. `k`
counts the round's parks from 1. Nothing is recorded, no reply is requested
and the workstream stays `in-shed`, however long the answer takes: a question
has no timeout, and a restart finds the round parked and leaves it so. What
the asking turn ended with does not matter: a turn that asked and then failed,
or that a service stop interrupted, waits for its answer like one that ended
waiting, since the answer is delivered on the thread that asked; the member
gets no new attempt, and what the turn contributed before asking is kept.

The [answer](trace.md#tools-and-delivery) is queued as turn `answer_<q>` on the
member's thread, with the asking turn's system prompt. Once every waiting
member has its answer queued, the shed controller requests the round again:
transition `shed-round-<n>-resume-<k>`, caused by the park it follows, moves
the shed from `waiting-<n>` back to `round-<n>` with the reason `round <n>
against <revision> resumes: every member that asked has its answer` and
publishes a `shed-round` operation whose input pins the round's own revision,
whatever the owner edited since, and carries `resume: <k>`. The resumed
operation runs the queued answer turn through the same turn path, with the
same view and tools, and records the round once every member has ended. The
answer turn continues the attempt that asked: it holds what the member
contributed before asking, its objections are numbered after them, it may
concede any of them, and the member's record names the attempt as its turn. A
member that asks again in its answer turn parks the round again, with the next
`k`. A service stop that interrupts an answer turn is recovered like one that
interrupts an attempt: the member's next attempt, while attempts remain,
carries the answers the member received in the round under `The answers to
the questions you asked in this round:`.

The owner's actions in the shed keep working while a round is parked, the
ones that need no conclusion as during a running round; [skipping
debate](#skip-debate) is allowed too, since a parked round holds no turn, and
a skipped round never resumes. Abandoning the workstream while a round is
parked cancels nothing, since nothing runs: the answer is not delivered, and
the shed stays `waiting-<n>`, as the shed of an abandoned workstream stays
wherever it stopped.

### The record

The tools keep a turn's contributions in the turn's service-owned directory
of the attempt they belong to. Once every member's turn has ended, the
operation records one document per member with `RecordDocuments`, all in one
commit:
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

`service.Dissent` returns a workstream's dissent record from the trace alone:
every objection that stands (`shed.Entry`), with its ID, kind, member, round,
part, argument, citations, the revision it was made against, the owner's
`disposition` and `note` where the owner ruled on it, and whether it is
`blocking`. What an objection's kind means for the debate:

| `kind` | Blocking | Outcome |
| --- | --- | --- |
| `charter` | yes | A veto on the part it names. It stands until its member concedes it after a redraft. |
| `size` | yes | The draft goes back to the architect for a split. It stands until conceded. |
| `proof` | yes | The draft goes back to the architect for a proof. It stands until conceded. |
| `fit` | no | Advice to the owner, carried in the dissent record. It never blocks. |
| `owner` | yes | The [owner's own objection](#the-owner-in-the-shed). The architect answers it like any other, and it stands until the owner disposes of it. |

The owner's disposition overrides the kind: a sustained objection blocks
whatever its kind, and a dismissed or overruled one blocks no longer.
Consensus is a dissent record with no entry the owner has not disposed of,
advice included. It is never a vote,
and no member reports a confidence.

Recovery keys on the trace: a restart during the round finds the turns that
ended in their threads and runs only the members that had not finished, each
up to three attempts; one between the record and the transition finds the
round's files and records nothing again; a recorded outcome, the park of a
round included, completes the operation without running a member.

[Abandoning](#abandoning) the workstream cancels the members' running turns.
The round of an abandoned workstream records no file and ends `failed-<n>`
with the reason `round <n> failed: the workstream was abandoned, so the
committee is not heard`. A round whose files were committed before the
workstream was abandoned still ends `heard-<n>`, so the shed state never
contradicts the record.

`Options.Committee` supplies the execution engine and MCP host factory the
committee's turns run in. Without it no round is requested and no workstream
enters the shed except one whose debate the owner skipped at
[hand-in](#hand-in), which needs no committee. Any other sketched workstream
stays `sketched`, and a debate
whose next step is a round stays `replied-<n>`, until a service with a runner
starts. A round already requested stays pending: applying it returns `this service has
no agent runner for the committee` wherever it would start or run a turn, the
operation is retried, and no member's attempt is spent.

### The architect's reply

A heard round with open dissent gets one reply from the architect: one
`shed-reply` operation, run by the service's own reconciler on the
workstream's `agent_architect` thread. A round that ends with no dissent gets
none. Turn `reply-<n>-<k>` carries the operation ID as cause and the
architect's effective profile. Every committee turn of the round has ended by
then, so the committee holds no slot while the architect answers, nor between
rounds.

The turn gets a read-only private copy of a view staged under
`<root>/architect/<project-id>/<workstream-id>/<turn>/workspace`:

| Path in the view | Content |
| --- | --- |
| `spec.md`, `plan.json` | The latest recorded revisions. |
| `handed/<name>`, `charter.md`, `context.md` | As for [drafting](#the-turn). |
| `shed/round-<k>/` | Every member's record of every round so far, the architect's earlier replies and redrafts, and the owner's own files of the round. |
| `redraft/spec.md`, `redraft/plan.json` | After an invalid redraft, the files of it the architect delivered. |

The architect gets `file_read`, `reply`, `draft_write` and `ask` and nothing
else; see [the architect's question](#the-architects-question).
`reply` is a memory tool only the `architect` role can hold. It takes
`objection`, the ID of an objection that stood once the round was heard, and
`answer`; answering an objection again replaces the earlier answer, and an
answer that is refused is an ordinary result, `{"recorded":false,"reason":...}`.
The prompt lists every objection the owner has not disposed of with its ID,
kind, whether it blocks, member, round, and the part and citations it has, and
says what each kind asks of the architect.

The architect redrafts by delivering a changed `spec.md`, `plan.json` or both
with `draft_write`; a delivered file equal to the latest revision is no
change. The redraft is validated as a [draft](#recording-and-validation) is,
together with the latest revision of the file that was not delivered, before
anything is recorded:

- A valid redraft is recorded as the next revision of each file it changed
  (actor `agent`/`agent_architect`, cause the operation ID), in one commit with
  the reply; a file it left alone keeps its revision. The reply's `redraft`
  names both revisions, and the next round is pinned to them.
- An invalid redraft is never recorded, so it is never the revision the
  committee debates. It goes back to the architect: the next turn of the same
  operation opens with `Your redraft was not accepted, and the committee will
  not read it:` and every problem, and finds what it delivered under
  `redraft/`. Only what that turn delivers counts, and delivering nothing
  leaves the revision as it is. After three redrafts the last one is given up:
  the reply records its `problems` and the revision stays.
- A valid redraft of a file the owner has edited, whose edit no revision
  records yet, is given up the same way rather than written over the owner's
  file: the reply records the `problems` that say so, and the revision stays.
  See [the owner in the shed](#edit).

The reply is one document, `shed/round-<n>/reply.json` (record ID
`shed-round-<n>-reply`, actor `agent`/`agent_architect`, cause the operation
ID). It holds the round, the `revision` the round debated, the last turn, the
`answers`, the `redraft` revisions when a redraft was recorded, the `problems`
of a redraft that was given up, and the `failure` of a turn that did not end
normally. A failed turn, and one interrupted by three service stops, is the
round's reply all the same: it is recorded with the failure, its redraft is
dropped, and the debate goes on. Answers are merged over the operation's
turns; an interrupted turn contributes none.

Recovery keys on the trace: a restart during the turn finds it interrupted and
starts the next one; one between the record and the transition finds
`reply.json` and records nothing again; a recorded outcome completes the
operation without running the architect. [Abandoning](#abandoning) the
workstream cancels the running turn, and the reply of an abandoned workstream
records no file, whether its turn was cancelled, never started or ended while
the owner abandoned the workstream, and ends `failed-<n>` with the reason `the
reply to round <n> failed: the workstream was abandoned, so the architect's
reply is not recorded`. A reply whose file was committed before the workstream
was abandoned still ends `replied-<n>`.

Without `Options.Architect` the controller asks for no reply and no
[redraft](#redraft), so the shed stays `heard-<n>` or `concluded-<n>` until a
service with a runner starts. One already requested stays pending: applying it
returns `this service has no agent runner for the architect` wherever it would
start or run a turn, and no attempt is spent.

The redraft the owner asks for takes the same turn path, with its own
operation, transition, states and record; everything this section says of a
reply holds for it, except that what it answers is the owner's note.

### Concluding the debate

Debate ends early by consensus, as soon as a heard round leaves no dissent
open: transition `shed-concluded-<n>`, caused by `shed-round-<n>-heard`, with
the reason `debate concluded by consensus after round <n>: no objection
stands`. Consensus is among the members whose turns ended normally: when some
turns of the round failed the reason adds `; the turns of <f> of <m> members
failed`, and when all of them failed nothing was reviewed and the reason is
`debate concluded after round <n> without a review: the turns of all <m>
members failed, so no objection stands and nobody agreed`. Otherwise it ends
at the cap, once the reply to the round limit is recorded: caused by
`shed-reply-<n>-replied`, with the reason `debate stopped after round <n>, at
the shed.max_rounds cap of <max>, with <k> objections standing, <b> of them
blocking; the cap approves nothing` (`1 objection standing` for one). The round limit is `shed.max_rounds`, read
from the loaded configuration when the step is taken, until the owner asks for
[further rounds](#more-debate) or for a [redraft](#redraft); from then on it is
the last round they asked for, and the reason names it instead: `at round <n>,
the last of the further rounds the owner asked for` where a request for further
rounds reaches the limit, and `at round <n>, the round that debated the redraft
the owner asked for` where the redraft does.

Either way the workstream stays `in-shed`, the dissent that stands keeps
standing, and no later pass starts a round. The conclusion commits with a
notice for the chief of staff (event key `concluded`), delivered through the
[outbox](#event-delivery): `Debate concluded: <reason>. The workstream stays
in-shed until the owner decides.` When dissent stands, that is followed by
`Open dissent:` and one line per entry of the dissent record with its ID,
kind, the owner's disposition where there is one, `blocking` or `advisory`,
member, round, the part it has, revision and argument; a conclusion by
consensus has no such line. The notice ends with the
[packet](#the-ratification-gate) to present and the recommendation in it, and
asks the chief of staff to make the decision the owner has to take now the
attention of its status. A debate whose every standing
objection the owner disposed of concludes with the reason `debate concluded
after round <n>: the owner disposed of every objection that stood`, and its
notice lists them with their disposition.

## The owner in the shed

The owner takes part in the debate directly, through six API calls and their
CLI wrappers. Each is recorded with the owner as actor (`owner`/`local`), and
each transition reaches `events.jsonl` with a notice for the chief of staff.
The workflow subject `shed-owner` tracks them, separately from `shed` so that
an owner action never races a running round:

| `shed-owner` state | Meaning |
| --- | --- |
| `objected-<n>` | The owner objected in round `n`. |
| `ruled-<n>` | The owner ruled on an objection in round `n`. |
| `overruled-<n>` | The owner overruled an objection in round `n`. |
| `more-<n>` | The owner asked for further rounds after debate concluded at round `n`. |
| `redraft-<n>` | The owner asked the architect for a redraft after debate concluded at round `n`. |
| `ratified-<n>` | The owner [ratified](#the-ratification-gate) the spec and the plan in round `n`. |
| `skipped` | The owner skipped debate, in the shed or at [hand-in](#hand-in). No committee turn starts again. |
| `invalid-edit` | An owner edit was read, found invalid and not recorded. |

The round an action is recorded under is the round the `shed` state has
reached, or round 1 before the first round runs. The owner's objections,
rulings, requests for a redraft and ratifications of a round keep the revision
the file was opened against, as a member's record keeps the revision its round
was pinned to; a request for further rounds is about rounds, not revisions, and
records none.

`skipped` is the value of the action alone: a later objection, ruling or
reported edit moves the subject on, and none of them un-skips the debate,
which stands on its recorded `shed-owner-skip` transition.

### Object

`POST /v1/shed/object/<workstream-id>` with `{"argument": "..."}` adds the
owner's own objection to the current round. It is recorded in
`shed/round-<n>/owner.json`, a record of the same shape as a member's under
the member name `owner`, with kind `owner` and the ID `owner-r<n>-<k>`. Unlike
a member's objection it needs no part and no citation. It stands in the dissent
record and blocks, the architect answers it in its reply to the round like any
other, and no member's turn settles it: the owner disposes of it with a ruling
or an overrule.

### Rule

`POST /v1/shed/rule/<workstream-id>` with `{"objection": "<id>",
"disposition": "sustain"|"dismiss", "note": "..."}` rules on one objection that
stands. The rulings of a round are one document, `shed/round-<n>/rulings.json`,
each with the objection, the disposition and the owner's note; disposing of the
same objection again replaces the earlier disposition. A sustained objection
blocks whatever its kind, until its member concedes it after a redraft. A
dismissed one stays in the dissent record with its disposition, no longer
blocks, and neither the architect answers it again nor does the debate run on
for it.

### Overrule

`POST /v1/shed/overrule/<workstream-id>` with `{"objection": "<id>",
"reason": "..."}` overrules one objection that stands, charter vetoes included.
It is a disposition of the same kind as a ruling, `overruled`, recorded in the
same `shed/round-<n>/rulings.json` against the revision the documents are at,
and it settles the objection exactly as a dismissal does. The two differ in
what they say: a dismissal settles an objection while debate runs, and an
overrule is the owner's decision to proceed in spite of it at the
[ratification gate](#the-ratification-gate).

### Redraft

`POST /v1/shed/redraft/<workstream-id>` with `{"note": "..."}` sends the spec
and the plan back to the architect once debate has concluded. The request is
recorded in `shed/round-<n>/redraft.json` against the round it concluded at,
with the revisions it was made against and the owner's note, and makes the
round limit `n+1`. The controller then asks the architect for the redraft as
one turn (state `redraft-<n>`, operation `shed-redraft`, transition
`shed-redraft-<n>`, or `shed-redraft-<n>-resume-<k>` for a redraft resumed
after the architect's question), recorded as `shed/round-<n>/redrafted.json` with the
revisions it wrote, as its reply to a round is recorded; the shed moves to
`redrafted-<n>` and round `n+1` debates what the architect wrote, whether the
redraft changed anything or not. The turn carries the owner's note and the
dissent that stands, and is bounded by the drafting bounds of a reply: an
invalid redraft comes back to the architect, and a redraft the owner's own
unrecorded edit stands in the way of is given up. The request is refused with
`conflict` while debate has not concluded, on a skipped debate, and on a
conclusion the owner already asked a redraft for; an empty note is a
`validation` refusal.

### Edit

The owner edits `spec.md` and `plan.json` in the workstream's directory under
the trace; no command records them. Every shed pass reads both files first: a
file that differs from the latest recorded revision is recorded as the next
revision, actor `owner`/`local`, cause `owner-edit`, before any turn reads it,
and the next round debates it. An edited `plan.json` is validated exactly as
the architect's draft is, against the other document as the file leaves it:
the two are one draft, so an edit that does not validate records neither file,
the recorded revisions stay the ones under debate, and the problems are
reported once to the chief of staff as a notice (transition
`shed-owner-invalid-<digest>`, state `invalid-edit`). Reading records nothing
when nothing was edited.

While a file holds an edit no revision records, a redraft of it by the
architect is [given up](#the-architects-reply) rather than written over the
owner's file, so the reply is recorded and the debate goes on with the revision
it debated.

### Skip debate

`POST /v1/shed/skip/<workstream-id>` records that debate is skipped. A
`sketched` workstream enters the shed without a committee; one already in the
shed stays there. No further committee turn or architect reply starts, and the
workstream still needs the owner's ratification of both documents. It is
refused while a round or a reply is running, and on a debate already skipped,
including one skipped at [hand-in](#hand-in) with `skip_debate`, which skips
debate before round 1 can start. A round [parked on a member's
question](#a-members-question), and a reply or redraft [parked on the
architect's](#the-architects-question), is not running: the skip is recorded
and it never resumes.

### More debate

`POST /v1/shed/more/<workstream-id>` with `{"rounds": <k>}` asks for `k`
further rounds once debate has concluded. The request is recorded in
`shed/round-<n>/more.json` against the round it concluded at, and makes the
round limit `n+k`; the controller resumes from `concluded-<n>` with round
`n+1`. A request replaces `shed.max_rounds` rather than adding to it, so the
rounds the owner asked for are the rounds that run, whether that is beyond the
configured cap or short of it. `k` must be between 1 and `shed.max_rounds`. It is refused while debate
is still running, on a debate that never concluded, and on a skipped one. A
conclusion the owner did not follow with a request stays a conclusion.

Every action is refused with `conflict` unless the workstream is `in-shed`
(`sketched` or `in-shed` for skip and ratification), with `validation` for an
empty argument, an unknown disposition, an empty note or a round count out of
range, and with `not_found` for an objection that does not stand. The response
is a `ShedResponse`: `project`, `workstream`, `action`, `round`, the
`objection` an objection or disposition concerns, the `rounds` a request asked
for, and the recorded `detail`.

## The ratification gate

Debate that concluded, and debate the owner skipped, put the decision to the
owner. The shed pass presents it as the ratification packet, and
`osmia ratify` is the decision.

### The packet

Every shed pass records the packet of a workstream in the shed whose debate has
concluded or been skipped, as the next revision of
`shed/round-<n>/packet.json` (record ID `shed-round-<n>-packet`, actor
`service`/`shed`, cause `ratification-packet`) whenever what it says differs
from the revision already recorded. A workstream at no decision point has no
packet. A packet holds:

- `round`, the round debate ended after, and `skipped`, whether the owner
  skipped it;
- `revision`, the revisions of `spec.md` and `plan.json` the owner decides on;
- `conclusion`, the recorded reason debate ended;
- `dissent`, the dissent record with every entry that blocks ratification
  first, each marked `blocking` or advisory and carrying the owner's
  disposition where there is one;
- `recommendation`: `ratify: no objection stands`, `ratify: nothing blocks, and
  <k> objections stand as advice on the record` (`1 objection stands` for
  one), or `do not ratify yet:
  ratification is blocked by <k> objections (<ids>); overrule or sustain each
  one, or ask for a redraft`.

Because the pass records a new revision whenever the packet changes, every
owner action that changes what it says is followed by the packet that says it.
`GET /v1/packet/<workstream-id>` serves the latest recorded packet of the
latest round with its revision and the time it was recorded, and `not_found`
for a workstream that has none.

### Ratifying

`POST /v1/ratify/<workstream-id>` with `{"spec": <s>, "plan": <p>}` ratifies
exactly those revisions. `osmia ratify` reads the packet and pins the revisions
it names, so a ratification of revisions that have moved since is refused
rather than silently applied to what the owner did not read.

Ratification is refused with `conflict`, and every reason that applies is
listed in the one refusal:

- `<asked> are ratified, and the current revisions are <current>: read the
  packet again`;
- `objection <id> (<kind>, by <member> in round <n> on <part>) blocks and has
  no disposition`, or `... blocks and is sustained and is not conceded`, one
  line per entry of the dissent record that blocks;
- `the plan is not valid: <problem>`, one line per problem of the recorded spec
  and plan, validated exactly as a draft is;
- `the workstream is sketched and debate is not skipped: it is ratified in the
  shed`.

A workstream that is neither `in-shed` nor `sketched` is refused by the state
check every owner action of the shed makes, and revisions below 1 with
`validation`. Revisions already ratified are not recorded again while their
sealing is requested, pending or running: that call reports the sealing the
record asked for, and records the ratification again only once that sealing
has failed, as described below.

What passes is recorded in `shed/round-<n>/ratification.json` (actor
`owner`/`local`, cause `owner-shed`) with the revisions it approves, the
dissent record it was given over and the dispositions in force, and the owner
subject moves to `ratified-<n>`; the reason is `the owner ratified <revisions>
after round <n>`, followed by `, over <k> objections the owner disposed of`
where there were any. The gate itself changes no state beyond that record: the
record is the request for the [sealing](#sealing), which moves the workstream
on. The response is a `RatifyResponse`: `project`, `workstream`, `round`,
`spec`, `plan`, `sealing` and the `detail`, which is the recorded reason
followed by `; the sealing is asked for`.

`sealing` is the state of the sealing the record asked for: `requested` until
the sealing controller publishes the operation, then `pending` while it is
queued or waiting to retry and `running` while it runs. Ratifying revisions
already ratified records nothing while their sealing is requested, pending or
running, and answers with the detail `workstream <id> is ratified at
<revisions> already; the sealing is asked for`, or `...; sealing <k> is
pending` (followed by ` after a failed attempt: <reason>` once an attempt has
failed) or `...; sealing <k> is running`. Once that sealing has
[failed](#what-a-sealing-refuses), the same call records the ratification
again as the next revision of its file, with the owner subject moving to
`ratified-<n>` again and the reason `the owner ratified <revisions> after
round <n> again; sealing <k> failed and the sealing is asked for again`; the
detail is `workstream <id> is ratified at <revisions> already; sealing <k>
failed and the sealing is asked for again`, which the CLI prints, and
`sealing` is `requested` again. That is how the owner asks for a sealing after
one failed, once whatever failed it is put right.

### Sealing

Ratification ends with the seal of design §5.1: the upstream commit and the
hash of the ratified spec are recorded, the plan's footprints are taken, and
the feature branch is created in a workspace the service owns on the project's
clone. The service performs the version control; no agent creates a branch,
and nothing is pushed. The sealing controller runs in every reconciliation
pass after the shed controller, and its reconciler runs each sealing as a
durable operation on the repository boundary, so a restart neither loses nor
repeats one.

#### Asking for a sealing

The controller reads the owner's latest ratification of every workstream that
is `in-shed` or `sketched`: the record of the latest round at its latest
revision. When no sealing of that record was asked for, it publishes one as
the operation `seal` (input `seal`, `round`, `spec`, `plan` and
`ratification`, the record's revision) on the workflow subject `seal`, which
tracks a workstream's sealings:

| `seal` state | Meaning |
| --- | --- |
| `sealing-<k>` | Sealing `k`, the latest asked for, is queued, running or waiting to retry. |
| `failed-<k>` | Sealing `k`, the latest asked for, failed for a reason a retry does not put right. |

The request is the transition `seal-<k>` (actor `service`/`sealing`, cause the
ratification record's transition, `shed-round-<n>-ratification-<r>`) with
the reason `the owner ratified <revisions> in round <n>; sealing <k> fetches
upstream, records the seal and the footprints and creates the feature branch`.
`k` is the number after the highest sealing asked for on the workstream,
whatever the subject reads, so no number is ever reused. A sealing that
succeeds is recorded by the feature state it moves, not by the subject. A
sealing that was still queued or retrying when a later one was asked for
finds itself superseded when it runs: it records its failure (the transition
`seal-<k>-failed` below) and leaves the subject to the later sealing, moving
it nowhere. A service stop between the record of a ratification and the
request leaves nothing owed but the next pass; one after a request leaves the
operation pending for the next service, which inspects the clone before
retrying. A ratification whose sealing failed is not asked for again by the
controller: the owner asks by ratifying again, which records the ratification
as a new revision, and the controller seals that record.

#### What a sealing does

Each attempt, in order:

1. Reads the ratification the operation names and the workstream's state,
   and [refuses](#what-a-sealing-refuses) what cannot be sealed.
2. Reads the ratified revisions of `spec.md` and `plan.json`, loads the
   [entity map](knowledge-base.md) and resolves every unit's footprint:
   the entities it names and every entity that is part of them, with their
   path patterns, exactly as `kb.Map.ResolveEntities` resolves them. A name the
   map does not resolve to an entity with a path pattern refuses the sealing.
3. Finds the clone's remote whose URL names the project's `upstream`
   (`owner/repository`, however the URL spells it) and fetches its
   `base_branch` into `refs/remotes/<remote>/<base_branch>`.
4. Takes the feature branch `osmia/<workstream-id>`. A branch of that name
   the clone already holds is the feature branch an earlier attempt created:
   its commit is the seal, and it must be on the fetched branch. Otherwise the
   branch is created from the fetched commit.
5. Checks the branch out in the workstream's workspace,
   `<root>/branches/<project-id>/<workstream-id>`, a Git worktree of the
   clone, unless the clone has that worktree already. That worktree alone is
   forgotten and made again when its directory is gone; the owner's other
   worktrees are never pruned. A directory in the way that is no worktree of
   the clone is reported and left alone.
6. Records `seal.json` as the workstream document `seal` (actor
   `service`/`sealing`, cause the operation ID), unless this sealing recorded
   it before the attempt was interrupted, and moves the feature state to
   `ratified` from `in-shed`, or from `sketched` when the owner skipped debate
   on a workstream that never entered the shed, in one transition (ID
   `ratified`, cause the operation ID) with the chief of staff's notice. The
   reason is `sealed <revisions> at <commit> of <remote>/<base_branch>
   (<spec hash>); feature branch <branch> is checked out in <workspace>; the
   footprints of <n> units are recorded`, and the operation's result carries
   it as evidence.

`seal.json` holds `version` 1, `seal` (the sealing's number), `round`,
`revision` (`spec` and `plan`), `spec_hash` (`sha256:` and the hex digest of
the spec revision's content), `base` (`remote`, `branch`, `commit`), `branch`,
`workspace` and `footprints`, one per unit in plan order with `unit`,
`entities` and `paths`. Every sealing that completes records the next revision
of the file; the seal in force is the latest.

A fetch that fails, a remote the clone lacks, and a branch or worktree that
cannot be created are infrastructure: the attempt returns the error, the
operation records a retry with it and the next attempt starts again from what
the clone holds, so an interrupted or failed attempt never creates a second
branch or workspace. The workstream stays in the shed until every step has
succeeded. Git runs in the clone with hooks disabled and no terminal prompt,
with the owner's Git configuration, SSH agent and SSH command in reach,
because fetching their remotes may need their credentials; unless the owner
set `GIT_SSH_COMMAND`, `GIT_SSH` or `core.sshCommand`, the fetch runs SSH in
batch mode, so a passphrase or an unknown host key fails the fetch instead of
waiting for a terminal. No session ever runs it.

#### What a sealing refuses

A sealing that finds one of these records the transition `seal-<k>-failed`
(cause `seal-<k>`) on the seal subject, to `failed-<k>` while the subject
reads `sealing-<k>` and to the value it already has otherwise, with the
reason `sealing <k> of <revisions> failed: <why>`, a notice for the chief of
staff (`The sealing of <revisions> failed and the workstream is not ratified:
<why>. Once that is put right, ratifying the same revisions again asks for
the sealing again.`) and the failure as the operation's result, and the
workstream keeps its state:

- `the workstream is <state>, not in the shed`: it is abandoned, or otherwise
  past the shed, when the attempt starts; or it moved while the attempt ran,
  found when the move to `ratified` refuses.
- `the owner abandoned the workstream; its feature branch <branch> stays in
  the clone`: the workstream was abandoned while the attempt ran, found by the
  check made again right after the branch is created, before the seal is
  recorded. The branch stays, as the design keeps an abandoned workstream's
  branch. An abandonment that lands between that check and the record leaves
  `seal.json` on the abandoned workstream too; the move to `ratified` refuses
  it all the same, and the workstream stays abandoned.
- `the owner's latest ratification is of <revisions> in round <n>, not of
  <revisions> in round <n>`: the owner ratified other revisions since, and
  that ratification is sealed instead; `the owner ratified <revisions> in
  round <n> again after this sealing was asked for; the later record is sealed
  instead` when they ratified the same ones again.
- `the ratified plan does not parse: <error>`.
- `the plan's footprints name what the entity map does not resolve: <unit>:
  <name>, ...`: the entity map changed since the plan was validated.
- `the clone has a branch <branch> at <commit> that is not on
  <remote>/<base_branch>; it is not the service's feature branch`: a branch of
  the feature branch's name the service did not create, or one whose history
  upstream no longer holds; move it away and ratify again.

## Building

A sealed workstream is built unit by unit. The building controller runs in
every reconciliation pass after the sealing controller. For every `ratified`
workstream whose latest `seal.json` revision no build was asked for, it
publishes the operation `build` (input `seal`, the sealing's number) on the
repository boundary, with the transition `build-<k>` (actor
`service`/`building`, cause `seal-<revision>` of the seal document) that moves
the workflow subject `build` to `requested-<k>` with the reason `seal <k> is
recorded; the build records the state of every unit of the sealed plan and
moves the workstream to building`.

The operation reads the plan revision the seal names and, in one commit,
records each unit's state on its own workflow subject, `unit-<unit-id>` (see
[`UnitSubject`](trace.md#atomic-workflow-state-and-outbox)), and moves the
feature state from `ratified` to `building` with the chief of staff's notice.
Every transition has the actor `service`/`building` and the operation ID as
its cause:

- `unit-<id>-planned` moves each unit from no state to `planned`, with the
  reason `unit <id> is in the plan of seal <k>`, followed by ` and waits for
  <ids> to merge` for a unit with dependencies.
- `unit-<id>-ready` then moves each unit that depends on no unit from
  `planned` to `ready`, with the reason `unit <id> is ready: it depends on no
  unit`. A unit becomes `ready` once every unit it depends on has merged; no
  unit has merged when the build runs, so a unit with dependencies stays
  `planned` until [landing](#landing-a-unit) merges the last of them.
- `building` moves the feature state, with the reason `seal <k> is recorded;
  the states of the <n> units of its plan are recorded: ready <ids>; planned
  <id> (waiting for <ids>), ...` (`none` for an empty list). The operation's
  result carries it as evidence.

The unit states are those of design §5.2: `planned`, `ready`,
`implementing`, `reviewing`, `approved` and `merged`, plus `waiting` and
`contested`. The build records `planned` and `ready`.

A build is idempotent by its operation: its inspection finds the move to
`building` it caused and completes the operation with it, and an attempt that
finds that move records nothing more. The build fails, recording no
transition and with the reason as its result (`building on seal <k> failed:
<why>`), when the workstream is no longer `ratified` (`the workstream is
<state>, not ratified`, as after the owner abandoned it), when a later seal
replaced its seal (`it is not the latest seal of the workstream`), when the
sealed plan does not parse (`the sealed plan does not parse: <error>`), and
when the workflow changed between its reads and its commit, because the owner
moved the workstream or a unit already has a state (`the workflow changed
while the build ran; the workstream is <state>`).

### Starting units

The `[mason] max_clean_turns` setting defaults to 3 and must be positive. A
mason turn that ends cleanly without `ask` or `done` is classified from its
final response and tool counts. The controller records a chief-of-staff event
for every such turn. For `asked_in_prose` it queues a turn on the same thread
that names `ask`; for `claims_done` it names `done`. An `unclear` response gets
a turn naming both tools. `gave_up` contests the unit immediately. When the
number of clean turns without an outcome reaches `max_clean_turns`, the unit
becomes `contested` with a bound-exhaustion reason instead of receiving another
turn. These decisions are read from owned responses after a restart.

With `Options.Threads` set, the mason controller runs in every reconciliation
pass, after answer delivery and before the scheduler. It first
[parks and resumes](#a-masons-question) units on their masons' questions.
It then starts `ready` units of `building` or `assembled` workstreams while
fewer than `capacity.masons` units are `implementing` in workstreams no pause
covers. A workstream starts no unit while `capacity.per_workstream` of its
units are `implementing`, whether or not a turn of theirs is queued or
running. A `waiting` unit counts toward neither limit. A
workstream a runtime pause covers (a `factory` pause, a `project` pause on the
active project or a `workstream` pause on it) starts no unit, and its
`implementing` units take no mason slot, so the slots go to workstreams that
are not paused. Before it starts anything, a pass reads every `implementing`
unit from the trace, so after a restart the units in flight hold their slots
and none is started twice.

Each free slot goes first to the workstream earliest in the project's priority
order (`PUT /v1/runtime/priority`), then to those it does not name; among
equals, to the workstream that started a unit least recently, one that never
did first (a unit resuming from `waiting` is not a start), then in workstream
ID order. The order is taken again after every start, so equals take turns
within one pass. A workstream is passed over when it has no unit to start. In
a workstream the controller takes the first `ready` unit in the plan's
dependency order (every unit follows the units it depends on, and otherwise
keeps its place in the plan) that is entangled with none of the workstream's
`implementing` or `waiting` units. Two units are entangled when one depends
on the other, when their footprints resolved through the entity map
(`kb/entities.json`) intersect, or when either footprint does not resolve to
exactly one entity per name. An entangled unit stays `ready` and keeps its
place, and a later `ready` unit that is disjoint from every unit in flight
starts ahead of it. Units in `reviewing`, `approved` or `contested` do not
hold back an entangled unit: the foreman
[rebases](#rebasing-units-in-flight) the units in flight after each landing.

Starting a unit assembles its mason bundle from the sealed spec and plan, opens
the unit's workspace, then records the transition `unit-<id>-implementing`
from `ready` to `implementing` (actor `service`/`mason`, cause
`unit-<id>-ready`) with the reason `unit <id> is the next ready unit of the plan
of seal <k>; its mason works in the unit's workspace on <unit-branch>, created
from <feature-branch> at <commit>`. The transition carries one
[notice](#event-delivery) for the chief of staff, `Unit <id> is implementing:
its mason works on it in its unit workspace on <unit-branch>.` A unit that
resumes from `waiting` raises no notice: its question and the answer that
resumed it are the chief of staff's own choices. It then creates the unit's
mason thread,
`mason-<id>` with the role `mason`, and queues its first turn,
`mason-<id>-implement`, with the role's profile, the unit's bundle in its
prompt and the transition as its cause. The scheduler dispatches that turn
like any other thread turn. A unit is started once: the transition ID is
fixed and a unit already `implementing` or `waiting` is never started again. A unit found `implementing` without its first mason turn, as after a
stop between the two, gets its workspace and that turn on the next pass.

A unit is blocked when its spec no longer matches its seal (the bundle
refuses it) or when its workspace cannot be opened, as when a directory the
clone does not know is in its place. A blocked `ready` unit is not started and
stays `ready`, with no workspace or thread made for it; a blocked
`implementing` unit gets no turn. Either way the unit takes no mason slot, its
workstream starts no other unit, the other workstreams go on, and the
controller tries again on every pass, so the unit starts once the cause is
gone. Why is recorded on the workflow subject `blocked-mason-<id>`
(`blocked-mason_<hash>` for a unit whose subject is hashed): the transition
`<subject>-<k>` moves it to `blocked-<k>` (actor `service`/`mason`) with a
notice for the chief of staff, `The mason controller is blocked: <reason>. It
tries again on every pass.` A reason the subject's latest transition already
records is not recorded again. The reasons are `unit <id> stays ready: its
mason bundle cannot be assembled: spec does not match its seal: ...` and
`unit <id> stays ready: its workspace cannot be opened: <error>`, and for an
`implementing` unit the same two with `unit <id> is implementing and its
mason's first turn is not queued` in place of `unit <id> stays ready`.

### A mason's question

A mason holds `ask`. Its question is recorded and reaches the chief of staff
through [event delivery](#event-delivery) like any other
[question](#questions), and the mason's turn ends with the outcome `waiting`,
which parks its thread. The mason controller then records
the transition `unit-<id>-waiting-<q>` from `implementing` to `waiting` (actor
`service`/`mason`, cause `question_<q>_open`) with the reason `unit <id> is
waiting: its mason asked question <q>; the unit's workspace is kept and it
takes no mason slot until the answer arrives`. The unit's workspace stays as the turn
left it. A `waiting` unit takes no mason slot and does not count toward
`capacity.per_workstream`, so a free slot goes to another unit, of its own
workstream when one is disjoint from every unit in flight there. The waiting
unit still holds back the units entangled with it. Nothing times the question
out.

The chief of staff answers the question or escalates it to the owner, whose
ruling it relays. The answer is then queued as the mason's next turn on its
thread, `answer_<q>`, with the asking turn's system prompt, on a view of the
same workspace. In the same pass, before the scheduler dispatches that turn,
the mason controller records `unit-<id>-implementing-<q>` from `waiting` back
to `implementing` (cause `question_<q>_answered`) with the reason `unit <id>
resumes implementing: the answer to question <q> is its mason's next turn`. A
unit resumes whatever the free slots, so more than `capacity.masons` units, or
more than `capacity.per_workstream` units of one workstream, can be
`implementing` for a while; the scheduler still runs at most `capacity.masons`
mason turns at once, and no unit starts until fewer units are `implementing`. A mason that asks again in its answer turn parks the unit
again, on the transition named after its new question.

Parking and resuming are derived from the unit's state, the mason's thread
and the workstream's questions on every pass, so a parked unit, its question
and its answer survive a restart, and a unit whose state moved since the pass
read it is left to the next pass.

If a service stop interrupts a mason turn, its surviving file view is copied
into the same unit workspace before the interrupted thread claim is released.
The service then queues one continuation on that thread with the recovered
files. If the interrupted turn asked a question, the unit waits for the answer
turn instead. The unit keeps its workspace and is never started as a new unit.

### Finishing units

A mason turn ends its unit's work with the Osmia tool `done`, which only the
mason holds. It takes `outcome`, what the unit's work now does, and
`criteria`, one entry for every criterion the unit addresses in the sealed
plan: `criterion` as the plan cites it (`spec#<n>`), `done` (what the mason
did), `evidence` (why the criterion holds) and `proof` (where the proof
lives). No argument names a state, and the outcome is text: whatever it
says, an accepted report sends the unit to `reviewing`, never further.

`done` also takes an owner-facing card: `headline` (required, at most 64
characters), `happened` (required, at most 140 characters, naming concrete
work and observable results) and `needs_you` (at most 140 characters, empty
unless the owner has a specific action). The service collapses whitespace,
removes control and zero-width characters, and refuses line breaks and
identifiers using the same rules as `set_status`. A missing or invalid card
field produces an ordinary `{"recorded":false,"reason":"…"}` result naming
that field. No field is silently truncated.

A report the service refuses is an ordinary tool result,
`{"recorded":false,"reason":"<reason>"}`, so the mason reads why, fixes the
report and calls `done` again in the same turn. The unit does not move and
the turn does not fail. The reasons are `outcome is required: say what the
unit's work now does`, `unit <id> does not address criterion "<criterion>";
report on <criteria> alone`, `criterion <criterion> is reported twice`,
`criterion <criterion> has no done|evidence|proof`, `the report misses
<criteria>: report on every criterion of unit <id>`, `unit <id> is <state>,
not implementing`, `this turn already reported its unit done; end the turn`
once one report was accepted. Input the tool's schema refuses, such as an
argument the tool does not take, a missing report field or a value of the wrong type,
comes back as a tool error before the report is checked; the unit does not
move and the turn goes on.
A turn whose report was accepted ends with the outcome `done`, whose report is
the mason's report as JSON and whose card is the normalised owner-facing card.
A turn that ends without an accepted report, or
fails after one, leaves its unit `implementing`.

On its next pass the mason controller finishes each `implementing` unit whose
mason thread's last turn ended cleanly with `done`. It snapshots the unit's workspace
as the unit's [candidate](#unit-workspaces), then records, in one commit, the
next revision `<k>` of the document `units/<id>/report.json` (ID
`unit-<id>-report`, actor `service`/`mason`, cause the turn's response) and the
transition `unit-<id>-reviewing-<k>` from `implementing` to `reviewing` with
the reason `the mason of unit <id> reported done on turn <turn>; its candidate
is <commit> on <unit-branch>, from <feature-branch> at <base>, and its report is
units/<id>/report.json revision <k>`. The transition carries one
[notice](#event-delivery) for the chief of staff, `Unit <id> is reviewing: its
mason reported done on turn <turn>; its report is units/<id>/report.json
revision <k>. Headline: <headline>` followed by `; Needs you: <needs_you>`
when the latter is set. The document holds `unit`, `turn`,
`seal`, `outcome`, `criteria`, `card`, `branch`, `base` (the feature branch commit the
workspace descends from) and `candidate`. A unit in `reviewing` takes no mason
slot, so its workstream can start its next `ready` unit.

A unit whose workspace does not descend from the tip of the feature branch,
as after another unit landed, is not finished until the foreman has
[rebased](#rebasing-units-in-flight) the workspace; the snapshot then holds
the mason's work on the new tip. A unit whose rebases left conflicts is not
finished while a conflicted file of its candidate still carries a conflict
marker line, one starting with `<<<<<<<` or `>>>>>>>`: it stays
`implementing`, and its mason gets the turn `mason-<id>-markers-<n>`, after
its done turn `n`, naming those files. A unit whose candidate cannot be made
for any other reason stays `implementing` and is [blocked](#starting-units)
with the reason `unit <id> stays implementing: its mason reported done, and
its candidate cannot be made: <error>`: it takes no mason slot, its
workstream starts no other unit, and the next pass tries again.

### Preparing a unit review

The service prepares a reviewing unit from its recorded report and seal. It
reads the diff between the report's base and candidate commits directly, even
if either branch has moved. The request carries the sealed spec and plan, the
mason's criterion report, the resolved seal footprint, and charter and local
context scoped to the unit's entities. Missing report fields, commits, seals,
or resolved entities stop preparation with a recorded block reason. The
prepared identity in `units/<id>/review.json` records the subject, candidate, base, spec
and plan revisions, the diff digest, report revision and seal number. Repeated
preparation of the same evidence leaves that revision unchanged.

### Reviewing a unit

The reviewer controller offers each `reviewing` unit to its durable reviewer
thread. The thread ID belongs to the unit, so a revised candidate returns to
the same reviewer. The scheduler admits reviewer turns within
`capacity.reviewers` and offers review turns before mason turns when both are
queued. Pauses hold new review turns. The review prompt includes the recorded
identity, exact diff, sealed spec and plan, criterion report, resolved
footprint and local context. The reviewer has a read-only, isolated turn with
`ask` and `verdict` tools.

`verdict` requires `satisfactory` or `material_findings`, evidence for every
unit criterion, and, for material findings, at least one finding naming a
criterion, severity, evidence and action. For a satisfactory verdict, the
reviewer explains each changed path outside the sealed footprint in
`extra_paths`. Unresolved or ambiguous path mappings, or an unexplained extra
path, keep the unit in `reviewing` for another review and an owner approved
plan amendment when the footprint needs to change. The completed turn's verdict is
stored in `units/<id>/review.json` with the candidate commit, base commit,
spec and plan revisions, diff digest and reviewer turn. Only a clean turn
with a recorded verdict moves the unit. Before approval, the service compares
the reviewed identity with the current report, seal, documents and workspace
branches; stale inputs return the unit to `reviewing` with a reason.
A satisfactory verdict moves it to
`approved`; material findings increment the durable bounce count and return
it to `implementing` with findings for the mason. At `shed.max_bounces`, the
unit enters `contested` and raises an owner gate instead. The owner uses
`osmia contested <workstream> <unit> <review|revise> <note>` to record a
direction for that candidate. `review` resumes review and still needs a new
reviewer verdict; `revise` passes the findings to the mason. A ruling is
recorded before the transition so restart reconciles it once. The mason's
next report makes a new candidate and the reviewer sees a new exact request.

A reviewer question moves the unit to `waiting` and reaches the chief of
staff through the ordinary question event. The answer returns on the same
reviewer thread and resumes `reviewing` with the candidate intact. An answer
alone cannot approve the unit. An interrupted turn is continued on the same
thread. A result recorded before a stop is applied after restart without
another reviewer turn.

### Landing a unit

The librarian's [refresh pass](knowledge-base.md#refresh-after-landing) follows
each recorded landing. A ready unit waits while that refresh is pending, so
its next file-based bundle can include the new local knowledge.

The landing controller runs in every reconciliation pass after the building
controller. It runs one landing and rebase sequence at a time per project:
while a landing operation of the project has no result, it asks for nothing.
Otherwise it first [rebases](#rebasing-units-in-flight) the units a landing
left behind. While a unit of a building or assembled workstream that is not paused is
behind its feature branch, or a rebase has no result, it asks for no landing.
Otherwise it takes the building and assembled workstreams that are not paused, in priority
order, and their units in dependency order, and asks to land the first
`approved` unit
whose latest `units/<id>/review.json` revision `<k>` is a satisfactory
verdict no landing was asked for. It publishes the operation `land` (input
`unit`, `review`, `candidate` and `base`) on the repository boundary with the
transition `landing-<id>-<k>` (actor `service`/`foreman`, cause
`unit-<id>-review-<k>`), which moves the workflow subject `landing-<id>` to
`requested-<k>`.

The operation first checks the feature branch. A tip whose only parent is the
approved base and whose message carries this operation's `Osmia-Operation`
trailer is an interrupted landing: the commit is recorded and not made again.
Otherwise the approval must still be current: the workstream `building` or `assembled`, the
unit `approved` by review revision `<k>`, and the recorded report, seal,
candidate diff, unit branch tip, feature branch tip and latest spec and plan
revisions those the approval names. The service then commits the
candidate's tree once on the approved base, by `Osmia <osmia@localhost>` and
stamped with the time the landing was asked for, and fast-forwards the
feature branch and its workspace to it. No agent takes part. The message's
subject is the text of the criteria the unit addresses, from the sealed spec;
its body lists each criterion, and its trailers name the workstream
(`Osmia-Workstream`), unit (`Osmia-Unit`), candidate (`Osmia-Candidate`),
base (`Osmia-Base`), approval (`Osmia-Approval`, `units/<id>/review.json
revision <k>`) and operation (`Osmia-Operation`).

One trace commit then records `units/<id>/landing.json`, which holds the
unit, operation, approval, reviewer turn, candidate, base, spec and plan
revisions, seal, criteria, branch, landed commit and its message; the
transition `unit-<id>-merged` from `approved` to `merged` (cause the
operation ID, reason `unit <id> landed as <commit> on <branch>: <approval>
approved candidate <candidate> from <base>, spec <s>, plan <p>, seal <n>;
criteria <criteria>`) with the chief of staff's notice `Unit <id> merged: it
landed as <commit> on <branch>.`; `landing-<id>` moving to `landed-<k>`; and,
for every `planned` unit that depends on it and whose other dependencies have
all merged, `unit-<dep>-ready` with the reason `unit <dep> is ready: every
unit it depends on has merged: <ids>`. The mason controller then starts the
ready unit from the landed commit. [Workstream status](#workstream-status)
shows each unit's latest landing.

A stale approval is refused: nothing is committed, the unit stays `approved`,
`landing-<id>-<k>-refused` moves `landing-<id>` to `refused-<k>` with the
reason `unit <id> did not land: <why>` as the operation's result, and the
chief of staff is told that the unit lands once an approval of its current
candidate, base, spec and plan is recorded. The same approval is not asked
to land again. A landing whose outcome is recorded completes on inspection
without touching the branch.

### Rebasing units in flight

A landing moves the feature branch, and the workspace of every other
unfinished unit of the workstream is then behind it: its branch does not
descend from the new tip. The landing controller rebases each such unit, in
any state but `planned` and `merged`, once no writer holds its workspace. A
mason turn of the unit that is claimed, including one a stop interrupted
whose view the mason controller has not copied back yet, or that a turn
operation names and that has not completed, holds the workspace, and the
unit waits. The scheduler's [gate](#running-turns) dispatches no new mason
turn of a unit that is behind, so a unit's writer finishes, or is stopped and
recovered into its workspace, and none starts before the rebase.

The controller publishes the operation `rebase` (input `unit`, `rebase` `<k>`
and `onto`, the feature branch tip) on the repository boundary with the
transition `rebase-<id>-<k>` (actor `service`/`foreman`, cause the landing
operation that made the tip), which moves the workflow subject `rebase-<id>`
to `requested-<k>`. A unit already rebased onto that tip is not asked again.

The operation first checks the unit branch. A tip whose only parent is the
new feature branch tip and whose message carries this operation's
`Osmia-Operation` trailer is an interrupted rebase: its snapshot is read from
the `Osmia-Snapshot` trailer, and nothing is snapshotted or committed again.
Otherwise the feature branch must still be at `onto` and the workspace must
not descend from it. The service snapshots the workspace, untracked files
included, then merges the change the snapshot holds since the feature branch
commit it descends from onto the new tip, and commits the merged tree once
with the new tip as its only parent, by `Osmia <osmia@localhost>` and stamped
with the time the rebase was asked for. Its trailers name the workstream,
unit, snapshot, base, new tip and operation. A path both sides changed
carries conflict markers, `<<<<<<< <new tip>` above the feature branch's
lines and `>>>>>>> <snapshot>` below the unit's, and a path one side deleted
and the other changed keeps the changed side; either is a conflicted path.
The unit branch, its index and its files then move to the rebased commit. No
agent takes part. The merge runs `git merge-tree --write-tree`, which needs
Git 2.38 or later.

One trace commit then records `units/<id>/rebase.json` (the unit, rebase
number, operation, the unit's state, branch, base, new tip, snapshot, rebased
commit, conflicted paths and the report revision current after the rebase)
and the rebase's outcome: `rebase-<id>-<k>-rebased` moving `rebase-<id>` to
`rebased-<k>`, or `rebase-<id>-<k>-conflicted` moving it to `conflicted-<k>`
with a notice for the chief of staff, `Unit <id> conflicts with feature
branch <branch> at <tip> in <paths>. Its mason resolves the conflict markers
against the sealed spec; the unit is reviewed again before it lands.` A
refused rebase, of a unit with no workspace, one already current or one whose
feature branch moved on again, moves `rebase-<id>` to `refused-<k>` and
changes nothing.

A clean rebase of the candidate the unit's latest report names records the
next revision of `units/<id>/report.json` (actor `service`/`foreman`, cause
the operation) with the rebased commit as `candidate` and the new tip as
`base`. The candidate and its base changed, so no approval carries over: an
`approved` unit returns to `reviewing` through
`unit-<id>-reviewing-rebase-<k>` (reason `the approval no longer holds:
unit <id>'s candidate <snapshot> from <base> was rebased onto <tip> as
candidate <commit>, recorded in units/<id>/report.json revision <r>; the
rebased candidate is reviewed before it lands`), with the notice `Unit <id>
returns to review: its approved candidate was rebased onto <tip>.`, and a
`reviewing` unit's review starts again on the same transition. The reviewer
then [prepares](#preparing-a-unit-review) the rebased candidate as a new
review. An `implementing` or `waiting` unit keeps its state and its mason
works on the rebased files.

Conflicts go to the unit's mason and never to its reviewer. They are
unresolved while no report revision was recorded after the rebase that left
them. The landing controller moves a `reviewing` or `approved` unit with
unresolved conflicts to `implementing` through
`unit-<id>-implementing-rebase-<k>`, with a notice, and queues its mason the
turn `mason-<id>-rebase-<k>`, whose prompt lists the conflicted files,
explains the markers and carries the unit's bundle with the sealed spec to
resolve them against. A `waiting` or `contested` unit keeps its state until
its answer or the owner's ruling moves it on; the same rules then apply. The
mason reports `done` as usual, and the mason controller
[finishes](#finishing-units) the unit only once no conflicted file of its
candidate still carries a marker, so a candidate the reviewer receives never
does.

A rebase whose outcome is recorded completes on inspection without touching
the workspace.

### Unit workspaces

The service gives each unit of a workstream a workspace of its own: a Git
worktree of the clone at `<root>/units/<project-id>/<workstream-id>/<unit-id>`,
on the branch `osmia-unit/<workstream-id>/<unit-id>`, created from the tip of
the workstream's feature branch. Its name follows from the workstream and the
unit alone, so a service started again finds the same worktree, and the feature
branch commit it descends from is its base. Opening a unit's workspace again
returns it as it is. Nothing in the service removes a unit's workspace, and
pruning forgets only worktrees whose directories are gone. A unit of a
workstream without a feature branch has no workspace: `the clone has no
feature branch <branch>`.

A unit's workspace is lent to a mason turn of that unit alone, and only
once it exists (`unit <unit-id> of workstream <workstream-id> has no
workspace`); a turn of another role, or one without a workstream and unit, is
refused with `a unit workspace is lent to a mason turn of a unit alone`. The
lease never removes the workspace. Such a turn works on a
[private view](isolation.md) of every file of the workspace but its `.git`,
under the turn's [isolation](isolation.md#enforced-execution): no VCS
executable, no VCS metadata readable or writable, and no environment but the
service's. The view leaves out the workspace's symlinks and special files, at
the top and nested, so the turn never sees them. Whatever the turn's result,
the view is copied back into the workspace: the workspace then holds the
view's regular files and directories, with each file's owner execute bit, and
nothing else but what it keeps. VCS metadata, symlinks and special files the turn put into its
view are not copied back. The workspace's own `.git`, symlinks and special
files are left as they are, and so are the directories holding them, even when
the turn removed such a directory from its view; a file or directory the turn
put in the place of one of them is not copied back. A candidate snapshot
therefore records the workspace's symlinks unchanged.

The service can snapshot a unit's workspace as its candidate: a commit of the
worktree's whole tree, untracked files included and files the repository
ignores left out, on top of the commit the worktree is on, by `Osmia
<osmia@localhost>`. The owner's own excludes file does not apply. The unit's
branch moves to the candidate. A workspace with nothing new since its last
snapshot is not committed again: its commit is the candidate. A workspace
that does not descend from the feature branch is refused before anything is
committed: `workspace <path> is at <commit>, which does not descend from
<branch>`.

## Assembly and final review

The assembly controller runs in every reconciliation pass after the landing
controller. A `building` workstream moves to `assembled` once every unit of
its sealed plan is `merged`, and not before: the transition `assembled`
(actor `service`/`foreman`, cause the last unit's move to merged, reason
`every unit of the sealed plan has merged onto <branch>: <ids>; the feature
branch is rebased onto upstream and read against the sealed spec and the
charter next`) tells the chief of staff. Final-review gaps add follow-up units
without moving the feature back from `assembled`.

### Asking for a final review

For an `assembled` workstream whose units, including follow-ups, have all
merged and that is not paused, the controller asks for
final review `<k>` when none has a result pending and no review was asked for,
and no reviewed report records, the current inputs: the feature branch tip,
the latest seal, its spec and plan revisions and the latest charter revision.
A service without a committee runner asks for none. It publishes the
operation `final-review` (input `review`, `commit`, `seal`, `spec`, `plan`
and `charter`) on the runner boundary with the transition `final-review-<k>`
(actor `service`/`final-review`, cause `assembled`), which moves the workflow
subject `final-review` to `requested-<k>`. A review that failed is asked for
again only once one of its inputs changes.

### Running a final review

The operation first checks its inputs. The workstream must still be
`assembled` and read the seal, spec, plan and charter revisions the review
was asked for, and, before the rebase, its feature branch must still be at
`commit`; otherwise the review fails with the reason.

The foreman then rebases the feature branch one last time: it fetches the
project's configured `base_branch` from the remote whose URL names its
`upstream`, and replays each commit the branch holds on top of the fetched
commit, in a temporary worktree, keeping each commit's message and author and
committing as `Osmia <osmia@localhost>` at the time the review was asked for.
A commit whose change upstream already holds is dropped, and a branch already
on upstream stays as it is. `final/rebase.json` records the review, the
operation, the branch, the upstream remote, branch and commit, the commit the
branch was at and the rebased commit; only then do the branch and its
workspace move to the rebased commit, so a review a stop interrupted moves
the branch to the recorded commit, whatever upstream did since. A replay that
conflicts moves nothing and fails the review with the conflicted paths. Fetch
and Git errors leave the operation pending for another attempt.

The workstream's first committee member, its thread created when debate was
skipped, then reads the rebased commit in a turn the service runs itself,
like a [shed round](#a-members-turn)'s. Its read-only view holds `branch/`,
the rebased commit's tracked files, `branch.diff`, the branch's change to the
upstream commit, the sealed `spec.md` and `plan.json`, the charter revision
the review reads as `charter.md`, and each unit's latest `report.json` and
`landing.json` under `units/<id>/`. The member holds `file_read` and
`final_report`, nothing that writes, runs or fetches. `final_report` takes an
optional `summary` and `criteria`, one entry per criterion of the sealed
spec, cited as `spec#<n>`, with either `evidence`, what in the branch shows
the criterion holds, or a `gap`, what does not. A call that leaves out a
criterion, repeats one, cites one the spec does not hold, or gives an entry
both or neither is refused with the reason; a later accepted call replaces an
earlier one. A turn a stop interrupted is abandoned and started again, up to
the committee's attempt bound.

One trace commit then records the next revision of `final/report.json` with
the transition `final-review-<k>-reviewed` or `final-review-<k>-failed`, which
moves `final-review` to `reviewed-<k>` or `failed-<k>`, and a notice for the
chief of staff. The report holds the review number and operation, the
outcome, the branch, the commit it was asked for and the reviewed commit, the
upstream remote, branch and commit, the seal, spec hash, spec, plan and
charter revisions, the reader and its turn, the summary and, for every sealed
criterion in spec order, its text with the evidence or the gap. A failed
report holds the failure instead of criteria, and the conflicted paths when
the replay conflicted: the member's turn ended without a report or with
another status, the turn was interrupted too often, the workstream was
abandoned, or an input changed. A review whose outcome is recorded completes
on inspection.

Each reported gap also records a follow-up unit in `final/followups.json` in
that commit. Its record names the criterion, gap, review and report revision.
The unit inherits its criterion's proof and the combined footprints of the
sealed plan units addressing that criterion. It enters `ready` and uses the
ordinary mason, exact-candidate reviewer and foreman landing path while the
feature stays `assembled`. The sealed spec and plan remain unchanged; any
change to their intent or footprint requires an owner-approved amendment.

### A current report

A final report authorises a later owner approval or delivery only while it is
current, shows every criterion, and every follow-up unit has landed. A failed
report never does. A reviewed report stops being current
when the feature branch moves from the reviewed commit, or the latest seal,
its spec hash, the latest spec or plan revision, or the latest charter
revision differs from the one it read; the reason names the first input that
changed. The controller asks for a new whole-branch review after follow-up
units land. The report, and
whether it is current, reads the same after a restart.

### Owner delivery approval

`osmia delivery <workstream-id>` presents the current final report with
unshown criteria first, retaining each criterion's evidence or gap, followed
by a pull request description drafted from the final report and landed-unit
records. The JSON form includes the reviewed commit, governing revisions,
draft text and its SHA-256 hash. A report with gaps can be read but cannot be
approved.

`osmia approve <workstream-id>` accepts the draft. To edit it, use
`osmia approve <workstream-id> <description-file>`. The service requires the
review number, report revision, commit and draft hash that were presented, then records the
exact chosen description and its hash in `final/delivery.json`. The record
also pins the final report revision, branch commit, seal, spec hash, spec,
plan and charter revisions. Its owner workflow transition and document are
one trace commit. Repeating the same decision returns the recorded ruling.
Restarting preserves an edited description; a newly drafted description does
not replace it. [Publication](#publication) checks the current final report
and the description it sends against this approval. A changed branch or
governing revision requires a new review and approval; a changed description
requires a new approval. Approving a delivered workstream returns `conflict`.
Approving again after a publication was refused records a new approval
revision even when nothing else changed, which asks for another publication.

### Publication

The publication controller runs after the assembly controller in every pass.
For an assembled, unpaused workstream whose latest approval passes the
delivery gate, it asks for one `publish` repository-boundary operation per
approval revision `k`, moving the `publication` subject to `requested-<k>`.
The operation pins the approval revision, reviewed commit and description
hash, and the project's `landing` style, fork, upstream and base branch at that
time, so a retry publishes the same way. While a publication has no result,
no other publication of the workstream is asked for.

Each attempt first checks the approval: the workstream must be `assembled`,
and the current final report, feature branch, governing revisions and latest
approval must still match approval `k` and its description. Otherwise the
publication is refused before the fork or GitHub is touched. The delivery
commit is the reviewed commit with `commit-per-unit`, which keeps each unit's
reviewed commit. With `squash`, it is one commit on the upstream commit the
final review rebased onto, holding the reviewed commit's tree, with the
description's title as its subject. The local feature branch never moves.

The service then asks the fork remote, the clone's remote whose URL names the
configured fork, for its `osmia/<workstream-id>` branch. The branch may
already be at the delivery commit. If it is absent, or at a commit that an
earlier publication of the workstream recorded, the service pushes with that
commit as the expected value, so a concurrent change makes the push fail.
Any other commit refuses the publication. `final/publication.json` records
the delivery commit with status `pushing` before the push. Next, the service
lists the pull requests on the upstream repository from that fork branch. It
keeps one open pull request against the base branch whose head is the
delivery commit and whose body is the approved description. It opens a pull
request if none exists and refuses any other state, including a closed pull
request. The title is the first line of the description without heading
marks. One trace commit then records `final/publication.json` with status
`opened`, the fork branch, delivery and reviewed commits, style, pull request
number and URL, title and description. The same commit records the feature
transition to `delivered`, with a notice for the chief of staff, and
`published-<k>`.

A refusal records `refused-<k>` with its reason and tells the chief of staff;
the workstream stays assembled. Git, GitHub and storage errors leave the
operation pending, and each retry inspects the fork branch and pull requests
again. A push or pull request creation interrupted by a failure or a restart
therefore resumes without a second push or pull request.

The service pushes with the owner's Git configuration, SSH agent and
credential helper, as it fetches. It finds and opens pull requests through
the GitHub REST API with the `GITHUB_TOKEN` of its own environment, or through
`Options.PullRequests`. No session, prompt, tool or workspace receives either
credential.

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
| `units` | One `{"unit", "state"}` per unit of the sealed plan and each final-review follow-up, in order, once their states are recorded; `card` holds that unit's latest completed turn card when present, and `landing` its latest `units/<unit>/landing.json` once it [landed](#landing-a-unit): the reviewed candidate and base, the approval, the governing spec, plan and seal, the criteria and the feature branch commit; empty before |
| `open_questions` | Questions in the workstream without a ruling |
| `gates` | Open owner decisions as `{"kind","reference"}`: an `escalation` with its inbox number, `ratification` with the workstream ID, or `contested` with the unit ID; empty when none wait |
| `context_mode` | The project's context mode, as in `/runtime`: `file` for [file-based context](context.md) |
| `status` | `null` until the chief of staff writes one; otherwise `goal`, `attention` (empty when nothing needs the owner), `note`, `agents`, `revision` and `updated_at` |

`set_status` requires a non-empty `attention` while any gate is open and
refuses a non-empty one when no gate is open. The chief of staff writes the
wording. A refusal is an ordinary `{"stored":false,"reason":"…"}` result;
an open-gate reason names its kind and reference. `osmia status` prints the
gates under each workstream status.
`osmia status <workstream>` prints each available unit card and landing
beneath its unit, apart from the chief of staff's status.

Without an active project or its trace, the list is empty. If the trace
cannot be read, the list is empty and carries a `workstreams` diagnostic with
code `internal`. A workstream whose sealed plan cannot be read is listed with
no `units`, and the list carries a `units` diagnostic with code `internal`
naming it (`cannot read the unit states of workstream <id>; check the trace
repository`); the other workstreams are listed as ever. For one workstream, a
malformed ID returns `validation`, no configured project returns
`no_project`, a workstream the active trace does not hold (or no trace at all)
returns `not_found`, and an unreadable trace, or unit states of that
workstream that cannot be read, returns `internal`; these messages name the
workstream or project.

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
  [context bundle](context.md), latest stored status and open inbox escalations
  rendered from the trace at acceptance.

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
hand-in, the committee's rounds and the architect's replies once a draft is
sketched, and every queued chief-of-staff turn. All four use one `Enforcement`:

| Part | Production value |
|---|---|
| `Engine` | `coreadapter.CoreEngine`: each role's enforcer for its configured `sandbox` and `image` |
| `Hosts` | `coreadapter.MCPHost` serving host turns with `CoreTransport` and container turns with `ContainerTransport` |

`Options.Threads` binds the thread dispatcher to isolated turns that grant only
the chief of staff, with `set_status`, `answer`, `escalate`, `relay_ruling`,
`route_amendment` and `propose_charter`, and the mason, which may write and
execute in its view and holds `file_read`, `file_write`, `ask` and
[`done`](#finishing-units). A thread turn of any other role
fails with the recorded reason `role has no service grant`. A mason turn works
on a view of its [unit's workspace](#unit-workspaces), which is copied back into
the workspace after the turn. The chief of staff's workspace is an empty
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
runner operation except the librarian's `kb-extract` and `kb-refresh`, the
architect's `architect-draft` and the shed's `shed-round` and `shed-reply` actions, which
the service reconciles itself. The [thread dispatcher](trace.md#turn-dispatch) is the
intended binding; it receives the service-owned repository handle, which callers
must not close. The [M1 demonstration](m1-demonstration.md) uses this path with
fake engines.

`Options.Librarian` supplies the execution engine and MCP host factory the
librarian's extraction and refresh turns run in. Without it these operations
fail with a recorded reason, so a service without an execution engine still
registers projects and reports extraction failure in status.

With `Options.Threads` set, the service also starts ready units (see
[starting units](#starting-units)), runs queued workstream turns on its own,
delivers outbox events to each chief of staff (see
[event delivery](#event-delivery)) and queues answers on their askers' threads
(see [questions](#questions)). Event delivery, answer delivery, the mason
controller and the scheduler, in that order, replace any `Schedule` hook in `Options.Reconciliation`; without
`Options.Threads` that hook runs. In both cases the
[architect controller](#architect-drafting), then the
[shed controller](#the-shed-debate), then the
[sealing controller](#sealing), then the
[building controller](#building) run first.

A pass reconciles its pending operations in stage order, across workstreams,
so the factory finishes work before it widens it: the turns the scheduler
dispatched and every other operation first, then the shed's `shed-round` and
`shed-reply` operations, then `architect-draft` operations. Within a stage
they keep workstream order.

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
`architect-draft`, `shed-round` and `shed-reply` reconcilers run those, staged
in their own views, and such a turn the scheduler found queued gets no turn operation.
The gate holds a turn that a pause in `runtime.Effective` covers:
a `factory` pause, a `project` pause on the active project, or a `workstream`
pause on the turn's workstream. Chief-of-staff turns are never held, so the
chief of staff stays reachable while everything is paused. A held turn gets no
operation and stays queued; a turn already in flight is not interrupted, in
either pause mode. The gate reads the runtime store on every pass, so after a
pause is cleared the loop's next periodic pass runs the held turns with no new
message or operation. The gate also holds every mason turn of a unit whose
workspace does not descend from the tip of its workstream's feature branch,
until the foreman [rebases](#rebasing-units-in-flight) the workspace onto it.

The scheduler also dispatches within the configured `[capacity]`. Mason
turns use `capacity.masons`, reviewer turns use `capacity.reviewers`, and both
limits apply across workstreams. Committee turns are not dispatched by the scheduler: a
[shed round](#a-members-turn) runs every member of its workstream's committee
at once, outside `capacity.per_workstream`, so two workstreams in the shed run
two committees at the same time. Every other role runs one turn at a time per
workstream. Each workstream runs at most the project's
`capacity.per_workstream` of the turns the scheduler dispatches at once; a
round's turns in flight count toward that number, so they hold the
workstream's slots against other roles while the round runs, and hold none
between rounds. Chief-of-staff turns take no slot and
run even when every slot is taken. A turn holds its slots while it is in
flight, so they are free again once it completes, whether it succeeded, failed,
is waiting or was cancelled. A claim a restart interrupted holds no slot,
although its thread stays reserved. A parked thread holds no slot. The capacity
check runs before the service's gate, so the gate sees only candidates with a
free slot. A candidate without a free slot stays queued and is offered again on
a later pass.

Each pass offers queued turns to the free slots one at a time, finishing work
before widening it: reviewer turns first, then mason, committee and architect
turns, then every other role's. Within one of those stages the turn of the
workstream first in the project's [runtime priority](runtime.md) order goes
first; workstreams the order does not name follow those it names, in the same
order the [mason controller](#starting-units) starts units. Among workstreams
of equal priority, the one whose last turn of that stage was dispatched least
recently goes first, so equal workstreams take a stage's slots in turn, within
a pass and across passes. The last dispatch of each stage is read from the
workstream's turn operations in the trace, so a restart continues the rotation
rather than resetting it. A turn without a free slot, or one the gate holds,
such as a paused workstream's, is not dispatched and leaves its workstream's
place in the rotation unchanged. Candidates, capacity counts per workstream,
priority lookups and the rotation are keyed by project and workstream, while
the role kinds' slots are shared across projects.

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

At the start of every reconciliation pass, after the architect and shed
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
When the turn is queued, its system prompt names the workstream, carries
`questions.Guidance` (what to do with an open question and with the owner's
ruling), and renders the workstream's [context bundle](context.md), latest
stored status and open inbox escalations from the trace. The bundle and durable
status and escalation context are assembled at enqueue time for event turns and
owner conversation turns alike. This gives a fresh backend session the current
values even when bounded replay omits the tool calls that recorded them.

Delivery claims every event with one new attempt token and queues the turn
`events.TurnID(token)`. An event stays unacknowledged until that turn
completes. Each pass looks at the chief-of-staff turns an unacknowledged
event's claims name:

| Turn | What the pass does |
|---|---|
| `done` (see the conversation `state` column) | Acknowledges the event, without another turn |
| Queued or running | Leaves the event alone, even when its claim's lease ran out |
| `failed`, including a turn a restart interrupted, or no turn | Releases a claim of this service session that still holds the event, then delivers the event again in a new turn with a new token, together with any other pending event |

So an event reaches the chief of staff until a turn carrying it succeeds: a
turn that fails or that a stop interrupts is not retried, but its events go
out once more in one new turn, and a restart at any point neither loses an
event nor delivers it twice alongside a turn still in flight. An event
claimed by this session with no turn yet, left by a pass that stopped before
queueing it, is released and delivered in the next turn. An abandoned
workstream's events are not delivered and stay unacknowledged. A failing store call, chief-of-staff profile lookup or bundle assembly
stops the loop, as the scheduler's errors do; an event another claim holds is
skipped and retried on a later pass.

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
asks nothing again. An open question whose event turn failed, or was
interrupted by a stop, reaches the chief of staff again in a new event turn.
A restart between a recorded answer and its delivery
queues the answer turn once. An escalated question stays escalated and its
asker stays parked until the owner rules. A restart after the owner's ruling
delivers its event once the window closes; a restart after the relay queues
each asker's answer turn once; a restart with an answer turn queued runs it
once.
