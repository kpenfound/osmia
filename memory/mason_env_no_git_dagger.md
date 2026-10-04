---
name: mason-env-no-git-dagger
description: This mason workspace blocks the git binary and dagger check finds no checks, so tests can't be executed here
metadata:
  type: project
---

In this osmia mason workspace, the `git` binary is replaced by a stub that always exits 126 ("git: not granted to this session"), even outside the Bash tool's sandbox. Since almost every osmia test touches a git-backed `trace.Repository`, `go test` fails on essentially every package with "trace git read-tree: exit status 126". `dagger check` also returns "no checks selected" / `dagger -m osmia-dev functions` fails to resolve the local module path — the Dagger module system does not appear to be usable from inside this workspace either.

**Why:** The mason's `done` tool description says it "runs the project's checks on it and sends it to review" — verification happens in a separate pipeline after reporting done, not inside this interactive workspace.

**How to apply:** Don't spend time fighting `go test`/`dagger check` in a mason workspace for this project — `go build ./...`, `go vet ./...` (type-checks test files without running them, confirmed useful), and `gofmt -l` are the only reliable local verification tools. Rely on careful manual tracing of the exact code paths (read the called packages' source, not just assume behavior) to gain confidence before calling done. See [[osmia-beekeeper-turns-architecture]] for an example of this kind of manual verification on a non-trivial reconcile/scheduler wiring change.
