# M1 trace repository

`internal/trace` persists the service's typed records in the dedicated local Git
repository at `<root>/projects/<project-id>`. `Create` accepts a resolved
`config.Root`, the configured project, a timestamp and an actor. It reserves the
repository with an exclusive directory creation and preserves an existing
`config.toml`. A target clone and the Osmia root must be separate, non-nested
paths. `Repository` is a trace handle; it does not implement a target workspace
or provide delivery operations.

`CreateWorkstream` reserves a supplied `config.WorkstreamID`. Project and
workstream manifests retain their identity and creation provenance. `Workstreams`
returns identities validated against those manifests, including terminal streams.
The package supplies storage APIs; onboarding and lifecycle commands are separate.

## Files and records

Creation initializes `charter.md`, `kb/entities.json`, `kb/`, `notes/` and
`workstreams/`. Workstream creation initializes `handed/`, `shed/`, `amendments/`,
`questions/`, `units/` and `agents/`, plus the empty document, transition and cost
logs. Spec and plan files appear when their first document revision is appended.
Empty directories exist on disk; Git records files.

Every record carries a schema named `osmia.trace.<kind>`, version `1`, record ID,
one-based revision, project and optional workstream/unit identity, timestamp,
actor, cause and causal depth. Actors name an owner, service component or agent.
The caller supplies stable causal message/operation/record references; storage
does not infer authority, readiness or workflow transitions from them.

| Go record | File relative to the project or workstream | Payload |
| --- | --- | --- |
| `Document` | `documents.jsonl` and its document path | Path and complete content of each revision |
| `Transition` | `events.jsonl` | Subject, prior/resulting state and reason |
| `Question` | `questions/<id>/question.jsonl` | Asking actor, original question and owner-facing text |
| `Ruling` | `questions/<question-id>/rulings.jsonl` | Question revision, decision, owner response, returned answer and affected references |
| `Agent` | `agents/<id>/identity.jsonl` | Stable role/thread identity and backend session at that revision |
| `TurnRequest` | `agents/<agent-id>/log.jsonl` | Thread/turn identity, profile, resume identity, system prompt, request and replay context |
| `TurnResponse` | `agents/<agent-id>/log.jsonl` | Exact request revision, thread/turn identity, adapter result and any execution failure |
| `Cost` | `ledger.jsonl` | Adapter ledger entry with attempt, full scope, time and explicit cost knowledge |

Only documents can be project-scoped. Project document paths are `charter.md`,
`kb/entities.json`, `kb/<name>.md` and `notes/<role>.md`; workstream document paths
are `spec.md`, `plan.json` and `handed/<name>`. Handed inputs are immutable.
Document paths retain one record identity. Agent role and thread IDs stay stable
across backend-session revisions. An agent may have no backend session before
its first turn. Failed execution may have no session identity when it includes
an explicit failure. Unknown cost is stored as zero with `CostKnown: false`;
it is distinguishable from a known zero-cost result.

`Append(ctx, record)` accepts concrete record values. Revisions must begin at one
and increase consecutively for each kind/ID within its scope. It preserves prior
JSONL entries, writes the latest document content to its ordinary file and commits
the affected files. For example, changing a spec retains both full document
revisions in `documents.jsonl` and both versions of `spec.md` in Git history.
There is no terminal-history deletion operation.

`Read[trace.Document](repository, workstreamID)` enumerates typed revisions;
`Get[trace.Document](repository, workstreamID, id, revision)` retrieves one.
Use an empty workstream ID for project documents. Enumeration follows file and
line order, not a global event ordering. Reads use ordinary files, including the
owned request/final-response log, without backend transcript files, the target
clone or Hearsay. The execution payloads reuse the core adapter contracts;
trace accounting does not change the operational ledger adapter's file format.

## Failure and ownership boundaries

One handle holds an exclusive process lock until `Close`; concurrent calls on
that handle serialize. External writers must not modify or move the repository
while it is open. Files use root-confined operations and temporary-file replacement
with file and directory sync. Existing symlinks, hardlink aliases, special files,
absolute/traversal paths, unsupported schema versions, unknown or duplicate JSON
fields, invalid provenance and inconsistent revision identities are rejected.
Every existing path component is checked before access.

