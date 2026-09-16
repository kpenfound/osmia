# Core adapter boundary

`internal/coreadapter` is the service-facing contract. It implements prepared-turn translation, role-scoped MCP hosting and provider
workspace leases. It contains no controller or delivery implementation. `adaptertest` supplies scripted,
process-free fixtures for every port, including resource cleanup. Each script
returns configured results/errors, records calls, honours pre-cancellation and
fails when exhausted. It does not emulate an enforcing sandbox or durable store.

The direct dependency is pinned to
`github.com/kpenfound/busybees/core v0.0.0-20260915214133-94e7a8d3105e`.
Only public core packages may be imported inside adapters. No sibling checkout
or local replacement is needed. The public `vcs.Workspace` compile-time fixture
keeps the dependency checked by Go without executing a session or repository.
Go dependency checksums live in `go.sum`; `dagger.lock` records the Dagger module
and container images, not Go module versions.

## Boundary map

Package names below are relative to `github.com/kpenfound/busybees/core`.
Capabilities describe this pinned version, based on its public API and README.

| Osmia port | Core package / primitive | Boundary and upstream gaps |
|---|---|---|
| `Turns` | `agent.Runner`, `Request`, `Result`, backend implementations | Runs a prepared turn and returns backend identity, final response, outcome, cost knowledge and failure detail. Resume is backend-dependent: the pinned Codex path ignores `ResumeID`. Uniform resumable backends are an upstream gap. Osmia owns log replay, turn queues and profile selection. |
| `Sandboxes` | `agent.Profile`, sandbox/container primitives; `vcs.Workspace` | Core supplies execution settings and mounts. **Upstream gap:** no verified enforcement boundary covering VCS executable denial, metadata protection, clean host environment, delivery-credential exclusion and repository-tool escalation. The contract separates required isolation from verified isolation; flags or prompts are not verification. Unsupported requirements must fail before launch. |
| `MCPHosts` | `mcphost.Registry`, role policies, transports | Hosts supplied role-scoped tools with explicit capabilities. Osmia supplies handlers, fixed routes and outcome validation. No workflow handlers are inherited from busybees. |
| `Workspaces` | `vcs.Provider`, `Workspace`, `Directory` | Caller owns acquisition and lifetime across turns. Core exposes the provider interface but **no reusable concrete git-worktree provider** in the public module; that implementation is an upstream gap. The lease port neither commits nor rebases nor delivers. |
| `Reviews` | `review.Runner`, `Bundle`, `ReadArtifact`, findings | Accepts supplied context and diff; retains partial artifacts, findings and session accounting. Core does not bind approval to spec/plan/candidate revisions: Osmia carries that identity and owns approval checks. Interactive review threads belong to Osmia. |
| `Retries` | `ops.ClassifyFailure`, `RetryPolicy.Decide`, `SelectModel` | Classification and bounded retry advice only; no sleeping or dispatch. **Upstream gap:** model fallback selection does not resolve named profiles across agent binaries. Osmia resolves its profile graph and supplies the selected profile. |
| `Ledger` | `ops.Ledger`, `Spend` | Accounting storage and totals only. Core append is not an idempotent transaction with workflow state. Attempt reconciliation and durable trace integration belong to Osmia; unknown costs remain visible. |
| `Budgets` | `ops.OverBudget`, `EvaluateWindow`, `Streaks`, `CapacityPause`, `Degraded` | Numeric threshold/window primitives; pauses, scope selection and degradation responses belong to Osmia. `BudgetRequest` receives already-selected spend. Episode tracking can remain private to implementations; this contract adds no controller state. |
| `Capacity` | `ops.SharedPool` | All-or-none slot claims. Core's queued-member FIFO order is not Osmia's stage/workstream scheduling policy. The adapter must preserve caller ordering and return unavailable claims without blocking; it must not delegate scheduling to core. |
| `Wakeups` | `ops.Bus`, `NewWake`, `Wake.Wait` | Coalescing hints plus caller ticks. Bus delivery is lossy; it never substitutes for the durable outbox or authoritative state reconciliation. |

