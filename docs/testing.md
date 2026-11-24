# Testing

Start with [CONTRIBUTING.md](../CONTRIBUTING.md) for the contributor workflow.

The test suite uses fake agents, GitHub clients, providers and container engines.
Filesystem and version control tests use temporary directories and local
repositories. Tests never need a live model, remote push or real delivery.

## Run the checks

Run focused checks for what you changed. The check service runs the full suite
on every change, so you do not need the whole suite passing before you finish.
Select tests with the Go check's flags, from the repository root, with the
Dagger engine available:

```sh
dagger check --progress=report --go-test=TestA
dagger check --progress=report --go-test=TestA --go-test=TestB
dagger check --progress=report --go-package=internal/intakejourney
dagger check --progress=report --go-package=internal/executionjourney --go-test=TestExactReviewDemonstration
```

`--progress=report` prints each check's result and the output of failing tests.
Use `--progress=plain` only when you need every container step, such as the
exact `go test` command a check ran.

The Go check runs the suite with the official Dagger Go module, in the test
container from the `osmia-dev` module. That container provides the Go toolchain,
`jj` and Chromium, and sets `OSMIA_BROWSER`, so the browser tests in
`internal/browsertest`, `internal/intakejourney`, `internal/executionjourney`,
`internal/lifecycle` and `internal/web` run with the rest of the suite; without
`OSMIA_BROWSER` they skip. Each package's tests run in their own container, in
parallel with the other packages. The release check builds and exercises the
versioned archive. See [dagger.toml](../dagger.toml) for the registered
checks.

Each Go package runs at most four tests concurrently, including race and flake
runs. This bounds the number of service fixtures competing for CPU and filesystem
operations within a check. Use `--parallel 4` to bound concurrent checks as well.

Never run `go test` on the host, in any role and for any purpose: iterating, a
single test, a mutation check or reproducing a flake. Tests start processes that
leak onto the machine they run on, so they run only inside Dagger. Host `gofmt`,
`go build` and `go vet ./...` are allowed; `go vet ./...` type-checks test files
without running them.

## Validate as a mason