Git uses an isolated local configuration, no inherited Git routing or credentials,
no global/system configuration, and disabled hooks. It hashes supplied bytes using
plumbing commands, bypassing attributes and filters. Only trace-owned files enter
its commits; project configuration is left in place. Linked worktrees, redirected
object stores, Git symlinks and changes to the isolated `.git/config` are rejected.
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
kind and body for eventual chief-of-staff delivery. Local operation intents also
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
claim, acknowledgement and release history, sorted by event ID.

The repository serializes calls under its exclusive process lock. Publication
writes immutable Git objects using a private index, syncs the objects, writes and
syncs a recovery journal under `.git`, then atomically replaces and syncs the
branch ref. That ref is the visibility boundary. Ordinary workflow, transition
and owned agent files are materialized from the committed objects before the journal is removed.
Before publication, recovery retains the prior state; after publication, recovery
finishes materializing the complete new state. Store reads and writes finish any
pending publication before inspecting the files. Direct filesystem readers must
open through the store first after an interruption; they may otherwise see
unfinished materialization. Unreferenced preparation objects are never state.

An error after ref publication can mean the transaction committed. Retry its
original identity to discover the result, rather than inventing a new identity.
The journal only authorizes recovery of workflow, transition, agent identity and
owned turn-log files; unrelated append failures and corrupt files remain diagnostic errors. Durability assumes a
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
readiness and joins its reconciliation loop during shutdown. It does not create
a trace for an uninitialized project. `service.Options.Reconciliation` supplies
local adapters and optional clocks, ticks and retry/scan intervals. With no
adapter for a boundary, its operations remain unknown and retryable; the service
does not launch work through an adapter lacking identity-based inspection.
Production capability enforcement remains the execution adapter's responsibility.
This controller supplies M1 recovery infrastructure, not lifecycle scheduling,
capacity decisions or owner authorization.

## Durable threads and queued turns

`CreateThread` publishes an immutable `Agent` identity and an empty thread in the
workstream's `workflow.json`, using the same transaction journal as state/outbox
updates. The identity contains the role, stable thread ID and workstream; the
thread snapshot retains its current backend-session reference and status.
`Thread` returns a detached snapshot with every accepted request, its sequence,
claim, captured response and completion time. The initial identity remains in
`agents/<id>/identity.jsonl`; the latest session is in the thread snapshot and
its captured responses. Managed identity and turn records cannot use `Append`.

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
its response to the owned log. The request supplies profile/backend provenance;
the response references that exact request and preserves its cause, depth and
scope, plus result timestamps, outcome and failure details. Changed responses
under an existing turn are rejected. `CompleteTurn` requires a captured result
and atomically releases the reservation, making the oldest successor eligible.
It retains a status of `idle`, `waiting`, `failed` or `interrupted` based on the
result. These are thread execution states, not feature or unit transitions.
All mutations use the state/outbox publication boundary and signal its wakeup
hint. Pending turns are inspected through `Thread` and reserved with `ClaimTurn`;
they do not use the notification `Ready`/`Claim` lease API.

Exact mutation retries are idempotent. On reopen, an active claim from a previous
service session with no durable response is exposed as `interrupted`, with its
reservation, session directory and queued successors intact. It cannot be stolen
or blindly rerun, and stale service sessions cannot submit a new result. A
captured result remains `captured` and can be completed after restart without a
backend call. The interruption recovery policy is separate. As with operation
workers, callers must join turn execution before closing the repository handle.

`internal/thread.Runner` joins the queue to `coreadapter.Turns`. `RunNext` accepts
caller-prepared execution resources and outcome policy, fills the immutable
scope/profile/messages from the claimed request, runs once, captures the result,
and completes the turn. Its clock is injected. Cancellation still records the
partial result with a non-cancelled persistence context. After a successful claim,
a persistence error leaves the turn reserved and returns the available claim and
response so the caller can reconcile the same identity; it never automatically launches another attempt.
Per-turn leases belong to the caller until the adapter is invoked, and competing
calls must not share leases. The runner never reads private backend transcripts
and does not choose scheduling, resume/replay or isolation policy.
