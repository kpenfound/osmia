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
The socket and any web listener keep serving beside it.

The node's state, including its identity and keys, lives in `<root>/tailnet`,
mode 0700, outside every target repository. On first run the node needs to log
in. With `TS_AUTHKEY` set in the service's environment it uses that auth key;
without one it prints a login URL to stderr every few seconds until someone
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
   auth key, or start it without one and open the login URL it prints to
   stderr and that `osmia status` shows as `Tailnet:`. The node's state is kept
   in `<root>/tailnet`, so later starts need neither.
3. On a phone or laptop signed in to the same tailnet, open
   `http://osmia/` (or the node's MagicDNS name).

Any device your tailnet's access rules let reach the node can read everything
and make owner decisions, with no login of Osmia's own. Restrict who reaches
the node with tailnet access rules. The connection is plain HTTP on port 80
inside the tailnet's encrypted transport.

## Web page

The service serves one page at `/` on every listener. It uses the same API as
the command line, so an action on the page is validated, recorded and
announced as the matching command's is. It fits phone and laptop widths and
updates live from the service's event stream.

The page is laid out like a chat client:

- The header shows whether the page is live, the slots in use per role, the
  number of pauses in force, a button for a new workstream and a settings
  menu. The slots open to each role's use and the work waiting for a slot and
  why. The pauses open to every pause with who set it (you, the daily budget
  or a provider usage limit), why and when, where you resume one or pause the
  factory, a project or a workstream, softly or hard.
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
  of staff's recommendation. A ratification also shows the dissent record; a
  delivery shows the final report's criteria with their evidence or gaps.
  The Documents tab reads and edits the draft spec and plan, the dependency,
  and the debate actions; the Trace tab walks the workstream's record. The
  `…` menu pauses the workstream or jumps to its dependency, debate actions
  or abandonment. It also archives a delivered or abandoned workstream,
  abandons and archives one in progress, and unarchives an archived one.
  Archiving deletes nothing; see [the command line](cli.md).
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
- Approve a delivery, editing its pull request description first if you want.
- Send a message in a workstream's conversation. Enter sends where there is a
  keyboard; Shift+Enter starts a new line.
- Pause or resume the factory, a project or a workstream, softly or hard.
- Set or clear a project's priority order and each role's profile.
- Register a project, read and edit its charter, remove it from active work,
  and hand in new work.
- Abandon a workstream, archive a finished one and unarchive it.
- Reload the configuration.

Submitting a feed action, such as a ruling on a contested unit or ratifying a
proposal, removes its card from the feed right away, before the service has
answered. The card stays off the feed through any refresh that still lists the
action, since the service may not have gotten to it yet, and comes back on
its own, actionable again, if the service is still listing it two minutes
after acknowledging the submission, so it can never disappear for good.
Submitting one card never hides or changes any other. If a submission fails,
the card comes back at once with the error shown and what you typed still in
it. Every form that submits an action, including new project, archive and the
feed's own action forms, clears back to its defaults once it succeeds; a
failed submission leaves it exactly as you left it, with the error shown.

An answer applies to the entry as the page showed it. If the packet, report or
entry changed since, the answer is refused rather than applied to something
you did not read, and the page shows the current version. The reload control
is lit while a reload has something to apply.

When the connection drops, the page shows that it is reconnecting and retries
with a growing delay up to 10 seconds, immediately when the browser comes back
online or the page becomes visible again.

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

The root, `listen.socket`, `listen.web` and `listen.tailnet` keep their loaded
values until the service restarts; the reload lists the ones the files change
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

The `Open` line appears only with `listen.tailnet` set. Any `2xx` response
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

## API reference

[`openapi.json`](openapi.json) is an OpenAPI 3.1 description of every endpoint
under `/v1`, with its path parameters, request body, response and error shape.
Open it in any OpenAPI viewer, such as Swagger UI, Redoc or Scalar, or generate
a client from it. The command line's `--json` output is built from these
responses; see the [command line](cli.md) for each command's shape.

Every failure returns `{"error": {"code": ..., "message": ...}}`. `GET
/v1/events` is a `text/event-stream`: each frame names the event's kind and
carries the event as JSON, and the first event of every stream is `resync`,
which asks the client to read every view again.
