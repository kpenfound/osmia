# Working on Osmia

## Design

[docs/design.md](docs/design.md) is the source of truth, and issues describe outcomes without overriding it. Follow its feature dependencies. When a change conflicts with the design, raise the conflict instead of silently changing product scope.

## Architecture guardrails

- Go owns state transitions, scheduling, capacity and owner gates. Models run inside turns, never on the scheduling path.
- The service owns version control and delivery credentials. Runtime agents receive scoped files and tools and never commit, rebase or push. This is a product requirement, not a restriction on the branch and pull-request workflow used to develop Osmia.
- Target-project factory state lives under the Osmia root, outside the target repository.
- The trace, durable threads and explicit owner decisions survive restarts and retries. Reconcile side effects before retrying them.
- `github.com/kpenfound/agentcore` is a pinned dependency, with no sibling checkout and no copy of its internals.

## Web page design skills

Masons and reviewers who change or review `internal/web` load these skills first:

- Build with [.agents/skills/frontend-design/SKILL.md](.agents/skills/frontend-design/SKILL.md): plan, check the plan for generic defaults, build, then critique.
- Review with [.agents/skills/web-design-guidelines/SKILL.md](.agents/skills/web-design-guidelines/SKILL.md): the interface checklist and its output format.
- Build and review with [.agents/skills/osmia-design-language/SKILL.md](.agents/skills/osmia-design-language/SKILL.md): the page's tokens, type, components, nest tube, Conversation and Activity split, banned defaults and the quieter, distill and polish passes. It also records the chosen fonts and colours.

## Checks

- Run focused checks for what you changed. The check service runs the full suite. [docs/testing.md](docs/testing.md#validate-as-a-mason) covers how to select and run checks.
- Never run `go test` on the host, in any role and for any purpose. Tests start processes that leak onto the machine they run on, so they run only inside Dagger. `gofmt`, `go build` and `go vet` are fine on the host.
- Fakes live only in tests, and tests use only fake agents, GitHub clients, providers and container engines. They never launch real model sessions, the live factory, remote pushes or pull requests. The one exception is `internal/checks/daggercli_test.go`, built with the `daggercli` tag and run by the `dagger-cli-contract` check.
- List every API endpoint in `service.Routes`. After changing a route or a request or response type, run `dagger generate` and keep `docs/openapi.json` current.
- Report the checks you ran and their results. If validation is blocked, state the exact blocker.

## Comments

Follow the no-comments and no-deltas skills. The one exception: accurate doc comments on exported identifiers and packages, including under `internal/`, are allowed.

## Missing agentcore capability

When a feature needs functionality agentcore lacks, report the gap in your report and add it to the list of agentcore gaps in [docs/architecture.md](docs/architecture.md#agentcore-gaps).
