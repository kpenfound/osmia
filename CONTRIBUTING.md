# Contributing to Osmia

Read [the design](docs/design.md) before proposing or changing behavior. It is
the source of truth for product scope and feature dependencies. Keep changes focused,
and keep target-project factory state outside the repository Osmia works on.

## Check a change

Run focused checks for what you changed. The check service runs the full suite on
every change, so you do not need the whole suite passing before you finish. Select
tests by name or package with the Go check's flags, from the repository root:

```sh
dagger check --progress=report --go-test=TestA --go-test=TestB
dagger check --progress=report --go-package=internal/service
```

The checks need a working Dagger container engine and run in containers. The Go
check's container provides the Go toolchain, `jj` and Chromium, so the browser
tests run there too. Add `--env=race` to run the selected tests with the race
detector when changing concurrent scheduling, recovery or shared state.
[docs/testing.md](docs/testing.md#run-the-checks) covers listing tests,
rerunning a failing check, cached results, lint and known flakes.

Validate only through Dagger, from the view root or the repository root. Do not
use Docker, Compose, a copy of the tree or a toolchain installed in a sandbox for
validation.

Never run `go test` on the host, including for a single test or a flaky test.
Tests can leave processes behind on the machine that runs them. `gofmt`,
`go build` and `go vet ./...` are safe on the host.

Use fake agents, GitHub clients, providers and container engines in tests. Use
temporary directories and local repositories for filesystem and VCS cases.
Tests must not launch live model sessions or perform remote pushes or delivery.

## Review the change

- Format Go with `gofmt` and document user-visible behavior or configuration.
- After changing an API route or its request or response types, update
  `service.Routes` and run `dagger generate` to refresh `docs/openapi.json`.
  The generate check fails while the committed file is stale.
- Check the diff for local state, secrets, generated files and unrelated edits.
- Report each check actually run and its result. If a check cannot run, give the
  exact blocker.

The [testing guide](docs/testing.md) maps the checks to the behaviors they cover.
