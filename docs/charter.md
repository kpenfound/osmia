# Charter

Each project has one charter, `<root>/projects/<project-id>/charter.md`: your
rules as a contributor to that project. You edit the file directly. Plans,
reviews and the committee cite its rules, so Osmia refuses to hand in work
while it has none.

## Template

`osmia project add` writes a template with guidance headings for scope,
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

The charter is a trace document (see [trace](trace.md#charter)). Creation
records the template as revision 1. Whenever the service reads the charter
(`osmia status` and hand-in), it compares `charter.md` with the latest recorded
revision. If they differ, it first records the file as a new revision with the
owner as actor and cause `owner-edit`, then uses that revision. Reading again
without an edit records nothing. Every edit you make is versioned before any
reader sees it.

## Status and hand-in

`osmia status` shows whether the charter is ready (has at least one rule), how
many rules it has, the recorded revision and any numbering diagnostics. The
same data is `project.charter_state` in the `/v1/config` response.

`osmia handin` checks the charter first. With an empty charter it fails with
`charter_empty`, naming the project and the path to `charter.md`. See the
[command line](cli.md) for its current behaviour.
