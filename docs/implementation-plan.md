# Design coverage and completion plan

Scope: [design v0.2](design.md), the active factory scope in `bees.toml`, and the
optional Hearsay integration. This is a feature-level assessment; work-item
decomposition belongs to the development factory. Bootstrap, the Go module and
factory initialization are already present.

## Coverage

| Design area | Implementation and evidence |
| --- | --- |
| Durable record and turns, §§3–4,8–9 | Trace transactions, explicit owner decisions, durable threads and operation reconciliation. Tool calls record intent and completion; interrupted effects remain visible. Private role-note bodies stay private. |
| Planning and ratification, §§5.1–5.3 | Hand-in, architect, adversarial shed, revision-pinned owner dispositions, sealing and amendments. Phone and API document edits refuse stale revisions, local file edits and already-ratified documents. |
| Implementation and delivery, §§5.2–5.5 | Unit DAG scheduling, footprint gates, candidate review, contests, amendments, serial landing, final review, follow-ups and approved publication. Models execute inside turns; Go owns transitions and scheduling. |
| Chief of staff, §§6,9.4 | Durable questions, citations, escalations, owner rulings, notices, charter proposals, status and factory controls. Committed-code inspection produces a citation; answers using it derive a serialized librarian knowledge refresh from immutable evidence. |
| Workspaces and dependencies, §7 | Git and Jujutsu, durable same-project base graphs, pinned sealing bases, drift/review invalidation, ancestor waits and integration detection. Explicit-base replay drops a squashed dependency's implementation history and preserves resumable conflicts. |
| Dependent publication, §7.6 | Fork requests target the published parent branch. Integration triggers separate terminal delivery maintenance: renewed review, fresh owner approval and a new upstream request. Both request URLs remain recorded; delivered implementation never reopens. |
| Service and configuration, §§10,13–14 | Multiple projects register and reload live, share capacity and drain independently. Detached serving has readiness, private logs, root ownership and Unix-socket shutdown. |
| Phone and CLI, §11 | Project onboarding, hand-in, charter/spec/plan reads and edits, dependency selection, shed/charter decisions, abandonment, trace navigation, pauses, priorities, profiles and delivery approvals. Browser tests exercise actual API effects and stale revisions. |
| Optional memory, §§12,15,17 | Authenticated role tools, bounded scoped bundles, local fallback and health reporting. Watches persist cursors, deduplicated events and chief notices atomically. A local setup export supplies entity, scope, authority and anchor data. |
| Hearsay ingestion, §12.2 | Companion connector in Hearsay's `internal/connector/osmia`, registered in its binary. It reads committed trace snapshots from a read-only mount, uses durable page cursors and stable record/revision identities, distinguishes owner and work sources, excludes private notes and handles deletions. Shared configuration fixtures verify authority separation. |

## Implementation sequence and acceptance

The work follows these dependencies:

1. Complete the independent file-based workflow: multi-project registration and
   drain/reload recovery, owner interfaces, notices and code knowledge feedback.
2. Persist the acyclic workstream-base graph before using it for sealing, drift,
   review or publication. Record the selected sealing revision before creating a
   branch. Keep waits outside agent capacity and preserve revision-pinned owner
   approval throughout base movement and restart.
3. Connect optional memory through the provider boundary. Fix source identities,
   authority and cursor contracts before ingesting history. Keep local rulings
   durable before any external effect, and keep memory events informational.
4. Qualify the combined behavior using fake agents, local repositories, fake
   hosting clients, HTTP fixtures and browser tests inside Dagger. Review changed
   configuration and documentation together with the code.

Focused dependency checks cover a three-feature chain on both backends,
interruption after branch creation, parent movement, missing and restored
branches, abandonment, squash integration and retained-workspace conflict
resolution. Integration checks retain an undelivered parent as the dependency
even when its initial commit is already upstream, and release a ratified child's
wait when a merged parent's branch was removed before the child sealed.
Delivery-maintenance tests cover fresh approval, a lost hosting
response, restart reconciliation, one upstream request and terminal feature
state, including reviews that find a gap. Graph and hand-in tests cover cycles
and stable retry identities.

Memory checks cover delegated credentials, role grants, bounded fallback,
source cursor replay, committed snapshots, private notes, document deletions,
owner authority, knowledge-gap provenance and watch cursor resumption without
duplicate notices. The shared setup fixture is checked by both Osmia's exporter
and Hearsay's actual configuration loader and authority policy.

## Configuration and qualification

The owner selected fork-hosted dependent requests followed by new upstream
requests after integration. The owner will configure Hearsay deployment, secrets,
GitHub repositories and Discord channels separately. No live data sources or
credentials are installed by this implementation. [Hearsay setup](hearsay.md)
explains how to merge the generated fragments and pin anchors after ingestion.

Authoritative rulings use durable connector ingestion. Agent `assert` remains a
proposal tool; an optional immediate owner-assertion fast path requires a shared
identity with connector ingestion and is not enabled. The design distinguishes
this optional optimization from durable ruling delivery.

Platform sandbox confinement and a live Hearsay deployment require release
qualification in the intended environment. Automated tests exercise the
execution boundary with fake enforcers and never launch real model sessions,
push to remote hosting or create live pull requests.

The explicit-base replay implementation carries a removal TODO for the reusable
busybees/core capability. Osmia continues to use its pinned core dependency and
requires no sibling checkout to build.

Validation passed with `DAGGER_X_RELEASE=v1.0.0-beta.14 dagger check`: all four
Osmia checks passed, including 926 Go tests, the browser suite, generated-file
consistency and release version round-trip. Browser tests skip in the Go-only
container and run in the separate Chromium check. Focused race checks passed
dependency chains, conflict recovery, sealing recovery, knowledge-gap refresh,
memory setup and watch resumption. Hearsay passed all ten checks, including 420
unit tests, integration, lint, configuration generation and image validation.
Host `go vet ./internal/...`, JavaScript syntax and diff whitespace checks also
passed. No tests run on the host.

Per-workstream profiles, automatic refusal of cross-workstream footprint overlap
and multiple masons inside one unit remain outside the required design scope.
