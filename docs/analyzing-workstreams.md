# Analyzing workstream sessions

How to find out, from a factory's own records, why a workstream is stuck,
looping or expensive. The trace records every transition, turn and session,
so a question about what the factory did is answered from its databases, not
from memory or guesses. The method moves from counts to a timeline, from the
timeline to the one pattern that repeats, and from that pattern to the code
that produces it.

## Ground rules

- Read only. Analysis never writes to the root. Open each trace with
  `sqlite3 -readonly`, which reads while the service runs and never waits on
  or blocks its writes. Never open a trace with Osmia's own code while the
  service holds it.
- Inspect Jujutsu workspaces with `jj --ignore-working-copy`; any other `jj`
  command snapshots the working copy and changes the record you are reading.
- Put scripts and intermediate output in a scratch directory, not the repository.
- Keep the factory stopped while a loop is diagnosed. Every pass of a loop costs
  sessions.

## Where the evidence lives

Paths are under the root, `~/.local/share/osmia` by default. Each trace is
one SQLite database, `trace.db`, in its own directory.

| Path | What it answers |
|---|---|
| `runtime.json` | Pauses with their sources, archived workstreams (title, outcome, archive time, delivery link, `resolution_evidence`), profile overrides |
| `projects/<p>/config.toml` | The project's clone path, base branch and landing style |
| `projects/<p>/trace.db` | The project's charter, KB, entity map, role notes and lasting decisions |
| `workstreams/<p>/<w>/trace.db` | The workstream's complete trace |
| `units/<p>/<w>/<u>`, `drifts/<p>/<w>` | Unit and drift resolution workspaces |

A trace database has five tables:

| Table | What it holds |
|---|---|
| `records` | Every record revision: `seq` (commit order), `path`, `schema`, `id`, `revision` and `body`, the record's JSON. `path` is the record's logical path, such as `workstreams/<w>/events.jsonl` |
| `files` | `path` and `content` of the manifests (`project.json`, `workstreams/<w>/workstream.json`: creation time and workspace backend), role notes and inbox numbering |
| `workflow_log` | The workflow's append-only lists: `list` is `transactions` (each with its `events[]` and their `operation`), `deliveries` or `operations` (each attempt's `claim`, `observe`, `effect`, `retry` with `failure` and `retry_at`, and `result`) |
| `workflow_threads` | Each agent's thread by `agent_id`: its queued turns with their `claim`, `attempts` and `completed_at`, and the `request_id` and `response_id` of the turn's records |
| `commits` | One row per write, with its time and message |

The records you will read most, by `schema`:

| `schema` | What it answers |
|---|---|
| `osmia.trace.transition` | Every transition: `subject`, `from`, `to`, `reason`, `actor`, `cause`, `at`, `unit` |
| `osmia.trace.cost` | One cost record per session: `$.entry.Scope` (`Role`, `Unit`, `Thread`, `Turn`), `$.entry.At`, `$.entry.Usage` (`CostUSD`, `Turns`) |
| `osmia.trace.turn-request`, `osmia.trace.turn-response` | Each turn's request (`agent_id`, `actor`, `cause`, `prompt`, `profile`) and response (`result.FinalResponse`, `result.Outcome`) |
| `osmia.trace.status` | The chief of staff's status revisions: `goal`, `note`, `agents` |
| `osmia.trace.document` | Every document revision: `$.path` names the document and `$.content` holds it. The latest revision of a document is its row with the highest `seq`. `tools/<id>.json` is one audit record per tool call (`scope.Role`, `scope.Turn`, `name`); `units/<u>/` holds `report.json`, `review.json`, `rebase.json`, `landing.json` and `checks-<n>.json`; `drift/rebase.json` is the latest drift rebase record: outcome, conflicts, candidate, review count, verdict |

Each repo's librarian has a workstream of its own, `w_` followed by the
first 32 hex digits of `sha256("librarian:<project id>")`. Its workflow holds
the knowledge base extraction and refresh operations.

Here `<p>` is the one-repo project's ID; its charter, KB and settings are its
own. For a cross-project workgroup also inspect
`workgroups/<group-id>/trace.db`: the authoritative workgroup spec, membership, original
shared chief conversation, consolidated questions and approved change journals
are there, outside member traces. Member roles' asking-turn links lead back to
their own traces. A published `delivered` workflow state is not itself a merge;
use a merged outcome matching the current publication's PR or an owner merge
record. Closed without merge and outcomes for earlier PRs remain unresolved.
Archive pins `resolution_evidence` as `merged`, `cancelled` or `unresolved`;
only merged or cancelled evidence can resolve an archived member. An older
`resolved` flag without evidence is re-derived from retained history or held
unresolved. Archived-member identity and cleanup evidence distinguish
retirement from unexplained live absence; retirement alone proves no merge.

