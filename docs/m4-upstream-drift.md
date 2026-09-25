# M4 upstream drift demonstration

Two tests in `internal/service/drift_demo_test.go` show
[drift rebases](service.md#drift-rebases) end to end:

- `TestM4DriftCadenceDemonstration` keeps two building workstreams of one
  project current on the project's `upstream_rebase` cadence.
- `TestM4DriftConflictDemonstration` covers an owner-requested drift rebase
  whose feature branch conflicts with upstream, and a restart in the middle of
  it.

Both drive the service through its local API client, which the `osmia`
commands also use. Fake masons, reviewers and a fake chief of staff run
through core's fake enforcer and an in-memory MCP transport. Each test uses a
temporary Osmia root and a local Git clone whose `upstream` remote is a local
bare repository. Neither test starts a model or a container, pushes to a
remote or opens a pull request.

Run them with `dagger check`, or run just the demonstrations inside Dagger:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,'TestM4Drift.*Demonstration',-v,./internal/service \
  combined-output
```

## Cadence: two workstreams stay current

The owner adds `upstream_rebase = "3h"` to the project's `config.toml` and
restarts the service (see [configuration](configuration.md#project-configtoml)).
Two workstreams are built on the same two-unit plan with `capacity.masons = 2`.
Each starts `resume`, whose mason writes `internal/trace/built.go` and ends
without reporting done, so the unit stays `implementing` with that work in
its workspace. Upstream's `main` then gains `UPSTREAM.md`.

The service's clock moves three hours past the later sealing. At its next
schedule read the foreman asks for drift rebase 1 of each workstream, with a
reason starting `upstream_rebase 3h0m0s has elapsed since the sealing at
<time>`. The two run one after the other on the project's lander. For each
workstream:

- the feature branch is rebased onto upstream's new `main`;
- `seal.json` gets revision 2, which keeps the seal number and moves its
  upstream base to the fetched commit;
- `resume`'s workspace is carried onto the new tip. It keeps the mason's
  unfinished `built.go`, gains `UPSTREAM.md`, and the unit stays
  `implementing`;
- `drift/rebase.json` records `replayed`, `carrying`, then `rebased`.

A clean drift rebase raises no upstream moved event. `osmia status
<workstream-id>` shows `Drift: rebase 1 rebased at <time>` with no `Moved:`
line.

## On demand: a conflict, a reviewed resolution and a returned approval

This test's plan has `resume` and `audit`, which depends on `resume`, with
`capacity.masons = 1`. After sealing, upstream's `main` adds
`internal/trace/built.go` as a placeholder. `resume`'s mason adds the same
path with its own content, and `resume` is reviewed and lands. `audit` is
then built on the feature branch at `resume`'s landing.

### 1. The owner asks while a unit is under review

While `audit`'s reviewer reads its candidate, the owner asks for a drift
rebase of the project (`osmia project rebase <project-id>`, `POST
/v1/projects/rebase`). The answer is `Covered: <workstream> drift rebase 1`,
with nothing skipped. The reviewer then approves `audit`. At the next pass the
foreman asks for drift rebase 1 before any landing. Its reason starts `the
owner asked for a drift rebase at <time>`.

### 2. The conflict holds the lander

Replaying `resume`'s landing onto upstream conflicts in
`internal/trace/built.go`. `drift-1-conflicted` records the conflict, and
the feature branch and the seal stay where they were. The drift operation
holds the lander, so approved `audit` does not land. The chief of staff gets
an upstream moved event saying the feature branch conflicts with upstream in
`internal/trace/built.go`, and `osmia status` shows `Drift: rebase 1
conflicted` with one `Moved:` line.

The drift mason's turn `drift-mason-1-resolve-1` names the conflicted path
and carries the sealed spec. Its view of the resolution workspace holds the
conflict markers. The mason writes a resolution distinct from both the
feature's file and upstream's placeholder.

### 3. A restart while the mason works

The service stops while the resolve turn is still running. On restart the
turn is `interrupted`, its view is copied back into the resolution workspace,
and one continuation, `drift-mason-recover-1`, finds the resolution and
reports done. The replay is not started again, and no second resolve turn is
queued. `drift/rebase.json` records the conflict once (the opening record and
round 1).

### 4. Review before the branch moves

The foreman stages the resolution, finishes the replay and records the
candidate as `resolved`. The drift reviewer's turn `drift-reviewer-1-1`
reads the candidate against the sealed spec while the feature branch is still
at `resume`'s landing. After its satisfactory verdict, the feature branch moves
to the candidate, `seal.json` revision 2 moves the seal's base to upstream,
and the resolution workspace is removed.

### 5. The approval returns to review

`audit`'s approved workspace does not descend from the new tip. The drift
carries it with `rebase-audit-1`, which is clean. The carry keeps the reviewed
resolution of `internal/trace/built.go` and `audit`'s own change in
`internal/trace/audit.go`; its recorded base is the feature tip before drift.
The candidate changed, so `audit` moves from `approved` back to `reviewing`
with the reason `the approval no longer holds: ...`, and the chief gets an upstream
moved event: `unit audit's approval no longer holds`. The drift rebase then
records `rebased`. `audit`'s reviewer reads the rebased candidate on the new
base and approves it. Only then, after drift rebase 1 has its result, is
`audit` landed on the resolved branch.

`osmia status` shows `Drift: rebase 1 rebased at <time>` with a `Moved:` line
for each of the two events. The chief of staff received both events in its
turns.

## What to inspect

Each workstream's trace holds the drift transitions (`drift-<k>`,
`drift-<k>-conflicted`, `drift-<k>-carrying`, `drift-<k>-rebased`), the
owner's `drift-request-<k>`, and every revision of `drift/rebase.json` and
`seal.json`. The `drift-mason` thread holds the interrupted resolve turn and
its one continuation, and the `drift-reviewer` thread holds the one review.
`units/audit/rebase.json` records the carry. The outbox holds the
`upstream-moved` events.
