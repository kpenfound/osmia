# Architecture

Osmia runs as one long-lived Go service. The command line and embedded web page
call its local API; agents do not drive the workflow. This page is a map for
operators and contributors. The [design](design.md) defines product behavior.

## From hand-in to delivery

```mermaid
flowchart LR
    Owner[Owner: CLI or web page] --> API[Service API]
    API --> Trace[Repo, workgroup and workstream traces]
    Trace --> Controllers[Go controllers and scheduler]
    Controllers --> Turns[Scoped agent turns]
    Turns --> Trace
    Controllers --> Workspaces[Git or Jujutsu workspaces]
    Controllers --> Delivery[Service-owned push and publication]
    Workspaces --> Delivery
    Trace --> API
```

A project is exactly one repo with its own charter, settings, entity map and dedicated project trace. The service re-seeds the entity map by itself from the upstream default branch when an architect draft starts, before the owner's plan edit, a ratification or an amendment draft is validated, and again before a sealing resolves footprints, and records a revision only when the map changes; no agent writes it. Each workstream has its own complete trace, branch and PR. A cross-project workgroup's separate trace under `workgroups/<group-id>/` owns its workgroup spec, fixed membership, shared chief and decisions. Direct single-project groups keep their chief in the workstream trace. A design session can start members in other projects while its conversation stays at the session's project; the workgroup adopts that thread even for one selected member.

Nothing is authoritative in two places, and every trace stays
outside target clones. Each trace is one SQLite database, `trace.db`, in its
own directory under the root; every document is a revision in it, so the
owner edits the charter, spec and plan with `osmia edit` or the web UI, never
as files. Every write is one database transaction. Go reads current state on each reconciliation pass;
an event wakes a pass early but does not carry the state transition.

One owner start hands the design brief to production drafting and allocation;
the actual design conversation becomes the shared chief thread. For several
selected projects, one ratification pins the workgroup spec and all member revisions. Until every pinned member seal
completes, no member has effective group approval or can build or release a
contract edge. Restart reconciles the same durable decision, without another
gate. Every PR still needs its own delivery approval and owner signing.

Ordinary file, issue and stdin hand-ins use an owner-selected ordered project list, with the first project home. A single project uses ordinary drafting and ratification. Several projects use the shared allocation and join path and all-member packet without an architect selection turn. Design start pins the brief text, revision and ordered list durably, preserving the same member identities through recovery. Each member's charter governs its slice and units; the shared spec and contracts satisfy every member's charter.

Repo/member bindings preserve shared authority when a terminal member is
archived (by the owner, or by the chief of staff when its pull request merged) and legitimately cleanup-deleted. Archive identity, verified
resolution and cleanup intent keep survivor reads, conversation, dispatch,
events, questions and group journals usable without reviving a retired trace.
Unknown or corrupt live history yields errors or recoverable holds. Pending
approved effects retain their original pins; unavailable history never grants
completion. Archive does not establish merge or release dependencies.

Resolution retires a member's own documents and workspaces from active state.
The workgroup spec remains active for the shared chief and unresolved members
until all members resolve (merged or cancelled). A ready but unmerged PR keeps
its member unresolved. Traces and decisions remain until archive.
The same snapshot-based predicate governs live resolution, archive pins and
pane labels: cancellation/abandonment, or delivery with an owner merge record
or merged outcome matching the current publication's PR. Archive records pin
`resolution_evidence`; missing merge or cancel evidence holds the group
unresolved even after cleanup. One-member groups use the same predicate,
separately from their existing delivery cleanup. Every delivered member's
current PR is observed at most once per configured `pull_request_poll` interval
until a matching merged or closed outcome is recorded, including members
without same-repo bases.
Merge-only dependency handling is a Go controller: contract edges resolve on
the ratified contract, and hard-block and merge-order edges on the recorded
upstream PR merge. Expected waits have no inbox or notification side effect.

