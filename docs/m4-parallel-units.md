# M4 parallel units and shared slots demonstration

`TestM4ParallelUnitsDemonstration` in `internal/service/parallel_demo_test.go`
builds two workstreams of one project on shared mason slots through the
service's local API client, the same API used by the `osmia` commands. Fake
masons and a fake chief of staff run through core's fake enforcer and an
in-memory MCP transport. The test uses a temporary Osmia root, a local Git
clone and bare upstream. It starts no model, container, remote push or pull
request.

Run it with `dagger check`, or run just the demonstration inside Dagger:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM4ParallelUnitsDemonstration,-v,./internal/service \
  combined-output
```

## Setup

The configuration sets `capacity.masons = 2` and `capacity.per_workstream = 3`.
The entity map has `internal.trace`, part of `internal`, and the top-level
entities `internal.upload` and `internal.audit`. Both workstreams are ratified
on the same four-unit plan, and no unit depends on another:

| Unit | Footprint |
| --- | --- |
| `resume` | `internal.trace` |
| `dedupe` | `internal.trace` |
| `upload` | `internal.upload` |
| `audit` | `internal.audit` |

`resume` and `dedupe` are entangled because their footprints intersect.
`upload` and `audit` are disjoint from every other unit. Below, `hi` is the
workstream the owner ranks first and `lo` the other one. `lo` has the lower
workstream ID, so it would win a tie on ID.

## The walkthrough

### 1. A paused factory

The owner pauses the factory (`osmia pause all`) before both workstreams
reach `building`. No unit starts. `osmia status <workstream-id>` shows every
ready unit with `Waiting: Waits while a factory pause is in force, set by
operator.`. Paused workstreams are not compared for overlap, so neither has
an advisory yet.

### 2. Parallel units and priority

The owner sets the priority order `hi`, then `lo` (`osmia priority set <hi>
<lo>`), and resumes the factory. In the first pass `hi` takes both slots.
`resume` starts, `dedupe` is skipped because it is entangled with `resume`,
and `upload`, which is disjoint, starts next to `resume`. `units/dedupe/dispatch.json`
records `entangled`. `resume`'s mason reports done and the unit moves to
`reviewing`. Its slot goes to `hi` again, this time to `dedupe`, even though
`lo` has never started a unit. With `resume` out of `implementing`, `dedupe`
is no longer entangled.

Status now explains every wait. In `hi`, `audit` shows `Waits for a mason
slot: all 2 are in use.`. In `lo`, every unit shows `Waits for a mason slot:
all 2 are in use, and higher-priority workstreams start first: <hi>.`.

### 3. Restart while a mason works

`dedupe`'s mason saves a file and is still working when the owner clears the
priority (`osmia priority clear`) and the service stops. On restart, the
mason controller reads both `implementing` units of `hi` from the trace before
it starts anything, so the two slots stay taken. `dedupe` keeps its
workspace, including the file from the interrupted turn, and gets exactly one
continuation, `mason-dedupe-recover-1`. Its first turn does not run again and
no unit starts a second time.

### 4. Round-robin at equal priority

The continuation reports done and `dedupe` moves to `reviewing`. Both
workstreams now have equal priority, so the freed slot goes to the one that
started a unit least recently: `lo`, which has never started one, starts
`resume`. While `lo`'s `resume` is implementing, its `dedupe` records
`entangled`. `lo`'s `resume` then reports done. Its slot goes back to `hi`,
which started a unit longer ago than `lo`, and `hi` starts `audit`. Each unit
started once, each first mason turn ran once, and the final state has two
units `implementing` and `lo`'s three remaining ready units waiting for a
slot.

### 5. The overlap advisory

Both workstreams build on seals that cover the same entities, so each chief
of staff receives an `overlap-advisory` event about the other while both are
still `building` and no pull request is open:

```text
Workstream <other> of this project builds in subsystem internal, internal.audit, internal.upload: both sealed footprints cover internal.audit, internal.trace, internal.upload (seal 1 of this workstream, seal 1 of that one). Its changes may conflict with this workstream's before both pull requests are open. This is an advisory and blocks neither workstream; the owner can pause or reprioritise either.
```

`osmia status <workstream-id>` lists the same advisory. It blocks neither
workstream: both kept starting units after it was raised.

## What to inspect

In each workstream's trace, `events.jsonl` holds the unit transitions in the
order they were decided, and `units/<unit>/dispatch.json` holds the history
of why each ready unit waited (`paused`, `priority`, `capacity`, `entangled`)
until it `started`. `hi`'s `mason-dedupe` thread holds the interrupted turn
and its one continuation. The `overlap-<other>` transition and its outbox
event hold the advisory.
