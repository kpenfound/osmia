# M6 Jujutsu workspaces demonstration

`TestM6JujutsuWorkspacesDemonstration` in
`internal/service/jujutsu_demo_test.go` takes the same handed-in feature from
hand-in to an owner-approved pull request twice through the service's local
API client, the same API the `osmia` commands use: once on Git worktrees and
once on Jujutsu workspaces. It shows that the backend changes nothing the
owner sees.

## The backend choice

The top-level [`workspaces`](configuration.md#top-level-configtoml) key picks
the backend of each new workstream's workspaces: its feature branch, units and
drift resolution.

| Value | New workstreams get |
| --- | --- |
| `auto` (default) | Jujutsu workspaces when `jj` 0.45.0 or later is on the service's `PATH`, Git worktrees otherwise |
| `git` | Git worktrees |
| `jujutsu` | Jujutsu workspaces; without a supported `jj` the hand-in is refused with `unavailable` |

Jujutsu workspaces live under the Osmia root on the clone's Git store. The
repository stays Git: branches are Git refs, landings are Git commits, and the
service pushes the feature branch to the fork with Git. Agents get copies of
their workspace's files and no version control either way. A workstream keeps
the backend it was created on; `osmia status` prints it as `workspaces=` beside
the workstream's state. See [workspace backends](service.md#workspace-backends).

## What the demonstration shows

Each run uses a temporary Osmia root, a local Git clone with a bare upstream
and a bare fork, file-based context, fake agents and a fake pull request host.
It starts no model, container, tailnet, remote push or real pull request. The
two runs differ only in `workspaces = "git"` or `workspaces = "jujutsu"`, and
go through the same steps:

1. Service status names the configured backend for new workstreams. The design
   is handed in with debate skipped and sealed, and the workstream's status
   reports its backend. The owner ratifies the packet.
2. `resume`'s mason asks a question, the chief of staff escalates it, and the
   owner answers it through the inbox.
3. `resume` and then `dedupe` are built, reviewed and land. The unit and
   feature workspaces are on the run's backend; on Jujutsu, their Jujutsu
   repositories exist under the root.
4. Final review presents the delivery. The owner approves it, and the service
   pushes the feature branch to the fork and opens one pull request whose body
   is the approved draft.

Throughout, no `.jj` appears anywhere in the owner's clone. At the end the
test reads the fork. Its only ref is the feature branch, each commit on it has
only Git's `tree`, `parent`, `author` and `committer` headers, and no tree
holds a `.jj` path. The Jujutsu run's fork matches the Git run's commit for
commit: the same author, message, tree, parent count and paths. Only the
workstream ID and the commit and operation IDs in the `Osmia-` trailers
differ, and those differ between any two runs.

How Jujutsu workspaces recover from a stopped mason turn, a restart in the
middle of a landing and conflicting upstream drift is the
[M6 Jujutsu recovery demonstration](m6-jujutsu-recovery.md).

## Running it

The demonstration needs `jj`. `dagger check` installs the pinned release in
its test container. To run just the demonstration inside Dagger:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=sh,-c,'curl -fsSL https://github.com/jj-vcs/jj/releases/download/v0.45.1/jj-v0.45.1-$(uname -m)-unknown-linux-musl.tar.gz | tar -xz -C /usr/local/bin ./jj' \
  with-env-variable --name=OSMIA_REQUIRE_JJ --value=1 \
  with-exec --args=go,test,-count=1,-run,TestM6JujutsuWorkspacesDemonstration,-v,./internal/service \
  combined-output
```

Without `jj` on `PATH` the test skips, unless `OSMIA_REQUIRE_JJ` is set, when
it fails.
