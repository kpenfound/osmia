# M6 Jujutsu recovery demonstration

`TestM6JujutsuRecoveryDemonstration` in
`internal/service/jujutsu_recovery_demo_test.go` takes one workstream on
[Jujutsu workspaces](service.md#workspace-backends) from hand-in to an
owner-approved pull request through the three events Jujutsu workspaces
recover from: a mason turn cut short, the service dying in the middle of a
landing, and upstream drift that conflicts with the feature branch and with
the units in flight. The owner acts through the service's local API client,
the same API the `osmia` commands use.

The test uses a temporary Osmia root with `workspaces = "jujutsu"`, a local
Git clone with a bare upstream and a bare fork, file-based context, fake
agents and a fake pull request host. It starts no model, container, tailnet,
remote push or real pull request. Its plan has `resume`, and `dedupe` and
`audit`, which depend on `resume` and are built side by side with
`capacity.masons = 2`.

## What the demonstration shows

1. **A stopped mason turn keeps its edits.** `resume`'s first mason turn
   changes `internal/trace/git.go` and adds `internal/trace/kept.go`, and is
   still working when the owner sets a hard pause on the workstream. The pause
   stops the turn and the edits stay in the unit's workspace. When the owner
   clears the pause, the continuation's prompt names the two files the
   stopped turn changed, and the continuation finds them in its view and
   reports done. See [hard pause](service.md#hard-pause).
2. **The service dies in the middle of a landing.** `resume` is reviewed and
   approved. Its landing moves the feature branch, and the service stops
   before the landing is recorded. The feature workspaces' Jujutsu repository
   holds the checkpoint taken before the attempt. The restarted service
   restores that repository to the checkpoint's operation-log entry before
   anything else runs, and the retried landing finds the commit it made by
   its `Osmia-Operation` trailer: the feature branch holds that one landing,
   and `units/resume/landing.json` records it.
3. **Upstream drift conflicts with the feature branch and both units in
   flight.** `dedupe` is built on `resume`'s landing and waits for review,
   and `audit`'s mason has left `internal/audit/audit.go` unfinished in its
   workspace. Upstream changes `internal/trace/git.go` and adds its own
   `internal/trace/dedupe.go` and `internal/audit/audit.go`. `dedupe`'s
   reviewer asks for a drift rebase of the project, as the owner would with
   `osmia project rebase`, and approves `dedupe`. The pending drift rebase
   holds the project's lander, so the approval does not land.
   - The feature branch's replay conflicts in `internal/trace/git.go`. The
     drift mason resolves the markers in the resolution workspace, the drift
     reviewer approves the resolution, and the feature branch and the seal's
     base move to it. See [drift rebases](service.md#drift-rebases).
   - Carrying the units onto the resolved branch finishes for both. Each
     rebased commit stores the conflict in the unit's file and carries the
     unit's change ID. `dedupe` returns from `approved` to `implementing`,
     and `audit` stays `implementing`. Each unit's mason gets a turn that
     names the conflicted file and finds the markers in its view. It
     resolves them and reports done, and the unit is reviewed again on the
     resolved branch and lands. See
     [rebasing units in flight](service.md#rebasing-units-in-flight).
4. **The owner approves the delivery.** Final review presents it, the owner
   approves it, and the service pushes the feature branch to the fork and
   opens one pull request whose body is the approved draft. The delivered
   branch holds `resume`'s kept edits and each mason's resolution.

Throughout, the test checks two things of each unit that was rebased:

- **The trace follows the unit through its rebases.** Every rebased commit
  carries the change ID `units/<unit>/change.json` records, and
  `osmia trace <workstream> commit <commit>` of the unit's first snapshot and
  of each rebased commit names the unit by that change. It names the unit for
  every candidate a reviewer read.
- **No approval crosses a changed candidate or base.** The unit was
  reviewed again on each base a rebase moved it to. It landed the exact
  candidate and base its last review approved, never a candidate from
  before a rebase. `dedupe`'s first approval, on `resume`'s landing, never
  landed.

## Running it

The demonstration needs `jj`. `dagger check` installs the pinned release in
its test container. To run just the demonstration inside Dagger:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=sh,-c,'curl -fsSL https://github.com/jj-vcs/jj/releases/download/v0.45.1/jj-v0.45.1-$(uname -m)-unknown-linux-musl.tar.gz | tar -xz -C /usr/local/bin ./jj' \
  with-env-variable --name=OSMIA_REQUIRE_JJ --value=1 \
  with-exec --args=go,test,-count=1,-run,TestM6JujutsuRecoveryDemonstration,-v,./internal/service \
  combined-output
```

Without `jj` on `PATH` the test skips, unless `OSMIA_REQUIRE_JJ` is set, when
it fails.
