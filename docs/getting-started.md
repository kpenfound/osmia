# Getting started

This guide takes you from nothing to a pull request on your fork that you
approved. Follow the steps in order.

## What the first release supports

- One project at a time.
- Git worktrees: the service creates each workstream's feature branch in a
  worktree of your clone and pushes it to your fork itself.
- File-based context: the charter, the knowledge base and your rulings reach
  agents as files.
- The web page on your tailnet, beside the command line on the local socket.

Jujutsu, several projects and the optional Hearsay context source come later
(see the [design](design.md#18-implementation-milestones)). Nothing in this guide
depends on them.

You need `git`, a clone of the project you contribute to whose remotes include
the upstream repository, a fork of it that you can push to, and the agent CLI
your profile names (`claude`, `codex` or `opencode`), signed in. For the
phone page you also need a [Tailscale](https://tailscale.com) tailnet.

## 1. Install a release

Download the archive for your platform and `checksums.txt` from the
[release](release.md), then verify and install:

```sh
version=v0.1.0
name=osmia_${version}_darwin_arm64   # or linux_amd64, linux_arm64, darwin_amd64
grep " $name.tar.gz\$" checksums.txt | shasum -a 256 -c   # sha256sum -c on linux
tar -xzf "$name.tar.gz"
sudo install -m 0755 "$name/osmia" /usr/local/bin/osmia
osmia --version
```

`osmia --version` prints the version and commit, for example
`osmia v0.1.0 (5e8a15b3915095d9e00bbc3ab3e5cc97970f2be1)`. On macOS, clear the
quarantine attribute of a browser download first; see
[installing from an archive](release.md#installing-from-an-archive).

## 2. Write `~/.osmia/config.toml`

Everything Osmia keeps lives under one root, `~/.osmia` by default (every
command accepts `--root PATH`). Create it and its top-level file:

```toml
version = 1
active_projects = []

[profiles.default]
agent = "claude"
model = "your-model-name"

[listen]
tailnet = "osmia"

[notify]
webhook = "https://ntfy.sh/your-private-topic"
```

- **Profiles and roles.** A profile names an `agent` (`claude`, `codex` or
  `opencode`) and a `model`, and optionally `effort`, `fallback`, `timeout` and
  `max_turns`. Every role (`architect`, `committee`, `chief_of_staff`,
  `foreman`, `librarian`, `mason`, `reviewer`) uses the profile named `default`
  unless you bind it in a `[roles.<role>]` table with `profile = "<name>"`,
  and, if you want confinement, `sandbox`. Keys and defaults are in the
  [configuration reference](configuration.md#top-level-configtoml).
- **`listen.tailnet`** makes the service join your tailnet under that hostname
  so a phone signed in to the same tailnet can open `http://osmia/`. On first
  run the node must log in: start the service with `TS_AUTHKEY` set to a
  Tailscale auth key, or without one and open the login URL the service prints
  to stderr (`osmia status` shows it on its `Tailnet:` line). The node's state
  is kept in `<root>/tailnet`, so later starts need neither. There is no login
  of Osmia's own: your tailnet's access rules decide who can reach the page and
  make decisions. Leave `tailnet` out to use the command line alone.
  See [reach Osmia from your phone](service.md#reach-osmia-from-your-phone).
- **`notify.webhook`** is optional. With it set, every decision that opens in
  your inbox, and the daily budget pause, is posted once as plain text to that
  `http` or `https` URL, such as an [ntfy](https://ntfy.sh) topic, with the
  tailnet address to open. The URL is shown in `/v1/config`, so avoid one that
  embeds a secret you do not want there. See
  [notifications](service.md#notifications).

## 3. Run the service

```sh
osmia serve
```

The service runs in the foreground; leave it running. In another terminal:

```sh
osmia status
curl --unix-socket ~/.osmia/osmia.sock http://localhost/v1/health
```

`osmia status` reports the service healthy, `Project: none configured` and,
with `listen.tailnet` set, a `Tailnet:` line that reads `up` once the node has
logged in. `/v1/health` returns the service name, API version and
`build.version` and `build.commit`, the ones `osmia --version` printed. A
restart-required or reload-required setting appears among the diagnostics; after
editing the file, `osmia reload` applies what it can
([reload](service.md#reload)).

## 4. Add a project and write its charter

```sh
osmia project add myproject --upstream owner/repo --fork you/repo --clone ~/src/repo
```

The command prints the project ID (`p_…`). The service creates the project's
trace repository under `<root>/projects/<project-id>/`, outside your clone, seeds
a map of the repository's subsystems, and asks the librarian for the first
knowledge-base extraction. `osmia status` shows the extraction
`succeeded` when it has finished. Registration never writes to your clone; see
[project registration](configuration.md#project-registration).

Hand-in is refused until the project has a charter with at least one rule. The
charter is your rules as a contributor to this project, in
`<root>/projects/<project-id>/charter.md`. Open it and write numbered rules
under the template's headings:

```markdown
1. Every bug fix comes with a test that fails without it.
2. Run the full suite before opening a pull request.
```

There is nothing further to ratify: the service records your edit as a new
charter revision the next time it reads the file, and `osmia status` then shows
`Charter: ready (2 rules, revision 2)`. Rules later proposed by the chief of
staff are decided with `osmia charter`. See [charter](charter.md) for the
format. The [onboarding demonstration](m2-onboarding.md) shows these steps run
end to end with fake agents.

## 5. Hand in a workstream and decide

Write the feature down in a Markdown file, or point at a GitHub issue URL, and
hand it in with the project ID:

```sh
osmia handin p_… design.md
```

The command prints the workstream ID (`w_…`). Add `--skip-debate` for small
work: the committee does not debate it and the draft waits for your ratification.
Follow progress with `osmia status <workstream-id>`, or on the page.

The architect drafts a spec and a plan of units, the committee debates them in
the shed, and the chief of staff presents them to you. From then on Osmia stops
at every decision that is yours and waits. Each is an **inbox entry**, and you
can answer it from either place:

- **The phone page.** Open `http://osmia/` (or the node's MagicDNS name). The
  inbox lists every open decision with its question, the options, the
  recommendation and what waits on it, and each card carries its own controls.
  The page follows the service as it works, so a new decision appears without a
  reload. See the [web page](service.md#web-page).
- **The command line.** `osmia inbox` lists the same entries, each with the
  command that answers it.

The decisions, in the order a workstream meets them:

| Decision | Page | Command |
| --- | --- | --- |
| Ratify the spec and plan (or rule on an objection, or ask for a redraft) | ratification card | `osmia ratify <workstream-id>`, `osmia shed …` |
| A question a worker escalated | escalation card: answer, accept the recommendation or use an option | `osmia answer <number> "ruling"` or `--accept` |
| An amendment to the sealed spec or plan | amendment card | `osmia amendment <workstream-id> <n> approve\|reject\|round\|overrule [note]` |
| A unit that needs your direction | contested card | `osmia contested <workstream-id> <unit> review\|revise "note"` |
| Approve delivery | delivery card | `osmia delivery <workstream-id>`, then `osmia approve <workstream-id>` |

Ratifying seals the spec and plan: the service fetches upstream, creates the
feature branch in a worktree of your clone, and masons build the units, each
reviewed and landed in order. You can talk to the workstream's chief of staff
meanwhile with `osmia send <workstream-id> "…"` or on the page, and hold work
with `osmia pause`.

When every unit has landed and the final review is done, the workstream's
delivery entry opens. Read the final report and the drafted pull request
description; on the page you can edit the description before approving. Approve
it on the page, or with `osmia approve <workstream-id> [description-file]`. The
service then pushes the branch to your fork and opens the pull request against
the upstream itself, with your description; no agent pushes. `osmia delivery
<workstream-id>` shows the pull request it was delivered as, and the workstream
ends `delivered`.

## Where to go next

The milestone guides are reference material for what each part does and how it
is tested:

- [Command line](cli.md), [configuration](configuration.md), [service API](service.md) and [charter](charter.md).
- [Turn isolation](isolation.md) for sandboxes.
- The demonstrations run the whole path with fake agents:
  [first release](m5-first-release.md), [web page](m5-web-page.md),
  [onboarding](m2-onboarding.md), [hand-in](m2-demonstration.md),
  [chief of staff](m2-chief-of-staff.md), [exit](m2-exit.md),
  [implementation and landing](m3-exit.md), [parallel units](m4-parallel-units.md),
  [upstream drift](m4-upstream-drift.md), [amendments](m4-amendments.md) and
  [reliability](m4-reliability.md).
