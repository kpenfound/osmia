# web-driver

A Dagger module that drives a web service with Playwright and Chromium and
returns the Directory of Osmia's verification lever contract: a version 1
`report.json` and the evidence files its claims name. The contract itself, with
the run ID, flow tags, limits and `list`, is described once in
[docs/testing.md](../../docs/testing.md). This page covers only what the module
adds.

Install it in a product by git ref and call it from the product's verify
function:

```sh
dagger module install github.com/kpenfound/osmia/modules/web-driver@<ref>
```

The [verification fixture](../../testdata/verification-fixture) is a complete
product built on it.

## Functions

- `run` drives a Service you built. Bind-and-drive happens inside the driver
  container, so no host port is published.
- `run-app` takes the built app as a Container, starts it as a Service that is
  unique to the run ID, and drives it. Prefer it: the run ID reaches both the
  app and the browser without further work.

Both take the lever's run ID, a step list, the port the app listens on (default
8080), an optional `flow` that tags every claim, and a per-step timeout in
seconds (default 10).

## Fresh runs

Declaring a function uncached does not stop the operations inside it from being
cached. The module therefore puts the run ID into the driver container's
environment, so the browser executes anew for every run ID, and `run-app` puts
it into the app container's environment, so the app starts anew and, for
example, regenerates a start-up value. Image pulls, the `npm ci` layer and the
product's own build stay cached. A product that calls `run` with its own
Service must fold the run ID into that Service's container the same way.

The driver runs from a Playwright image pinned by digest, for amd64 only, and
installs `playwright-core` from the committed `package-lock.json`.

## Steps

A step list is one string. Steps are separated by `;` or a newline. A step is a
verb followed by arguments separated by `|`:

```
goto notes; fill id=note | Remember the milk; click text=Add note; snapshot
```

| Verb | Arguments | What it does |
| --- | --- | --- |
| `goto` | `[path]` | Loads the path relative to the Service's base URL. No path or `.` means the root, and leading slashes are dropped. An absolute URL or any other origin fails the step. |
| `click` | `target` | Clicks the element. |
| `fill` | `target \| text` | Fills the field with the text. |
| `press` | `[target \|] key` | Presses a key on the element, or on the page when there is no target. |
| `snapshot` | none | Saves the page's accessibility tree with element refs as `snapshots/NN-snapshot.yml`. |
| `screenshot` | `[full]` | Saves a PNG of the viewport, or of the whole page with `full`, as `screenshots/NN-screenshot.png`. |
| `console` | none | Saves the console messages so far as `console/NN-console.log`. The claim fails if any are errors or uncaught exceptions. |
| `requests` | none | Saves the requests so far as `requests/NN-requests.log`. The claim fails if any request failed or answered HTTP 500 or above. |

A target is a Playwright selector such as `text=Add note`, `id=note` or
`role=button`, or an element ref such as `e10` from an earlier `snapshot` step
of the same list. Refs are numbered per snapshot, so take a snapshot first and
write the refs from it into a later list.

The step list is written to pass Osmia's agent-argument rules for lever
arguments: no `:`, no `#`, no `?` at the start of a value, no `..`, no
shorthand flags. Write a character those rules refuse as a percent escape and
the driver decodes it: `%3A` for `:`, `%2F` for `/`, `%23` for `#`, `%3F` for
`?`, `%3B` for `;`, `%7C` for `|` and `%25` for `%`. Paths are relative to the
Service's base URL for the same reason.

## Report

Each step yields one claim named `step NN <step text>` with a status, a detail
and evidence. Every claim cites `log.txt`, the text log of the run. When
`flow` is set, every claim carries it.

- `pass`: the step ran and, for `console` and `requests`, found nothing wrong.
- `fail`: the step ran and failed, for example a click on a missing element. A
  `screenshots/NN-failure.png` shows the page at that point. The driver stops
  at the first failure.
- `inconclusive`: the step did not run, because an earlier step failed, the
  step list could not be parsed, or the browser did not start.
