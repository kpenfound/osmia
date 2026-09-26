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

[Releases](docs/release.md) documents `osmia --version`, building release
archives with Dagger and installing from an archive.

The [core adapter boundary](docs/core-adapter.md) maps execution contracts to the
pinned dependency and records upstream capability gaps.

The [turn isolation reference](docs/isolation.md) documents service-owned file
views, role grants and fail-closed host/container execution requirements.

The [configuration reference](docs/configuration.md) documents the supported
loader schema, defaults, local root layout and project registration.

The [trace repository reference](docs/trace.md) documents typed M1 records,
revision history, atomic workflow transactions, durable outbox leases, local
operation recovery, agent queues with owned turn logs, the chief of staff's
workstream status, and the feature spec and plan formats with their validator
in the dedicated local Git repository.

The [knowledge base reference](docs/knowledge-base.md) documents
`kb/entities.json`, its seed from CODEOWNERS and directory structure,
footprint resolution, and the librarian's extraction pass that writes the
subsystem prose and the refined map.

The [turn context reference](docs/context.md) documents the file-based bundles of
charter rules, knowledge-base prose, entities and rulings that turns receive.

The [runtime override reference](docs/runtime.md) documents persisted operator
choices and their resolution against configuration.

The [local service API](docs/service.md) documents Unix-socket ownership, the
versioned M1 endpoints, the in-process client and the embedded web page.

The [M1 demonstration](docs/m1-demonstration.md) runs a durable role thread
across a service restart with fake engines and explains how to inspect its trace.

The [M2 onboarding demonstration](docs/m2-onboarding.md) walks a project
from `osmia project add` to a ready charter and a local knowledge base.

The [M2 hand-in demonstration](docs/m2-demonstration.md) takes handed
material to a validated spec and plan with a fake architect and chief of staff.

The [M2 chief-of-staff demonstration](docs/m2-chief-of-staff.md) takes a
worker's question through the chief of staff, the owner's inbox and ruling,
and back to the worker, across a service restart.

The [M2 exit demonstration](docs/m2-exit.md) hands in four designs and takes
them through debate, the owner's overrule, a skipped debate and an abandon to
sealed, ratified plans with feature branches, across a restart in the middle
of a sealing.

The [M3 sequential implementation demonstration](docs/m3-exit.md) takes a
ratified two-unit plan through a mason's question and answer to a recorded
report and candidate commit for the first unit. Its
[landing demonstration](docs/m3-exit.md#landing-and-recovery-demonstration)
lands three reviewed units in dependency order, rebasing an approved unit in
flight back to review, refreshing the knowledge base from a landed learning
and recovering an interrupted landing across a restart.

The [M3 final review and delivery demonstration](docs/m3-exit.md#final-review-and-delivery-demonstration)
routes a final-review gap through a follow-up unit, then exercises owner
approval and resumable publication in both delivery styles with local fakes.

The [M4 parallel units demonstration](docs/m4-parallel-units.md) builds two
workstreams on shared mason slots: disjoint units in parallel, entangled units
in sequence, freed slots by priority and then in turn, an overlap advisory to
each chief of staff, and a restart in the middle of a mason's turn.

The [M4 upstream drift demonstration](docs/m4-upstream-drift.md) keeps two
workstreams current with upstream on the project's cadence, then resolves an
owner-requested drift rebase's conflict through a reviewed mason turn across a
restart. Along the way it returns an approval to review before it lands.

The [M4 amendments and standing rulings demonstration](docs/m4-amendments.md)
follows a mason's amendment through the shed and owner decision, then a
ratified charter rule into another workstream's bundle.

The [M4 reliability demonstration](docs/m4-reliability.md) keeps one
workstream going through the daily budget, a hard pause, a live profile
switch, a configuration reload, a provider usage limit and a crash while a
unit lands, then a restart.

The [M5 first-release demonstration](docs/m5-first-release.md) takes one
workstream from hand-in to a delivered pull request, notifying a fake webhook
once for every owner decision and the daily budget pause, across restarts.

The [charter reference](docs/charter.md) documents the charter template, the
numbered-rule format and how owner edits are recorded.

The [command line](docs/cli.md) documents the foreground service, project
registration and thin local operator clients, including JSON output and exit
codes.
