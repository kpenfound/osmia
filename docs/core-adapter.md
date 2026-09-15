# Core adapter boundary

`internal/coreadapter` is the service-facing contract. It contains no running
backend, controller or delivery implementation. `adaptertest` supplies scripted,
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
