# Running the service

`osmia serve` runs one service that serves its API and web page on a Unix
socket, and optionally on a loopback address and your tailnet. The
[command line](cli.md) talks to it over the socket; the page and your phone
use the other listeners. [Configuration](configuration.md) describes the
settings named here, and the [API reference](#api-reference) describes every
endpoint.

## Web listener

When `listen.web` is set, the service also binds that loopback TCP address and
serves the same API and page on it. A browser on the same machine opens the
page at `http://<listen.web>/`. A bind failure, such as an address in use, is a
startup error that names `listen.web`.

The loopback interface is the boundary: there is no authentication, and any
local user or process that can connect may use the API. Two checks keep a
browser from reaching it on another site's behalf. The `Host` header must name
`localhost` or a loopback address, which refuses DNS rebinding, and requests
other than `GET` and `HEAD` must send `Content-Type: application/json`, which a
cross-site form cannot send. A refused request gets `forbidden`. The socket
applies neither check.

## Tailnet listener

When `listen.tailnet` is set, the service joins your tailnet through embedded
Tailscale under that hostname and serves the same API and page on the node's
port 80, so a phone or laptop on the tailnet reaches it at `http://<hostname>/`.
HTTPS also serves on port 443 by default, using a Tailscale-issued certificate
for the full MagicDNS name: `https://<host>.<tailnet>.ts.net/`. The certificate
covers that full name, so use it instead of the short hostname. The socket and
any web listener keep serving beside it. HTTP on port 80 remains available
and does not redirect.

Enable **HTTPS certificates** in the tailnet admin console. Issuing a
certificate puts the node's `*.ts.net` name in public Certificate Transparency
logs: the name becomes public, but access does not. To opt out, set
`listen.tailnet_https = false` under `[listen]`. It defaults to `true`, and
changing it requires restarting the service, just like `listen.tailnet`.

If certificates are disabled, certificate retrieval fails, or the HTTPS
listener stops, the service stays up and HTTP keeps serving. HTTPS is reported
as unavailable with a reason. HTTPS belongs to its node: replacing the node or
shutting down closes that node's TLS listener.

The node's state, including its identity and keys, lives in `<root>/tailnet`,
mode 0700, outside every target repository. On first run the node needs to log
in. With `TS_AUTHKEY` set in the service's environment it uses that auth key;
without one it logs a message every few seconds, in the [service's structured
log](#service-log), with the login URL as its `login_url` field, until someone
opens it and approves the node. Later starts reuse the stored node state and
need no key. The auth key is read only from the environment, never written to
configuration or runtime state, and agent sessions never inherit the
service's environment. Tailscale may rename the node, for example to
`osmia-1` when the hostname is already taken on the tailnet.

Tailnet membership is the boundary: there is no in-app authentication, and any
device the tailnet's access rules let reach the node may use the API. The same
browser checks as on the web listener apply; the `Host` header must be an IP
address, the configured hostname, or a name the node holds on the tailnet,
such as its MagicDNS name.

The tailnet never takes the service down. A node that cannot join at startup,
whether for a missing auth key, a pending login, an unreachable control server
or an unusable state directory, leaves `osmia serve` running, and the service
tries to join again every few seconds. A tailnet that drops while the service
runs is joined again the same way. The local command line keeps working over
the socket throughout.

### Tailnet state

`osmia status` prints the node's state as a `Tailnet:` line when
`listen.tailnet` is set:

| State | Meaning |
|---|---|
| `up` | The node is on the tailnet and the listener serves. |
| `connecting` | The node is starting or reaching the control server. |
| `needs_login` | The node must be approved. The login URL is shown when Tailscale gives one; otherwise the reason says the node awaits approval by a tailnet admin. |
| `down` | There is no working node. The reason says why, such as the error from the last join. |

`osmia status` also prints a `Tailnet HTTPS:` line, and the API's tailnet
status includes `https_state`, `https_reason` and `https_address`. HTTPS is
`up` with the full HTTPS address, `off` with the configuration reason, or
`unavailable` with the failure reason. The address is present only when up.

### Reach Osmia from your phone

[Getting started](getting-started.md#2-write-configosmiaconfigtoml) puts this setup in
the order of a first run.

1. Set the hostname the node takes on your tailnet in the top-level
   configuration file, and restart `osmia serve` (a reload keeps the node
   already joined and reports `listen.tailnet` as requiring a restart):

   ```toml
   [listen]
   tailnet = "osmia"
   ```

2. Log the node in. Start the service with `TS_AUTHKEY` set to a Tailscale
   auth key, or start it without one and open the login URL from its
   [structured log](#service-log) or from `osmia status`'s `Tailnet:` line.
   The node's state is kept in `<root>/tailnet`, so later starts need neither.
3. On a phone or laptop signed in to the same tailnet, open
   `http://osmia/` (or the node's MagicDNS name).

Any device your tailnet's access rules let reach the node can read everything
and make owner decisions, with no login of Osmia's own. Restrict who reaches
the node with tailnet access rules. Use the full HTTPS address from
`osmia status` when available. Plain HTTP on port 80 also remains served
inside the tailnet's encrypted transport.

## Web page

The service serves one page at `/` on every listener. It uses the same API as
the command line, so an action on the page is validated, recorded and
announced as the matching command's is. It fits phone and laptop widths and
updates live from the service's event stream. The browser tab shows the bee
emoji (🐝) as the page's favicon.

Action outcomes and busy controls belong to the design session, exploratory
session or workstream where the action was taken. Switching away and back
shows that instance's latest outcome, including a response received while
another instance was open. Create forms start with an empty outcome when
opened or switched to another project.

The project pane is the main entry point. Each one-repo project shows its workgroups and member workstreams with named waits. **Create project** registers a repo with its own charter. **Hand in a feature** selects active projects through a multi-select; the first is home for the shared chief and conversation. **Open a design session** opens a read-only chat whose brief starts with that project. Ask the agent to change the project list; inferred changes need your agreement.

Above **Start workgroup from brief**, the read-only **Brief** panel shows the latest revision, project names with home marked and text collapsed by default. It updates live and Start sends the displayed revision. A stale brief shows a conflict and refreshes the panel without allocating work. One member per selected project is created without an architect selection turn. The list stays fixed until ratification; changing it requires abandoning and handing in again. One ratification covers the workgroup spec, graph and slices; the original conversation continues as the shared chief, at the session's project even when it is not a member.

Member labels are planning, ready, building, paused, blocked (naming the edge
or merge), PR open, delivered (merged) and cancelled. Expected merge waits
create no inbox entry or notification. The workstream graph appears only for
several members or recorded edges. Exploratory sessions appear under their
project outside workgroups and retain their direct writable-worktree chat.
The **Osmia** heading returns to the project pane. **Create project**, **Hand in a feature**, **Open a design session** and **Open an exploratory session** use shared forms from the header's **+**, sidebar and project cards. Card actions preselect that project; global actions ask for it. A cross-project workgroup appears under each member's project card and sidebar group row. Member labels are project names only.

**Project management**, from the cog menu or a project card, shows the chosen
project's own charter and whether it is ready for hand-in. **Read charter**
loads the current revision; **Save charter** saves against that revision. An
untouched editor follows another client's save live. Unsaved edits keep their
text and revision, with a notice that the charter changed elsewhere; a stale
save is refused and keeps the draft and error until **Read charter** explicitly
loads the latest text.

Project management shows the project's name, upstream, optional fork, clone, base branch and knowledge extraction state. **Extract knowledge**, **Rebase workstreams** and removal affect that project. Removing it is refused while any member in any workgroup is unresolved. Project changes, charter saves and extraction state arrive live, including API changes. Pause, capacity caps and `git_operations` affect only that project's member, subject to graph edges.

**Delivered (merged)** requires an owner merge record or a recorded merged
outcome matching the current publication's PR, live or archived. An open,
unmerged PR shows **PR open**; ready, draft and closed-without-merge PRs remain
unresolved. An archived member without pinned merge or cancel evidence shows
**blocked**, with a wait explaining that the workgroup remains unresolved.
The workgroup spec stays active until every member is merged or cancelled,
including in one-member workgroups. Trace history remains until owner archive.

Selecting a member opens its existing view, with a direct workstream link
that survives reload; an original design link follows the adopted conversation
after start. Shared questions appear once in the project overview and can be
answered from either member, preserving the original entry and project pin.
Owner-approved archive and eligible cleanup of a terminal member do not remove
the survivor's shared conversation or active workgroup spec. Unexplained missing
live history yields a truthful diagnostic. Archive does not count as merge.

The member view is laid out like a chat client:

- The header shows whether the page is live, the slots in use per role, the
  number of pauses in force, a button for a new workstream and a settings
  menu. The slots open to each role's use and the work waiting for a slot and
  why. Check slots appear alongside mason, reviewer and committee slots: the
  factory shares `capacity.checks` (default 2) across every project's unit,
  final-review and session checks. Waiting entries name the workstream and unit,
  final review number or session. Counts and waits update as runs start and
  finish. Check slots are separate from agent and per-workstream limits;
  waiting does not consume the check timeout. Reload applies a changed limit to
  new admissions while checks already running finish. The pauses open to every
  pause with who set it (you, the daily budget or a provider usage limit), why
  and when, where you resume one or pause the
  factory, a project or a workstream, softly or hard. A backend activity
  button sits beside them, marked whenever an operation is in flight and
  distinctly when one is blocked or has failed; opening it lists every entry
  the backend activity record holds, including an entry that is blocked or
  failed, with its kind, the workstream or project it concerns, its phase and
  progress, and any failure.
- The list on the left holds every workstream, ordered by last activity,
  newest first: a state transition, a trace entry, a thread message, a
  question, an answer, an owner decision or a delivery outcome. Opening,
  selecting or refreshing a workstream never moves it; only new activity
  does, the next time the list loads. Each row shows its goal, project and
  state, how many inbox entries wait on it, a dot when it changed since you
  last looked at it, whether it is working and whether it is paused. Archived
  workstreams are kept in a collapsed Archived group at the bottom. At phone
  widths the list is behind the button at the top left and covers the page
  until you pick a workstream or close it.
- The main area shows the selected workstream: its goal, state, project and
  units counted by state, and its feed. The feed lists, in the order they
  happened, your messages and the chief of staff's answers and actions, each
  status the chief of staff wrote, each agent session with its role, profile
  and outcome, and each change of the workstream's or a unit's state. The
  latest status's attention note is highlighted; a finished session opens to
  its report or failure. Its inbox entries sit below the feed, above the
  message field: what each asks, what waits on it, its options and the chief
  of staff's recommendation. A ratification also shows the objections the
  chief of staff escalated and your own, with the ones it dismissed and why
  folded below them; a
  delivery shows the final report's findings and the units serving each
  criterion.
  The Documents tab reads and edits the draft spec and plan before
  ratification, each save a new revision as with `osmia edit`, the dependency,
  and the debate actions; the Trace tab walks the workstream's record. The
  `…` menu pauses the workstream or jumps to its dependency, debate actions
  or abandonment. It also archives a delivered or abandoned workstream,
  abandons and archives one in progress. Archive keeps the title and a few
  other facts in the archived list, then permanently deletes the workstream's
  own trace, including full check output. It cannot be undone; see
  [the command line](cli.md).
- The settings menu opens new projects, projects and their charters, the
  priority order, each role's profile with its provider's usage today, and
  the loaded configuration with any [disk drift](#disk-drift) and settings
  that need a restart. The settings button is marked while a reload has
  something to apply.

Which workstream is selected and what you have seen are kept in the browser,
so another browser has its own. The list's order comes from the service and
is the same in every browser.

From the page you can:

- Answer an escalation, accept its quick reply, or pick one of its options.
- Ratify a plan, sustain or overrule an objection, or ask for a redraft.
- Decide a contested unit, an amendment or a charter proposal.
- Accept the final review and approve each workstream's delivery, seeing its
  PR title and editing its description and commit message. Draft publication,
  updates and marking ready each require that member's own fresh approval and
  service-owned signing; there is no group-wide publication approval.
- Opening a workgroup, workstream, design or exploratory conversation shows
  its newest entry, including direct links and returning to Feed. New entries
  scroll the shown conversation to its end even while reading older history.
  Refreshes without new entries keep your scroll position; your composer draft
  and keyboard focus stay in place.
- Send a message in a workstream's conversation. Enter sends where there is a
  keyboard; Shift+Enter starts a new line.
- Pause or resume the factory, a project or a workstream, softly or hard.
- Set or clear a project's priority order and each role's profile.
- Register one repo per project, read and edit its charter (each save a
  new revision, as with `osmia edit charter`), remove eligible projects
  from active work and select projects when handing in new work.
- Open a project design chat, review readiness questions and collapsed
  assumptions, then start a workgroup from its brief.
- Request amend or cancel in the shared conversation and approve its single
  impact summary, choosing whether cancellation closes open PRs or leaves drafts.
- Abandon a workstream or permanently archive a finished one.
- Reload the configuration.

Submitting a feed action, such as a ruling on a contested unit or ratifying a
proposal, removes its card from the feed right away, before the service has
answered. The card stays off the feed through any refresh that still lists the
action, since the service may not have gotten to it yet, and comes back on
its own, actionable again, if the service is still listing it two minutes
after acknowledging the submission, so it can never disappear for good.
Submitting one card never hides or changes any other. If a submission fails,
the card comes back at once with the error shown and what you typed still in
it. A submission the backend cannot admit right now, because an operation it
needs is still in flight, shows distinctly from one the service rejects
outright: the former names the blocking operation and is
worth retrying as is, the latter needs the input or the decision changed.
Every form that submits an action, including new project, archive and the
feed's own action forms, clears back to its defaults once it succeeds; a
failed submission leaves it exactly as you left it, with the error shown.

An answer applies to the entry as the page showed it. If the packet, report or
entry changed since, the answer is refused rather than applied to something
you did not read, and the page shows the current version. The reload control
is lit while a reload has something to apply.

When the connection drops, the page shows that it is reconnecting and retries
with a growing delay up to 10 seconds, immediately when the browser comes back
online or the page becomes visible again.

## Sessions

A session is a lighter-weight workstream for exploratory work: a direct chat
with one agent on its own copy of a registered project's code, without the
rest of the factory around it. There is no shed, no spec, no plan, no units,
no reviewer and no committee, and no chief of staff speaks for it; opening,
listing and reading a session appear alongside your other workstreams, but a
session is never scheduled for the shed, a mason, a reviewer, a check run
before review, a dependency refresh or delivery.

You open a session in a registered project with `osmia session open
<project> --profile <name> [--title ...]` or the matching API call; see the
[command line](cli.md) for the exact form. The profile is one the service
already runs mason turns on for that project, and an unknown project or an
unknown or missing profile is refused. The session starts a worktree of its
own at the project's default branch head, on a branch and profile that stay
fixed for its whole life: nothing changes a session's profile once it is
open, and working on another one means opening another session.

The agent's own chat happens only through the web page or the API, never the
command line. Its context carries over between messages the way a direct
Claude or Codex session's does: the first message starts the agent's own
model session, and every later one resumes it rather than replaying
everything said before, so the service only has to add what changed since
the agent's last turn, such as a commit you made or a check you ran. If
the runtime ever reports that model session unresumable, the next message
starts a fresh one seeded with the conversation's recent history instead,
and the chat says plainly that its context was rebuilt. Only one of the
agent's turns runs at a time; a message sent while one is already running
is refused as busy, and you can stop a running turn, which keeps whatever
changes it already made and lets the next message pick up from there.

Because the agent itself cannot touch version control or hold delivery
credentials, review your session's work and move it along yourself:

- **Changes** shows the worktree's uncommitted edits against the session's
  head, and the commits already made against its starting commit.
- **Commit** `osmia session commit <session> "message"` stages every
  current change onto the session's branch under your message. It is
  refused while the agent is mid-turn, on an empty message, when there is
  nothing to commit, or once you have ended the session.
- **Check** `osmia session check <session>` checks the session's latest
  commit. With the Jev boost on, it runs the same smart selection of checks
  a unit's own check run uses for the files that changed since the
  session's starting commit; with it off, it runs the full `dagger check`
  the service runs before every review. Either way it tells you whether the
  run was smart or full, its status and exit code, and keeps its output.
  Only one check runs at a time.
- **Push** `osmia session push <session>` sends the session's branch to the
  project's remote under its own branch name, never the default branch,
  never forced and never opening a pull request on its own. A push is
  refused if the remote branch has moved on somewhere the session's own
  history does not reach.
- **End** `osmia session end <session>` is refused while the agent is
  mid-turn. Once ended, the session refuses further messages, commits,
  checks and pushes; its worktree and the agent's stored context are
  released, while its branch, its commits, its chat and its trace all stay
  readable, and it is listed as ended.

### Destroying sessions

`osmia session destroy <session-id> [--json]` permanently removes an exploratory
session, whether open or ended; you do not need to end it first.
`osmia design destroy <design-id> [--json]` removes a design session whose
marker is still `open`, before you start a workgroup from its brief.

Destroy removes the session's dedicated trace, including its conversation,
thread, documents and check records, any worktree, and stored model-session
and turn state under the Osmia root. For an exploratory session it also deletes
the local `osmia/session/<id>` branch, including unpushed commits. For a design
session it also removes its one-workstream workgroup from the project's
`workgroups.json`. The session disappears from lists, status and the sidebar;
reading it by ID reports not found.

This cannot be undone. Destroy keeps no archive record, retained title or
tombstone. Pushed branches stay on the remote: destroy never pushes, deletes
remote branches or changes pull requests or issues. If interrupted, removal
finishes automatically after the service restarts; the session stays hidden
while removal is pending.

Destroy is refused without changing anything while a turn is running or being
started, while a session commit, check, push or end is still in progress
(including the external check and push), or while a design's workgroup start
is in progress. A design that has already started a workgroup cannot be
destroyed. Ordinary workstreams, workgroups and the librarian workstream are
also refused; their existing abandon/archive controls still apply. Wait for
busy work to finish before retrying. An unknown or already destroyed ID reports
not found. The CLI prints the service's refusal message and exits non-zero.

## Reload

`osmia reload` (or the page's reload control) reads the top-level
`config.toml` and the `config.toml` of every project in `active_projects`, and
validates them together. If any file fails, nothing changes: the loaded
configuration stays in force and the error names the file, the field and why.
`osmia config` shows that failure until a reload succeeds.

When every file passes, the new configuration replaces the loaded one in one
step. Profiles, role bindings and sandboxes, capacity, budgets, shed and mason
limits, the webhook and project settings apply to what the service decides
next. A turn already running finishes on the configuration it started with.
Projects added to `active_projects` start; projects removed from it stop
taking new work and drain what is in flight before the reload returns.

The root, `listen.socket`, `listen.web`, `listen.tailnet` and
`listen.tailnet_https` keep their loaded values until the service restarts; the reload lists the ones the files change
as requiring a restart.

Pauses, priorities and profile overrides are runtime state, not
configuration, and a reload leaves them as they were. An override naming a
profile the new configuration lacks stops applying, with a diagnostic, and
applies again once a reload brings the profile back.

### Disk drift

`osmia config` and the page compare the configuration files on disk with the
loaded configuration, so you can see whether a reload has something to apply
without applying it. Each file is `unchanged`, `changed` when a setting it
holds differs from the loaded one, or `invalid` when it cannot be read or does
not validate, with the reason. A comment or formatting change leaves a file
`unchanged`.

## Notifications

With `notify.webhook` set, the service posts every new inbox entry to the
webhook once, as plain text you can act on without the page open:

```text
Osmia needs your decision.
Project: <project-id>
Workstream: <workstream-id>
Kind: <escalation, ratification, contested, amendment, delivery or publication>
Question: <the entry's question on one line>
Recommendation: <the entry's recommendation on one line, when it has one>
Open: http://<tailnet-name>/
```

A failing publication opens with `Osmia cannot publish a pull request.`
instead, since it takes no decision.

The webhook also receives one post when the daily budget pauses dispatch:

```text
Osmia paused dispatch: the daily budget is reached.
Project: <project-id>
Spend: USD <known spend>[ or more]
Limit: USD <budget.per_day>
Clears: <next local midnight, RFC 3339>
Open: http://<tailnet-name>/
```

The `Open` line appears only with `listen.tailnet` set. When tailnet HTTPS is
up it uses the full `https://<host>.<tailnet>.ts.net/` address; otherwise it uses
the HTTP address shown above. Any `2xx` response
counts as delivered. `or more` follows the spend while some attempts have
unknown cost. A per-unit budget crossing files an amendment request, which is
an inbox entry and is posted as one. A provider usage-limit pause is not
posted.

Each entry or pause is posted once, including across restarts. A new revision
of a packet or final report, or a unit contested again, counts as a new entry.
Entries already open when notifications are turned on are not posted, and an
entry decided before its post is sent is dropped. A failed post is retried
with a growing delay and given up after 5 attempts. A reload applies a set,
changed or removed webhook.

While a notification problem stands, `osmia status` and `osmia config` show a
`notify` diagnostic describing it. They never show the webhook's URL.

## Service log

While `osmia serve` runs, foreground or detached, everything it writes to its
standard output and standard error is a structured log record: one JSON
object per line, with a `time`, a `level` and a `msg`, and the record's
details as named fields rather than only in the message text. In detached
mode that output goes to `<root>/service.log`, so that file is JSON lines
too. This covers the service's own messages, embedded Tailscale's (including
the tailnet login URL, as the `login_url` field of a record whose `msg` is
`tailnet message`) and any subprocess output that reaches the service's own
output.

A failure of the service process, at startup or after it started, is one
`"level":"ERROR"` record on that same stream, with the same credential
redaction and guidance `osmia doctor` points to as before, and the same exit
code. The `osmia serve --detach` launcher is not the service process: its own
"ready" line and, when the detached service does not become ready, its own
failure message stay human-readable text on the terminal that started it,
never log records.

Filter the stream with any JSON-lines tool, for example `jq`:

```sh
osmia serve 2>&1 | jq -c 'select(.level == "ERROR")'
```

### GitHub activity log

Every GitHub touch point the service makes produces one record once it
completes, whether it succeeds or fails: a GitHub REST API request (finding,
opening or updating a pull request; fetching a handed-in issue; the commit
and content requests of the `read_remote_file` tool) or a git remote
operation against a project's upstream or fork (resolving the remote,
fetching, looking up a remote branch, pushing). Every such record carries
`"activity":"github"`, the one field value that marks it as GitHub activity;
no other kind of record carries it. Filter on it to see every GitHub touch
point and nothing else:

```sh
osmia serve 2>&1 | jq -c 'select(.activity == "github")'
```

Each GitHub activity record also carries:

- `action`: what was done, such as `pulls.find`, `issues.fetch` or
  `workspace.push`.
- `access`: `read` or `write`.
- `repository`, for a REST request, or `remote` and `branch`, for a git
  remote operation.
- `method` and `path`, for a REST request.
- `status`, the REST response status, or `no_response` when none arrived.
- `commits`, for a git remote operation that involved any, such as the pushed
  commit and the expected remote commit.
- `outcome` and `duration`.
- `error`, when the touch point failed; redacted of credentials, never the
  text of an issue, pull request, remote file or commit message.
- `project`, and `workstream` and `unit` when there are any, naming what the
  request was made for, when the service knows them.

A failed request, including an unexpected status, a transport failure or a
rejected push, logs at `ERROR`; a successful one logs at `INFO`.

### Factory action log

Beside the GitHub activity log, the service logs its own internal actions,
each carrying `project`, and `workstream` and `unit` when there are any:

- `service started` (`version`, `commit`, `root`, `listeners`) and
  `service stopped` (`reason`).
- `project started`, `project draining` and `project stopped` (`reason`).
- `reload succeeded` (`digest`, `restart_required`, `added`, `removed`) and
  `reload failed` (`error`).
- `api request handled` (`listener`, `method`, `path`, `status`,
  `duration`), with no request or response body.
- `operation started`, then `operation finished` (`outcome`, `duration`) or
  `operation failed` (`error`, `duration`): the start and outcome of an
  operation the service runs, such as a draft, a debate round, sealing,
  building, checks, landing, rebasing, publication or cleanup, named by its
  `action` and `boundary`.
- `agent turn dispatched`, then `agent turn finished` (`outcome`,
  `duration`): a queued turn handed to an agent, named by its `role`,
  `profile` and `unit`, with no prompt or transcript.
- `pause set` (`scope`, `mode`, `reason`) and `pause cleared` (`scope`).
- `tailnet joined`, a join failure and `tailnet listener stopped; rejoining`:
  the tailnet's own state changes, as distinct from the `tailnet message`
  records embedded Tailscale's own logging produces.
- `notification post attempt succeeded` (`kind`, `duration`) and
  `notification post attempt failed` (`kind`, `attempts`, `gave_up`,
  `duration`, `error`): never the webhook URL.
- `reconciliation pass hook failed` (`hook`, `error`): a schedule hook that
  stopped a reconciliation pass.

A failed or error-level outcome logs at `ERROR` or `WARN`, higher than the
matching routine record's `INFO`.

## API reference

[`openapi.json`](openapi.json) is an OpenAPI 3.1 description of every endpoint
under `/v1`, with its path parameters, request body, response and error shape.
Open it in any OpenAPI viewer, such as Swagger UI, Redoc or Scalar, or generate
a client from it. The command line's `--json` output is built from these
responses; see the [command line](cli.md) for each command's shape.

`GET /v1/workgroups/{id}/feed` returns the shared chief conversation,
statuses and actions in `entries`, and one current summary per member in
`members`. Each summary includes `workstream`, `repo`, `state`,
`display_state`, unit counts by state in `units`, `wait`, `pr_link`,
`running` and `archived`. Archived cards use pinned archive facts; their
unit counts are empty. Entries about one member include its `workstream`
and `repo`. `POST /v1/workgroups/{id}/conversation` accepts `{"text":"..."}`
and acknowledges the message after it is durable, without a member-address
note. These endpoints support both single-member and multi-member groups.
Authority locks and membership or retirement holds return an error.

For a member of a multi-member workgroup, `GET /v1/feed/{workstream}`
contains only member activity: transitions, worker sessions, checks and final
reviews. Shared chief conversation, statuses, actions and chief sessions
belong to the workgroup feed. Single-member feeds also contain those chief
entries. `GET` and `POST /v1/conversation/{workstream}` keep the member's
conversation address and member note for CLI and API clients.

Project views include the project's single repo in the `repos` API field;
member status includes workgroup membership.
Workstream status separates the stored workflow `state` from `display_state`
and its named `wait`;
publication state `delivered` alone does not establish merge. Runtime archive
records expose `resolution_evidence` (`merged`, `cancelled` or `unresolved`)
and `resolved`, pinned from history before cleanup. Only merge or cancel
evidence resolves an archived member; a missing pin keeps the workgroup and
workgroup spec active with a visible diagnostic. See
[resolution and retention](configuration.md#resolution-retention-and-shared-lifecycle).

Every failure returns `{"error": {"code": ..., "message": ...}}`. `GET
/v1/events` is a `text/event-stream`: each frame names the event's kind and
carries the event as JSON, and the first event of every stream is `resync`,
which asks the client to read every view again.

`GET /v1/health` reports the API as available whenever it answers at all,
separately from the backend, which is `ready`, `not_ready` while startup or
project recovery runs, or `degraded` while a backend operation has failed or
stalled, naming the operation when it is not simply ready. `GET /v1/activity`
lists every operation the backend activity record holds: its kind, the
project and workstream it concerns, its phase, its progress when measurable,
the time of its last progress and any failure. Both answer promptly without
waiting on the trace or on a factory effect in progress, which is what makes
them useful while something else is slow.

Every core read endpoint (`/status` and a workstream's status, `/config`,
`/runtime`, `/inbox`, a conversation, a feed, trace views, and the first read
behind `/events`) answers within a short bound even while a Git operation, a
check, an agent turn, reconciliation or recovery is blocked, because none of
them waits on that work. `/status` alone goes further: when a fresh answer
cannot be assembled within that bound, it falls back to its last snapshot
instead, carrying an `X-Osmia-Stale: true` response header; a client that
cares should retry rather than treat that body as current.

Every owner-action write (hand-in, ratify, a shed action, an amendment
decision, answering an inbox entry, moving a unit, delivery approval,
abandon, pause, priority and archive) answers with one of three outcomes: a
success response once the command is durably
accepted; an error with a `4xx` code and a reason when it is rejected, which
includes a command whose request or revision is now stale against current
state; or `503 unavailable` naming the backend operation in the way when a
bounded wait for something the write needs does not clear in time. None of
the three is ever silently dropped, and resubmitting the same command,
including after a restart, never applies it twice.
