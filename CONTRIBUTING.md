# Contributing to Osmia

Read [the design](docs/design.md) before proposing or changing behavior. It is
the source of truth for product scope and feature dependencies. Keep changes focused,
and keep target-project factory state outside the repository Osmia works on.

## Check a change

Run the complete check suite through Dagger from the repository root:

```sh
dagger check --progress=report
```

The check needs a working Dagger container engine. It runs the Go suite,
including browser tests, and release checks in containers. `--progress=report`
prints each check's result and the output of failing tests. For selected tests
or one package, use the Go check's selection flags:

```sh
dagger check --progress=report --go-test=TestA --go-test=TestB
dagger check --progress=report --go-package=internal/service
```

Add `--env=race` to run them with the race detector when changing concurrent
scheduling, recovery or shared state. [AGENTS.md](AGENTS.md#validation) covers
listing tests and rerunning a failing check.

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
  `dagger check` fails while the committed file is stale.
- Check the diff for local state, secrets, generated files and unrelated edits.
- Report each check actually run and its result. If a check cannot run, give the
  exact blocker.

The [testing guide](docs/testing.md) maps the checks to the behaviors they cover.
