# M4 reliability demonstration

`TestM4ReliabilityDemonstration` in `internal/service/reliability_demo_test.go`
takes one workstream from ratification to a landed unit through the service's
local API client, the same API used by the `osmia` commands. On the way, the
daily budget, a hard pause, a live profile switch, a configuration reload, a
provider usage limit and a crash during landing each interrupt the work.
Fake masons, a fake reviewer and a fake chief of staff run through core's fake
enforcer and an in-memory MCP transport. The test uses a temporary Osmia root,
a local Git clone and bare upstream. It starts no model, container, remote
push or pull request.

Run it with `dagger check`, or run just the demonstration inside Dagger:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM4ReliabilityDemonstration,-v,./internal/service \
  combined-output
```

## Setup

The configuration has one mason slot (`capacity.masons = 1`), a daily budget
of USD 1.00 (`budget.per_day = "1.00"`) and two profiles: `default` on
`claude` and `other` on `codex`. Every role is bound to `default`. The service
counts days in UTC. The plan has four independent units, started one at a time
in plan order:

| Unit | Footprint | What interrupts it |
| --- | --- | --- |
| `upload` | `internal.upload` | its session spends past the daily budget |
| `audit` | `internal.audit` | a hard pause of the workstream |
| `dedupe` | `internal.upload` | a live profile switch, then a reload |
| `resume` | `internal.trace` | a provider usage limit, then a crash while it lands |

The pinned core restricts an agent's built-in tools for `claude` only, and
refuses any other backend for a turn whose grants name tools. Every role's
grants name tools, so outside tests a turn on `codex` fails before it starts;
see [execution adapters](core-adapter.md#execution-adapters). The
demonstration's fake engine stands in for a `codex` backend that restricts
its tools as `claude` does, so the switch and the fallback below show how
Osmia selects profiles and replays the owned log.

## The walkthrough

### 1. The daily budget pauses the factory

`upload`'s mason reports done, and its session reports a known cost of
USD 1.25. The next pass pauses the factory softly, attributed to the daily
budget. The fake architect and chief of staff report no cost, so the spend is
a lower bound. `osmia status` shows `Daily budget: USD 1.25 of USD 1.00 spent
on 2026-09-16 (at least; <n> attempt(s) have unknown cost)`, and the pause's
reason begins:

```text
Daily budget reached: known spend on 2026-09-16 is USD 1.25 of the USD 1.00 per-day budget; dispatch resumes at the next local day; <n> attempt(s) have unknown cost, so actual spend may be higher
```

`audit` does not start, and
`osmia status <workstream-id>` shows `Waits while a factory pause is in
force, set by daily-budget.` for it.

The owner resumes (`osmia resume all`). The budget pauses at most once per
local day, so `audit` starts although spend is still over the limit.

### 2. A hard pause stops a mason in flight

`audit`'s mason writes `internal/audit/audit.go` and is still working when the
owner hard-pauses the workstream (`osmia pause <workstream-id> --hard --reason
"Stop the mason"`). The service stops the session. The turn completes as
`interrupted` with the pause as its stop, and no failure. The unit stays
`implementing`, and its continuation does not run while the pause holds.

The owner resumes the workstream. The continuation `mason-audit-recover-1`
runs on the same thread. It resumes the stopped backend session, and its
prompt says a hard pause stopped the last turn. The file from the stopped turn
is still in the unit's workspace. The mason reports done.

### 3. A live profile switch across binaries

While `dedupe`'s first mason turn runs on `default` (`claude`), the owner
switches masons to `other` (`osmia profiles set mason other`). The running
turn finishes on `claude`. It ends without calling an outcome tool, so the
service queues a follow-up, `mason-dedupe-clarify-1`, on the same thread. That
turn runs on `codex`. It cannot resume a `claude` session, so it starts a
fresh session whose prompt opens with a bounded replay of the thread's owned
log, which includes the first turn's response. `osmia profiles` shows `mason`
on `other` from `owner_override`. While the follow-up runs, the owner clears
the switch (`osmia profiles clear mason`). `resume` waits: `Waits for a mason
slot: all 1 are in use.`

### 4. Reload

Still during `dedupe`'s follow-up, the owner edits `config.toml` to give
`default` a fallback to a profile that does not exist
(`fallback = "missing"`). `osmia reload` fails with `validation`, naming the
file and `profiles.default.fallback`. The loaded configuration and its digest
stay as they were, and `osmia status` reports the failed reload.

The owner corrects the file to `fallback = "other"`. The reload succeeds with
a new digest and clears the failed reload. The project's next pass rebuilds
its turn runners from the reloaded configuration. The follow-up reports done.

### 5. A provider usage limit selects the fallback

`resume` starts on `default`. Its first session on `claude` ends with a
blocking rate-limit report (`rejected`, `five_hour`). The runner records the
provider's limit and, in the same turn, retries on the fallback the reload
added. The second attempt runs on `other`, replaying the owned log, and the
mason reports done. The turn's attempts record the first as an
infrastructure failure and the second's reason as `fallback from profile
default to other after 1 infrastructure failures: provider usage limit`.
`osmia status` lists the limit and shows `mason` on `other` from
`provider_fallback`, with the reason `Provider claude usage limit
(rejected)`.

### 6. A crash while landing, and a restart

The reviewer approves `resume`, and the foreman lands it. The landing makes
its commit and moves the feature branch, then fails before it records
anything, on every retry, until the service stops. The feature branch now
holds one landing commit, and the unit is still `approved`, with no
`landing.json`.

On restart the landing's inspection finds that commit on the feature branch
by its `Osmia-Operation` trailer. The retry records it without committing
again: the feature branch still holds that one commit, `landing.json` names
it, and `resume` moved to `merged` once. After the restart the budget has not
paused the factory again that day, masons still fall back from the limited
provider, and the configuration digest is the one the reload applied.

## What to inspect

`runtime.json` under the root holds the owner's controls, the provider limit
and `budget_paused_on`. In the workstream's trace, `events.jsonl` holds the
unit transitions. The `mason-audit` thread holds the stopped turn and its
continuation. Each turn's attempts in the `mason-dedupe` and `mason-resume`
threads record the profile, whether the session resumed or replayed the owned
log, and why. `units/resume/landing.json` records the landing, and the
landing operation's history records each interrupted attempt and what the
retry after the restart observed.