Permanently archiving a workstream closes its database, moves its trace directory aside
and deletes it, together with its temporary workspaces, once any busy or
pending work has cleared.
See
[design §4.5](design.md#45-where-they-live) and
[§5.1](design.md#51-feature-states).

## Responsibilities

| Part | Owns | Main code |
| --- | --- | --- |
| Service composition | Startup and shutdown, project runtimes, HTTP handling and the endpoint list | [service](../internal/service) |
| Local API types | Request, response, event and activity JSON contracts | [api](../internal/api) |
| Intake and owner gates | Hand-in, design, debate, ratification, questions, charter decisions and amendments | [intake](../internal/intake) |
| Unit execution | Dispatch, checks, exact-candidate review, landing and rebase | [execution](../internal/execution) |
| Final review and delivery | Draft publication and pushes, Cloud check watching, follow-ups, mark-ready approval and own-PR outcomes | [delivery](../internal/delivery) |
| Workgroup lifecycle | Shared approvals, edges, archive cleanup, pull-request polling, base refresh and drift | [lifecycle](../internal/lifecycle) |
| Runtime observations | Activity health, event subscriptions, bounded reads and tailnet connections | [activity](../internal/activity), [eventstream](../internal/eventstream), [readview](../internal/readview), [tailnet](../internal/tailnet) |
| Review evidence | Review bundles, diffs, check reports and check selection records | [review](../internal/review), [checks](../internal/checks) |
| Trace and reconciliation | Versioned records, recoverable transactions, outbox operations and retry | [trace](../internal/trace), [reconcile](../internal/reconcile) |
| Scheduling | Readiness, capacity, priority and dispatch order across workstreams | [scheduler](../internal/scheduler) |
| Durable threads | Queued turns, owned logs, continuation and replay | [thread](../internal/thread) |
| Execution boundary | Scoped file views, tool grants (including the `Web` permission), environment, sandbox verification | [isolation](../internal/isolation), [coreadapter](../internal/coreadapter) |
| Target workspaces | Service-owned Git and Jujutsu operations | [workspace](../internal/workspace) |
| Interfaces | Local client and browser page | [cli](../internal/cli), [web](../internal/web) |
| Jev judgments | Judgment identity, records, reuse, acceptance and costs, over agentcore's Jev client with its retries and cool-down | [jev](../internal/jev) |
| Transition routing | Exits, hard prerequisites, the `decide-transition` table, the route journal, per-target effects and the worker that keeps Jev off the scheduling path | [router](../internal/router) |

The service uses a pinned `agentcore` dependency for reusable execution,
MCP, sandbox and VCS primitives. Osmia keeps its state machine, owner decisions
and recovery policy in this repository.

### Agentcore gaps

Capabilities Osmia needs that the pinned agentcore does not provide, and the
local code that stands in for each. When agentcore provides one, the stand-in is
replaced by it and the entry removed.

| Capability | Stand-in |
| --- | --- |
| The Docker bridge gateway address, for reaching host MCP servers from a container. agentcore computes it internally and does not export it | `bridgeGateway` in [internal/coreadapter/mcp.go](../internal/coreadapter/mcp.go) |
| Git replay that survives a restart. The pinned `vcs/git` replayer keeps no record of the branch's pre-replay tip or the replay boundary, and drops its state when it finishes | `replayRecord` with `loadReplayRecord`, `saveReplayRecord`, `ensureAttached` and `replayFinishedButNotSettled` in [internal/workspace/git.go](../internal/workspace/git.go) |
| Deterministic Jujutsu replay. The pinned `vcs/jj` replayer duplicates commits with random change IDs and wall-clock times, so replaying one range twice does not reproduce the same commit | `ReplayIn`, `ReplayInFrom`, `ContinueReplay` and `Replaying` on `Jujutsu` in [internal/workspace/jujutsu.go](../internal/workspace/jujutsu.go) |

## Decisions and side effects

An owner decision changes the workflow only through the service API. The trace
commits its transition and any outbox intent as one recoverable publication.
Reconciliation inspects durable intent and the external result before retrying
an operation. This matters for branch changes and publication, where repeating
an effect blindly could duplicate or invalidate work.

The scheduler reads queued turns and capacity from trace state. Its `Pass`
method publishes dispatch intent without running a model. Reconciliation hands
that intent to the thread runner, which executes a scoped turn and records its
response. Landing checks the reviewed candidate, base and governing document
revisions before merging it. A changed candidate returns to review. Delivery
pushes the signed branch and opens the pull request as a draft at assembly,
watches its Dagger Cloud statuses and pushes every later commit to it; marking
it ready requires final-review acceptance and each workstream's own owner
approval of the PR description. Amend and cancel use one impact-summary
decision in the shared conversation; later approvals remain separate.

## Transition routing

The state machines that make decisions do not let their callers pick the next
state. A unit, a workstream, a final-review round, a contest or objection, a
dependency wait and a drift resolution each raise an **exit** to the one
transition router ([router](../internal/router)): the trigger that caused it,
the default today's code would take, the legal targets with a line on each, the
evidence from the trace, and the authority the exit carries, if any. The
router commits the result through the trace `Transaction` that already commits a
transition, and records who decided it (`decided_by`), the judgment and the
default it left. [Design §9.6](design.md#96-jev-judgments) defines the
behaviour; this section locates it.

- **Hard prerequisites** filter the targets when the exit is built and are
  checked again against current state each time a target is applied, whoever
  chose it (a Jev pick, a degraded default, a pick recovered after a restart, a
  chief of staff's move, a pinned decision, a card), because a budget or an operation
  can change without the subject's state version changing. They are: a
  candidate exists, for the targets that need one; no landing, rebase or
  publication is in flight; `merged` only after a landing; budgets and the loop
  guard, which counts moves Jev routed; `approved` only on an exit that
  carries a satisfactory reviewer verdict for the current candidate or an owner
  decision; and an owner gate (ratification, delivery approval and publication,
  abandonment, a unit approved without independent review, an intent change)
  only on an exit that carries an owner decision or one of Jev's existing gate
  decisions. A chief of staff's move and a `decide-transition` answer carry
  neither; a chief move is otherwise a pinned move, entered as the chief made it
  with `chief_of_staff` as its `decided_by`. A
  target that has become illegal records `Refuse`, commits nothing, runs no
  effect and leaves the subject held with one notice for the chief of staff.
  The checks depend on the target and never on how it was reached: a pinned
  decision and a default that carries its own entry effects meet the budget and
  loop-guard limits like a Jev pick, and only the landing or rebase whose
  completion raised a unit's exit is exempt from the operations check. The
  owner's pause holds a target that starts work differently: nothing is
  committed or recorded, no work is dispatched and the exit is offered again
  once the pause lifts, since a pause is not a failure.
- **Pinned moves.** An owner's card or command, the recovery card and a gate
  decision Jev already makes authorise a move; they do not choose it again. The
  router applies the exact target with its shared effects, records the authority
  as the transition's decider, emits a `followed` route span and sends Jev no
  request.
- **Every other exit** with at least two legal targets sends Jev one
  `decide-transition` request: a per-trigger summary built from records only,
  capped at 72 KiB, with the narrow questions (the default fits; which target
  fits, asked in two orders; stuck; would change intent; and the trigger's own).
  A versioned code table turns the answers into the next state and records the
  row that chose it. A retry or a send-back needs a modest probability; a
  redraft, a reopen, another debate round, a return to delivered and a dropped
  dependency need `[jev] decision_threshold`. Orderings that disagree, or a
  best target under its bar while the default does not fit, hold the subject and
  tell the chief of staff once; a way forward that would change intent goes to
  the owner.
- **Soft policies** (`mason.max_clean_turns`, the review send-back count,
  `maxFinalRounds`, dependency waits and the chief of staff's own rules for
  contests and `move_workstream`) set the default and no longer stop the
  router; Jev may depart from them at the target's bar.
- **Degraded mode.** A request that times out, is cooled down, fails or is too
  large commits the default, marks the transition and the route span
  `fallback` and shows Jev degraded in the status. The router waits for Jev in
  place for up to its timeout; an exit raised on the scheduling path goes to the
  router's worker, so Jev is never called there.
- **Effects** of each target (a mason's fresh allowance, a fresh check run or
  review, the approval record, notices) are registered once and shared by every
  caller.
- **Stalls and the recovery card.** The stall watchdog
  ([stall_watchdog.go](../internal/service/stall_watchdog.go)) is code only and
  raises a `stalled` exit for a unit, workstream or workgroup change quiet for
  `[loop] stall_after` ([stall_changes.go](../internal/service/stall_changes.go)
  watches the changes). A stall nobody clears, and a move that belongs to the
  owner, end on the recovery card
  ([recovery_card.go](../internal/service/recovery_card.go)), which pins the
  exact move and applies it as a pinned owner decision.
- **Dependency exits and waivers.** An upstream that ended, or whose changes
  are already in the base branch, raises an exit from the dependent's wait
  ([dependency_exits.go](../internal/lifecycle/dependency_exits.go)). Dropping a
  dependency writes a waiver in the dependent's own trace; every reader of
  dependency edges reads the effective edges, so the waived edge stops blocking
  everywhere and is never reported as merged.
- **Not routed.** Project registration and creation, the workgroup-change
  journal, publication operations, outbox delivery and turn admission only
  recover from crashes. They never raise an exit and never call Jev; the stall
  watchdog raises the stall of a workgroup change that sits too long, as it does
  for any subject.

The whole path is exercised by journeys that run the service, its scheduler and
router, a fixture Jev, the trace, the telemetry sink, the feed, the command line,
Hearsay and the cards ([testing](testing.md#routing-journeys)).

## Staying responsive under backend work

Core reads bound their wait on the trace or runtime lock to short,
validation-sized critical sections so they answer promptly whatever else is
blocked; `/status` alone keeps a cached fallback (`coreReadBound`,
[bounded.go](../internal/readview/bounded.go)), answering with its
last snapshot, marked with the `X-Osmia-Stale: true` header, when a refresh
cannot finish in time. `readview.Consistent`
([consistent.go](../internal/readview/consistent.go)) retries a
multi-read response from scratch rather than let it span two generations of
the trace or the runtime store. The in-process activity record
([activity.go](../internal/activity/activity.go)) is a separately locked
structure that every long-running operation reports to and that `GET
/health` and `GET /activity` read without touching the trace mutex or the
operation lock a factory effect holds, so both stay prompt while the effect
they describe is blocked. Every owner-action write admits a command as
accepted, rejected or temporarily unavailable, checks it against the
revision its client saw, and gives it a stable identity so a resubmission,
including one recovered after a restart, is never applied twice. Archive
cleanup reports each pass and each background trace removal to the same
record ([archive_cleanup.go](../internal/lifecycle/archive_cleanup.go)). The
[design](design.md#101-one-process) document defines the exact guarantees;
this page only locates the code.

## Agent boundary

The service selects a workspace and copies only approved files into a private
turn view. It chooses the role's tools and environment and checks the prepared
execution policy before the agent runs. Agents receive no VCS tool or delivery
credential. Writable output is captured by service code before the turn view is
removed; the service then updates the target workspace.

Web access is a separate `Web` permission, not part of `Network`. It adds the
backend's own web tools (Claude `WebSearch` and `WebFetch`, Codex `web_search`,
OpenCode `websearch` and `webfetch`) to a turn's native tools; the service runs
no search or fetch of its own and keeps no record of web reads. Only the
planning roles hold it: the design session, the exploratory session, the
architect on every turn path (including a workgroup's project-spec draft and
delta amendment turns), the ordinary chief of staff and the committee in plan
and amendment debate rounds (including a workgroup's delta review). Masons, reviewers, final reviewers and the
foreman never do, whatever a service grant
says, and no configuration key changes that. A role that holds it keeps its
read-only view and its service tools, gains no write, shell or `Network`
grant, and is told in its system prompt that web content is evidence rather
than authority and that instructions inside a page are not instructions to it.
In sbx the owner's network policy decides which hosts a fetch reaches; the
service adds none. The risk that a page carries hostile instructions is
accepted because those roles can only read and propose. When a backend or
placement cannot hold a web-granted turn to its granted tools, the turn is
prepared again once without `Web` and runs without web tools, its system prompt
saying so; the mismatched session never runs, and the reduced session must
match the reduced grant exactly or the turn is refused.

## Jev judgments

A Jev judgment is a bounded question that turn code, or a unit's check run,
asks through the service's `jev.Judge`, never scheduling code: a judgment waits
on a network call. The
Judge is one per service and reads the `[jev]` settings on each call. It is
never nil: without Jev's API key it is unconfigured, and every judgment takes
its fallback. The
[design](design.md#96-jev-judgments) defines what a judgment may decide.
Turn code that informs a session, rather than replacing its result, uses the
thread runner's `Advise` hook: it runs after the turn is claimed and before
the session starts, its text ends the prompt, and the turn's response records
it.

A judgment supplies the turn's scope, the trace record that caused it, a task
name and version, the records its state was built from, the
agentcore `jev/systemone` request, and an acceptance function
holding the task's thresholds. Two rules keep recorded decisions meaningful:

- Bump the version whenever the questions, the meaning given to their answers
  or the thresholds held in code change. The request is part of a judgment's
  identity, but the acceptance function is not, so a changed threshold under
  the same version leaves earlier decisions as they were recorded. A threshold
  the owner configures, such as the check selection's, goes in the judgment's
  `Acceptance` value instead, which is part of its identity and record.
- Use the answers only when the decision is `Accepted`. A fallback, whatever
  its reason, takes the path the workflow takes without Jev's answer, so that
  path must exist and be the one tested with an unconfigured Jev. A declined
  fallback still carries the answers for evaluation; they must not drive the
  workflow.

The Judge records the judgment and its cost itself. It asks through
agentcore's `jev.Judge`, which owns the API client, the retry of a timeout or
unavailable provider, the cool-down after rate limits and outages, and the
status the runtime reports. A consumer records what it
did with the decision as it records any other result, and can cite the
decision's ID. Keep task questions, thresholds and interpretation in the
consumer's package, or in a package of their own when more than one program
asks the judgment, as [checkselect](../internal/checkselect) does for the
service and `cmd/smartcheck` ([smart checks](smart-checks.md)); `jev` holds
only what every judgment shares, and agentcore's `jev` knows nothing of
Osmia.

## Storage and deployment

The root defaults to `~/.local/share/osmia` and the top-level configuration to
`~/.config/osmia/config.toml`, following the XDG base directories; an explicit
root holds its own `config.toml`, which Osmia reads and never writes. Runtime overrides,
the project registry and every project's settings (in `project-store/`), project traces, workgroup traces and workstream trace databases and workspaces live
in the root; the target clone remains separate. The
service binds a Unix socket for local clients and can serve the same API on a
loopback listener or an embedded tailnet node. Tailnet membership controls access
to the remote page; there is no separate Osmia login. See
[configuration](configuration.md) and [getting started](getting-started.md).
