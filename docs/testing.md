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
