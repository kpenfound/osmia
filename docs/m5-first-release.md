# M5 first-release demonstration

`TestM5FirstReleaseDemonstration` in
`internal/service/first_release_demo_test.go` takes one workstream from
hand-in to a delivered pull request through the service's local API client,
the same API the `osmia` commands use. `notify.webhook` is set, and a fake
webhook checks that every owner decision on the way, and the daily budget
pause, reaches it exactly once.

The test uses a temporary Osmia root, a local Git clone on worktrees with a
bare upstream and a bare fork, file-based context, fake agents and a fake
pull request host. It starts no model, container, tailnet, remote push or
real pull request.

Run it with `dagger check`, or run just the demonstration inside Dagger:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM5FirstReleaseDemonstration,-v,./internal/service \
  combined-output
```

## Setup

The configuration has one mason slot, a daily budget of USD 1.00
(`budget.per_day = "1.00"`) and `notify.webhook` pointing at the fake webhook.
Failed posts are retried after two seconds, doubling each time. The plan has
two units: `resume` addresses `spec#1`, and `dedupe` addresses `spec#2` and
depends on `resume`.

## The walkthrough

Each step waits until the webhook has accepted the notification before the
owner decides through the API.

| Step | What happens | Notification | Owner's decision |
| --- | --- | --- | --- |
| 1 | The design is handed in with debate skipped, and its packet waits in the shed. | `ratification` | `POST /v1/ratify/<workstream>` |
| 2 | `resume`'s mason asks a question, and the chief of staff escalates it. | `escalation` | `POST /v1/inbox/<n>` |
| 3 | The answer turn asks to amend `spec#1`. The architect drafts, the committee debates one round and the chief of staff presents the packet. | `amendment` | `reject` through `POST /v1/amendment/<workstream>/<n>` |
| 4 | The turn that brings the ruling to the mason costs USD 1.25, so the factory pauses. | `budget_pause` | `DELETE /v1/runtime/pause` for the factory |
| 5 | `resume` is reviewed and lands. `dedupe`'s mason gives up, and the unit is contested. | `contested` | `revise` through `POST /v1/contested/<workstream>/dedupe` |
| 6 | `dedupe` is reviewed and lands, and final review presents the delivery. | `delivery` | `POST /v1/delivery/<workstream>` |

After approval the service pushes the feature branch to the local fork and
opens one pull request whose body is the approved draft. The workstream ends
`delivered` and the inbox is empty.

Every decision post names the project, the workstream and the kind. The
budget pause post names the project, `Spend: USD 1.25 or more` and
`Limit: USD 1.00`; it has no workstream because the pause holds the whole
factory.

## Restarts

The service restarts twice:

- **A sent notification is not posted again.** After the escalation is
  notified, and before the owner answers, the service stops and starts. The
  escalation is still open, and the webhook receives nothing new.
- **A pending notification is not lost.** Before `dedupe` is contested, the
  webhook starts answering `503`. The service records the contested
  notification as pending, the first post fails, and the service stops. The
  ledger at `<root>/notifications.json` holds that one pending record. Once
  the webhook answers `200` again, the service starts and posts it once.
  Nothing sent before the restart is posted again.

At the end the webhook has accepted exactly six posts, in order:
`ratification`, `escalation`, `amendment`, `budget_pause`, `contested` and
`delivery`. The ledger records each of them as sent. The webhook also checks
that each post was recorded as pending before it arrived.

See [notifications](service.md#notifications) for the post format and the
ledger's rules.
