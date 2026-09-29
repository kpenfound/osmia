# Osmia

Osmia is a personal software factory for feature work on repositories you
contribute to. You give it a feature description and make the decisions that
need your judgement. It plans, builds and reviews the work, then delivers an
approved feature branch and pull request in your repository or from your fork.

One service manages workstreams, agent capacity and the durable record. It keeps
its configuration in `~/.config/osmia` and factory state in
`~/.local/share/osmia` by default, outside the repositories it works on. Agents receive scoped files and tools; the service
owns version control and delivery credentials.

## Start here

The [getting started guide](docs/getting-started.md) covers installation,
configuration, project registration, hand-in, owner decisions and delivery. It
uses one project, local file context and the web page on a tailnet. A local
session starts with:

```sh
osmia serve
# In another terminal:
osmia status
osmia inbox
```

Osmia is under construction. The [design](docs/design.md) defines the intended
behavior and feature dependencies; the guides describe implemented behavior.

## How it works

1. Register a project and write its contributor charter. Osmia builds a local
   knowledge base from the clone.
2. Hand in a feature. An architect drafts a numbered specification and a plan;
   reviewers challenge them before you ratify them.
3. Masons implement plan units in scoped workspaces. Reviewers check each
   candidate, and the service lands approved units in dependency order.
4. A final review shows the evidence for each criterion. You approve delivery;
   the service pushes the branch and opens the pull request.

Questions, amendments and contested reviews come to one owner inbox. The trace
keeps the documents, decisions and turns through restarts. See the
[architecture guide](docs/architecture.md) for the components and boundaries.

## Documentation

| Need | Read |
| --- | --- |
| Install and complete a first workstream | [Getting started](docs/getting-started.md) |
| Commands and settings | [CLI](docs/cli.md), [configuration](docs/configuration.md) |
| Run the service, web page and notifications | [Running the service](docs/running.md) |
| Write a project's charter | [Charter](docs/charter.md) |
| Understand the components | [Architecture](docs/architecture.md) |
| Configure optional memory | [Hearsay integration](docs/hearsay.md) |
| Run and understand the checks | [Testing](docs/testing.md) |
| Contribute a change | [Contributing](CONTRIBUTING.md) |
| Build or install a release | [Release](docs/release.md) |

The [design](docs/design.md) is the source of truth for product scope.
