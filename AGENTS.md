# Working on Osmia

## Design and scope

- Read [docs/design.md](docs/design.md) before proposing, planning or implementing a feature. It is the source of truth; issues describe outcomes and do not override it.
- Follow the milestone order in the design and the active `filter.milestone` in `bees.toml`. Leave work-item decomposition to the development factory.
- Keep behavior changes and the design consistent. Surface a conflict with the design instead of silently changing product scope.
- Repository bootstrap, the Go module, initial Dagger checks and factory configuration are already initialized. Do not create work items to repeat them.
- Hearsay is optional and scheduled last. File-based context must support every earlier milestone.

## Architecture guardrails

- Keep state transitions, scheduling, capacity and owner gates in Go. Models run inside turns, never on the scheduling path.
- Osmia's service owns version control and delivery credentials. Its runtime agents receive scoped files and tools; they do not commit, rebase or push. This is a product requirement, not a restriction on the development factory's normal branch and pull-request workflow.
- Keep target-project factory state under the Osmia root, outside the target repository. Osmia's own design and development configuration belong here.
- Preserve the trace, durable threads and explicit owner decisions through restarts and retries. Reconcile side effects before retrying them.
- Reuse `github.com/kpenfound/busybees/core` through a pinned dependency. Do not require a sibling checkout or copy its internals into this repository.

## Missing busybees/core functionality

- When an Osmia feature needs functionality missing from busybees/core, open an upstream feature request in `kpenfound/busybees`.
- If the needed logic can be implemented in Osmia while awaiting upstream support, a temporary local implementation is allowed. Leave a `TODO` comment beside that code describing the capability needed and stating that the extra code must be removed when busybees/core provides it.
- If the Osmia feature is blocked until upstream implements the capability, comment on the blocked Osmia issue explaining the dependency and linking the upstream feature request, and ensure it receives the needs-human label (`bees:needs-human` in this factory) so the upstream request can be prioritized. Use the factory's escalation mechanism when it owns label changes.

## Validation

- Run `dagger check` before declaring a change complete. Use the pinned experimental release:

  ```sh
  DAGGER_X_RELEASE=v1.0.0-beta.13 dagger check
  ```

- The factory exports `DAGGER_X_RELEASE` for its sessions. Direct `go test` or `go build` runs are useful during iteration but do not replace the Dagger checks.
- Add meaningful tests for changed behavior and regressions, especially state transitions, recovery, owner gates and execution boundaries. Use temporary directories and local repositories for filesystem and VCS tests.
- Tests must use fake agents, GitHub clients, providers and container engines. Never launch real model sessions, the live factory, remote pushes or pull requests from tests.
- Report the checks actually run and their results. If validation is blocked, state the exact blocker; do not report success.
- `dagger check` runs automatically on pull requests using Dagger Native CI

## Housekeeping

- Keep changes focused on the issue. Avoid unrelated refactors, generated churn and dependencies without a concrete need.
- Format Go with `gofmt`; keep module and Dagger lockfiles consistent with dependency changes.
- Keep `.bees/`, local runtime state, credentials, logs and build artifacts out of version control. Reference secrets through environment variables.
- Update documentation with user-visible behavior and configuration changes. Keep the README concise while the product is under construction.
- Review the diff for accidental files and secrets. Describe the resulting behavior and validation clearly in pull requests.

## Comments and documentation

- Comments and documentation always represent the current state. Its never helpful to reference a change in behavior or how things used to work
- Comments and documentation should never reference github issues, pull requests, or commits
- Comment blocks can describe a function API or specific behavior of nearby code, but never a whole feature. That belongs in actual documentation
