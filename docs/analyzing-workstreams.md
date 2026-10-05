# Analyzing workstream sessions

How to find out, from a factory's own records, why a workstream is stuck,
looping or expensive. The trace records every transition, turn and session,
so a question about what the factory did is answered from files, not from
memory or guesses. The method moves from counts to a timeline, from the
timeline to the one pattern that repeats, and from that pattern to the code
that produces it.

## Ground rules

- Read only. Analysis never writes to the root. Stop the service, or leave it
  running and read, but never open the trace with Osmia's own code while the
  service holds it.
- Inspect Jujutsu workspaces with `jj --ignore-working-copy`; any other `jj`
  command snapshots the working copy and changes the record you are reading.
- Put scripts and intermediate output in a scratch directory, not the repository.
- Keep the factory stopped while a loop is diagnosed. Every pass of a loop costs
  sessions.

## Where the evidence lives

Paths are under the root, `~/.local/share/osmia` by default.

| Path | What it answers |
|---|---|
| `runtime.json` | Pauses with their sources, archived workstreams, profile overrides |
| `projects/<p>/config.toml` | The project's clone path, base branch and landing style |
| `projects/<p>/workstreams/<w>/workstream.json` | When the workstream was created and its workspace backend |
| `.../events.jsonl` | Every transition: `subject`, `from`, `to`, `reason`, `actor`, `cause`, `at`, `unit` |
| `.../ledger.jsonl` | One cost record per session: `.entry.Scope` (`Role`, `Unit`, `Thread`, `Turn`), `.entry.At`, `.entry.Usage` (`CostUSD`, `Turns`) |
| `.../agents/<agent>/log.jsonl` | Each turn's request (`actor`, `cause`, `prompt`, `profile`) and response (`result.FinalResponse`, `result.Outcome`) |
| `.../workflow.json` | `transactions[].events[]` with their `operation`, and `operations[]`: each attempt's `claim`, `observe`, `effect`, `retry` (with `failure` and `retry_at`) and `result` |
| `.../status.jsonl` | The chief of staff's status revisions: `goal`, `note`, `agents` |
| `.../tools/` | One audit record per tool call: `scope.Role`, `scope.Turn`, `name` |
| `.../units/<u>/` | `report.json`, `review.json`, `rebase.json`, `landing.json`, `checks-<n>.json` |
| `.../drift/rebase.json` | The latest drift rebase record: outcome, conflicts, candidate, review count, verdict |
| `units/<p>/<w>/<u>`, `drifts/<p>/<w>` | Unit and drift resolution workspaces |

The project's librarian has a workstream of its own, `w_` followed by the
first 32 hex digits of `sha256("librarian:<project id>")`. Its
`workflow.json` holds the knowledge base extraction and refresh operations.

Timestamps mix UTC and local offsets. Convert them before you sort or bucket
them.

## 1. Take inventory

List every workstream with its feature state, whether it is archived, its
session count, the span of its events and the goal from its latest status.
Active, high-session workstreams are where to start.

```sh
cd ~/.local/share/osmia/projects
for w in */workstreams/*/; do
  id=$(basename $w)
  state=$(jq -r 'select(.subject=="feature") | .to' $w/events.jsonl | tail -1)
  sessions=$(wc -l < $w/ledger.jsonl 2>/dev/null)
  goal=$(tail -1 $w/status.jsonl 2>/dev/null | jq -r '.goal // empty' | head -c 80)
  echo "$id [$state] sessions=$sessions $goal"
done
```

## 2. Profile one workstream

Break its sessions down by role, by role and unit, and by agent thread, with
cost. The shape usually points at the problem straight away: reviewer
sessions far above mason sessions, or nearly all of a role's sessions on one
or two units, means something repeats.

```sh
W=projects/<p>/workstreams/<w>
jq -r '[.entry.Scope.Role, .entry.Usage.CostUSD] | @tsv' $W/ledger.jsonl \
  | awk -F'\t' '{n[$1]++; c[$1]+=$2} END {for (r in n) printf "%-16s %5d $%.2f\n", r, n[r], c[r]}'
jq -r '[.entry.Scope.Role, .entry.Scope.Unit] | @tsv' $W/ledger.jsonl | sort | uniq -c | sort -nr
```

## 3. Build a timeline

Bucket sessions by time and role, and list the feature and unit transitions
beside them. Find the point where the session rate jumps while the states stop
moving. The transitions just before that point are where the stall began.

```sh
jq -r '[(.entry.At | .[0:13]), .entry.Scope.Role] | @tsv' $W/ledger.jsonl | sort | uniq -c
jq -r 'select(.unit != null) | [.at[0:19], .unit, .from, .to] | @tsv' $W/events.jsonl
```

## 4. Find the repeating pattern

A loop shows up as the same transition many times: a subject that moves to
the state it is in, with the same reason each time.

```sh
jq -r 'select(.from == .to) | [.subject, .reason[0:80]] | @tsv' $W/events.jsonl | sort | uniq -c | sort -nr | head
jq -r 'select(.schema=="osmia.trace.turn-request") | .cause' $W/agents/chief_of_staff/log.jsonl \
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
  jq -r '([.operations[]? | select(.result != null) | .event_id] | unique) as $done
    | .transactions[] | .events[]? | select(.operation != null)
    | select((.id as $id | $done | index($id)) | not)
    | [.operation.action, .id, .body] | @tsv' $W/workflow.json
  ```

- Line up timestamps across workstreams. A stall that starts seconds after
  another workstream's operation begins is rarely a coincidence.
- Read an operation's attempt history: how many attempts, the last failure,
  and the gap between each `at` and its `retry_at`. A gap that never grows
  means the retry backoff is not working.

  ```sh
  jq -r --arg e <event id> '[.operations[] | select(.event_id==$e)]
    | length, (map(.kind) | group_by(.) | map("\(.[0]) \(length)")),
      (map(select(.failure)) | last | .failure[0:300])' $W/workflow.json
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
jq -r 'select(.schema != "osmia.trace.turn-request") | [.turn_id, (.result.FinalResponse // "")[0:200]] | @tsv' \
  $W/agents/<agent>/log.jsonl | sed -n '1,3p;$p'
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
   find */workstreams/*/tools -name '*-1.json' -print0 | xargs -0 cat \
     | jq -r 'select(.scope.Role=="chief_of_staff") | .name' | sort | uniq -c | sort -nr
   ```

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