Run focused checks for what you changed; the check service runs the full suite.
Select the tests that exercise the change, or the package that holds it, as in
[Run the checks](#run-the-checks). Add `--env=race` to run the selected tests
with the race detector when changing concurrent scheduling, recovery or shared
state, or `--env=flake` to run each selected test five times when reproducing a
flake.

A mason validates only through Dagger, from its view root. The view holds an
empty Git repository at its root, which the Dagger CLI needs to find
`dagger.toml`; the repository has no commits, remote or hooks and is never
captured, and the turn still has no version control. Plain `dagger check`, its
listing (`dagger check -l`), the Go check's `--go-package`, `--go-test` and
`--env` flags and `dagger generate` all work from the view root, with no
workspace flag, no copy of the tree and no `.git` of the mason's own. The lint
check runs staticcheck over the project:

```sh
dagger check --progress=report 'dag+check://?check=lint&staticcheck-project=.'
```

There is no other route: masons do not validate with Docker or Compose, a copy
of the tree made into a Git root, or a toolchain installed in the guest. The
Osmia sandbox template has no Docker Engine, and the guest reaches the engine
only through the host's forward
([Dagger for masons](configuration.md#dagger-for-masons)). If a run cannot load
the workspace, report that blocker instead of switching routes. Report the
checks you ran and their results.

### Select checks

- Only `--go-package` and `--go-test` select Go tests, and they combine in one
  call. A check name such as `go:stale` selects nothing and fails with `no
  checks matched pattern`.
- `--go-test` matches exact test names, not prefixes. A prefix such as
  `--go-test=TestDaggerClient` ends with `no checks selected` after a few
  seconds. Alone, it selects every test with that name in any package; add
  `--go-package` to pick one.
- `dagger list go-tests -a --go-package=internal/intakejourney` lists the tests
  of a package, and `-f=cli` prints each one as the flags that select it.
- `dagger check -l` lists the checks. `dagger list checks --all -f=link` lists
  the link of every check, including checks outside the Go module such as
  `release:version-round-trip`.
- To rerun a failing check exactly, pass the link `--progress=report` names it
  by back to `dagger check`, quoted:

  ```sh
  dagger check --progress=report 'dag://go/packages/tests/test?go-package=internal/config&go-test=TestA'
  ```

- A `dag+check://` link, such as the lint link, runs in a call of its own.
  Different links cannot share one call, and the Go check's flags do not
  combine with a link.

### Cached results

Dagger replays the cached result, a cached failure included, for an unchanged
source tree and an identical selection. A run that finishes in a second or two
is a replay and proves nothing about the current tree. To force a real run,
change the selection (for example the whole package instead of one test) or add
a throwaway `_test.go` file with a unique comment to the package under test, and
delete it afterwards.

### What the test container holds

The Go check's test container carries only Go sources, `testdata/**` and the
files that tests `go:embed`. It does not carry `scripts/*.sh` or `docs/*`. A
test that needs a repository script or page embeds it, as
`scripts/build_sandbox_test.go` does with `build-sandbox.sh` and
`docs/telemetry_test.go` does with `telemetry.md`, or keeps its fixtures under
`testdata`.

### Lint pitfalls

The lint check runs staticcheck with the checks in
[staticcheck.conf](../staticcheck.conf). It compiles every test package, so
changing a type or helper signature can break a test far from the change. It
rejects:

- literal U+200B or U+2028 characters in strings (ST1018);
- a struct literal that copies every field of another struct (S1016);
- unused functions and methods (U1000), such as a helper left without callers
  after its last caller is deleted;
- deprecated APIs (SA1019);
- identical expressions on both sides of a binary operator (SA4000).

### Size runs

The full suite takes well over ten minutes, which is why the check service runs
it. Selected tests take seconds, and a whole package takes minutes. A
whole-package `--env=race` run is much longer than a plain run, so select the
tests that exercise the concurrent code by name. Do not wait with a
`pgrep -f 'dagger check'` loop: it matches itself and never ends.

After a change to the schema, a route or an API type, run `dagger generate` so
`docs/openapi.json` is current; the generate check fails while it is stale.

### Known failures and flakes

- `dagger-cli-contract` runs the real Dagger CLI in a nested Dagger session. A
  mason sandbox whose engine does not provide nested sessions fails it while
  connecting. Report that as an environment blocker; it says nothing about the
  change unless the change touches `internal/checks`.
- Under load these tests can time out or fail and then pass alone:
  `TestProjectAddRecoversAtEachStep`, `TestLandingDemonstration`,
  `TestChangePassAppliesPendingChangeAfterArchivedCrossRepoLeader` and
  `TestAmendOnProductionContinuedDesignThread`. Rerun them selected by name
  before treating the failure as real.
- A flaky browser test can point to a page bug rather than load.

## What the suite covers

| Boundary | Representative tests | What they establish |
| --- | --- | --- |
| Workflow and owner gates | [intake](../internal/intake), [intakejourney](../internal/intakejourney), [execution](../internal/execution), [executionjourney](../internal/executionjourney), [delivery](../internal/delivery), [lifecycle](../internal/lifecycle) | Ratification, questions, amendments, checks before review, exact-candidate review, landing, final review and approved delivery |
| Reviewer questions through assembly | [executionjourney](../internal/executionjourney/api_question_wait_end_to_end_test.go), `TestReviewerQuestionOwnerRulingReachesFinalReview` | Production ask, chief escalation, owner ruling and answer delivery lead to exact-candidate approval, all unit landings and final review, including delivered-answer and completed-verdict restarts. Unanswered questions keep the unit waiting. |
| Recovery | [trace](../internal/trace), [reconcile](../internal/reconcile), [intakejourney](../internal/intakejourney), [executionjourney](../internal/executionjourney) and [lifecycle](../internal/lifecycle) restart demonstrations | Atomic records, durable operations and reconciliation after interruptions |
| Capacity and scheduling | [scheduler](../internal/scheduler), [executionjourney](../internal/executionjourney) parallel work tests | Role and shared check-slot accounting, smart unit-check selection across projects, priority, pauses and disjoint unit dispatch |
| Engine maintenance admission | [service gate tests](../internal/service/engine_gate_test.go), [execution journeys](../internal/executionjourney/engine_gate_test.go) | Busy refusals name admitted checks and Dagger turns; restart and prune exclude new starts without writing check or turn evidence for the wait |
| Check client authentication | [runner tests](../internal/checks/client_test.go), [candidate export journeys](../internal/executionjourney/check_export_test.go) | Exact allow-listed check and listing environments, resolved engine selection, token redaction and persisted Cloud trace links; all CLI calls are fakes |
| Agent boundary | [isolation](../internal/isolation), [coreadapter](../internal/coreadapter) | Scoped file views, grants, environment allowlist and execution policy checks |
| Workspaces | [workspace](../internal/workspace), [executionjourney](../internal/executionjourney) Jujutsu demonstrations | Git and Jujutsu behavior, rebase and recovery using local repositories |
| Operator interface | [cli](../internal/cli), [browsertest](../internal/browsertest), [intakejourney](../internal/intakejourney), [executionjourney](../internal/executionjourney), [lifecycle](../internal/lifecycle) | Commands, API behavior, owner decisions and live page updates |
| Jev judgments | [jev](../internal/jev), [thread](../internal/thread), [execution](../internal/execution), [executionjourney](../internal/executionjourney), [checkselect](../internal/checkselect), [smartcheck](../cmd/smartcheck) | Fallback on every failure, cool-down, recorded decisions reused after restart, interrupted attempts, mason classification thresholds and evidence, check selection in the service and against local repositories, and the chief of staff's `decide` results with their fallbacks, reuse and binding |

The [service](../internal/service) tests cover composition, startup, shutdown,
project runtime bindings and HTTP dispatch. Workflow journeys use the same
composition root, with fake execution boundaries and local repositories.

The [preview journey](../internal/servicetest/preview_test.go),
`TestChiefPreviewImageIsServedAndSharedWithTheWorkgroup` and
`TestArchivingAMemberRemovesItsPreviewImageFromEveryRecord`, sends an owner
message to a production chief turn. The chief
inspects the member's committed Service code and names its call and port in the
preview tool request. Offline Dagger and Chromium commands capture the built
member's exact source; the journeys check both feeds, image bytes and headers,
archive placeholders and deletion of the member trace without bytes in retained
presentations.
`TestBrowserChiefPreviewThroughMemberArchive` in
[browsertest](../internal/browsertest/browser_preview_journey_test.go) uses the
same running-service fixture to check a live thumbnail, its full-size link and
the archived placeholder, including a fresh page that requests no deleted image.

Preview argument tests cover the required non-empty `call` and `port` from 1
to 65535, the `/` path default and the `5m` timeout default with a `1m` to `30m`
range. They refuse CLI options, provider schemes, client environment names,
escaping paths and symlinks, and host-writing or interactive steps before a
Service command starts. Assignment values, quote removal, comma-separated
values and whitespace use Dagger's reading for checks while accepted argv is
preserved. The boundary fails closed, including scheme-like values containing
`:`. Capture tests verify only an engine Service ID reaches the image command;
recovery tests retain owner-pinned arguments and reuse recorded images.

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
consumer with an unconfigured Jev as well as with accepted, declined and failed
judgments.

Service tests run checks with a fake `ReviewChecks`: fixtures built on
`projectFixture` pass every check, and the unit check tests script listings,
passing, failing and incomplete runs. No test reaches a Dagger engine.

Running-service tests outside `internal/service` use
[`servicetest`](../internal/servicetest) to start the real service and web
listener, drive the API, and seed sealed builds. Its
[`journey`](../internal/servicetest/journey) package is the one test harness of
`executionjourney`, `intakejourney` and `lifecycle`: `Options`, `Start`,
`Enforce`, the ready-access and active-project helpers, and the intake,
execution and lifecycle controller builders. `internal/service` tests cannot
import it, because `journey` imports `internal/service`; they bind the shared
fixture bodies in `fixturekit` and `intakefixture` to the service's unexported
state and keep no copy of the logic. Its `Project` helpers provide
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

## Routing journeys

Five journeys run the transition router through the running service rather
than its parts against fixtures. They use fake masons, checks and pull requests
and the fixture Jev (`internal/jev/jevfixture`), which answers every
`decide-transition` request that the default fits unless a test scripts it to
depart, hold or fail. Select them with `--go-package` and `--go-test`:

| Journey | Test | What it follows |
| --- | --- | --- |
| A unit goes quiet and Jev departs | `TestJourneyAStalledUnitIsDepartedFromByJev` (`internal/executionjourney`) | The scheduler's stall watchdog raises `stalled`, the router's worker asks Jev, the departure is committed with `decided_by`, `default_to` and the judgment, the `departed` route span, "decided by Jev" in the feed, the trace API and `osmia trace`, and one Hearsay decision with no author. |
| A dependent whose upstream ended | `TestJourneyADependentOnAnAbandonedUpstreamLeavesWaitingAndGoesOn` and `TestJourneyADependentOnAnUpstreamMergedOutsideTheFactoryLeavesWaitingAndGoesOn` (`internal/lifecycle`) | The exit from waiting, the waiver (Jev's drop) or the adopted merge, the dependent's first unit starting and its mason's turn queued where the mason pass skipped it while it waited, unblocked through repeated reconciliation and a restart, and the Hearsay waiver decision. |
| A chief move is pinned | `TestChiefOfStaffMovesAUnitOutOfAContestItRaisedOrRuled` (`internal/executionjourney`) | The chief of staff's move out of a contest it raised or ruled on, entered with no request to Jev and `chief_of_staff` as its `decided_by`, and the route record's chief authority. |
| A hold becomes a stall, then a recovery card | `TestJourneyAnUnresolvedHoldBecomesAStallAndARecoveryCardTheOwnerApproves` and `TestJourneyAStallJevKeepsHoldingEndsOnARecoveryCardTheOwnerApproves` (`internal/executionjourney`) | A held stall, the stall that follows, the one card, its approval with the answer the card carries as a pinned owner decision, and the card closing. |
| A Jev outage | `TestJourneyAScriptedJevOutageCommitsDefaultsInDegradedMode` and `TestJourneyAJevThatNeverAnswersIsWaitedOnOnlyUntilItsTimeout` (`internal/executionjourney`) | A unit built, checked, reviewed and contested on defaults alone, `fallback` in the transitions, the route records and the spans, the status showing Jev degraded, and no request waiting longer than Jev's timeout. |

Each journey also checks that every request Jev got was the narrow questions of
a routed exit, so the step journals (project registration and creation, the
workgroup-change journal, publication operations, outbox delivery and turn
admission) asked Jev nothing. The service exposes two hooks for them in
`internal/service/testing_routing.go`: one lets the next scheduler pass read the
records for stalls without waiting on the wall clock, and one runs a Hearsay
publish pass against a fake connectors listener
(`internal/servicetest/fixturekit/hooks.go`).

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
| Intake and ratification | [intakejourney](../internal/intakejourney), `TestHandInToRatifiedPlan` hands a design in through the API and follows the architect draft, sketch, committee rounds, objections, redraft, ratification packet and the owner's ratify to a seal; `TestSealingInterruptedAfterRatifyingOverAnOverruledObjectionResumesAfterRestart` follows the capped debate, overrule, interrupted seal and restart | [intake](../internal/intake) and [intakejourney](../internal/intakejourney) architect, shed, ratification and build tests exercise their own transitions, including `TestSkippedDebateRatifiesWithoutACommittee`, the committee question and cap tests and the committee round tests. Implementation tests start with sealed builds. |
| Review and revision | [executionjourney](../internal/executionjourney), `TestExactReviewDemonstration` checks actionable findings, revised candidates, reviewer tool scope, questions and a restart | [execution](../internal/execution), [review](../internal/review) and [checks](../internal/checks) tests cover bundles, footprints, stale identity and interrupted results, starting with a completed candidate. |
| Landing and rebase | [executionjourney](../internal/executionjourney) landing and parallel-work demonstrations and [lifecycle](../internal/lifecycle) drift demonstrations check controller wiring | [execution](../internal/execution) landing interruption cases start with an approved candidate and drive the production reconciler with a controlled clock. Git and Jujutsu rebase fixtures prepare unit reports and local workspace changes. |
| Final review and delivery | [executionjourney](../internal/executionjourney), `TestDeliveryDemonstration` follows a final-review gap through a follow-up unit to approved publication; [lifecycle](../internal/lifecycle) `TestDraftFirstFlowFromTheLastMergeToDelivery` takes a workstream from its last merge to a ready pull request on a started service with a fake pull request host that shows Dagger Cloud statuses | [delivery](../internal/delivery) tests cover report decisions. `TestFinalReviewReadsTheRebasedBranchAsAWhole` and the final-review staleness and exactly-once tests in [executionjourney](../internal/executionjourney) run the controller passes on a started service. The lifecycle cloud-check tests (`TestFailedCloudChecksAreRerunOnceAndTheFreshRunSettlesTheReview` runs a fake `dagger`, `TestFinalChecksWaitForTheLoadStatusAndTheCloudRunBeforeSettling`) and the triage and moved-branch tests start from the same last merge. [executionjourney](../internal/executionjourney) delivery and publication tests start with an assembled, reviewed feature and cover styles, approval invalidation and side-effect recovery. |
| Amendments | [intakejourney](../internal/intakejourney), `TestAmendmentDemonstration` follows a mason request through approval and continuation across restarts | [intakejourney](../internal/intakejourney) draft, debate, decision and application tests cover the respective stage, including an architect question that parks the draft until its answer, rejection and stale owner decisions. |
| Runtime controls | The [executionjourney](../internal/executionjourney) `TestReliabilityDemonstration` takes one workstream, built through production intake, through the daily-budget pause, the owner's resume, a configuration reload, a provider usage-limit fallback, a crash at the landing boundary and a restart | The budget, hard pause, profile switch, slot wait, reload and usage-limit controls each have their own test in the same package (`TestDailyBudgetPausesDispatchUntilTheOwnerResumes`, `TestHardPauseStopsAMasonAndResumingContinuesItsSession` and the others beside it). Capacity, classification, turn failure and interrupted-mason tests start with sealed builds. |
| Questions and the inbox | The [executionjourney](../internal/executionjourney) `TestChiefOfStaffQuestionsAndInbox`: workers ask, the chief answers one question and escalates another, the owner rules, and the ruling reaches the asking reviewer once across two restarts | The chief's reply and status, parked threads, the escalation across a restart and the ruling notice each have a test beside it. |
| Workgroup archive and change | [lifecycle](../internal/lifecycle) `TestArchivedUpstreamMergeOrderRepairAndFreshApprovalAcrossRestart` and `TestChangePassAppliesPendingChangeAfterArchivedCrossRepoLeader` | The draft naming an unmerged upstream, the archive and cleanup hold, the project spec kept active, the archive and change races and the follow-up after merged work each have their own lifecycle test. |
| Workspace backend compatibility | The [executionjourney](../internal/executionjourney) Jujutsu delivery demonstration compares the delivered branch from both backends | Workspace and Jujutsu recovery tests cover backend operations and interruption points. |
| Interfaces | [browsertest](../internal/browsertest) browser scenarios and [executionjourney](../internal/executionjourney) browser, notification, session and trace demonstrations check their actual transport and presentation paths | API, inbox and trace tests use records appropriate to the requested view. |

Retain a matrix at the journey level when the comparison itself is the
contract: Git/Jujutsu delivered-branch equivalence is an example. A fixture
should establish its starting state and fail clearly if setup fails; unrelated
workflow decisions belong in the tests responsible for those decisions.

## Hearsay Compose smoke run

The one test that talks to a real Hearsay is not part of `dagger check`. It is a
script that builds Hearsay, runs it under Docker Compose and drives Osmia's
publisher and read path against it; see
[the Compose smoke run](hearsay-compose-smoke.md). Its Go test carries the
`hearsaysmoke` build tag, so `go vet -tags hearsaysmoke ./internal/service/`
type-checks it and no other run compiles it.

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
`test-runtime` function returns. It has `jj` and Chromium installed
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

A passing local run on four CPUs is a reproducible performance check, but is
not a measurement of the CI engine's processor speed or competing workload.
Use the CI package durations to verify performance after publication.
