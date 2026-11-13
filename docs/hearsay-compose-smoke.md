# Hearsay Compose smoke run

`scripts/hearsay-compose-smoke.sh` connects the whole path against a real
Hearsay. It runs Hearsay under Docker Compose with the configuration that
`osmia project memory` generates, lets the service's publisher post a project's
records, and reads them back through Osmia's own Hearsay read path. Nothing in
Hearsay is faked. The one thing replaced is the model provider of Hearsay's
distill and assert tiers, which a local stub answers (`deploy/hearsay-smoke/modelstub.py`).
[Hearsay integration](hearsay.md) describes what is posted and read; this page
is the procedure that checks it end to end.

Everything runs in containers, so its output is **container evidence**: it says
what ran in which containers, and it is not a run of a live factory or of
Hearsay's published image.

## Run it

From the repository root, with Docker and `docker compose`:

```sh
scripts/hearsay-compose-smoke.sh [evidence-dir]
```

The script downloads the Hearsay revision it was written against, builds it,
and needs network access for that and for Go modules and base images. It exits
non-zero if any check below fails. The raw requests and answers are left in the
evidence directory, which defaults to a new temporary directory and is printed
at the end.

| Variable | Effect |
|---|---|
| `HEARSAY_REF` | Hearsay commit to build. The default is the revision the script was written against. |
| `HEARSAY_SRC` | An existing Hearsay tree to build instead of downloading `HEARSAY_REF`. Use it to run a branch that is not published. |
| `KEEP=1` | Leaves the Compose project (`osmia-hearsay-smoke`) running and keeps the scratch files. |
| `HTTPS_PROXY`, `NO_PROXY`, `PROXY_CA_CERT_B64` | Passed to the steps that download Go modules. The Compose network needs no proxy. |

## What it does

1. **Builds Hearsay** from its Go version's image into a static binary, and
   wraps it in an image shaped like the one Hearsay publishes (Alpine, the
   binary as the entrypoint, user `nobody`).
2. **Compiles Osmia's smoke test** (`internal/service/hearsay_smoke_test.go`,
   build tag `hearsaysmoke`) once.
3. **Generates Hearsay's configuration** by running the memory setup
   (`buildMemorySetup`, which the `osmia project memory` endpoint and command
   call) for the test project. The four fragments go into the Compose
   configuration directory beside the files in `deploy/hearsay-smoke/config`:
   the stub model tiers and one outside principal, `smoke-outsider`, a person
   who is neither the owner nor acting for the owner.
4. **Validates** the directory with `hearsay config validate`.
5. **Starts Hearsay under Compose** in the shape of Hearsay's own deploy recipe:
   Postgres with pgvector, the `hearsay migrate up` job, and the connectors,
   distiller, assert-worker and API as four containers of one image, plus the
   model stub. The connectors (`/hooks/<source id>`) and the API (`/v1/<call>`)
   listen on different ports, so `[hearsay.publish] hooks_url` is
   `http://connectors:8081` and `[hearsay] url` is `http://api:8080`.
6. **Runs `TestHearsaySmokeRun`** in a container on the Compose network.

`TestHearsaySmokeRun` builds a project without the repo opt-in, with one
feature workstream, and then:

- seals a spec, records an owner ruling on an escalated question and the
  chief of staff's relay of that ruling, and records a chief logged assumption;
- runs the service's own publisher pass (`publishPass`) against the connectors
  through a recording relay, and checks every post was accepted, was made with
  the publisher's token, and went to the source and with the `author` the
  routing rules give: the project on the work source with the owner as author
  and no `repos`; the sealed spec as a workstream `document` and the ruling as
  a `decision` on the owner source with the owner as author; the assumption as
  a `decision` on the work source with no author; nothing for the relay;
- runs a second pass with nothing changed and checks it sent no request;
- reads the project's `work:` entity and the workstream's through Osmia's read
  path, as each of the three class agents acting for the owner: the
  `worker` (a mason), the `orchestrator` (the chief of staff) and the
  `observer` (an exploratory session). Each read uses the role's tools
  (`get_bundle` with no scope for the project default, `get_bundle` and
  `traverse` on the workstream entity, `get_l1` on the spec) and the bundle
  provider that assembles a turn's context, which must request exactly the
  project and workstream entities. It checks the ruling is served `ratified`
  with decider `owner`, the assumption `inferred` with decider
  `chief_of_staff`, the relay is not a decision, and the spec is a `spec`
  document from the owner source. It checks the observer is offered no `assert`;
- checks that every read carried the owner's token with the owner as principal
  and a class agent's id and token, and none carried the publisher's;
- reads the same entities as `smoke-outsider`, directly and through each class
  agent acting for that person, and checks `get_bundle` answers what an unknown
  scope answers and `traverse` finds nothing.

The script also checks from the stub's log that no decision text reached the
model: decisions are placed from their structured fields.

## What a passing run settles

- **Who a class agent's read is checked against.** Hearsay checks a project's
  access list, here the poster and the named author, against the person the
  agent acts for, not against the agent. A class agent that acts for the owner
  therefore reads a project posted without `repos`, and a class agent that acts
  for anyone else reads nothing of it. The project is never opted in and the
  publisher's token is never used to read.
- **The authority tiers.** The owner source's `decision` is served `ratified`
  and the work source's `inferred`, whatever each names as author or decider.
- **`traverse`.** Its `start` is a list of `{type, id}` node references.

## Recorded run

Container evidence from a run on 12 November 2026. Hearsay was built in a
`golang` container from the revision the script defaults to and run under Docker
Compose 5.7 (Postgres `pgvector/pgvector:pg16`, the migration job and the four
services, all healthy). Osmia's publisher pass and read path ran in a
`golang:1.26.6` container on the Compose network. The model provider was the
stub. The evidence directory's `RESULT.txt` names the full Hearsay revision.

| Check | Result |
|---|---|
| `hearsay config validate` on the generated fragments | valid: 2 sources, 1 scope, 6 principals, 1 authority policy, 2 model tiers |
| First publisher pass | 8 posts, all 202: the project, 2 workstreams, 2 charter revisions, the spec document, the ruling, the assumption |
| Project post | on the work source, `author` the owner principal, no `repos` |
| Spec, ruling | on the owner source, `author` the owner principal |
| Assumption | on the work source, no `author` |
| Second pass, nothing changed | 0 posts, 0 requests |
| Worker, orchestrator and observer class agents acting for the owner | each reads the ruling `ratified` (decider `owner`), the assumption `inferred` (decider `chief_of_staff`), the spec as a `spec` document, through `get_bundle` and `traverse` |
| Turn bundle for a workstream-tied turn | requests the project and the workstream entity, mode `hearsay`, for all three classes |
| Credentials on the 21 reads | the owner's token and principal with the class agent's id and token; never the publisher's |
| `smoke-outsider`, directly and through each class agent | the unknown-scope bundle for both entities; `traverse` answers 404 |
| Decision text received by the model stub | none |

What the run does not cover: it drives a fixture project's trace through the
publisher, not a live factory; Hearsay's published image, Helm and the real
model provider are not involved; and the configuration comes from the function
behind `osmia project memory`, not from a running service.
