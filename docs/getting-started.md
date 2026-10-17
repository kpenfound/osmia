# Getting started

This guide takes you from nothing to a pull request in your repository or from your fork that you
approved. Follow the steps in order.

## Supported features

- Projects with one charter and one or more repos, each with its own clone,
  settings, knowledge base and entity map.
- Feature workgroups with one shared chief of staff, optional project design
  chat, cross-repo contracts and merge dependencies.
- Git worktrees, or Jujutsu workspaces on your clone's Git store: the service
  builds each workstream's feature branch in them and pushes it to your repository or fork
  itself.
- File-based context: the charter, the knowledge base and your rulings reach
  agents as files.
- The web page on your tailnet, beside the command line on the local socket.

This walkthrough uses one project and file-based context. Optional
[Hearsay memory](hearsay.md) can be configured separately.

You need `git`, a clone of the project you contribute to whose remotes include
the upstream repository and, optionally, a fork you can push to. Without a
fork, you need permission to push feature branches to upstream. You also need the agent CLI
your profile names (`claude`, `codex` or `opencode`), signed in. For the
phone page you also need a [Tailscale](https://tailscale.com) tailnet.

Installing `jj` 0.45.0 or later is optional. With it on the service's `PATH`,
new workstreams work in Jujutsu workspaces; without it, in Git worktrees.
Your repository, feature branch and pull request are plain Git either way (see
[`workspaces`](configuration.md#top-level-configtoml)).

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

## 2. Write `~/.config/osmia/config.toml`

The top-level configuration lives at `~/.config/osmia/config.toml`, and
everything else Osmia keeps lives under one root, `~/.local/share/osmia`; both
honor `XDG_CONFIG_HOME` and `XDG_DATA_HOME`. Every command accepts
`--root PATH` for a self-contained root that holds its own `config.toml`.
Create the top-level file:

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
  Tailscale auth key, or without one and open the login URL from its
  [structured log](running.md#service-log) (`osmia status` also shows it on
  its `Tailnet:` line). The node's state
  is kept in `<root>/tailnet`, so later starts need neither. There is no login
  of Osmia's own: your tailnet's access rules decide who can reach the page and
  make decisions. Leave `tailnet` out to use the command line alone.
  See [reach Osmia from your phone](running.md#reach-osmia-from-your-phone).
- **`notify.webhook`** is optional. With it set, every decision that opens in
  your inbox, and the daily budget pause, is posted once as plain text to that
  `http` or `https` URL, such as an [ntfy](https://ntfy.sh) topic, with the
  tailnet address to open. The URL is shown in `/v1/config`, so avoid one that
  embeds a secret you do not want there. See
  [notifications](running.md#notifications).

## 3. Run the service

```sh
osmia doctor
osmia serve
```

`osmia doctor` checks the root, the configuration, the tools Osmia runs and
each project's clone, remotes and GitHub access, and prints how to fix what it
finds; see [`doctor`](cli.md#doctor). Run it whenever `osmia serve` fails.

The service runs in the foreground; leave it running. In another terminal:

```sh
osmia status
curl --unix-socket ~/.local/share/osmia/osmia.sock http://localhost/v1/health
```

`osmia status` reports the service healthy, `Project: none configured` and,
with `listen.tailnet` set, a `Tailnet:` line that reads `up` once the node has
logged in. `/v1/health` returns the service name, API version and
`build.version` and `build.commit`, the ones `osmia --version` printed. A
restart-required or reload-required setting appears among the diagnostics; after
editing the file, `osmia reload` applies what it can
([reload](running.md#reload)).

## 4. Add a project and write its charter

```sh
osmia project add myproject --upstream owner/repo --fork you/repo --clone ~/src/repo
```

For a repository you can push to directly, omit `--fork`:

```sh
osmia project add myproject --upstream you/repo --clone ~/src/repo
```

An `origin` remote pointing to `you/repo` is sufficient. Delivery pushes an
Osmia feature branch and opens a pull request against the configured base
branch; it does not push to that base branch or merge the request.

The command prints the project ID (`p_…`). The service creates the project's
trace, `<root>/projects/<project-id>/trace.db`, outside your clone, seeds
a map of the repository's subsystems, and asks the librarian for the first
knowledge-base extraction. `osmia status` shows the extraction
`succeeded` when it has finished. Registration never writes to your clone; see
[project registration](configuration.md#project-registration).

Hand-in is refused until the project has a charter with at least one rule. The
charter is your rules as a contributor to this project. Open it in your editor
(`$VISUAL`, or else `$EDITOR`):

```sh
osmia edit charter p_…
```

and write numbered rules under the template's headings:

```markdown
1. Every bug fix comes with a test that fails without it.
2. Run the full suite before opening a pull request.
```

Save and quit. The command records your edit as the next charter revision and
prints its number; there is nothing further to ratify, and `osmia status` then
shows `Charter: ready (2 rules, revision 2)`. To write the charter from a file
instead, use `osmia edit charter p_… --from charter.md`; the project pane's
charter editor does the same. Rules later proposed by the chief of
staff are decided with `osmia charter`. See [charter](charter.md) for the
format.

To add another repo and list the project's repos:

```sh
osmia project add-repo p_… --upstream owner/other --clone ~/src/other
osmia project repos p_…
```

Its repo configuration and KB are separate; the charter is shared. The project
pane offers the same creation and repo-add actions. Existing one-repo projects
are wrapped automatically on startup, retaining IDs, charter and history;
existing exploratory sessions stay outside workgroups.

## 5. Start a workgroup and decide

Write the feature down in a Markdown file, or point at a GitHub issue URL, and
hand it in with the project ID:

```sh
osmia handin p_… design.md
```

The command creates an initial workstream with the document as its brief
and prints its ID (`w_…`). With one active repo, it stays a one-workstream
workgroup. With several active repos, the architect selects the affected repos;
the service validates that selection and creates one member per selected repo.
A single selected repo gets the ordinary packet; several get one ratification
covering the project spec, graph and slices. The original conversation continues
as the group's shared chief-of-staff thread. No design session or extra graph
approval is required. Add `--skip-debate` for small
work: the committee does not debate it and the draft waits for your ratification.
Follow progress with `osmia status <workstream-id>`, or on the page.

If you want to discuss the design first, open a project design session:

```sh
osmia design open p_…
osmia design send w_… "Design resumable uploads across both repos."
osmia design read w_…
osmia design start w_…
```

The design agent reads every repo but cannot change code, use version control,
access delivery credentials or start work. It asks only material readiness
questions with recommendations and lists other inferences as collapsed,
overridable assumptions. Once it records a brief, one owner start hands it to
the architect; the brief wins over its reference transcript. The architect
chooses affected repos, and Go allocates members without another owner step.
For several repos the packet includes the project spec, contracts, graph and
per-repo slices and plans, all covered by one ratification. The actual design
conversation continues as the shared chief's thread for status, escalations,
amend and cancel. See [design sessions](cli.md#design-sessions).

The architect drafts a spec and a plan of units, the committee debates them in
the shed, and the chief of staff presents them to you. From then on Osmia stops
at every decision that is yours and waits. Each is an **inbox entry**, and you
can answer it from either place:

- **The phone page.** Open `http://osmia/` (or the node's MagicDNS name). The
  inbox lists every open decision with its question, the options, the
  recommendation and what waits on it, and each card carries its own controls.
  The page follows the service as it works, so a new decision appears without a
  reload. See the [web page](running.md#web-page).
- **The command line.** `osmia inbox` lists the same entries, each with the
  command that answers it.

The decisions, in the order a workstream meets them:

| Decision | Page | Command |
| --- | --- | --- |
| Ratify the spec and plan (or rule on an objection, or ask for a redraft) | ratification card | `osmia ratify <workstream-id>`, `osmia shed …` |
| A question a worker escalated | escalation card: answer, accept the recommendation or use an option | `osmia answer <number> "ruling"` or `--accept` |
| An amendment to the sealed spec or plan | amendment card | `osmia amendment <workstream-id> <n> approve\|reject\|round\|overrule [note]` |
| A contested unit the chief of staff raised to you | contested card, with its findings | `osmia contested <workstream-id> <unit> review\|revise "note"` |
| Approve delivery | delivery card | `osmia delivery <workstream-id>`, then `osmia approve <workstream-id>` |

Publishing needs `GITHUB_TOKEN` in the environment of `osmia serve`, set to a
token that can open pull requests on the upstream. Without it the service
pushes nothing and holds the publication in the inbox until you restart it with
the token. A publication that keeps failing, such as when GitHub refuses the
token, also shows in the inbox with its last failure. It takes no answer: fix the cause and
the next attempt publishes. Attempts wait longer after each failure, up to five
minutes apart.

Ratifying seals the spec and plan: the service fetches upstream, creates the
feature branch in a worktree or Jujutsu workspace of your clone, and masons
build the units, each reviewed and landed in order. You can talk to the workgroup's shared chief of staff
meanwhile with `osmia send <workstream-id> "…"` or on the page, and hold work
with `osmia pause`. The conversation also shows what the chief of staff did on
your behalf, such as resolving a contested unit or moving a stuck one. You can
move a stuck unit yourself with
`osmia move <workstream-id> <unit> <state> "note"`.

When every unit has landed and the final review is done, the workstream's
delivery entry opens. Read the final report's findings and the drafted pull
request description; on the page you can edit the description before
approving. Approving a report with blocking findings accepts them as they are;
otherwise the chief of staff plans the work that resolves them and a new final
review follows. Approve
it on the page, or with `osmia approve <workstream-id> [description-file]`. The
service then pushes the branch to your repository or fork and opens the pull request against
the upstream itself, with your description; no agent pushes. `osmia delivery
<workstream-id>` shows the pull request it was delivered as, and the workstream
records workflow state `delivered`. The project pane shows **PR open** until
a detected or owner-recorded merge, then **delivered (merged)**. The owner
merges the PR; Osmia never merges or pushes a base branch.

Each PR needs its own owner approval of its description and commit message and
is signed as the owner. This covers drafts, updates and marking ready; there
is no workgroup-wide publication approval. A merge-order dependent publishes
a draft linking the upstream PR, then updates to the merged upstream commit,
repairs within its slice, re-runs checks and final review and asks for fresh
delivery approval before becoming ready. Merge is the only release signal.
Expected waits appear in status without an inbox entry or notification.

Resolution retires each member's own documents and workspaces from active
context. The shared project spec stays active for the chief and unresolved
members until all members resolve (merged or cancelled). Trace history at both
levels is retained until owner archive. The same lifetime applies to one-member
workgroups. A ready or closed-without-merge PR remains unresolved; archive and
cleanup supply no merge evidence. You can request amend or cancel in the
shared chat; approve one impact summary, and at cancellation choose whether
open PRs are closed or left as drafts. Merged work is never reverted.

## Where to go next

- [Command line](cli.md) for every command and flag.
- [Configuration](configuration.md) for profiles, sandboxes, capacity and budgets.
- [Running the service](running.md) for the web page, phone access, reload and notifications.
- [Charter](charter.md) for writing a project's rules.
- [Hearsay](hearsay.md) for optional memory.
