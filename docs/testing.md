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
| Workflow and owner gates | [intake](../internal/intake), [intakejourney](../internal/intakejourney), [execution](../internal/execution), [executionjourney](../internal/executionjourney), [delivery](../internal/delivery), [lifecycle](../internal/lifecycle) | Ratification, questions, amendments, checks before review, exact-candidate review, landing, final review and approved delivery |
| Reviewer questions through assembly | [executionjourney](../internal/executionjourney/api_question_wait_end_to_end_test.go), `TestReviewerQuestionOwnerRulingReachesFinalReview` | Production ask, chief escalation, owner ruling and answer delivery lead to exact-candidate approval, all unit landings and final review, including delivered-answer and completed-verdict restarts. Unanswered questions keep the unit waiting. |
| Recovery | [trace](../internal/trace), [reconcile](../internal/reconcile), [intakejourney](../internal/intakejourney), [executionjourney](../internal/executionjourney) and [lifecycle](../internal/lifecycle) restart demonstrations | Atomic records, durable operations and reconciliation after interruptions |
| Capacity and scheduling | [scheduler](../internal/scheduler), [executionjourney](../internal/executionjourney) parallel work tests | Role and shared check-slot accounting, smart final-check selection across projects, priority, pauses and disjoint unit dispatch |
| Agent boundary | [isolation](../internal/isolation), [coreadapter](../internal/coreadapter) | Scoped file views, grants, environment allowlist and execution policy checks |
| Workspaces | [workspace](../internal/workspace), [executionjourney](../internal/executionjourney) Jujutsu demonstrations | Git and Jujutsu behavior, rebase and recovery using local repositories |
| Operator interface | [cli](../internal/cli), [browsertest](../internal/browsertest), [intakejourney](../internal/intakejourney), [executionjourney](../internal/executionjourney), [lifecycle](../internal/lifecycle) | Commands, API behavior, owner decisions and live page updates |
| Jev judgments | [jev](../internal/jev), [thread](../internal/thread), [execution](../internal/execution), [executionjourney](../internal/executionjourney), [checkselect](../internal/checkselect), [smartcheck](../cmd/smartcheck) | Fallback on every failure, cool-down, recorded decisions reused after restart, interrupted attempts, mason classification thresholds and evidence, check selection in the service and against local repositories, and the chief of staff's question signals with their fallbacks and reuse |

The [service](../internal/service) tests cover composition, startup, shutdown,
project runtime bindings and HTTP dispatch. Workflow journeys use the same
composition root, with fake execution boundaries and local repositories.

Test counts are useful for finding the suite, but assertions and exercised
boundaries determine confidence.

The [execution scheduling journeys](../internal/executionjourney/scheduling_path_test.go) start the service with real
controllers over seeded approved units and local repositories. Gates hold a
landing and a scheduling step while all three conversation APIs complete,
hold a landing and a draft while another workstream merges, and establish
simultaneous landings and simultaneous masons within one workstream. Restart
cases pin seal, build, landing, rebase, drift and publication effects, including
completed work on a shared jj clone surviving another operation's interruption.
The jj fixture kills a child service while its clone checkpoint is open, then
restarts through `Start` and verifies the completed operation was not replayed.
[conversation journeys](../internal/executionjourney/conversation_path_test.go) cover captured replies and completion-hook effects;
[model operation journeys](../internal/executionjourney/model_operations_test.go) and
[intake architect tests](../internal/intakejourney/architect_test.go) cover model workflow recovery;
[network polling journeys](../internal/executionjourney/network_polls_test.go) cover interrupted polling and exactly one durable
observation. These gates establish the interleavings without sleep-based timing.

Code that asks Jev judgments tests with
agentcore's `jev/jevtest.Provider`, which returns scripted answers
or `systemone` errors and can block until a request is cancelled. Cover the
consumer with the boost disabled as well as with accepted, declined and failed
judgments.

Service tests run checks with a fake `ReviewChecks`: fixtures built on
`projectFixture` pass every check, and the unit check tests script listings,
passing, failing and incomplete runs. No test reaches a Dagger engine.

Running-service tests outside `internal/service` use
[`servicetest`](../internal/servicetest) to start the real service and web
listener, drive the API, and seed sealed builds. Its `Project` helpers provide
`SeedBuild`, `SeedBuilding`, `SeedBuildingPaused`, `BuiltAs` and `BuiltPaused`.
Execution hooks and private fixture operations go through `service.TestHooks`,
`StartForTest` and `InspectForTest`, separate from production options.

