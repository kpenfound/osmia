# Contributing to Osmia

Read [the design](docs/design.md) before proposing or changing behavior. It is
the source of truth for product scope and milestone order. Keep changes focused,
and keep target-project factory state outside the repository Osmia works on.

## Check a change

Run the complete check suite through Dagger from the repository root:

```sh
DAGGER_X_RELEASE=v1.0.0-beta.14 dagger check
```

The check needs a working Dagger container engine. It runs the Go suite, browser
tests and release checks in containers. For one package or selected tests, use
the Dagger container command in [AGENTS.md](AGENTS.md#validation). Run a race
check there when changing concurrent scheduling, recovery or shared state.

Never run `go test` on the host, including for a single test or a flaky test.
Tests can leave processes behind on the machine that runs them. `gofmt`,
`go build` and `go vet ./...` are safe on the host.

Use fake agents, GitHub clients, providers and container engines in tests. Use
temporary directories and local repositories for filesystem and VCS cases.
Tests must not launch live model sessions or perform remote pushes or delivery.

## Review the change

- Format Go with `gofmt` and document user-visible behavior or configuration.
- Check the diff for local state, secrets, generated files and unrelated edits.
- Report each check actually run and its result. If a check cannot run, give the
  exact blocker.

The [testing guide](docs/testing.md) maps the checks to the behaviors they cover.
