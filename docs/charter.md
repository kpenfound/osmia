# Charter

Each project has one charter: your rules as a contributor to that project. It
is the `charter.md` document in the project's trace, and you edit it with
`osmia edit charter <project-id>` or in the web UI. Plans, reviews and the
committee cite its rules, so Osmia refuses to hand in work while it has none.
Each project is one repo with its own charter, KB, entities and settings. In a
cross-project workgroup each charter governs its member's slice, plan and units;
the workgroup spec and contracts must satisfy all member charters. Unqualified
`charter#n` means the member's own rule; `<project>/charter#n` names another
project's rule. Unknown projects or rules are refused. Charter conflicts go
to the owner and cannot be dismissed by the chief. Amendments are recorded
and decided in the project whose rule changes.

## Template

`osmia project add` records a template with guidance headings for scope,
dependencies, testing and pull-request shape. The guidance sits in HTML
comments. The template states that the repository's own contributor documents
(CONTRIBUTING, AGENTS.md, CLAUDE.md and similar) are binding. It contains no
rules, so a new charter is empty.

Budgets, capacity, models and other factory settings belong in
[configuration](configuration.md), not in the charter. The template says so;
Osmia does not check it.

## Rule format

A rule is a Markdown ordered-list item written as `N. text`, indented at most
three spaces, under any heading or none:

```markdown
## Testing

1. Every bug fix comes with a test that fails without it.
2. Run the full suite before opening a pull request;
   a flaky test is reported, never retried silently.
```

- A rule is numbered by the number you write, across the whole document. The
  citation form is `charter#<n>`, for example `charter#2`.
- Lines that follow an item continue its text. A blank line, a line holding
  only an HTML comment, a heading, a bullet or a code fence ends it.
- Headings, other text, bullets, `N)` items, HTML comments and fenced code
  blocks are not rules. An item with no text is not a rule.
- Each rule records the nearest heading above it.

Numbering problems are reported in `osmia status` and never stop Osmia from
reading the charter:

- A gap (for example rules 1, 2 and 4) is reported with the missing numbers.
- A number used by more than one item is reported with its lines. That number
  cannot be cited until you renumber it; the other rules still can.
- An item with no text is reported.

A charter is **empty** when it has no rules. Guidance alone is empty.

## Owner edits and revisions

The charter is a trace document, and every change to it is a revision.
Creation records the template as revision 1. `osmia edit charter <project-id>`
opens the latest revision in `$VISUAL` or `$EDITOR`, or reads it from a file
with `--from <file>`, and records what you save as the next revision, with the
owner as actor and cause `owner-edit`. The web UI's **Project management** view records an
edit the same way through the project's one charter editor. Each edit names the revision it started from; if another
revision was recorded since, the edit is refused as a conflict and nothing is
recorded, so you edit again from the latest. Saving unchanged content records
nothing. An untouched web editor follows new revisions live. An editor with
unsaved edits keeps its draft and original pin, shows that the charter changed
elsewhere, and keeps a stale-save refusal until **Read charter** explicitly
loads the latest text. See the [command line](cli.md).

## Rules from your rulings

When a ruling you gave a workstream is a standing rule for the project, its
chief of staff proposes the rule for the charter. You ratify or decline it with
`osmia charter` or by telling the chief of staff (see the
[command line](cli.md)).
A ratified rule is appended to the charter by the service, not by you: it
takes the number after your highest rule and goes under a `## Standing rulings`
heading at the end, which is added when the charter does not already end under
it:

```markdown
## Standing rulings

7. Uploads resume after a restart. <!-- ratified from workstreams/w_…/questions/3/rulings.jsonl revision 2 -->
```

The comment names the ruling the rule came from; it is not part of the rule's
text. The revision that appends the rule is recorded with the actor
`service`/`charter`. If you edit the charter while the rule is being written,
your edit is recorded as your revision and the rule is appended to it as the
next revision.
Every later bundle on the project carries the ratified rule as a notice. A
declined proposal leaves the charter as it is.

## Status and hand-in

`osmia status` shows whether the charter is ready (has at least one rule), how
many rules it has, the recorded revision and any numbering diagnostics. The
same data is `project.charter_state` in the `/v1/config` response.

`osmia handin` checks the charter first. With an empty charter it fails with
`charter_empty`, naming the project and the command that edits its charter,
`osmia edit charter <project-id>`, and creates nothing. See the [command line](cli.md) for the rest of hand-in.
