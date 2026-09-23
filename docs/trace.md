# Trace repository

`internal/trace` persists the service's typed records in the dedicated local Git
repository at `<root>/projects/<project-id>`. `Create` accepts a resolved
`config.Root`, the configured project, a timestamp and an actor. It reserves the
repository with an exclusive directory creation and preserves an existing
`config.toml`. A target clone and the Osmia root must be separate, non-nested
paths. `Repository` is a trace handle; it does not implement a target workspace
or provide delivery operations.

The local API review demonstration is `TestM3ExactReviewDemonstration` in
`internal/service/review_demo_test.go`. Run it inside Dagger with:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM3ExactReviewDemonstration,-v,./internal/service \
  combined-output
```

It builds candidates in a local Git fixture using fake mason and reviewer turns.
The first review records a criterion-linked finding and sends the unit back.
The mason revises the candidate, then the reviewer asks an owner question. A
restart preserves that question and candidate. The owner answer lets review
continue, but an unexplained changed path still prevents approval. A further
review explains the path and approves the current candidate. Inspect
`units/resume/report.json` and `units/resume/review.json` revisions,
`agents/reviewer-resume/log.jsonl`, `questions/1/`, and `events.jsonl` to follow
the requests, results, ruling, footprint refusal and exact approval.

`CreateWorkstream` reserves a supplied `config.WorkstreamID`. Project and
workstream manifests retain their identity and creation provenance. `Workstreams`
returns identities validated against those manifests, including terminal streams.
The package supplies storage APIs; the service's project registration calls
`CreateSeeded`, and lifecycle commands are separate.

## Files and records

Creation initializes `charter.md` with `CharterTemplate`, guidance without
rules, and records it as revision 1 of the `charter` document, plus
`kb/entities.json`, `kb/`, `notes/` and `workstreams/`. `Create` writes
`kb/entities.json` as `{}` without a document revision. `CreateSeeded` takes an
entity map the caller has already validated and commits it as revision 1 of
the `kb-entities` document. The [knowledge base](knowledge-base.md) reference
describes the file. Workstream creation initializes `handed/`, `shed/`, `amendments/`,
`questions/`, `units/` and `agents/`, plus the empty document, transition and cost
logs, and the chief-of-staff thread. Spec and plan files appear when their first document revision is appended.
Empty directories exist on disk; Git records files.

Every record carries a schema named `osmia.trace.<kind>`, version `1`, record ID,
one-based revision, project and optional workstream/unit identity, timestamp,
actor, cause and causal depth. Actors name an owner, service component or agent.
The caller supplies stable causal message/operation/record references; storage
does not infer authority, readiness or workflow transitions from them.

| Go record | File relative to the project or workstream | Payload |
| --- | --- | --- |
| `Document` | `documents.jsonl` and its document path | Path and complete content of each revision; a handed document also records its source |
| `Transition` | `events.jsonl` | Subject, prior/resulting state and reason |
| `Question` | `questions/<id>/question.jsonl` | Asking actor, its thread and turn, the question as asked, and once escalated the owner-facing text and the escalation; see [questions](#questions) |
| `Ruling` | `questions/<question-id>/rulings.jsonl` | Question revision, decision, owner response, returned answer, scope, citations and affected references; a ruling holds a returned answer, an owner response or both, and a scope only with a returned answer; see [questions](#questions) |
| `Agent` | `agents/<id>/identity.jsonl` | Stable role/thread identity and backend session at that revision |
| `TurnRequest` | `agents/<agent-id>/log.jsonl` | Thread/turn identity, accepted profile, system prompt, request and caller-supplied context |
| `TurnResponse` | `agents/<agent-id>/log.jsonl` | Exact request revision, thread/turn identity, adapter result (including the accepted `done` report and card) and any execution failure |
| `Cost` | `ledger.jsonl` | Adapter ledger entry with attempt, full scope, time and explicit cost knowledge |
| `Status` | `status.jsonl` | The chief of staff's goal, attention, note and agent lines; see [workstream status](#workstream-status) |

Only documents can be project-scoped. Project document paths are `charter.md`,
`kb/entities.json`, `kb/<name>.md` and `notes/<role>.md`; workstream document paths
are `spec.md`, `plan.json`, `seal.json`, the [seal](service.md#sealing) of a
ratified workstream, `handed/<name>` and the shed records
`shed/round-<n>/<agent>.json`, where `<n>` is a positive round number without
leading zeros and `<agent>` is the committee member's agent ID,
`shed/round-<n>/reply.json`, the architect's
[reply to the round](service.md#the-architects-reply),
`shed/round-<n>/redrafted.json`, its
[redraft](service.md#redraft) at the owner's request, a record of the same
shape as a reply, `shed/round-<n>/packet.json`, the
[ratification packet](service.md#the-packet) the chief of staff presents,
`units/<unit>/report.json`, a unit's
[report and candidate](service.md#finishing-units), where `<unit>` is the unit
ID, `units/<unit>/review.json`, the prepared review's candidate, base, spec
and plan revisions and diff digest followed by the reviewer's exact verdict,
criterion evidence, findings and bounce count, `units/<unit-subject>/ruling-<n>.json`,
the owner's direction after bounce `n`, and the files of [the owner's own part](service.md#the-owner-in-the-shed) in a
round: `shed/round-<n>/owner.json`, the owner's objections, a record of the
same shape as a member's; `shed/round-<n>/rulings.json`, what the owner ruled
and overruled about the objections that stand; `shed/round-<n>/more.json`, the
further rounds the owner asked for after debate concluded at round `n`;
`shed/round-<n>/redraft.json`, the redraft they asked for instead; and
`shed/round-<n>/ratification.json`, the
[ratification](service.md#ratifying) of the revisions in force. Handed inputs
are immutable. `spec.md` and `plan.json` are owner-edited: a revision of either
is recorded with the actor `owner`/`local` and the cause `owner-edit` when the
file differs from the latest recorded revision, as the charter is.
Only a handed document carries a `source`: `file:` and the absolute path it was
read from, the issue URL, or `stdin`.
Document paths retain one record identity. Agent role and thread IDs stay stable
across backend-session revisions. An agent may have no backend session before
its first turn. Failed execution may have no session identity when it includes
an explicit failure. Unknown cost is stored as zero with `CostKnown: false`;
it is distinguishable from a known zero-cost result.

An accepted `done` records its report and owner-facing card on the turn's
`Outcome`. The card has `headline` (required, at most 64 characters),
`happened` (required, at most 140 characters) and `needs_you` (at most 140
characters, empty unless an owner action exists). Whitespace is collapsed,
control and zero-width characters are removed, and line breaks or identifiers
are refused. The same card is copied beside the report in the unit's
`units/<unit>/report.json` document.

`Append(ctx, record)` accepts concrete record values other than `Status`,
which only `SetStatus` writes. Revisions must begin at one
and increase consecutively for each kind/ID within its scope. It preserves prior
JSONL entries, writes the latest document content to its ordinary file and commits
the affected files. For example, changing a spec retains both full document
revisions in `documents.jsonl` and both versions of `spec.md` in Git history.
There is no terminal-history deletion operation.

`RecordDocuments(ctx, documents)` records several document revisions of one
scope, the project or one workstream, as one commit through the journaled
publication boundary, so after a failure or an interruption either all of
them are recorded or none is. Every revision is checked against the recorded
history before anything is written; the charter is refused because the owner
edits it, and handed input because it is immutable. A `kb/<subsystem>.md`
revision with empty content records the subsystem's removal: the revision is
kept in `documents.jsonl` and the file is deleted from the tree and from
disk. The [knowledge-base extraction](knowledge-base.md#extraction) records
each pass this way, [architect drafting](service.md#architect-drafting)
records each draft's `spec.md` and `plan.json`, and a
[committee round](service.md#the-record) records one file per member.

`RecordDocumentsWith(ctx, documents, transactions...)` records document
revisions of one workstream the same way and applies the workflow
transactions in the same commit: after a failure either all of it is recorded
or none of it. A unit's report and its move to `reviewing` are recorded this
way.

`Read[trace.Document](repository, workstreamID)` enumerates typed revisions;
`Get[trace.Document](repository, workstreamID, id, revision)` retrieves one.
Use an empty workstream ID for project documents. Enumeration follows file and
line order, not a global event ordering. Reads use ordinary files, including the
owned request/final-response log, without backend transcript files, the target
clone or Hearsay. The execution payloads reuse the core adapter contracts;
trace accounting does not change the operational ledger adapter's file format.

## Charter

The owner edits `charter.md` directly, so it is one of the tracked files
allowed to differ from committed history, with a workstream's
[`spec.md` and `plan.json`](#owner-edited-workstream-documents); appending any
other record neither refuses nor commits such an edit. `Charter(ctx, at)` is the only way to read the charter:
it returns the latest recorded revision (document ID `charter`) after
recording `charter.md` as a new revision by the owner (`owner`/`local`, cause
`owner-edit`) when the file differs from the latest revision. A read without an
edit records nothing. The file itself is not rewritten, and the commit takes
the recorded bytes, so an edit saved meanwhile is recorded by the next read.
`Append` of a project `charter.md` revision is refused with `ErrConflict`
unless the file matches the latest recorded revision. The rule format is
described in [charter](charter.md).

## Owner-edited workstream documents

A workstream's `spec.md` and `plan.json` are the owner's to edit in place too.
`OwnerDocuments(ctx, stream, at, check)` returns the latest recorded revision
of both, by record ID, after recording each file that differs as a new
revision by the owner (`owner`/`local`, cause `owner-edit`, depth 0). A read
without an edit records nothing, the files themselves are not rewritten, and
the commits take the recorded bytes, as for the charter.

The two are one draft, so they are refused and recorded together: `check` is
given the content of both as the files leave them, by record ID, and may
refuse the edit, in which case neither file is recorded, both latest recorded
revisions stay current, and the error wraps `ErrOwnerEdit`. It runs while the
repository is held, so it must not read the trace. A workstream that records
neither document has nothing the owner may edit, and is reported as missing.

`OwnerEdits(stream)` names the files that hold an edit no revision records.
`RecordDocuments` refuses a revision of one of them with `ErrConflict` and
`ErrOwnerEdit` while its file does, so an agent's draft never writes over an
owner edit that has not been read yet. Which reads happen when is in
[the owner in the shed](service.md#the-owner-in-the-shed).

## Failure and ownership boundaries

One handle holds an exclusive process lock until `Close`, which unlocks it before
closing the descriptor, so a child process that still shares the descriptor does
not keep it held. Concurrent calls on that handle serialize. External writers
must not modify or move the repository while it is open. Files use root-confined operations and temporary-file replacement
with file and directory sync. Existing symlinks, hardlink aliases, special files,
absolute/traversal paths, unsupported schema versions, unknown or duplicate JSON
fields, invalid provenance and inconsistent revision identities are rejected.
Every existing path component is checked before access.

Git uses an isolated local configuration, no inherited Git routing or credentials,
no global/system configuration, and disabled hooks. It hashes supplied bytes using
plumbing commands, bypassing attributes and filters. Only trace-owned files enter
its commits; project configuration is left in place. Linked worktrees, redirected
object stores, Git symlinks and changes to the isolated `.git/config` are rejected.
These checks walk `.git` once before each operation's Git commands. The handle
keeps the file list of the commit HEAD names, so a read that finds HEAD
unchanged since that listing runs no Git and does not walk `.git`.
A local Git executable is required for trace and workflow operations. The service owns
this executable access; runtime agents receive no trace-repository handle.

`Open` completes journaled workflow-file publication, then checks manifests,
every record, workflow history and the committed file contents. For damaged
records or uncommitted files it returns a non-nil handle **and an error**; callers
must close that handle. Typed reads return valid records together with any
path/line corruption diagnostics. They never skip a damaged line silently or
truncate a log. A missing or modified committed file, or an uncommitted record file,
requires reconciliation and prevents further appends. Typed reads can inspect
pending records even when a prior Git commit failed.

An append is not an atomic workflow-state/outbox transaction. A write or Git
failure can leave inspectable, uncommitted files; the error does not claim rollback
or successful delivery. The append API reports that condition without repairing,
retrying or reconciling side effects. Controllers remain responsible for owner
gates and workflow policy.


## Atomic workflow state and outbox

`Transact(ctx, Transaction)` validates an expected state version for a workstream
and subject, and publishes a transition and zero or more outbox events together.
An absent subject has version zero and an empty state. Each transaction increments
that subject's version once; its `From` must match the current value. Subjects and
state names are supplied by Go controllers. The store does not implement lifecycle
policy or grant owner approval.

The transition carries the ordinary trace header, including actor, timestamp,
cause and causal depth. Its ID identifies the logical transaction in that
workstream; revision must be one. `EventID(transactionID, eventKey)` derives a
stable event ID. Event IDs are unique within a workstream, and ordinary notification events contain a
kind and body for [chief-of-staff delivery](service.md#event-delivery).
`Notice(transitionID, key, body)` builds one of kind `notice`, and
`SetFeatureState(ctx, header, to, reason)` moves the workstream's `feature`
subject from its current state and records a notice of the change in the same
transaction; a retry with the same header, state and reason returns the
committed state. `MoveFeatureState(ctx, header, from, to, reason)` does the
same from an expected current state and refuses any other with `ErrConflict`,
except for the retry of a committed transition.
`MoveFeatureStateWith(ctx, header, from, to, reason, transactions...)` is
`MoveFeatureState` that records the given transactions of other subjects in
the same commit, after the feature's; a retry of the committed feature
transition records none of them again. `UnitSubject(unit)` names the subject
holding a plan unit's state: `unit-<unit-id>`, or `unit_` and 32 hexadecimal
digits of the ID's SHA-256 for an ID longer than 64 characters.
`WorkflowStates(stream)` returns the current state of every subject that has
one. Local operation intents also
carry the operation described below. Callers retain the complete
request across retries, including timestamps and event order. An identical retry
returns its original resulting state even if later transactions exist. Reusing a
transaction ID with changed content, reusing an event ID in another transaction,
or supplying a stale version returns `ErrConflict`. Managed transitions cannot
be revised through `Append`.

`workstreams/<id>/workflow.json` holds the versioned transaction, delivery
and operation histories, plus durable thread queues. `Workflow` derives the
current subject state from that history. Each transaction also adds its transition to `events.jsonl`; the complete history of
both files is available in Git. `Outbox` returns all delivery intents with their
claim, acknowledgement and release history, sorted by event ID. Each entry's
`At` is its transition's timestamp.

The repository serializes calls under its exclusive process lock. Publication
writes immutable Git objects using a private index, syncs the objects the handle
has not synced yet (every object on its first publication), writes and
syncs a recovery journal under `.git`, then atomically replaces and syncs the
branch ref. That ref is the visibility boundary. Ordinary workflow, transition,
question and owned agent files are materialized from the committed objects before the journal is removed.
Before publication, recovery retains the prior state; after publication, recovery
finishes materializing the complete new state. Store reads and writes finish any
pending publication before inspecting the files. Direct filesystem readers must
open through the store first after an interruption; they may otherwise see
unfinished materialization. Unreferenced preparation objects are never state.

An error after ref publication can mean the transaction committed. Retry its
original identity to discover the result, rather than inventing a new identity.
The journal only authorizes recovery of workflow, transition, agent identity,
owned turn-log and private notes files, and of project documents other than
the charter; unrelated append failures and corrupt files remain diagnostic errors. Durability assumes a
local filesystem supporting atomic rename and file/directory synchronization.

## Delivery leases and wakeups

`Ready(workstream, now)` scans durable state for unacknowledged notification
entries without an active lease. Operation intents use the reconciliation API.
`Claim` takes an event ID, stable attempt token, worker ID, timestamp
and positive lease duration. Claims are exclusive within a repository session;
the timestamp and duration come from the service clock. An identical active
claim retry returns the original lease without extending it. Changed parameters
for the same token return `ErrConflict`; a superseded or restart-interrupted token
returns `ErrClaim`. Workers must respect the returned expiry. Each redelivery uses
a new attempt token.

`Acknowledge` and `Release` require the current session's active, unexpired token.
They fence out older attempts. Repeating a completed acknowledgement or release
is a no-op, even after reopen or a subsequent claim, and does not append duplicate
history. Acknowledged entries never become ready. Released and expired entries
become ready, as do claims from a previous repository session: acquiring the
exclusive repository lock establishes that the previous owner is gone. Closing a
handle ends its session; workers must stop using that handle and its claims.

`WaitWorkflow` uses the pinned core adapter's coalescing wakeup primitive. Successful
transactions and releases signal it after durable publication. Signals are only
latency hints: controllers scan on startup and after wakes and periodic ticks.
Lease expiry needs no signal, and reopen deliberately does not replay hints.
External inspection is supplied through the reconciliation adapter contract;
notification delivery policy remains separate from this storage API.

## Durable local operations

An outbox `Event` may carry an immutable `coreadapter.Operation` for a local
repository, runner or container boundary. Ordinary events remain notifications;
operation intents are reserved for reconciliation and cannot be claimed or
acknowledged through the notification delivery API. `OperationID(project,
workstream, eventID)` derives the required external identity. The operation's
boundary, action and JSON input are published with the originating state change.
Input uses the relevant workspace, prepared-turn or sandbox contract and contains
no secret values. Changing input on retry is an identity conflict.

`Operations(workstream)` returns the operation records derived from
`workflow.json`. Each retains the originating transition header and the complete
claim, observation, effect-attempt, retry, terminal-result and acknowledgement
history. Actions retain a service actor, timestamp, operation cause, causal depth,
repository session and attempt token. Results include terminal domain outcomes,
evidence and optional structured data. An infrastructure error leaves the intent
pending; it does not imply that no effect occurred.

`WithOperation` owns one synchronous reconciliation callback. Reconciliation is
serialized per trace repository, including across controllers, and `Close` joins
the current callback before releasing the repository lock. Operation claims use
execution ownership rather than expiring notification leases: a slow external
call cannot overlap a replacement worker. A callback must join all its external
calls before returning; cancellation alone is not proof that a remote process
stopped. Restart acquires the exclusive repository lock and discovers abandoned
claims by scanning. Callback handles cannot write after return. Results and
acknowledgements are immutable; publication errors are resolved by rereading the
same durable identity.

`internal/reconcile.Controller` scans all persisted workstreams on startup and
after wakeups and periodic ticks. Before each effect attempt it records an
adapter inspection by operation ID. A completed inspection supplies the terminal
result without applying again. Only an absent inspection permits an effect;
absence must prove that no previous attempt is running or can complete later.
Running, unreachable or unidentifiable effects are unknown and stay pending.
Adapter errors and unknown observations record a retry time (one second by
default). Retries inspect again, using the same operation ID. A persisted result
needs only acknowledgement after restart. Store/protocol errors stop the loop
and are returned to its owner. Inspection, effect and result writes all use the
same journaled publication boundary as workflow transactions.

The local service opens the active project's existing trace before reporting
readiness and joins its reconciliation loop during shutdown. Project
registration through the service API creates the trace and opens it the same
way without a restart. `service.Options.Reconciliation` supplies
local adapters and optional clocks, ticks and retry/scan intervals. With no
adapter for a boundary, its operations remain unknown and retryable; the service
does not launch work through an adapter lacking identity-based inspection.
Production capability enforcement remains the execution adapter's responsibility.

`reconcile.Options.Schedule` is an optional hook that runs at the start of every
pass, before operations are read, so the intent it publishes is reconciled in the
same pass. An error from it stops the loop. The service installs event delivery and its
queued-turn scheduler as this hook (see [the service](service.md)). The controller makes no
capacity or owner-authorization decisions.

## Durable threads and queued turns

`CreateThread` publishes an immutable `Agent` identity and an empty thread in the
workstream's `workflow.json`, using the same transaction journal as state/outbox
updates. The identity contains the role, stable thread ID and workstream; the
thread snapshot retains its current backend-session reference and status.
`Thread` returns a detached snapshot with every accepted request, its sequence,
claim, captured response and completion time. The initial identity remains in
`agents/<id>/identity.jsonl`; the latest session is in the thread snapshot and
its captured responses. Managed identity and turn records cannot use `Append`.

Every workstream has exactly one chief-of-staff thread, with agent, role and
thread ID `chief_of_staff` (`trace.ChiefOfStaff`). `CreateWorkstream` creates it
after committing the workstream. `EnsureChiefOfStaff` creates it for a
workstream that lacks one and otherwise returns the existing thread unchanged,
so repeated and concurrent calls leave one identity; an existing
`chief_of_staff` agent with another role or thread ID is refused with
`ErrConflict`. `ChiefOfStaffThread` looks the thread up. When the service opens
a project's trace, at startup or on activation, it ensures the thread for every
workstream.

`EnqueueTurn` atomically appends the request to `agents/<id>/log.jsonl` and the
thread queue. Acceptance under the repository lock assigns consecutive sequence
numbers. This order survives restart even when callers share timestamps or
submit concurrently. A turn ID identifies one immutable request within its
agent; duplicate requests return their existing sequence. Changed content under
that ID is rejected. A queued request fixes its profile, prompts and supplied
resume/history context at acceptance. Backend continuation and replay preparation
are separate from this storage API.

`ClaimTurn` reserves the oldest pending request with a caller-retained token,
service-session identity, session directory and start timestamp. There is at most
one active turn per agent. Messages accepted while it runs stay separate pending
requests. Unlike notification leases, turn claims never expire: elapsed time is
not evidence that a backend stopped. Duplicate claim calls do not authorize a
second execution; only the dispatcher that made the reservation may launch it.
A reused token cannot claim another request.

`CaptureTurn` atomically records the final or partial adapter result and appends
its response to the owned log. The request supplies the accepted profile; durable attempts identify any fallback
profile/backend. The response references that exact request and preserves its cause, depth and
scope, plus result timestamps, outcome and failure details. Changed responses
under an existing turn are rejected. `CompleteTurn` requires a captured result
and atomically releases the reservation, making the oldest successor eligible.
It retains a status of `idle`, `waiting`, `failed` or `interrupted` based on the
result. These are thread execution states, not feature or unit transitions.
A thread whose status is `waiting` and that has no unfinished turn is parked
(`Thread.Parked`); queuing a turn unparks it.
All mutations use the state/outbox publication boundary and signal its wakeup
hint. Pending turns are inspected through `Thread` and reserved with `ClaimTurn`;
they do not use the notification `Ready`/`Claim` lease API.

Exact mutation retries are idempotent. On reopen, an active claim from a previous
service session with no durable response is exposed as `interrupted`, with its
reservation, session directory and queued successors intact. It cannot be stolen
or blindly rerun, and stale service sessions cannot submit a new result. A
captured result remains `captured` and can be completed after restart without a
backend call. `AbandonTurn` is the recovery for a reservation a previous
service session never captured a result for: the exclusive repository lock
proves that session is gone, the turn is completed, and the thread's next
request becomes eligible. It refuses the current session's own reservation, a
captured turn and an unreserved one. `CancelTurns` completes the unfinished
turns of a workstream: queued turns and turns a previous session reserved
without a captured result. In each thread it stops at the first turn the
current session reserved or that has a captured result, whichever session
captured it, and leaves that turn and the thread's later turns to a later
call. The response each of them writes depends on the turn's final attempt,
so that the response always equals it:

| Final attempt | Response |
|---|---|
| none, or without a result | `AbandonTurn`: an `interrupted` result with the claim's start time and session directory; `CancelTurns`: a `cancelled` result with the given actor and reason. The final attempt receives the same result and failure |
| with a recorded result | the attempt's result and failure; the turn's status follows them, and a `CancelTurns` reason is not recorded |

As with operation
workers, callers must join turn execution before closing the repository handle.

`internal/thread.Runner` joins the queue to `coreadapter.Turns`. `RunNext` accepts
caller-prepared execution resources and outcome policy, fills the immutable
scope/profile/messages from the claimed request, selects continuation, captures the result,
and completes the turn. Its clock is injected. Cancellation still records the
partial result with a non-cancelled persistence context. After a successful claim,
a persistence error leaves the turn reserved and returns the available claim and
response or attempt evidence so the caller can reconcile the same identity.
Per-turn leases belong to the caller until the adapter is invoked, and competing
calls must not share leases. The runner never reads private backend transcripts
and does not choose scheduling or isolation policy.

Between capturing and completing a turn, the runner appends one `Cost` record
per attempt that returned a result. Its ID derives from the request ID and
attempt number, `Entry.AttemptID` is `thread.AttemptID(requestID, attempt)`, and
the entry carries the full turn scope and the attempt's start time and usage.
The header keeps the request's cause and depth. Existing cost IDs are skipped,
so completing a captured turn again after restart adds only missing entries.

For service-owned runtime isolation, supply `internal/isolation.Turns` as this
runner's `Turns` dependency. Pass context and outcome policy without pre-created
workspace, sandbox, MCP or execution overrides. Resource preparation then happens
inside the claimed turn, and a failed file view or host/container verification is
captured durably before execution. See [turn isolation](isolation.md).


## Turn dispatch

`thread.Dispatcher` is a `coreadapter.Reconciler` for runner-boundary operations
with action `thread-turn`. `thread.TurnOperation` builds the intent from a
`TurnInput` naming the workstream, agent and accepted turn ID; publish it with
the transition that authorizes the turn. Input with unknown fields is rejected.
The request content comes from the durable queue, not the operation.

Inspection reads only the thread snapshot:

| Durable turn state | Observation |
| --- | --- |
| Completed | Completed. The result outcome is the turn's status; data holds its sequence, request ID, response ID and attempt count |
| Captured, not completed | Absent. Apply records any missing cost entries and completes the turn without a backend call |
| Reserved without a captured result | Unknown. It may still be running or was interrupted; it is never relaunched |
| Queued behind an unfinished turn | Unknown, so the controller retries later |
| Queued and next | Absent. Apply calls `Prepare` for per-turn resources, then `Runner.RunNext` |

A captured backend or isolation failure is a terminal result with outcome
`failed` or `interrupted`, not an infrastructure retry. A turn that is still not
complete after Apply returns an error and stays pending. A restarted controller
finds this work by scanning operations, so no wakeup from before shutdown is
needed.

## Continuation and bounded replay

The thread runner selects continuation after claiming the turn. It uses the most
recent completed turn's actual profile and resulting session reference, including
a fallback profile. A healthy completed session can resume only when the backend
matches and the adapter's `ResumeChecker` confirms profile compatibility and
session availability. Profile name changes alone do not force replay; the checker
must explicitly accept model and effort changes. An absent checker, unsupported
backend, malformed reference, missing/corrupt state or incompatible profile
selects replay. Other inspection errors leave the turn reserved for reconciliation.
A failed or interrupted predecessor cannot authorize resume.

Fresh attempts receive deterministic JSON built exclusively from successful,
completed owned request/final-response pairs in queue sequence order. Each pair
includes request/response record headers, thread and turn identity, cause and
depth. Accepted caller-supplied `History` and `Resume` fields remain in the
immutable request for inspection but are not execution authority. Replay excludes
system prompts, nested history, private backend transcripts and partial failed
output. The current turn's accepted system prompt and request are supplied
separately.

`ReplayLimits` defaults to the newest 20 complete exchanges and 65,536 encoded
bytes, including provenance and the omitted-prefix count. Both bounds apply to
the chronological suffix; a pair is never truncated or skipped to fit an older
one. If the newest pair exceeds the byte bound, only the omission marker is
emitted. A bound too small for that marker fails preparation. The marker counts
omitted eligible exchanges; incomplete/failed turns are not eligible exchanges.

Each `QueuedTurn.Attempts` entry in `workflow.json` records intent before launch:
attempt number, actual profile/backend, resume or replay path and reason, source
session and queue boundary, replay start sequence and omitted-prefix count. The
result or failure is persisted before another attempt. The final owned response
must match the final attempt. Request, turn and claim identities remain stable
across all attempts; only one response is appended to the owned log.

A typed `ErrResumeUnavailable` returned before any work is accepted permits one
fresh replay attempt. A typed `ErrNotStarted` permits a service-approved fallback
from `Runner.Fallbacks`, bounded by `MaxRetries` (default zero, maximum ten).
Fallback always starts fresh. These errors must prove there are no outstanding
effects; output, outcome, usage or a resulting session prevents automatic retry.
Ordinary failures and cancellations are recorded without retry. An intent without
a result remains interrupted and reserved after reopen. Per-turn leases span
these safe retries and release once.

## Private role notes

`Repository.NotesTools` supplies `notes_read` and `notes_write` handlers bound
to an accepted project, role, thread and turn. The caller includes them in that
turn's role-scoped MCP host, for example through `isolation.Turns.Scoped`.
`notes_read` is a read tool and `notes_write` a memory tool. Every access
verifies the same service's active, uncaptured turn; queued, completed and
interrupted turns cannot use the handlers.
Input accepts no project, role or path selector, and unknown fields are rejected.

Notes live at `projects/<project-id>/notes/<role>.md` beneath the Osmia root,
shared by that role's workstreams in the project. Writes replace at most 65,536
bytes through the trace's atomic publication/recovery boundary; an empty string
clears the notes. Missing notes read as empty. Scope mismatches, path escapes,
symlink aliases and hardlink aliases are rejected. Notes remain private to the
bound role tools and are not included in replay context.

## Workstream status

The chief of staff keeps one status per workstream (design §6.4). Each
`Status` revision replaces the previous one as a whole: `goal` and `note` are
required, `attention` is non-empty exactly while an owner gate is open, and `agents` is a list, possibly empty, of
non-blank lines. The record ID is always `status`, and revisions are numbered
from one in `workstreams/<id>/status.jsonl`. Status records are never
project-scoped and carry no unit.

`Repository.SetStatus(ctx, agent, scope, content, at, check)` stores the next
revision. The scope must name this service session's active, uncaptured turn
of the agent's thread, and that thread's role must be `chief_of_staff`. Before
anything is written, `check` receives the content and the identifiers the
trace holds for the workstream: project and workstream IDs, agent, thread and
turn IDs, backend session IDs and the turn profiles' models, plus the open
owner gates. A check error is
returned as `*StatusRejected` and stores nothing. The actor is the agent, the
cause is the turn request's ID and the depth is one more than the request's.

`Repository.Statuses()` lists every workstream in manifest order with its
latest status (nil before the first), its feature state, the state of every
workflow subject read in the same pass (`Subjects`), its owner gates and open question
count. The feature state is the current value of the `feature` workflow
subject (`FeatureSubject`), empty until a transition records one. A question
is open while no ruling names it. A damaged record fails the whole read.
An escalated inbox batch is one gate until ruled. A recorded ratification
packet is a gate while its workstream remains in the shed. A contested unit
is a gate while its unit state is contested. Gates give a kind and reference:
the inbox number, workstream ID or unit ID respectively.

`internal/status` holds the checks and the tool. `status.Check` is the one
function that decides whether content is acceptable. Its heuristics reject:

- an empty goal or note, and missing agents;
- a goal that is more than one sentence or 200 characters, an attention or
  agent line over 400 or 200 characters, a note over 1,200 characters or six
  sentences, more than 32 agent lines, and line breaks anywhere but the note;
- identifiers: known trace identifiers shaped like identifiers (with a digit
  or underscore, or at least 16 characters) where they appear as a whole token,
  Osmia-style IDs (`p_`, `w_` or another short prefix followed by hexadecimal
  digits), UUIDs and other session tokens, commit hashes (7 to 40 hexadecimal
  characters with both a digit and a letter), model names (`claude-…`, `gpt-5`,
  `o3-mini`, bare family names such as `Sonnet`), URLs, absolute and relative
  paths, `a/b` refs other than slashed prose such as `and/or`, and file names
  with a common source or configuration extension.

Sentences end at `.`, `!` or `?` followed by white space or the end of the
text. Each message names the field and the identifier found.

`status.Tool(repository, agent, scope, now)` returns the `set_status` memory
tool for one claimed chief-of-staff turn and refuses any other role. Its input
is `goal`, `attention`, `note` and `agents`; unknown fields are rejected. A
status the check refuses is an ordinary tool result,
`{"stored":false,"reason":"…"}`, so the chief of staff reads why; a stored
one returns `{"stored":true,"revision":n}`. Turn isolation grants the tool to
`chief_of_staff` only; see [turn isolation](isolation.md#capabilities).

## Questions

A role's question, the chief of staff's one choice for it and the answer's
way back are records under `workstreams/<id>/questions/<n>/`, where `n` counts
the workstream's questions from 1. Each question also has a workflow subject,
`trace.QuestionSubject(n)` (`question_<n>`), whose state is `open`, `answered`,
`escalated` or, once the owner ruled on an escalation, `ruled`. The state
leaves `open` once, so a question gets exactly one choice; an escalated
question moves on to `ruled` and then `answered`. Every write below is one commit through the journaled publication
boundary: the records, the transition in `events.jsonl` and any event appear
together or not at all. Each write requires the calling scope to name this
service session's active, uncaptured turn, as `SetStatus` does, and a scope of
the wrong role or turn is an ordinary error. A request from the right turn
that cannot be recorded returns `*QuestionRefused` with a reason written for
the agent, and writes nothing.

| Call | Records | Transition |
| --- | --- | --- |
| `Ask(ctx, agent, scope, text, at)` | Revision 1 of `Question` `n`: `asked_by` (the agent), `thread`, `turn` and the scope's unit, with `question` as asked. | `question_<n>_open`, from nothing to `open`, with one `notice` event for the chief of staff: `Question <n> is open, asked by the <role>: <question>`. |
| `AnswerQuestion(ctx, agent, scope, n, text, citations, at)` | Revision 1 of `Ruling` `n`: `question_id`, the question's latest revision, `decision` `answer`, `returned_answer` and `citations`. | `question_<n>_answered`, `open` to `answered`. |
| `EscalateQuestions(ctx, agent, scope, request, at)` | For every listed question, its next `Question` revision: `sent_to_owner` holds the rephrasing and `escalation` holds `batch`, the batch's `questions`, `blocked`, `options` and `recommendation`. The batch ID is `escalation_` and the first listed question. `inbox` is the escalation's inbox number: one more than the highest of the project's escalations, in any workstream, so it is unique in the project and never reused. | `question_<n>_escalated` for each, `open` to `escalated`. |

In every record the actor is the calling agent, the cause is its turn
request's ID and the depth is one more than the request's.

`Ask` fails for a chief-of-staff scope, and `AnswerQuestion` and
`EscalateQuestions` fail for any other; these are ordinary errors, which the
tools pass on as tool errors. `Ask` refuses an empty question, and a second
question from a turn that already asked one. `AnswerQuestion` and
`EscalateQuestions` refuse a question that does not exist, is already
answered, is escalated (`question <n> is escalated to the owner; only the
owner's ruling answers it`), is ruled (`question <n> has the owner's ruling;
relay it with relay_ruling`), or was not asked through `ask` and so has no
workflow subject. An escalated question therefore cannot then be answered by
the chief of staff. An answer needs text and at least one citation; an open
question that already has a ruling record with its number fails with
`ErrConflict`. An escalation needs at least one question, a rephrasing, what is
blocked and a recommendation; options may be empty. A batch that lists a
question twice, or any question that is not open, escalates none of them.

`Questions(stream)` returns each question, oldest first, as a `QuestionState`:
the question as asked, its latest revision, the subject's state and the latest
revision of its ruling. An escalated question has no ruling, so it still
counts as open in the [workstream status](#workstream-status).

### The owner's ruling

`Inbox()` returns every escalation of the project as an `InboxEntry`, by inbox
number: the workstream, the batch, the rephrasing, what is blocked, the
options, the recommendation, when it was escalated, the batch's questions in
the order they were escalated and their shared state. Entries stay listed
after they are ruled on; the [service's inbox](service.md#inbox-and-rulings)
shows those still `escalated`.

| Call | Records | Transition |
| --- | --- | --- |
| `Rule(ctx, number, text, owner, at)` | For every question of inbox entry `number`, revision 1 of `Ruling` `n`: `question_id`, the question's latest revision, `decision` `ruling` and `owner_response`, the text as given. The actor is `owner`, the cause `question_<n>_escalated` and the depth one more than the escalation's. | `question_<n>_ruled` for each, `escalated` to `ruled`. The first carries one `notice` event for the chief of staff: `The owner ruled on inbox entry <number>, <batch> (questions <n>, …): <text>`. |
| `RelayRuling(ctx, agent, scope, n, text, reach, at)` | For every question in the batch of question `n`, revision 2 of its ruling: the owner's revision with `returned_answer`, the text the askers receive, and `scope`, `local` or `notify`. The actor is the calling agent, the cause its turn request's ID and the depth one more than the request's. | `question_<n>_answered` for each, `ruled` to `answered`. |

Each is one commit, so the ruling and its event, and the relay of a whole
batch, appear together or not at all. `Rule` needs no turn scope: it is the
owner's write. It fails with `ErrInboxEntry` for a number no escalation
carries, with `ErrRuled` for an entry that is no longer `escalated`, whether
its ruling was relayed or not, and with an ordinary error for blank text;
each writes nothing. `RelayRuling` fails with an ordinary error for a turn
that is not the chief of staff's. It refuses, with `*QuestionRefused`, a
question that does not exist, one that is already answered, one without the
owner's ruling (`question <n> has no ruling from the owner to relay`), blank
text and a scope other than `local` and `notify`.

A ruling counts from revision 1, so a ruled question no longer counts as open
in the [workstream status](#workstream-status). A `notify` ruling is a notice
in every later [bundle](context.md) on the project; a `local` one reaches only
the askers.

### Tools and delivery

`internal/questions` holds the tools. `questions.Tools(repository, agent,
scope, now)` returns the memory tools of one claimed turn by role: `ask` for
every role but the chief of staff, and `answer`, `escalate`, `relay_ruling`,
`route_amendment` and `propose_charter` for the chief of staff. The
[role grant](isolation.md#capabilities) enforces the same split whatever the
service grants. Unknown input fields are rejected. A refusal is an ordinary
tool result, `{"recorded":false,"reason":"…"}`, so the agent reads why.

| Tool | Input | Recorded result |
| --- | --- | --- |
| `ask` | `question` | `{"recorded":true,"question":"<n>","next":"…"}` |
| `answer` | `question`, `text`, `citations` | `{"recorded":true,"question":"<n>","next":"…"}` |
| `escalate` | `questions`, `rephrasing`, `blocked`, `options`, `recommendation` | `{"recorded":true,"batch":"escalation_<n>","questions":[…]}` |
| `relay_ruling` | `question`, `text`, `scope` (`local` or `notify`) | `{"recorded":true,"questions":[…],"scope":"…","next":"…"}`, naming every question of the batch |
| `route_amendment`, `propose_charter` | any object | Always `{"recorded":false,"reason":"reserved until amendments and standing rulings (M4)"}`; they read and write nothing. |

Before `answer` records anything, `questions.Resolve` checks every citation
against the trace, and the first one that names nothing refuses the answer
with its reason:

| Citation | Resolves when |
| --- | --- |
| `charter#<n>` | The latest charter numbers a rule `n` exactly once. The charter is read through `Repository.Charter`, which first records an owner edit. |
| `kb/<subsystem>.md` | That knowledge-base file exists and can be read. |
| `ruling#<record>` | The question's workstream records a ruling with that record ID. |
| `spec#<n>` | The workstream's latest recorded `spec.md` numbers an acceptance criterion `n` exactly once. |
| `plan#<unit>` | The workstream's latest recorded `plan.json` parses and holds the unit ID exactly once. |

