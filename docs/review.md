# Architecture and test review

Review date: 2026-09-27. Scope: the design, repository structure, execution and
storage boundaries, Dagger checks, test coverage by behavior and service test
profiling.

## Findings

| Priority | Finding | Evidence and action |
| --- | --- | --- |
| Medium | The Go test runner is installed from a moving branch. | [go.dang](../.dagger/modules/osmia-go/go.dang) installs `github.com/dagger/otel-go/cmd/otelgotest@main`. Pin a release or revision so the same source and runner behavior are used across check runs. |
| Medium | The execution boundary is checked with fake enforcers, leaving platform confinement outside these tests. | [Boundary tests](../internal/coreadapter/boundary_test.go) and [turn tests](../internal/isolation/turns_test.go) exercise grants and refusals without a live OS sandbox. Keep the fail-closed checks and validate supported host and container modes as part of release qualification. |
| Medium | The standard Go check does not use the race detector. | The Go check in [go.dang](../.dagger/modules/osmia-go/go.dang) runs `otelgotest -count=1`; concurrency tests exist in scheduler, thread, reconciliation and service. Run selected packages with `-race` in Dagger when changing capacity, shared state or recovery. |
| Medium | Repeated trace verification dominates the sampled service lifecycles. | CPU profiles of landing, parallel implementation and delivery demonstrations place about 54% of samples under directory walks and 44% under workflow loading (overlapping call stacks). Controller tests should use focused fixtures; any trace read optimization must preserve detection of invalid records, filesystem tampering and interrupted publication. See [profiling instructions](testing.md#profile-inside-dagger). |
| Low | Workflow orchestration is concentrated in one package. | [service](../internal/service) has about 27,000 lines of production Go and 40,000 lines of tests. Controllers have separate files, but cross-controller changes can be hard to survey. Keep policy in small Go components and avoid adding scheduling paths inside agent turns. |

## Validation

`dagger check` passed all four checks in a clean temporary copy of the tracked
repository files: Go tests, generated-file freshness, browser tests and release
version round-trip. The Go check reported 898 passed tests and eight browser
tests skipped there; those browser tests ran in the passing browser check.

An initial run in the working tree passed the browser and release checks but
could not snapshot the two Go checks because `.bees` changed during Dagger's
workspace sync. The clean copy excluded `.bees` and completed the full check.
Host `go vet ./...` and `go build ./cmd/osmia` also passed; no host tests ran.

Focused Dagger runs with `-race` passed for final-review fixture setup, concurrent
unit dispatch, status timeouts and a refresh held inside a fake librarian turn.

## Design alignment

The inspected paths follow the main design boundaries:

- [Scheduler](../internal/scheduler/scheduler.go) selects turns from durable
  state and publishes dispatch intent in Go.
- [Trace transactions](../internal/trace/transaction.go) publish state and
  operation intent as a recoverable transaction; the
  [reconciler](../internal/reconcile/controller.go) inspects and retries effects.
- [Turn isolation](../internal/isolation/turns.go) applies service role grants;
  the [core adapter](../internal/coreadapter/boundary.go) refuses widened
  requests and prepared policies that grant VCS access.
- [Landing](../internal/service/landing.go) ties the landing to the reviewed
  candidate and governing revisions. The service owns
  [publication](../internal/service/publication.go).
- [go.mod](../go.mod) pins `github.com/kpenfound/busybees/core` at `v0.5.0`.

These observations are static. They do not establish that every path is free of
defects. The suite has 907 `Test` functions, including 427 under service, 92
under trace, 49 under coreadapter and 20 under isolation. Tests exercise owner
gates, restarts, interruption points, local VCS operations and browser decisions
with fakes. Jujutsu and browser tests skip when their binaries are absent
outside their dedicated Dagger checks; the configured checks install those
binaries.
