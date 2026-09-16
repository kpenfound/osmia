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

The [M1 configuration reference](docs/configuration.md) documents the supported
loader schema, defaults and local root layout.

The [runtime override reference](docs/runtime.md) documents persisted operator
choices and their resolution against configuration.
