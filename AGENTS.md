# Working on Osmia

This file holds the rules for changing this repository. Each rule says why it exists, so you can tell where it stops applying. [docs/prompting.md](docs/prompting.md) covers the text Osmia gives its own agents.

## Design

[docs/design.md](docs/design.md) is the source of truth. Issues describe outcomes and never override it, because product scope is decided in one place. Follow its feature dependencies. When a change conflicts with the design, raise the conflict instead of silently changing product scope.

## Architecture guardrails

- Go owns state transitions, scheduling, capacity and owner gates. Models run inside turns and never on the scheduling path, so the factory behaves the same whatever a model says.
- The service owns version control and delivery credentials. Runtime agents receive scoped files and tools and never commit, rebase or push, so no agent can publish or rewrite history. This is a product requirement. It does not restrict the branch and pull-request workflow used to develop Osmia.
- Target-project factory state lives under the Osmia root, outside the target repository, so the factory never writes into the project it works on.
- The trace, durable threads and explicit owner decisions survive restarts and retries. Reconcile side effects before retrying them, so a retry does not repeat what already happened.
- `github.com/kpenfound/agentcore` is a pinned dependency, with no sibling checkout and no copy of its internals, so Osmia builds from one known version of it.

## Web page design

When you change or review `internal/web`, read these guides first. They are repository files, and the first two are vendored.

- Build with [.agents/skills/frontend-design/SKILL.md](.agents/skills/frontend-design/SKILL.md): plan, check the plan for generic defaults, build, then critique. It keeps the page from settling on a generic design.
- Review with [.agents/skills/web-design-guidelines/SKILL.md](.agents/skills/web-design-guidelines/SKILL.md): the interface checklist and its output format, so every review reads the same.
- Build and review with [.agents/skills/osmia-design-language/SKILL.md](.agents/skills/osmia-design-language/SKILL.md): the page's tokens, type, components, nest tube, Conversation and Activity split, banned defaults and the quieter, distill and polish passes. It also records the chosen fonts and colours, so the page stays one design.

## Checks

- Run focused checks for what you changed. The check service runs the full suite, so you need not get the whole suite passing before you finish. [docs/testing.md](docs/testing.md#validate-as-a-mason) covers how to select and run checks.
- Never run `go test` on the host, in any role and for any purpose. Tests start processes that leak onto the machine they run on, so they run only inside Dagger. `gofmt`, `go build` and `go vet` are fine on the host because they start no tests.
- Fakes live only in tests, and tests use only fake agents, GitHub clients, providers and container engines. They never launch real model sessions, the live factory, remote pushes or pull requests, because a test must not spend money or publish anything. The one exception is `internal/checks/daggercli_test.go`, built with the `daggercli` tag and run by the `dagger-cli-contract` check.
- List every API endpoint in `service.Routes`. After changing a route or a request or response type, run `dagger generate` and keep `docs/openapi.json` current, because the generate check fails while the committed file is stale.
- Report the checks you ran and their results. If validation is blocked, state the exact blocker, so a reader knows what was and was not verified.

## Comments and documentation

- Write no comments in code, except accurate doc comments on exported identifiers and packages, including under `internal/`. Code states what it does, and a comment that restates it drifts away from it.
- Describe current behaviour in doc comments, documentation and tests. Leave out change-history wording and development-phase labels, because a reader needs what is true now, and history belongs to version control.

## Missing agentcore capability

When a feature needs functionality agentcore lacks, report the gap in your report and add it to the list of agentcore gaps in [docs/architecture.md](docs/architecture.md#agentcore-gaps). Osmia does not modify or copy agentcore, so the gap list is how its owners learn what to add.