[`fixturekit`](../internal/servicetest/fixturekit) holds the service-independent
fake agents, checks and GitHub clients, local repository and configuration
builders, sealed-build inputs, clocks and test defaults. It never imports
`internal/service`. Both `servicetest` and the white-box fixtures in controller
and composition packages build on this lower layer.
[intakefixture](../internal/servicetest/intakefixture) shares scripted workflow
scenarios without importing the service. Each test package applies
`fixturekit.ApplyServiceTestDefaults` in `TestMain` to enable shuffling unless
the caller explicitly selects a shuffle setting.

[`browsertest`](../internal/browsertest) runs browser tests through
`servicetest` and owns the shared Chromium harness. Browser scenarios in
`intakejourney`, `executionjourney` and `lifecycle` use their subject's
running-service fixtures and share the same harness;
`TestMain` closes Chromium after each package's tests finish.

## Keep service tests focused

Service lifecycle tests start real reconcilers and write durable traces to
their SQLite databases, with target repositories in local Git repositories. The
Go check mounts `/tmp` in memory for its test commands. Temporary traces and
repositories survive service restarts within that command and are discarded
when the command ends. This exercises real databases, Git, Jujutsu and
filesystem operations, including the service's durability calls, without paying
for container-layer disk writes on every fixture transaction.

An open trace handle holds the trace's lock, so only the handle writes its
database. Each write is one transaction that checks only the records it
revises. Reads take no write lock: the handle decodes each committed record
once, keeps it, and reads only the records committed since; callers receive
independent copies of mutable fields. Tests outside the trace package write trace state through the trace
API or the service's edit endpoints, never by changing the database or files
under an open handle, and assert on commits through the trace's commit count.
Frequent polling
competes with the controllers doing the work. Parallel tests also share the
container's CPU and filesystem, so adding parallelism can increase contention.

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
| Intake and ratification | [intakejourney](../internal/intakejourney), `TestHandInToRatifiedPlan` | [intake](../internal/intake) and [intakejourney](../internal/intakejourney) architect, shed, ratification and build tests exercise their own transitions. Implementation tests start with sealed builds. |
| Review and revision | [executionjourney](../internal/executionjourney), `TestExactReviewDemonstration` checks actionable findings, revised candidates, reviewer tool scope, questions and a restart | [execution](../internal/execution), [review](../internal/review) and [checks](../internal/checks) tests cover bundles, footprints, stale identity and interrupted results, starting with a completed candidate. |
| Landing and rebase | [executionjourney](../internal/executionjourney) landing and parallel-work demonstrations and [lifecycle](../internal/lifecycle) drift demonstrations check controller wiring | [execution](../internal/execution) landing interruption cases start with an approved candidate and drive the production reconciler with a controlled clock. Git and Jujutsu rebase fixtures prepare unit reports and local workspace changes. |
| Final review and delivery | [executionjourney](../internal/executionjourney), `TestDeliveryDemonstration` follows a final-review gap through a follow-up unit to approved publication | [delivery](../internal/delivery) tests cover report decisions. [executionjourney](../internal/executionjourney) delivery and publication tests start with an assembled, reviewed feature and cover styles, approval invalidation and side-effect recovery. |
| Amendments | [intakejourney](../internal/intakejourney), `TestAmendmentDemonstration` follows a mason request through approval and continuation across restarts | [intakejourney](../internal/intakejourney) draft, debate, decision and application tests cover the respective stage, including an architect question that parks the draft until its answer, rejection and stale owner decisions. |
| Runtime controls | The [executionjourney](../internal/executionjourney) reliability demonstration combines pause, budget, profile changes, provider limits and recovery | Capacity, classification, turn failure and interrupted-mason tests start with sealed builds. |
| Workspace backend compatibility | The [executionjourney](../internal/executionjourney) Jujutsu delivery demonstration compares the delivered branch from both backends | Workspace and Jujutsu recovery tests cover backend operations and interruption points. |
| Interfaces | [browsertest](../internal/browsertest) browser scenarios and [executionjourney](../internal/executionjourney) browser, notification, session and trace demonstrations check their actual transport and presentation paths | API, inbox and trace tests use records appropriate to the requested view. |

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
and `/tmp` in memory. For example, this profiles a Git-backed execution journey
and prints CPU and blocking summaries. The binary and profiles stay in the
container:

```sh
dagger api call test-runtime \
  with-directory --path /src --source . --exclude .git,.bees,.dagger \
  with-workdir --path /src \
  with-exec --args=sh,-c,'go test -count=1 -parallel=1 -timeout=10m -run=TestMasonSessionsWithinCapacityRunAtOnce -v -o /tmp/execution.test -cpuprofile=/tmp/execution.cpu -blockprofile=/tmp/execution.block ./internal/executionjourney && go tool pprof -top -cum /tmp/execution.cpu && go tool pprof -top /tmp/execution.block' \
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
