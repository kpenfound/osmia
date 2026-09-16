# Trace repository

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
| `Document` | `documents.jsonl` and its document path | Path and complete content of each revision |
| `Transition` | `events.jsonl` | Subject, prior/resulting state and reason |
| `Question` | `questions/<id>/question.jsonl` | Asking actor, original question and owner-facing text |
| `Ruling` | `questions/<question-id>/rulings.jsonl` | Question revision, decision, owner response, returned answer and affected references |
| `Agent` | `agents/<id>/identity.jsonl` | Stable role/thread identity and backend session at that revision |
| `TurnRequest` | `agents/<agent-id>/log.jsonl` | Thread/turn identity, accepted profile, system prompt, request and caller-supplied context |
| `TurnResponse` | `agents/<agent-id>/log.jsonl` | Exact request revision, thread/turn identity, adapter result and any execution failure |
| `Cost` | `ledger.jsonl` | Adapter ledger entry with attempt, full scope, time and explicit cost knowledge |
| `Status` | `status.jsonl` | The chief of staff's goal, attention, note and agent lines; see [workstream status](#workstream-status) |

Only documents can be project-scoped. Project document paths are `charter.md`,
`kb/entities.json`, `kb/<name>.md` and `notes/<role>.md`; workstream document paths
are `spec.md`, `plan.json` and `handed/<name>`. Handed inputs are immutable.
Document paths retain one record identity. Agent role and thread IDs stay stable
across backend-session revisions. An agent may have no backend session before
its first turn. Failed execution may have no session identity when it includes
an explicit failure. Unknown cost is stored as zero with `CostKnown: false`;
it is distinguishable from a known zero-cost result.

`Append(ctx, record)` accepts concrete record values other than `Status`,
which only `SetStatus` writes. Revisions must begin at one
and increase consecutively for each kind/ID within its scope. It preserves prior
JSONL entries, writes the latest document content to its ordinary file and commits
the affected files. For example, changing a spec retains both full document
revisions in `documents.jsonl` and both versions of `spec.md` in Git history.
There is no terminal-history deletion operation.

`RecordDocuments(ctx, documents)` records several project document revisions
as one commit through the journaled publication boundary, so after a failure
or an interruption either all of them are recorded or none is. Every revision
is checked against the recorded history before anything is written; the
charter is refused because the owner edits it. A `kb/<subsystem>.md` revision
with empty content records the subsystem's removal: the revision is kept in
`documents.jsonl` and the file is deleted from the tree and from disk. The
[knowledge-base extraction](knowledge-base.md#extraction) records each pass
this way.

`Read[trace.Document](repository, workstreamID)` enumerates typed revisions;
`Get[trace.Document](repository, workstreamID, id, revision)` retrieves one.
Use an empty workstream ID for project documents. Enumeration follows file and
line order, not a global event ordering. Reads use ordinary files, including the
owned request/final-response log, without backend transcript files, the target
clone or Hearsay. The execution payloads reuse the core adapter contracts;
trace accounting does not change the operational ledger adapter's file format.

## Charter

The owner edits `charter.md` directly, so it is the one tracked file allowed to
differ from committed history; appending any other record neither refuses nor
commits such an edit. `Charter(ctx, at)` is the only way to read the charter:
it returns the latest recorded revision (document ID `charter`) after
recording `charter.md` as a new revision by the owner (`owner`/`local`, cause
`owner-edit`) when the file differs from the latest revision. A read without an
edit records nothing. The file itself is not rewritten, and the commit takes
the recorded bytes, so an edit saved meanwhile is recorded by the next read.
`Append` of a project `charter.md` revision is refused with `ErrConflict`
unless the file matches the latest recorded revision. The rule format is
described in [charter](charter.md).

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
same pass. An error from it stops the loop. The service installs its queued-turn
scheduler as this hook (see [the service](service.md)). The controller makes no
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
proves that session is gone, so the turn is completed with an `interrupted`
response carrying the claim's start time and session directory, and the
thread's next request becomes eligible. It refuses the current session's own
reservation, a captured turn and an unreserved one. As with operation
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
required, `attention` may be empty, and `agents` is a list, possibly empty, of
non-blank lines. The record ID is always `status`, and revisions are numbered
from one in `workstreams/<id>/status.jsonl`. Status records are never
project-scoped and carry no unit.

`Repository.SetStatus(ctx, agent, scope, content, at, check)` stores the next
revision. The scope must name this service session's active, uncaptured turn
of the agent's thread, and that thread's role must be `chief_of_staff`. Before
anything is written, `check` receives the content and the identifiers the
trace holds for the workstream: project and workstream IDs, agent, thread and
turn IDs, backend session IDs and the turn profiles' models. A check error is
returned as `*StatusRejected` and stores nothing. The actor is the agent, the
cause is the turn request's ID and the depth is one more than the request's.

`Repository.Statuses()` lists every workstream in manifest order with its
latest status (nil before the first), its feature state and its open question
count. The feature state is the current value of the `feature` workflow
subject (`FeatureSubject`), empty until a transition records one. A question
is open while no ruling names it. A damaged record fails the whole read.

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

## Feature spec and plan

A workstream's `spec.md` and `plan.json` are ordinary `Document` records with
IDs `spec` and `plan`. Every change is a new revision; `internal/plan` parses
and validates their content and never writes the trace itself.

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
