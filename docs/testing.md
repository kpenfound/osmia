# Testing

Start with [CONTRIBUTING.md](../CONTRIBUTING.md) for the contributor workflow.

The test suite uses fake agents, GitHub clients, providers and container engines.
Filesystem and version control tests use temporary directories and local
repositories. Tests never need a live model, remote push or real delivery.

## Run the checks

From the repository root, with the Dagger engine available:

```sh
dagger check --progress=report
```

The Go check runs the complete suite with the official Dagger Go module, in the
test container from the `osmia-dev` module. That container installs the pinned
`jj` binary and Chromium, and sets `OSMIA_BROWSER`, so the browser tests run with
the rest of the suite. Each package's tests run in their own container, in
parallel with the other packages. The release check builds and exercises the
versioned archive. See [dagger.toml](../dagger.toml) for the registered
checks.

Never run `go test` on the host. Tests can leave processes behind on the
machine running them. For a focused package or test, use the Go check's
selection flags described in [AGENTS.md](../AGENTS.md#validation), such as
`dagger check --go-test=TestA`. Host `gofmt`, `go build` and `go vet ./...` are
allowed; they do not execute tests.

## What the suite covers

| Boundary | Representative tests | What they establish |
| --- | --- | --- |
| Workflow and owner gates | [service](../internal/service) | Ratification, questions, amendments, exact-candidate review, landing, final review and approved delivery |
| Recovery | [trace](../internal/trace), [reconcile](../internal/reconcile), service restart demonstrations | Atomic records, durable operations and reconciliation after interruptions |
| Capacity and scheduling | [scheduler](../internal/scheduler), service parallel work tests | Slot accounting, priority, pauses and disjoint unit dispatch |
| Agent boundary | [isolation](../internal/isolation), [coreadapter](../internal/coreadapter) | Scoped file views, grants, environment allowlist and execution policy checks |
| Workspaces | [workspace](../internal/workspace), service Jujutsu demonstrations | Git and Jujutsu behavior, rebase and recovery using local repositories |
| Operator interface | [cli](../internal/cli), service browser tests | Commands, API behavior, owner decisions and live page updates |

Test counts are useful for finding the suite, but assertions and exercised
boundaries determine confidence.

## Keep service tests focused

Service lifecycle tests start real reconcilers and write durable traces in local
Git repositories. The Go check mounts `/tmp` in memory for its test
commands. Temporary repositories survive service restarts within that command
and are discarded when the command ends. This exercises real Git, Jujutsu and
filesystem operations, including the service's durability calls, without paying
for container-layer disk writes on every fixture transaction.

Each workflow read verifies trace files. Decoded JSONL records are reused only
when the file bytes match, and callers receive independent copies of mutable
fields. Each project retains at most 16 MiB of encoded JSONL in this cache,
along with its decoded records; larger files are decoded without retention. Changes, including same-size edits with unchanged timestamps, are
validated again. Frequent polling competes with the controllers doing the work. Parallel tests also share
the container's CPU and filesystem, so adding parallelism can increase contention.

- Keep one end-to-end journey for each distinct integration contract. Exercise
  decision, delivery-style and failure variants at the controller boundary.
  Before adding a journey, identify an assertion that the existing journeys
  and focused tests cannot establish.
- Mark isolated top-level tests parallel, including parents of parallel table
  cases. A serial parent prevents the other parallel tests from running until
  its children finish. Tests that change process environment variables must
  stay serial.
- For controller tests, seed the state immediately before the behavior under
  test. `seedBuild` records a sealed plan and unit states for a stopped service;
  `seedBuilding` supplies the same state to a running service, and
  `seedBuildingPaused` sets an owner pause before creating its builds.
  Documents and unit states are published together. Use `builtAs` and
  `builtPaused` when intake and ratification are part of the assertion.
  Landing fixtures capture
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

### Service coverage ownership

| Contract | Journey coverage | Focused coverage and fixture |
| --- | --- | --- |
| Intake and ratification | `TestHandInToRatifiedPlan` | Architect, shed, ratification and build tests exercise their own transitions. Implementation tests start with sealed builds. |
| Review and revision | `TestExactReviewDemonstration` checks actionable findings, revised candidates, reviewer tool scope, questions and a restart | Review bundle, footprint, stale identity and interrupted-result tests start with a completed candidate. |
| Landing and rebase | Landing, parallel-work and drift demonstrations check controller wiring | Landing interruption cases start with an approved candidate and drive the production reconciler with a controlled clock. Git and Jujutsu rebase fixtures prepare unit reports and local workspace changes. |
| Final review and delivery | `TestDeliveryDemonstration` follows a final-review gap through a follow-up unit to approved publication | Final-review tests cover report decisions. Delivery and publication tests start with an assembled, reviewed feature and cover styles, approval invalidation and side-effect recovery. |
| Amendments | `TestAmendmentDemonstration` follows a mason request through approval and continuation across restarts | Draft, debate, decision and application tests cover the respective stage, including rejection and stale owner decisions. |
| Runtime controls | The reliability demonstration combines pause, budget, profile changes, provider limits and recovery | Capacity, classification, turn failure and interrupted-mason tests start with sealed builds. |
| Workspace backend compatibility | The Jujutsu delivery demonstration compares the delivered branch from both backends | Workspace and Jujutsu recovery tests cover backend operations and interruption points. |
| Interfaces | Browser, notification and trace demonstrations check their actual transport and presentation paths | API, inbox and trace tests use records appropriate to the requested view. |

Retain a matrix at the journey level when the comparison itself is the
contract: Git/Jujutsu delivered-branch equivalence is an example. A fixture
should establish its starting state and fail clearly if setup fails; unrelated
workflow decisions belong in the tests responsible for those decisions.

## Profile inside Dagger

Measure the same tests with the same container, CPU affinity, race setting and
parallelism. For an isolated comparison, use `taskset -c` with four CPUs from
`/proc/self/status` inside the container and `GOMAXPROCS=4`, `-p=4` and
`-parallel=4`. Affinity applies to child Git and Jujutsu processes too.
Compare complete-package times as well as small fixtures; large histories make
repeated trace work more expensive.
Use `-count=1` to bypass Go's test-result cache and `-parallel=1` to isolate the
cost of a lifecycle from contention with other tests. Compilation, image pulls
and engine startup are separate from the durations printed by `go test`.

Profile in the Go check's test container, which the `osmia-dev` module's
`test-runtime` function returns. It has the pinned `jj` and Chromium installed
and `/tmp` in memory. For example, this profiles a Git-backed service test and
prints CPU and blocking summaries. The binary and profiles stay in the
container:

```sh
dagger api call test-runtime \
  with-directory --path /src --source . --exclude .git,.bees,.dagger \
  with-workdir --path /src \
  with-exec --args=sh,-c,'go test -count=1 -parallel=1 -timeout=10m -run=TestMasonSessionsWithinCapacityRunAtOnce -v -o /tmp/service.test -cpuprofile=/tmp/service.cpu -blockprofile=/tmp/service.block ./internal/service && go tool pprof -top -cum /tmp/service.cpu && go tool pprof -top /tmp/service.block' \
  combined-output
```

Cumulative CPU percentages overlap along call stacks; blocking profiles
aggregate time across goroutines. Neither should be added up as wall-clock time.

The trace decoding benchmark compares repeated decoding with reuse of verified
file content, including copying mutable record fields. Run it inside Dagger:

```sh
dagger api call test-runtime \
  with-directory --path /src --source . --exclude .git,.bees,.dagger \
  with-workdir --path /src \
  with-exec --args=go,test,'-run=^$',-bench=BenchmarkRecordDecoding,-benchmem,./internal/trace \
  combined-output
```

A passing local run on four CPUs is a reproducible performance check, but is
not a measurement of the CI engine's processor speed or competing workload.
Use the CI package durations to verify performance after publication.
