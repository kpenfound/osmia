# Osmia

A personal software factory for long-running feature work on repositories you
contribute to but do not own. Configured at the user level and driven through one
chief of staff, Osmia shares agent capacity across workstreams and projects while
keeping its configuration, plans and records outside the repositories it works on.

Hand it a feature. Agents plan it, debate the specification with you, build and
review it in pieces, and deliver a branch on your fork with a pull request you are
happy to put your name on. You make the decisions that need your judgement;
Osmia handles the work between them and stops at delivery.

**Under construction.** See the [design](docs/design.md) for the intended product
and the [milestones](https://github.com/kpenfound/osmia/milestones) for the build order.

The [core adapter boundary](docs/core-adapter.md) maps execution contracts to the
pinned dependency and records upstream capability gaps.

The [turn isolation reference](docs/isolation.md) documents service-owned file
views, role grants and fail-closed host/container execution requirements.

The [configuration reference](docs/configuration.md) documents the supported
loader schema, defaults, local root layout and project registration.

The [trace repository reference](docs/trace.md) documents typed M1 records,
revision history, atomic workflow transactions, durable outbox leases, local
operation recovery, agent queues with owned turn logs, and the feature spec
and plan formats with their validator in the dedicated local Git repository.

The [local entity map reference](docs/knowledge-base.md) documents
`kb/entities.json`, its seed from CODEOWNERS and directory structure, and
footprint resolution.

The [runtime override reference](docs/runtime.md) documents persisted operator
choices and their resolution against configuration.

The [local service API](docs/service.md) documents Unix-socket ownership, the
versioned M1 endpoints and the in-process client.

The [M1 demonstration](docs/m1-demonstration.md) runs a durable role thread
across a service restart with fake engines and explains how to inspect its trace.

The [charter reference](docs/charter.md) documents the charter template, the
numbered-rule format and how owner edits are recorded.

The [command line](docs/cli.md) documents the foreground service, project
registration and thin local operator clients, including JSON output and exit
codes.
