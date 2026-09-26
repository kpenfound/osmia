# M5 web page demonstration

`TestBrowserM5WebPageDemonstration` in `internal/service/web_demo_test.go`
takes one workstream from hand-in to a delivered pull request. The design
is handed in through the service's local API client, the same API
`osmia handin --skip-debate` uses. Every owner decision after that is made
on the embedded page, which headless Chromium opens over `listen.web` at
phone width.

The test uses a temporary Osmia root, a local Git clone on worktrees with a
bare upstream and a bare fork, file-based context, fake agents and a fake
pull request host. It starts no model, container, tailnet, remote push or
real pull request.

The demonstration runs in the `browser:test` check, which installs Chromium
and names it in `OSMIA_BROWSER`. Without a browser the test skips. Run it
with `dagger check`, or run the browser tests alone:

```sh
dagger check browser:test
```

## Setup

The configuration has one mason slot and a web listener on a free loopback
port (`listen.web = "127.0.0.1:0"`). The plan has two units: `resume`
addresses `spec#1`, and `dedupe` addresses `spec#2` and depends on
`resume`. The page is opened before the hand-in and is never reloaded.

## The walkthrough

| Step | What happens | Owner's decision on the page |
| --- | --- | --- |
| 1 | The design is handed in with debate skipped. The workstream appears in the shed, and its packet appears in the inbox. | **Ratify** on the ratification card |
| 2 | `resume`'s mason asks a question, and the chief of staff escalates it. | A written **Answer** on the question card |
| 3 | The answer turn asks to amend `spec#1`. The architect drafts, the committee debates one round and the chief of staff presents the packet. | **reject** with a note on the amendment card |
| 4 | `resume` is reviewed and lands. `dedupe`'s mason gives up, and the unit is contested. | **revise** with a note on the contested-unit card |
| 5 | `dedupe` is reviewed and lands, and final review presents the delivery. | A soft **Pause** of the workstream, with a reason |
| 6 | The delivery waits while the workstream is paused. | **Approve** of the drafted description, then **Resume** on the pause |

After the resume, the service pushes the feature branch to the local fork and
opens one pull request whose body is the approved draft. The workstream ends
`delivered` and the inbox is empty.

## What it checks

- **Each decision is recorded as the owner's.** The ratification record and
  the owner subject's `ratified-1` move, the ruling on the question, the
  amendment's `decision.json`, the contested unit's move back to
  `implementing` with the owner's note, and the delivery approval with the
  draft and pins the page showed are all in the trace with the owner as
  actor. The pause is in `runtime.json` with the owner as its source until
  the owner resumes it.
- **The pause holds the delivery.** While the workstream is paused, the
  approved workstream stays `assembled` and no pull request is opened.
- **The page follows events.** The new workstream's card, each inbox card as
  it is presented and as it leaves, the workstream's state, the merged unit
  and the pause all change from the page's event stream. The test marks the
  document when it opens the page and checks that the mark survives to the
  end, so nothing was reloaded.

See [the web page](service.md#web-page) for the page's views, controls and
event mapping.