Timestamps mix UTC and local offsets. Convert them before you sort or bucket
them.

## 1. Take inventory

List every workstream with its feature state, whether it is archived, its
session count, the span of its events and the goal from its latest status.
Active, high-session workstreams are where to start.

```sh
cd ~/.local/share/osmia/workstreams
for db in */*/trace.db; do
  id=$(basename $(dirname $db))
  sqlite3 -readonly -separator ' ' $db "
    SELECT '$id',
      '[' || coalesce((SELECT json_extract(body, '\$.to') FROM records
        WHERE schema = 'osmia.trace.transition' AND json_extract(body, '\$.subject') = 'feature'
        ORDER BY seq DESC LIMIT 1), '') || ']',
      'sessions=' || (SELECT count(*) FROM records WHERE schema = 'osmia.trace.cost'),
      coalesce((SELECT substr(json_extract(body, '\$.goal'), 1, 80) FROM records
        WHERE schema = 'osmia.trace.status' ORDER BY seq DESC LIMIT 1), '')"
done
```

## 2. Profile one workstream

Break its sessions down by role, by role and unit, and by agent thread, with
cost. The shape usually points at the problem straight away: reviewer
sessions far above mason sessions, or nearly all of a role's sessions on one
or two units, means something repeats.

```sh
W=workstreams/<p>/<w>/trace.db
sqlite3 -readonly $W "
  SELECT printf('%-16s %5d \$%.2f', json_extract(body, '\$.entry.Scope.Role'), count(*),
    sum(json_extract(body, '\$.entry.Usage.CostUSD')))
  FROM records WHERE schema = 'osmia.trace.cost' GROUP BY json_extract(body, '\$.entry.Scope.Role')"
sqlite3 -readonly $W "
  SELECT count(*), json_extract(body, '\$.entry.Scope.Role'), json_extract(body, '\$.entry.Scope.Unit')
  FROM records WHERE schema = 'osmia.trace.cost' GROUP BY 2, 3 ORDER BY 1 DESC"
```

## 3. Build a timeline

Bucket sessions by time and role, and list the feature and unit transitions
beside them. Find the point where the session rate jumps while the states stop
moving. The transitions just before that point are where the stall began.

```sh
sqlite3 -readonly $W "
  SELECT count(*), substr(json_extract(body, '\$.entry.At'), 1, 13), json_extract(body, '\$.entry.Scope.Role')
  FROM records WHERE schema = 'osmia.trace.cost' GROUP BY 2, 3 ORDER BY 2, 3"
sqlite3 -readonly -separator '	' $W "
  SELECT substr(json_extract(body, '\$.at'), 1, 19), json_extract(body, '\$.unit'),
    json_extract(body, '\$.from'), json_extract(body, '\$.to')
  FROM records WHERE schema = 'osmia.trace.transition' AND json_extract(body, '\$.unit') IS NOT NULL
  ORDER BY seq"
```

## 4. Find the repeating pattern

A loop shows up as the same transition many times: a subject that moves to
the state it is in, with the same reason each time.

```sh
sqlite3 -readonly $W "
  SELECT count(*), json_extract(body, '\$.subject'), substr(json_extract(body, '\$.reason'), 1, 80)
  FROM records WHERE schema = 'osmia.trace.transition' AND json_extract(body, '\$.from') = json_extract(body, '\$.to')
  GROUP BY 2, 3 ORDER BY 1 DESC LIMIT 10"
sqlite3 -readonly $W "
  SELECT json_extract(body, '\$.cause') FROM records
  WHERE schema = 'osmia.trace.turn-request' AND json_extract(body, '\$.agent_id') = 'chief_of_staff'" \
  | sed -E 's/[0-9]+/N/g' | sort | uniq -c | sort -nr | head
```

The second command shows what woke the chief of staff. Notices from a loop
reach it on every pass, so its turn count is often the largest cost of one.

Read the chief's latest status note too. It often states what is wrong. That
is a lead to confirm, not a conclusion.

## 5. Explain why it does not progress

A loop means something the looping component waits for never happens. Name
that thing, then look at whatever should have done it.

- Check what the component's state requires. For example, a review refused as
  stale needs a rebased candidate, and a rebase needs a foreman pass.