## Contract choices

Contracts use Osmia strings and records rather than core types or GitHub issue/PR
identity. The service chooses every role, profile, capability, scope, retry limit
and capacity order. Reported turn outcomes and review verdicts are evidence for
controllers; none of these ports changes workflow state or grants owner approval.

A prepared turn receives an acquired sandbox and MCP endpoints. The sandbox
request names workspace directory/access intent, a tool allowlist, file-write,
execution and network capabilities, a complete public environment, and scoped
provider/MCP credential references. Implementations must deny VCS tools,
writable VCS metadata, inherited environment and delivery credentials. Read-only
roles cannot gain write, execution or fetch access through repository tools.
These are requirements for later enforcement adapters, not security guarantees
provided by these data types or fixtures.

Resource leases are service-owned and can span turns. Release is idempotent;
a failed release may be retried with a non-cancelled cleanup context. Results may
accompany errors, preserving partial execution/review evidence. Context
cancellation is distinguishable with `errors.Is(err, context.Canceled)` and
unsupported requirements with `errors.Is(err, coreadapter.ErrUnsupported)`.

The upstream gaps above require reusable extensions in core, not copied
busybees internals. The contracts deliberately leave workflow states, readiness,
priority, owner gates, durable queues/logs/outbox and VCS delivery outside core.

## Execution adapters

`TurnRunner` translates a prepared turn into core's `agent.Request` and maps its
`Result` back to Osmia, retaining partial results alongside errors. The role is
core's profile name; the selected backend/model/effort and limits remain explicit.
MCP endpoints receive deterministic names. History is prepended as supplied by the
caller, and resume requests are rejected for incompatible backends. Empty outcome
allowlists accept nothing, including when core's nil list would accept everything.

`SessionExecutor` is the core-facing seam for an enforcing execution boundary.
Its capability check runs before the execution attempt. Its environment is the
complete caller allowlist, with literal values; its generic mounts are exactly
`ExecutionSettings.Mounts`. No VCS workspace resources or VCS environment are
forwarded, and core's VCS access flag is always false. This is translation, not
proof of filesystem isolation: the executor must enforce the verified sandbox's
capabilities, including read-only access and denial of repository-tool escalation.

**Live execution is unavailable through `CoreExecutor` at this pin.** Core's
public runner inherits host environment variables and automatically forwards some
container credentials; its process/backend abstraction is private. `CoreExecutor`
therefore returns a typed unsupported-environment error before launch. It does not
change the process-global environment or treat the VCS flag as isolation. A
reusable clean-environment execution extension belongs upstream in core. The
fake executor tests validate request translation, not actual isolation. Unresolved
credential references, unsupported sandbox/backend modes, cost caps, and backend
turn limits or resume modes that core ignores also fail before execution.

`WorkspaceAdapter.Select` is supplied by the service: it selects a core provider,
validates its capabilities and translates source, name, revision and access
requirements to the provider's request. The adapter validates the returned
directory and releases the exact acquired core workspace, including a partial
acquisition returned with an error. It never prunes, commits, merges or pushes.
Providers remain responsible for capability enforcement and acquisition rollback
when they return no workspace.

A prepared turn may transfer a workspace lease to `TurnRunner`; it is released
on success, failure, rejected execution and cancellation unless `RetainWorkspace`
is set. `Cleanup` transfers other per-turn leases, such as a sandbox or MCP host,
for reverse-order cleanup. Unlisted resources remain service-owned. Cleanup uses
a non-cancelled context, preserves execution errors, and reports release failures.
Workspace release is serialized, idempotent on success and retryable on failure.

`MCPHost` builds a fresh core registry for the supplied role, registering only
supplied tools intersecting the capability allowlist. An absent role or malformed
approved tool is rejected before hosting; hidden and unknown names cannot reach a
handler. `HTTPTransport` serves a caller-bound listener using core's authenticated
HTTP transport, with a caller-supplied token and endpoint. The service owns address
selection and credential delivery. Tests use in-memory SDK transports and fake
execution/providers; they launch no agents, container engines or VCS processes.
