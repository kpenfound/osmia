# Testing

Start with [CONTRIBUTING.md](../CONTRIBUTING.md) for the contributor workflow.

The test suite uses fake agents, GitHub clients, providers and container engines.
Filesystem and version control tests use temporary directories and local
repositories. Tests never need a live model, remote push or real delivery.

## Run the checks

From the repository root, with the Dagger engine available:

```sh
dagger check
```

The factory supplies the pinned experimental Dagger release through
`DAGGER_X_RELEASE`. Outside that environment, set it to `v1.0.0-beta.14`.
The Go check runs the complete suite in a Go container and installs the pinned
`jj` binary. The browser check installs Chromium and runs the page tests with
`OSMIA_BROWSER` set. The release check builds and exercises the versioned
archive. See [dagger.toml](../dagger.toml) for the registered checks.

Never run `go test` on the host. Tests can leave processes behind on the
machine running them. For a focused package or test, use the Dagger container
command in [AGENTS.md](../AGENTS.md#validation). Host `gofmt`, `go build` and
`go vet ./...` are allowed; they do not execute tests.

## What the suite covers

| Boundary | Representative tests | What they establish |
| --- | --- | --- |
| Workflow and owner gates | [service](../internal/service) | Ratification, questions, amendments, exact-candidate review, landing, final review and approved delivery |
| Recovery | [trace](../internal/trace), [reconcile](../internal/reconcile), service restart demonstrations | Atomic records, durable operations and reconciliation after interruptions |
| Capacity and scheduling | [scheduler](../internal/scheduler), service parallel work tests | Slot accounting, priority, pauses and disjoint unit dispatch |
| Agent boundary | [isolation](../internal/isolation), [coreadapter](../internal/coreadapter) | Scoped file views, grants, environment allowlist and execution policy checks |
| Workspaces | [workspace](../internal/workspace), service Jujutsu demonstrations | Git and Jujutsu behavior, rebase and recovery using local repositories |
| Operator interface | [cli](../internal/cli), service browser tests | Commands, API behavior, owner decisions and live page updates |

The [review](review.md) describes what was inspected and which risks remain.
Test counts are useful for finding the suite, but assertions and exercised
boundaries determine confidence.

## Keep service tests focused

Service lifecycle tests start real reconcilers and write durable traces in local
Git repositories. The standard Go and browser checks mount `/tmp` in memory
for their test commands. Temporary repositories survive service restarts within that command
and are discarded when the command ends. This exercises real Git, Jujutsu and
filesystem operations, including the service's durability calls, without paying
for container-layer disk writes on every fixture transaction.

Each workflow read verifies and decodes trace records; frequent
polling competes with the controllers doing the work. Parallel tests also share
the container's CPU and filesystem, so adding parallelism can increase contention.

- Keep the end-to-end demonstrations for lifecycle, recovery and wiring coverage.
- Mark isolated top-level tests parallel, including parents of parallel table
  cases. A serial parent prevents the other parallel tests from running until
  its children finish. Tests that change process environment variables must
  stay serial.
- For controller tests, seed the state immediately before the behavior under
  test. `seedBuild` records a sealed plan and builds its unit graph for amendment,
  review, landing, rebase, final-review and delivery fixtures. Landing fixtures capture
  completed mason reports, snapshot candidates and approve reviews directly;
  rebase fixtures create the required completed and queued turns.
- Use the running service when the assertion concerns scheduling, API wiring or
  recovery across a service restart. Use direct controller passes for individual
  decisions and idempotency. Keep rejection tests that protect owner gates,
  stale-candidate checks, capacity and execution boundaries.
- In running-service fixtures, use `serviceChanges` before the first predicate
  read and release the subscription when finished. Notifications are hints: read
  the durable state after each wake. The helper has a timer fallback for changes
  the event stream does not announce.
- Coordinate blocked fake turns with channels. Use elapsed-time assertions only
  when a timeout is the behavior being tested, with a small configured budget.

## Profile inside Dagger

Measure the same tests with the same container, race setting and parallelism.
Use `-count=1` to bypass Go's test-result cache and `-parallel=1` to isolate the
cost of a lifecycle from contention with other tests. Compilation, image pulls
and engine startup are separate from the durations printed by `go test`.

For example, this profiles a Git-backed service test and prints CPU and blocking
summaries. The binary and profiles stay in the container:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-mounted-temp --path /tmp \
  with-exec --args=sh,-c,'go test -count=1 -parallel=1 -timeout=10m -run=TestDisjointUnitsImplementConcurrently -v -o /tmp/service.test -cpuprofile=/tmp/service.cpu -blockprofile=/tmp/service.block ./internal/service && go tool pprof -top -cum /tmp/service.cpu && go tool pprof -top /tmp/service.block' \
  combined-output
```

Use the pinned `jj` installation from the focused-test command in
[AGENTS.md](../AGENTS.md#validation) when profiling Jujutsu tests. Cumulative CPU
percentages overlap along call stacks; blocking profiles aggregate time across
goroutines. Neither should be added up as wall-clock time.