Anything else is refused as not a citation. The citation check runs before the
write and outside its lock, so an owner edit between the two is not seen. A
refused `answer` records nothing of its own, but checking a `charter#<n>`
citation first records a pending owner edit of the charter, as every charter
read does.

`questions.Turns` wraps the turn runner the thread dispatcher uses. After the
wrapped run it reads the workstream's questions, and a turn that asked one
ends with the outcome `waiting` and the report `Asked question <n>`, whatever
the agent reported, so its thread parks. A turn that asked nothing keeps its
result. It forwards resume checks to the wrapped runner.

`questions.Deliverer.Pass` queues each answered question's
`returned_answer` on the thread that asked, as turn `answer_<n>`
(`questions.TurnID`) with request ID `request_answer_<n>`, actor
`service`/`questions` and cause `question_<n>_answered`. The prompt is
`questions.Prompt`: the question number, the question as asked, the answer and
the citations. For the owner's ruling it opens `The owner ruled on your
question <n>. The chief of staff relays the ruling.` instead of `Answer to
your question <n>.`, and the answer is the relayed text. A ruled question is
not delivered until the relay moves it to `answered`. The turn carries the asking turn's unit and system prompt and
the profile `Profile(role)` returns at delivery. The tools only record; this
pass is the one path that delivers. A question whose thread already holds its
answer turn is skipped, so a repeated pass or a restart between the record and
the delivery queues the turn exactly once. A question whose asking agent the
trace does not hold is skipped. A profile
error stops the pass. `questions.Deliver` is the single-question step.

## Feature spec and plan

A workstream's `spec.md` and `plan.json` are ordinary `Document` records with
IDs `spec` and `plan`. Every change is a new revision; `internal/plan` parses
and validates their content and never writes the trace itself. The service
records the architect's drafts, including drafts that fail validation, so the
latest revision is not always a valid one; see
[architect drafting](service.md#architect-drafting).

### spec.md

The spec is free Markdown. Only its acceptance criteria are parsed: the
ordered-list items written as `N. text` in the section headed
`Acceptance criteria` (any heading level, any case). The section ends at the
next heading of the same or a higher level, so subheadings inside it are
allowed. Numbered lists elsewhere, fenced code and HTML comments are ignored.
Lines that follow an item without a blank line continue its text.

```markdown
## Acceptance criteria