- Look for operations without a result in every workstream of the project, not
  only the stuck one. Some gates are project-wide.

  ```sh
  sqlite3 -readonly -separator '	' $W "
    SELECT json_extract(e.value, '\$.operation.action'), json_extract(e.value, '\$.id'), json_extract(e.value, '\$.body')
    FROM workflow_log t, json_each(t.body, '\$.events') e
    WHERE t.list = 'transactions' AND json_extract(e.value, '\$.operation') IS NOT NULL
      AND json_extract(e.value, '\$.id') NOT IN (
        SELECT json_extract(body, '\$.event_id') FROM workflow_log
        WHERE list = 'operations' AND json_extract(body, '\$.result') IS NOT NULL)"
  ```

- Line up timestamps across workstreams. A stall that starts seconds after
  another workstream's operation begins is rarely a coincidence.
- Read an operation's attempt history: how many attempts, the last failure,
  and the gap between each `at` and its `retry_at`. A gap that never grows
  means the retry backoff is not working.

  ```sh
  E='<event id>'
  sqlite3 -readonly $W "
    SELECT count(*) FROM workflow_log WHERE list = 'operations' AND json_extract(body, '\$.event_id') = '$E';
    SELECT json_extract(body, '\$.kind'), count(*) FROM workflow_log
      WHERE list = 'operations' AND json_extract(body, '\$.event_id') = '$E' GROUP BY 1;
    SELECT substr(json_extract(body, '\$.failure'), 1, 300) FROM workflow_log
      WHERE list = 'operations' AND json_extract(body, '\$.event_id') = '$E'
        AND json_extract(body, '\$.failure') IS NOT NULL
      ORDER BY seq DESC LIMIT 1;"
  ```

- Check the repository itself. Use `git merge-base --is-ancestor` on the
  feature branch and the unit branches, and compare the trees of the upstream,
  the feature tip and any candidate. Records describe what the service
  intended; the repository shows what happened.

## 6. Read what the agents said

For the agents that loop, read their final responses in order. Their words
explain things the transitions cannot. A reviewer may reject the same
candidate for the same reason every time while its mason insists the work is
done. An empty diff may be correct because upstream already contains the
change.

```sh
sqlite3 -readonly -separator '	' $W "
  SELECT json_extract(body, '\$.turn_id'), substr(coalesce(json_extract(body, '\$.result.FinalResponse'), ''), 1, 200)
  FROM records WHERE schema = 'osmia.trace.turn-response' AND json_extract(body, '\$.agent_id') = '<agent>'
  ORDER BY seq" | sed -n '1,3p;$p'
```

## 7. Confirm in the code

Only now open the code, with a hypothesis the data supports: the controller
that should have acted, the gate that held it, the bound that is missing.
Before you fix anything:

- Check whether the running binary is older than the code you are reading.
  `git log -S '<identifier>'` dates a behavior; an incident from before a fix
  is evidence about the old binary, not the current code.
- Write a regression test that reproduces the pattern, and run it with the fix
  removed to see it fail.

## Finding waste

When nothing is stuck but spend looks high, ask where the sessions go and
what they achieve.

1. Spend by role across every workstream. A role that coordinates should not
   cost more than the roles that do the work.
2. What triggers each role's turns. For the chief of staff, sort turns by
   requester (`.actor.id` of its turn requests) and sort the notices in its
   event prompts by kind.
3. What those turns did. Count the tool calls in `tools/` per turn. A turn
   that only reads files and rewrites its status did not need to run.

   ```sh
   for db in */*/trace.db; do
     sqlite3 -readonly $db "
       SELECT json_extract(json_extract(body, '\$.content'), '\$.name') FROM records
       WHERE schema = 'osmia.trace.document' AND json_extract(body, '\$.path') LIKE 'tools/%'
         AND revision = 1
         AND json_extract(json_extract(body, '\$.content'), '\$.scope.Role') = 'chief_of_staff'"
   done | sort | uniq -c | sort -nr
   ```
   (run from `~/.local/share/osmia/workstreams`)

4. Work repeated per item: reviews per merged unit, re-checks caused by
   rebases, librarian refreshes per landing.
5. Simulate a change on recorded data before you build it. Re-batch the
   recorded notices under a longer delivery window, or count the refreshes a
   different trigger would have run. That puts a number on the saving.

## Tuning a guard against history

A new bound or detector gets its threshold from the record, not by guessing.
Replay its rule over every workstream: walk the transitions and sessions in
time order, reset where the rule says progress happened, and report the
longest run with what it consisted of. Each run above the threshold should be
a real problem, and every healthy workstream should stay well below it. Break
the longest runs down by role and unit to check which kind each is. Set the
threshold between the two with a margin.

## Pitfalls

- Operation records come back sorted by event ID, not by time.
- The ledger includes zero-cost judgment calls. Count them separately from
  agent sessions.
- A project-wide gate can hide behind a quiet workstream: a pending extraction
  or refresh in the librarian's workstream holds back work everywhere.
