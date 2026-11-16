# Telemetry

Osmia sends OpenTelemetry **traces** of its own work to a collector you run.
Nothing else is sent: there are no metrics and the service keeps no local
store of spans. Traces are complete enough that these questions are answered
by queries over the spans alone:

1. How much of the time each role is at capacity or idle.
2. How much time each role spends, in total and per workstream.
3. How often work bounces back to the mason, and why.
4. How a workstream's time divides across its stages.

[Configuration](configuration.md#optional-telemetry) turns telemetry on and
points it at a collector. [`deploy/telemetry`](../deploy/telemetry/README.md)
runs a collector, ClickHouse and Grafana for it. The
[queries](#queries) below run against the `otel_traces` table that stack
writes.

## What is sent

Every interval the factory records becomes one span, sent when the interval
closes. Its start and end are the times stored in the trace, or the times the
scheduler and the service's capacity owners recorded, never the time of
export. Span names and attributes outside the GenAI conventions are in the
`osmia.*` namespace.

Two measures are kept apart on purpose:

- **Slot spans measure occupancy**: how long a pool counted a slot as in use,
  from the moment the pool granted it to the moment it gave it back. A slot is
  held before a runner claims the turn and after the model stops, so a held
  span is longer than the turn it covers.
- **Turn and check spans measure execution**: how long the agent or the check
  runner worked.

Capacity questions (query 1) read pool limit and slot spans. Time questions
(queries 2 to 4) read turn, unit state and workstream state spans.

### Traces and parents

| Trace | Holds | Root |
| --- | --- | --- |
| A workstream's trace | every span about that workstream, slot spans for its work included | `osmia.workstream`, which has no parent |
| The factory trace (run `service`) | pool limits, pauses, loop-guard trips, service start and uptime | none; the spans of one run are children of that run's `osmia.service.uptime` |
| The factory trace of slots (run `slots`) | slot spans of work that belongs to no workstream | none |

### Spans

| Span name | Parent | Start to end | One span per |
| --- | --- | --- | --- |
| `osmia.workstream` | none (the root) | creation to archive or removal | workstream |
| `osmia.workstream.state` | root | the transition that entered the state to the one that ended it | workstream state, owner-gate waits included |
| `osmia.unit.state` | root | the same, for a unit | unit state |
| `invoke_agent <role>` | root | the turn's recorded start to start plus duration | agent turn |
| `osmia.check.run` | root | the run's recorded start to finish | check run: unit, final review or session |
| `osmia.slot.held` | root, or none | the pool granting the slot to the pool giving it back | slot held |
| `osmia.slot.wait` | root, or none | the first capacity refusal to the end of the wait | wait for a slot |
| `osmia.pool.limit` | `osmia.service.uptime` of the run | service start or the reload that set the limit, to the reload that changed it or shutdown | interval of constant limit of a configured pool |
| `osmia.pause` | none | the pause being set to its release | pause |
| `osmia.service.start` | `osmia.service.uptime` | an instant | service start |
| `osmia.service.uptime` | none | service start to shutdown | service run |
| `osmia.loop_guard.trip` | `osmia.service.uptime` | an instant | loop-guard pause |

State spans abut: each one ends where the next begins, so a workstream's state
spans cover its life without a gap, from its recorded creation to the end of
its root. The state before the first recorded state is `created`; the last
state's span ends in `archived` or `removed`.

`osmia.service.start` and `osmia.loop_guard.trip` are spans of no duration that
also carry a span event of the same name.

### Attributes

Attributes are allow-listed. A key outside the list, or a value that is not a
single identifier-shaped word, is dropped before the span is sent.

| Attribute | Value | On |
| --- | --- | --- |
| `osmia.project.id`, `osmia.workstream.id` | IDs | everything about a workstream |
| `osmia.unit.id` | unit ID | unit state, turns of a unit, unit check runs, slot spans of a unit's work |
| `osmia.record.id` | the trace record the span came from | turns, check runs, `osmia.loop_guard.trip` |
| `osmia.state`, `osmia.next_state` | state names | `osmia.workstream.state`, `osmia.unit.state` |
| `osmia.cause` | see [causes](#cause-values) | state spans, the root, stopped or failed turns |
| `osmia.outcome` | see [outcomes](#outcome-values) | the root, turns, check runs, slot spans, pool limit spans, pauses, uptime |
| `osmia.role` | `mason`, `reviewer`, `committee`, `architect`, `checks`, `chief_of_staff` | turns, slot spans, role pauses |
| `osmia.backend`, `osmia.model`, `osmia.profile` | names | turns |
| `osmia.slot_wait_ms` | milliseconds from the turn's request to its runner's claim | turns |
| `osmia.cost_known`, `osmia.cost_usd` | `cost_usd` only when the backend reported a cost; otherwise `cost_known=false` and no cost | turns |
| `osmia.kind` | `unit`, `final`, `session` | `osmia.check.run` |
| `osmia.covered` | how many of the project's listed checks the run covered, when the selection recorded it | `osmia.check.run` |
| `osmia.pool.id` | the pool's identity: the role it serves, shared by every project that draws on it (`mason`, `reviewer`, `committee`, `architect`, `checks`) | slot spans, `osmia.pool.limit` |
| `osmia.pool.roles` | the roles the pool serves | slot spans, `osmia.pool.limit` |
| `osmia.pool.limit` | the pool's limit at the span's start (slot spans) or throughout (pool limit spans) | bounded pools |
| `osmia.pool.unbounded` | `true` | pools with no factory-wide limit (`architect`) |
| `osmia.wait_reason` | `capacity`, `workstream-cap` | `osmia.slot.wait` |
| `osmia.pause.scope` | `factory`, `role`, `project`, `workstream` | `osmia.pause`, `osmia.loop_guard.trip` |
| `osmia.pause.source` | `owner`, `daily-budget`, `provider-usage-limit`, `loop-guard` | `osmia.pause`, `osmia.loop_guard.trip` |

An unbounded pool never carries `osmia.pool.limit`, and no span ever carries a
limit of zero: a zero would read as "no capacity" when it means "no limit".

The resource carries `service.name` (`osmia`) and `service.version` only.
`OTEL_*` environment variables change nothing about the spans or where they go.

**No content.** No prompt, system prompt, reply, history, title, transition
reason, pause reason, question, answer or check output appears in a span name,
attribute, event, status or resource. Causes and outcomes are the fixed words
below; a pause's reason prose is never sent.

### Turn spans and the GenAI conventions

A turn is one invocation of an agent, so its span follows the OpenTelemetry
[GenAI agent spans](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-agent-spans/)
at one pinned version, **semantic conventions 1.40.0**. The name is
`invoke_agent <role>` and these attributes are set:

| GenAI attribute | Value |
| --- | --- |
| `gen_ai.operation.name` | `invoke_agent` |
| `gen_ai.agent.name` | the role |
| `gen_ai.agent.id` | the thread ID of the agent |
| `gen_ai.provider.name` | the backend |
| `gen_ai.request.model` | the model |

Token counts are not sent. The status of a turn that failed, and of a check run that did not pass,
is an error with no description.

### Cause values

`osmia.cause` on a unit state span says why the unit left the state:

| Cause | The unit moved |
| --- | --- |
| `dispatched` | from `ready` to `implementing` |
| `dependencies-met` | to `ready` |
| `candidate-reported` | from `implementing` to `checking`: the mason reported done |
| `rebased` | to `checking` after a rebase onto landed work |
| `checks-passed` | from `checking` to `reviewing` |
| `checks-failed` | from `checking` back to `implementing` |
| `checks-incomplete` | from `checking` to `contested` |
| `checks-retry` | from `checking` to `checking` |
| `review-approved` | to `approved` |
| `review-rejected` | from `reviewing` back to `implementing` |
| `review-stale` | from `reviewing` to `checking`: the candidate changed |
| `review-retry` | from `reviewing` to `reviewing` |
| `review-contested` | from `reviewing` to `contested` |
| `rebase-conflict` | back to `implementing` because a rebase conflicted |
| `contested` | to `contested` for another reason |
| `contest-ruled` | out of `contested` by a ruling |
| `landed` | to `merged` |
| `amendment-wait` | to `waiting` |
| `amendment-resolved` | from `waiting` to `implementing` |
| `moved` | by the owner or the chief of staff |
| `other` | any other transition |

A unit **bounces back to the mason** when its span ends in `osmia.next_state =
implementing` with cause `review-rejected`, `checks-failed`, `rebase-conflict`
or `contest-ruled`.

On a workstream state span `osmia.cause` is who moved the workstream out of the
state: `owner` (so an owner gate was ended by the owner), `chief`, `agent` or
`service`. The last state span and the root carry `archived` or `removed`.

A turn that was stopped or failed carries `osmia.cause`: the stop cause
(`hard_pause`, `owner`), `timeout`, or the failure class (`infrastructure`,
`behavioural`).

### Outcome values

| Span | `osmia.outcome` |
| --- | --- |
| `osmia.workstream` | the last workstream state, such as `delivered` |
| turn | `completed`, `waiting`, `failed`, `cancelled` |
| `osmia.check.run` | the run's status: `passed`, `failed`, `incomplete` |
| `osmia.slot.held` | `released`, `interrupted` |
| `osmia.slot.wait` | `granted`, `canceled`, `removed`, `blocked`, `completed`, `abandoned`, `interrupted` |
| `osmia.pool.limit` | `reloaded` (a reload changed the limit), `shutdown`, `interrupted` |
| `osmia.pause` | `released`, `replaced` (the same target was paused again) |
| `osmia.service.uptime` | `shutdown`, `interrupted` |

A **wait** span starts at the first refusal by capacity (or by the
per-workstream cap), not when the work was queued, and not while a pause holds
it back. It ends `granted` when the work gets its slot, `canceled` or
`removed` when the work goes away, and `blocked` when it stops waiting for
capacity for another reason, such as a pause. `completed` closes a workflow
reservation wait whose operation finished without getting the slot. `abandoned`
closes a workflow reservation wait that stopped being asked for. A wait a
restart left open ends by what became of its operation: see
[Restarts](#restarts).

`interrupted` is a span that an earlier run left open and no one closed: a
service that stopped without shutting down, or a slot interval a restart did
not carry over. See [Restarts](#restarts).

## IDs

Trace and span IDs are derived, never random, from the IDs the trace already
records, so the same interval has the same IDs in every run of the service. An
ID is the first 16 bytes (trace) or 8 bytes (span) of the SHA-256 of a domain
string followed by each key part, every one prefixed with its length as four
big-endian bytes.

| ID | Domain | Parts |
| --- | --- | --- |
| Workstream trace | `osmia.trace.workstream` | workstream ID |
| Root span | `osmia.span.root` | workstream ID |
| Workstream state span | `osmia.span.workstream` | workstream ID, `workstream-state`, ID of the transition that entered the state |
| Unit state span | `osmia.span.workstream` | workstream ID, `unit-state`, unit, ID of the transition that entered the state |
| Turn and check spans | `osmia.span.workstream` | workstream ID, `record`, record ID, record revision |
| Slot span of a workstream | `osmia.span.workstream` | workstream ID, `slot`, `held` or `wait`, source, pool, work, start in nanoseconds |
| Factory trace | `osmia.trace.factory` | run: `service`, or `slots` for slot spans without a workstream |
| Pool limit span | `osmia.span.factory` | `service`, `pool-limit`, pool, start in nanoseconds |
| Uptime span | `osmia.span.factory` | `service`, `uptime`, start in nanoseconds |
| Pause span | `osmia.span.factory` | `service`, `pause`, scope, project, workstream, role, source, the pause's set time in nanoseconds |

The state that ends the interval from creation to the first recorded state has
no entering transition; its ID part is `created`. The start span and a
loop-guard trip use `service`, `start` or `loop-guard-trip`, the run's start in
nanoseconds and, for a trip, the project, workstream and record. A slot span of
work that belongs to no workstream is in the `slots` factory trace, with the
parts `slot`, kind, source, pool, project, work and start.

A record that is rewritten has a new revision and so a new span, while the span
of one revision is always the same.

## Restarts

Because starts come from the trace and IDs are derived, a restart loses
nothing that was recorded: a state, turn or check interval that was open when
the service stopped is sent when it closes after the restart, with its recorded
start and the IDs it would have had without the restart.

Intervals that exist only in the service's memory are kept in two small files
in the root, written only while telemetry is on:

- `slot-intervals.json` holds the slots held and waited for, the waits for a
  shared workflow reservation (architect and committee operations)
  included. After a restart the interval keeps its original start and IDs and
  its span is sent once when it closes. An interval nothing takes up within ten
  minutes of the start is sent `interrupted`, ending at the start of the run;
  so is a check-pool slot held at the stop, which cannot survive a restart.

A reservation wait is found again by the same key it had before the restart:
project, workstream, pool and the operation's runner thread. The operation's
next admission ends it `granted` with the original start and IDs, and a
further refusal after the restart updates it instead of opening a second wait.
A reservation wait recovered from the ledger is never `abandoned` for the time
the service was down. While the service runs, each pass checks the operations
behind the recovered waits that no admission has taken up yet and closes each
wait once, at the time of the check, by what became of its operation:

| Outcome | The operation behind the wait |
| --- | --- |
| `removed` | belongs to a workstream that is no longer in the project |
| `canceled` | is no longer pending and its workstream was abandoned |
| `completed` | is no longer pending, finished without getting the slot, and its workstream was not abandoned |
| `blocked` | is still pending but held back, such as by a pause |

A wait whose operation is pending and not held stays open for that operation's
next admission.
- `service-intervals.json` holds the run's start, its open pool limit intervals
  and the last time the service recorded being alive (every 30 seconds). A run
  that never shut down is sent by the next start as `interrupted`, ended at that
  last time.

## Best effort

Telemetry never blocks, delays or fails a turn or a trace commit. Spans wait in
a bounded in-memory queue and are exported in the background; when the
collector is unreachable, slow or refusing, the newest spans are dropped. On
shutdown the service flushes the queue and waits at most five seconds. With
`[telemetry]` absent or `endpoint` empty the service builds no exporter, makes
no connection and sends no span.

## Queries

The queries run against the `otel.otel_traces` table the collector in
[`deploy/telemetry`](../deploy/telemetry/README.md) creates. They use these
columns: `Timestamp` (the span start), `Duration` (nanoseconds), `SpanName`,
`TraceId` and `SpanAttributes`. `SpanAttributes` is a `Map(String, String)`, so
every value reads as text: an integer such as `osmia.pool.limit` is converted
where it is used. Each query starts with a window; change its `toDateTime64` literals, or put
Grafana's `$__fromTime` and `$__toTime` macros in their place.

### 1. Role capacity and idle time

A bounded pool is **at capacity** while its held slots reach its limit at that
moment, and **idle** while it holds none. An unbounded pool is never at
capacity: it is reported busy or idle. This question reads occupancy, so it
uses `osmia.pool.limit` and `osmia.slot.held` spans, not turn spans.

The query cuts each pool's time wherever a limit or a held slot starts or
ends, counts the slots held in each piece and compares the count with the limit
in force in that piece. A limit that changes while slots stay held is
therefore compared piece by piece, and a pool with no held span is idle
throughout the time its limit spans cover. Time with no limit span, such as
while the service is down, is not counted.

```sql
WITH
    toUnixTimestamp64Nano(toDateTime64('2026-11-14 10:00:00', 9, 'UTC')) AS win_start,
    toUnixTimestamp64Nano(toDateTime64('2026-11-14 11:00:00', 9, 'UTC')) AS win_end,
    spans AS
    (
        SELECT
            SpanName,
            SpanAttributes['osmia.pool.id'] AS pool,
            greatest(toUnixTimestamp64Nano(Timestamp), win_start) AS s,
            least(toUnixTimestamp64Nano(Timestamp) + toInt64(Duration), win_end) AS e,
            SpanAttributes['osmia.pool.unbounded'] = 'true' AS unbounded,
            toInt64OrZero(SpanAttributes['osmia.pool.limit']) AS lim
        FROM otel.otel_traces
        WHERE SpanName IN ('osmia.pool.limit', 'osmia.slot.held')
          AND toUnixTimestamp64Nano(Timestamp) < win_end
          AND toUnixTimestamp64Nano(Timestamp) + toInt64(Duration) > win_start
    ),
    changes AS
    (
        SELECT
            pool,
            ev.1 AS t,
            sum(ev.2 * (SpanName = 'osmia.slot.held')) AS d_held,
            sum(ev.2 * (SpanName = 'osmia.pool.limit')) AS d_covered,
            sum(ev.2 * (SpanName = 'osmia.pool.limit') * (1 - unbounded) * lim) AS d_limit,
            sum(ev.2 * (SpanName = 'osmia.pool.limit') * unbounded) AS d_unbounded
        FROM spans
        ARRAY JOIN [(s, 1), (e, -1)] AS ev
        GROUP BY pool, t
    ),
    pieces AS
    (
        SELECT
            pool,
            (leadInFrame(t, 1, t) OVER next) - t AS ns,
            sum(d_held) OVER upto AS held,
            sum(d_covered) OVER upto AS covered,
            sum(d_limit) OVER upto AS lim,
            sum(d_unbounded) OVER upto AS unbounded
        FROM changes
        WINDOW
            next AS (PARTITION BY pool ORDER BY t ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING),
            upto AS (PARTITION BY pool ORDER BY t ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)
    )
SELECT
    pool,
    sum(ns * (covered > 0)) / 1e9 AS running_s,
    sum(ns * (covered > 0 AND unbounded = 0 AND held >= lim)) / 1e9 AS at_capacity_s,
    sum(ns * (covered > 0 AND held > 0)) / 1e9 AS busy_s,
    sum(ns * (covered > 0 AND held = 0)) / 1e9 AS idle_s
FROM pieces
GROUP BY pool
ORDER BY pool;
```

`running_s` is the time a limit span covers, `at_capacity_s` is zero for an
unbounded pool by construction, and `busy_s + idle_s = running_s`. Read the
result for a pool as follows: `at_capacity_s / running_s` is how much of the
time the pool had no free slot, and `idle_s / running_s` how much of the time
nothing used it. A configured pool that did no work has limit spans and no
held spans, so it is idle for the whole window rather than missing from the
result.

**Worked example.** Over 10:00 to 11:00, two mason slots are held for the whole
hour; the mason limit is 2 until 10:30 and 4 after. The reviewer pool (limit 1)
holds nothing. The checks pool (limit 2) holds slots 10:00 to 10:20, 10:10 to 10:40
and 10:15 to 10:30. The architect pool is unbounded and holds one slot from
10:05 to 10:21; the service was down from 10:40 to 10:50, so its limit spans
leave that gap:

| pool | running_s | at_capacity_s | busy_s | idle_s |
| --- | --- | --- | --- | --- |
| architect | 3000 | 0 | 960 | 2040 |
| checks | 3600 | 1200 | 2400 | 1200 |
| mason | 3600 | 1800 | 3600 | 0 |
| reviewer | 3600 | 0 | 0 | 3600 |

The mason pool is at capacity for half an hour (10:00 to 10:30, while two held
slots meet a limit of 2) and not after the limit rises to 4; the reviewer pool,
which did no work, is idle for the whole window; the unbounded pools are never
at capacity.

### 2. Time per role, in total and per workstream

Time a role spends working is execution, so this question reads turn spans.
`GROUP BY ... WITH ROLLUP` gives one row per role and workstream, one row per
role with an empty `workstream` (the role's total) and one row with both empty
(all roles).

```sql
WITH
    toDateTime64('2026-11-14 00:00:00', 9, 'UTC') AS win_start,
    toDateTime64('2026-11-15 00:00:00', 9, 'UTC') AS win_end
SELECT
    SpanAttributes['osmia.role'] AS role,
    SpanAttributes['osmia.workstream.id'] AS workstream,
    count() AS turns,
    round(sum(Duration) / 1e9, 1) AS execution_s,
    round(sum(toUInt64OrZero(SpanAttributes['osmia.slot_wait_ms'])) / 1e3, 1) AS slot_wait_s
FROM otel.otel_traces
WHERE SpanAttributes['gen_ai.operation.name'] = 'invoke_agent'
  AND Timestamp >= win_start AND Timestamp < win_end
GROUP BY role, workstream WITH ROLLUP
ORDER BY role, workstream;
```

`slot_wait_s` is the time those turns waited between being queued and being
claimed by a runner. It is a turn's own wait; the capacity query shows the
pool's.

### 3. Bounces back to the mason, and why

A unit state span is sent when the state ends and carries the cause of the
transition and the state it moved to, so a bounce is a span whose next state is
`implementing` and whose cause is one of the four that return a unit to the
mason. The query counts them by cause and by the state the unit left, and
compares with the units dispatched in the same window.

```sql
WITH
    toDateTime64('2026-11-14 00:00:00', 9, 'UTC') AS win_start,
    toDateTime64('2026-11-15 00:00:00', 9, 'UTC') AS win_end,
    (Timestamp + toIntervalNanosecond(toInt64(Duration))) AS moved_at
SELECT
    SpanAttributes['osmia.cause'] AS cause,
    SpanAttributes['osmia.state'] AS from_state,
    count() AS bounces,
    uniqExact(SpanAttributes['osmia.workstream.id'], SpanAttributes['osmia.unit.id']) AS units,
    round(bounces / (
        SELECT countIf(SpanAttributes['osmia.cause'] = 'dispatched')
        FROM otel.otel_traces
        WHERE SpanName = 'osmia.unit.state'
          AND Timestamp + toIntervalNanosecond(toInt64(Duration)) >= win_start
          AND Timestamp + toIntervalNanosecond(toInt64(Duration)) < win_end
    ), 2) AS per_dispatched_unit
FROM otel.otel_traces
WHERE SpanName = 'osmia.unit.state'
  AND SpanAttributes['osmia.next_state'] = 'implementing'
  AND SpanAttributes['osmia.cause'] IN ('review-rejected', 'checks-failed', 'rebase-conflict', 'contest-ruled')
  AND moved_at >= win_start AND moved_at < win_end
GROUP BY cause, from_state WITH TOTALS
ORDER BY bounces DESC, cause, from_state;
```

`review-rejected` is a reviewer returning the candidate, `checks-failed` a
failed check run, `rebase-conflict` a rebase onto landed work that conflicted
and `contest-ruled` a ruling after a contest. The `Totals` row sums them.

### 4. Workstream time by stage

A workstream's stages are its state spans, which leave no gap and include the
time spent waiting at owner gates. The query takes the workstreams whose root
span ended in the window, so every stage is complete, and splits each one's
life by stage. `ended_by_owner_s` is the part of that time spent in states the
owner moved the workstream out of: the owner's gates.

```sql
WITH
    toDateTime64('2026-11-14 00:00:00', 9, 'UTC') AS win_start,
    toDateTime64('2026-11-15 00:00:00', 9, 'UTC') AS win_end
SELECT
    SpanAttributes['osmia.workstream.id'] AS workstream,
    SpanAttributes['osmia.state'] AS stage,
    count() AS visits,
    round(sum(Duration) / 1e9, 1) AS seconds,
    round(100 * sum(Duration) / sum(sum(Duration)) OVER (PARTITION BY workstream), 1) AS percent,
    round(sumIf(Duration, SpanAttributes['osmia.cause'] = 'owner') / 1e9, 1) AS ended_by_owner_s
FROM otel.otel_traces
WHERE SpanName = 'osmia.workstream.state'
  AND TraceId IN (
      SELECT TraceId
      FROM otel.otel_traces
      WHERE SpanName = 'osmia.workstream'
        AND Timestamp + toIntervalNanosecond(toInt64(Duration)) >= win_start
        AND Timestamp + toIntervalNanosecond(toInt64(Duration)) < win_end)
GROUP BY workstream, stage
ORDER BY workstream, min(Timestamp);
```

A stage a workstream entered more than once has `visits` above one. For the
same breakdown of a unit's time, replace `osmia.workstream.state` with
`osmia.unit.state` and group by `osmia.unit.id` as well.