1. A plan with a dependency cycle is refused.
2. Every error names the unit or criterion at fault.
```

A criterion is cited as `spec#<n>` with its own list number. `plan.ParseSpec`
returns the criteria and these diagnostics, which never stop parsing:

- no `Acceptance criteria` section, or more than one;
- a section without numbered criteria;
- an item without text;
- a number used more than once, which makes that criterion uncitable;
- a gap in the numbering, starting from 1.

### plan.json

```json
{
  "version": 1,
  "units": [
    {
      "id": "parse-spec",
      "title": "Parse the acceptance criteria",
      "addresses": [
        {
          "criterion": "spec#1",
          "proof": {"kind": "new-test", "name": "TestParseSpec"}
        }
      ],
      "depends_on": [],
      "footprint": ["internal.plan", "trace"]
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `version` | Always `1`. A missing or other version is refused as `plan.ErrUnsupportedVersion` before anything else is read. |
| `units[].id` | Stable unit ID: 1-128 letters, digits, `_` or `-`, starting with a letter or digit. |
| `units[].title` | Optional short description. |
| `units[].addresses` | The criteria the unit addresses, each with its proof. |
| `units[].addresses[].proof.kind` | `new-test`, `existing-test`, `scripted-check` or `reviewer-judgement`. |
| `units[].addresses[].proof.name` | The test, the check, or what the reviewer will judge. |
| `units[].depends_on` | IDs of the units this one waits for. |
| `units[].footprint` | IDs or aliases of [local entity map](knowledge-base.md) entities. |

`plan.Parse` rejects unknown fields. `plan.Encode` writes absent lists as `[]`
with two-space indentation and a trailing newline, keeping unit order.
`Plan.Unit` and `Spec.Criterion` look up a unit or criterion, and refuse an ID
or number that is used more than once. `Plan.Addressing` lists the units that
address a criterion.

### Validation

`plan.Validate(spec, plan, entities)` is a pure function over the parsed spec,
the parsed plan and `kb/entities.json`. It returns every problem in one list,
each a `plan.Problem` with a kind and the unit, the criterion or both:

| Kind | Problem |
| --- | --- |
| `spec` | A spec diagnostic. |
| `unit` | A malformed or duplicate unit ID, or a criterion one unit addresses twice. |
| `unknown-dependency` | `depends_on` names a unit the plan does not have. |
| `dependency-cycle` | The dependencies form a cycle, reported once, starting at its smallest unit ID. |
| `uncovered-criterion` | No unit addresses a spec criterion. |
| `unknown-criterion` | A unit addresses a citation that is malformed, absent from the spec or numbered twice there. |
| `missing-proof` | An addressed criterion has no proof, an unknown proof kind or an unnamed proof. |
| `unresolved-footprint` | A unit declares no footprint, or a footprint name matches no entity with a path pattern. |

An empty list means the plan can be presented. The validator does not judge
whether a unit is too large.
