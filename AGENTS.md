# Working on Osmia

## Design and scope

- Read [docs/design.md](docs/design.md) before proposing, planning or implementing a feature. It is the source of truth; issues describe outcomes and do not override it.
- Follow the feature dependencies in the design.
- Keep behavior changes and the design consistent. Surface a conflict with the design instead of silently changing product scope.
- Repository bootstrap, the Go module and initial Dagger checks are already initialized. Do not repeat them.
- Hearsay is optional. File-based context must support the complete workflow without it.

## Architecture guardrails

- Keep state transitions, scheduling, capacity and owner gates in Go. Models run inside turns, never on the scheduling path.
- Osmia's service owns version control and delivery credentials. Its runtime agents receive scoped files and tools; they do not commit, rebase or push. This is a product requirement, not a restriction on the normal branch and pull-request workflow used to develop Osmia.
- Keep target-project factory state under the Osmia root, outside the target repository. Osmia's own design and development configuration belong here.
- Preserve the trace, durable threads and explicit owner decisions through restarts and retries. Reconcile side effects before retrying them.
- Reuse `github.com/kpenfound/busybees/core` through a pinned dependency. Do not require a sibling checkout or copy its internals into this repository.

## Missing busybees/core functionality

- When an Osmia feature needs functionality missing from busybees/core, open an upstream feature request in `kpenfound/busybees`.
- If the needed logic can be implemented in Osmia while awaiting upstream support, a temporary local implementation is allowed. Leave a `TODO` comment beside that code describing the capability needed and stating that the extra code must be removed when busybees/core provides it.
- If the Osmia feature is blocked until upstream implements the capability, comment on the blocked Osmia issue explaining the dependency and linking the upstream feature request, so the upstream request can be prioritized.

## Validation

- Run `dagger check` before declaring a change complete:

  ```sh
  dagger check --progress=report
  ```

  `--progress=report` prints each check's result and the output of failing tests. Use `--progress=plain` only when you need every container step, such as the exact `go test` command a check ran.
- Never run `go test` on the host, in any role and for any purpose: iterating, a single test, a mutation check and reproducing a flake included. Tests start processes that leak onto the machine they run on, so they run only inside Dagger. `gofmt`, `go build` and `go vet` are fine on the host; `go vet ./...` type-checks test files without running them.
- While iterating, select tests with the Go check's flags. Each test runs in the same container as the full suite, with the pinned `jj` and Chromium:

  ```sh
  dagger check --progress=report --go-test=TestA
  dagger check --progress=report --go-test=TestA --go-test=TestB
  dagger check --progress=report --go-package=internal/service
  dagger check --progress=report --go-package=internal/service --go-test=TestA
  ```

  `--go-test` alone selects every test with that name, in any package; add `--go-package` to pick one. `dagger list go-tests -a --go-package=internal/service` lists a package's tests, and `-f=cli` prints each one as the flags that select it.
- `--progress=report` names each check by its link. To rerun a failing check exactly, pass that link back to `dagger check`, quoted:

  ```sh
  dagger check --progress=report 'dag://go/packages/tests/test?go-package=internal/config&go-test=TestA'
  ```

  `dagger list checks --all -f=link` lists the link of every check, including checks outside the Go module such as `release:version-round-trip`.
- Add `--env=race` to run the selected tests with the race detector, or `--env=flake` to run each selected test five times when reproducing a flake.
- Browser tests of the web page run in the Go check, whose container installs Chromium and names it in `OSMIA_BROWSER`; without it they skip.
- Add meaningful tests for changed behavior and regressions, especially state transitions, recovery, owner gates and execution boundaries. Use temporary directories and local repositories for filesystem and VCS tests.
- Tests must use fake agents, GitHub clients, providers and container engines. Never launch real model sessions, the live factory, remote pushes or pull requests from tests.
- Report the checks actually run and their results. If validation is blocked, state the exact blocker; do not report success.
- `dagger check` runs automatically on pull requests using Dagger Native CI

## Housekeeping

- Keep changes focused on the issue. Avoid unrelated refactors, generated churn and dependencies without a concrete need.
- Format Go with `gofmt`; keep module and Dagger lockfiles consistent with dependency changes.
- List every API endpoint in `service.Routes`. After changing a route or a request or response type, run `dagger generate` and commit the updated `docs/openapi.json`; `dagger check` fails while it is stale.
- Keep local runtime state, credentials, logs and build artifacts out of version control. Reference secrets through environment variables.
- Update documentation with user-visible behavior and configuration changes. Keep the README concise while the product is under construction.
- Review the diff for accidental files and secrets. Describe the resulting behavior and validation clearly in pull requests.

## Comments and documentation

- Comments and documentation always represent the current state. Its never helpful to reference a change in behavior or how things used to work
- Name code, tests, documentation and user-facing messages after product features and behavior, without development phase labels.
- Comments and documentation should never reference github issues, pull requests, or commits
- Comment blocks can describe a function API or specific behavior of nearby code, but never a whole feature. That belongs in actual documentation
