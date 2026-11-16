---
name: osmia-design-language
description: Osmia's design language for the web page in internal/web. Tokens, type and density, buttons and components, how decisions, state and units are shown, the nest tube, the Conversation and Activity split, banned defaults, and the quieter, distill and polish passes. Load it to build or review any page change.
---

# Osmia design language

The page is a calm, dense tool for an owner who keeps it open all day. The conversation, decision cards and whatever needs the owner lead. Everything else recedes. Structure is felt, not seen: a surface step or a gap does the work a border used to do.

Boldness is spent in one place that comes from the product: the nest tube of units, and an Osmia-blue accent used sparingly. Everything around those stays quiet.

The page is dark only, in vanilla HTML, CSS and JS with no framework, build step or Node dependency. Use this skill with `frontend-design` (process) when building and with `web-design-guidelines` (checklist) when reviewing.

## Tokens

All colour lives in custom properties at the top of `internal/web/style.css`. A component never holds a literal colour; it names a token. A second palette must be able to override the tokens without touching a component rule. This skill states no hex values: colours are OKLCH, and the chosen values live in `style.css` and in the records below.

- **Colour space.** OKLCH everywhere, so a step in lightness reads as the same step on every hue. Tune lightness and chroma by hand; do not eyeball hex.
- **One tinted neutral.** Every grey (surfaces, text, borders) shares a single hue with very low chroma. No pure black or pure white, and no second neutral family.
- **Surface ramp.** Depth is a ladder of lightness steps, not shadows. Name the steps by role (sunken, base, raised, overlay) and keep the gap between neighbours small but visible. The sidebar sits one step below the main area; a card or control sits one step above what it rests on; a menu or dialog sits on the top step. Nest at most three steps deep.
- **Text ramp.** Primary, secondary and muted text on the same hue. Body text meets contrast on every surface step it sits on. Muted text still meets contrast for anything the owner must read.
- **Accent.** One cool, metallic blue after the blue orchard mason bee. It marks the primary action, the current selection, links and focus. If more than a few things on a screen are accent-coloured, none of them stand out.
- **Secondary.** A warm clay, for rare emphasis that must not read as an action. It never fills a button.
- **Status set.** Four states, each its own token (and a soft background variant): live, waiting, failed and needs-you. They are separate from the accent and from each other. Live is calm and settled, waiting is neutral to warm, failed is the one hard red, needs-you is the most attention-grabbing and is used only for things the owner must answer. Never carry meaning by colour alone: pair each state with a glyph or a word.
- **Borders.** A hairline in the tinted neutral at low contrast. A border stays only where it carries meaning: an input's edge, a focused or selected item, a contested unit, a divider between truly separate regions.
- **Radius and shadow.** Two or three radii by role (small for controls, larger for surfaces, round for pills and dots). Shadow only for things that float above the page, such as menus and dialogs.

## Type and density

- One sans and one mono, bundled as woff2 and served from the embedded assets. Never Inter, Roboto or a system default; never a silent fallback to system fonts.
- UI text is about 14px. Conversation prose is about 15–16px with generous line height. Inputs are 16px on touch devices so browsers do not zoom.
- Prose lines are capped at about 70ch. Tables and code may be wider and scroll inside their own box.
- Mono is for identifiers, commits, paths, code and anything the owner may copy. One monospace stack serves every ID, commit and code element. The mono must tell 0 from O and 1 from l.
- Hierarchy comes from size, weight and the text ramp, not from capitals. Sentence case everywhere: headings, buttons, labels, menu items, tabs. No all-caps.
- Numbers that line up or change use tabular figures.
- Density: tight vertical rhythm, compact controls on desktop, and 44px touch targets on phones. Space groups related things; large gaps separate regions.
- Metadata is laid out, not strung together. Use label and value pairs, a small grid, or a row of separate elements with a gap. Never join fields with a separator character.

## Buttons and components

Four button styles, one hierarchy per view:

- **Primary.** Accent fill. The one action the view wants. At most one per region, and in a decision card it is the recommended option.
- **Secondary.** A raised surface with a quiet edge. The other real choices.
- **Quiet.** Text with a hover surface. Navigation, dismiss, copy, expand and the long tail.
- **Danger.** The failed colour. Destroy, Abandon, Archive permanently and Remove project, always behind a confirmation that names the consequence. Danger is never the default button.

Component rules:

- Every interactive element has a visible `:focus-visible` state in the accent. Never remove an outline without a replacement. Hover and active states are more prominent than rest.
- Use `<button>` for actions and `<a href>` for navigation. Icon-only buttons carry an `aria-label`.
- Transitions list named properties and are short. Honour `prefers-reduced-motion`. Motion answers an action (opening, expanding, confirming); nothing animates on load.
- Native controls follow the theme: `color-scheme: dark`, dark selects, styled scrollbars.
- Containers earn their surface. Do not chop content into a row of identical cards. Group by surface step and spacing, vary the shape of things that differ in importance, and let a list be a list.
- Menus and dialogs are real elements (a native `<dialog>` is fine), trap and return focus, and close on Escape. A dialog names the consequence in plain words and puts the danger button beside a quiet cancel.
- Empty, loading and error states are designed. An empty state says what belongs here and what to do. A loading state keeps the layout from jumping. An error says what went wrong and how to fix it, without apologising.
- Forms share one layout: label above control, hint and error below, actions at the end.
- Keep every existing test hook (ids, `data-field`, `data-kind`, `aria-label`) under its original name when markup changes.

## Showing decisions, state and units

- **Decisions.** A decision card puts the question first. The recommendation is the primary button and the other options are secondary buttons. Revision, commit and "what was asked" details are a small mono footnote that opens in place, treated the same way on every card. "Decided by" reads the same way once answered. On a ratification card each objection carries its own Sustain and Overrule buttons. The delivery card reads as a release note: brief, findings, criteria, preview, and the publish button last.
- **Needs you.** Anything waiting on the owner shows the needs-you status and a count. The home view leads with a cross-workstream list of these, above the project cards. Sidebar rows show a state glyph and the count, not a text string of kind, project and state.
- **State.** A state shows as a glyph plus a word, in a status token. A workstream header carries a real state pill and a copyable mono ID. The connection is a dot, and only becomes a message when it is lost. Capacity, pauses and activity in the header share one visual language: small, quiet, same shape.
- **Units.** Units appear as the nest tube, not as counts or tags. A count such as "planned 2" is not a substitute.

## The nest tube

The signature element: a bee's nest tube divided into cells.

- One cell per unit, in plan order, in a single row that wraps or scrolls on narrow screens rather than shrinking below a tappable size.
- A cell is coloured by unit state from the status tokens. Unstarted units are the quiet neutral.
- A failing or contested unit is distinctly marked, with a shape or ring as well as colour, so it reads without colour vision.
- The unit's name shows on hover, on tap and as the cell's accessible name (`aria-label`), together with its state.
- A cell is a button. Activating it opens Activity filtered to that unit, through the URL state.
- The tube is the only decorative-looking thing in the header. No gradients, no glow. It is information, and it reappears wherever unit tags used to appear if it fits.

## Conversation and Activity

A workstream has two tabs, and the split is the point.

- **Conversation** is the default and holds what the owner reads and answers: owner messages, chief replies and actions, images, delivery briefs, owner command outcomes, and only the latest chief status. Decision cards sit above the composer. Machine events appear only as quiet one-line milestones: workstream state changes (ratified, building, assembled, delivered) and final review results. Tapping a milestone opens Activity at that entry. Sessions, unit transitions and check runs, failed ones included, never appear here.
- **Activity** holds every event the feed once showed, with no detail lost: sessions, unit and workstream transitions with reasons, check runs, final reviews and every chief status. Consecutive runs of events fold into an "N events" line that opens in place. The only filter is by unit; there are no event-type filters. Older chief statuses recede and the latest is clearly current.
- The tab, the unit filter and opened entries live in the URL, so loading a link restores the view and a debugging link can be shared.
- Unread markers count Conversation entries only.
- A workgroup keeps its shared conversation, and each member's machine events go to that member's Activity.

## Banned defaults

These are tells of generated pages. Do not use them in `internal/web`:

1. All-caps labels, including `text-transform: uppercase` and literal capitals.
2. Meta strings joined with ` · `. Lay the metadata out instead.
3. Rows of identical cards, one radius and one soft shadow on everything.
4. Cream backgrounds with a terracotta accent.
5. Inter, and the other reflex families (Roboto, system defaults).
6. Purple or other gradient washes as decoration.
7. `transition: all`.

Also avoid the other templated chrome the `frontend-design` skill lists: an eyebrow above every heading, accenting a single word in a heading, a trailing arrow on every link, a spaced em dash in labels and decorative numbering for content that is not a sequence.

## Passes

Run these in order on a finished view, looking at screenshots at a desktop width and a phone width. Each pass leaves the view working and its test hooks intact.

### Quieter

1. List what competes for attention on the screen: bright fills, borders, icons, badges, bold text.
2. Keep one primary action per region and one accent-coloured thing at a time. Demote the rest to secondary or quiet.
3. Remove borders that a surface step or a gap can replace. Lower the contrast of anything that is not the current task.
4. Replace colour-only emphasis with weight or position, and keep status colour for status.
5. Check that the owner's next action is still the first thing the eye lands on.

### Distill

1. For each element ask what the owner does with it. If nothing, remove it or move it to Activity, Settings or a disclosure.
2. Merge duplicates: one place to create a thing, one place for a setting, one way to show a state.
3. Cut copy to what is needed to act. Replace labels that restate the control. Prefer a specific verb to a generic one.
4. Replace separator strings and stacked tags with a laid-out pair of label and value, or with the nest tube.
5. Delete the CSS that no longer applies, and make sure no detail was dropped, only moved.

### Polish

1. Spacing and alignment: everything sits on the spacing scale, baselines line up and nothing is a pixel off.
2. States: hover, active, focus-visible, disabled, loading, empty and error exist for every interactive element.
3. Type and copy: sentence case, the right ramp step, the mono for identifiers, no all-caps, no joined meta strings, no ellipsis typed as three dots.
4. Tokens: no literal colour in a component, no `transition: all`, reduced motion honoured, status never carried by colour alone.
5. Touch and size: 44px targets on phones, 16px inputs, no horizontal page scroll at narrow widths, and a check at short heights.
6. Run the `web-design-guidelines` checklist over the files and fix what it finds.

## Foundations records

The foundations work fills these in. Each entry gives the pick and the reasons; the choice is a token change, so a later change updates the record here.

### Font picks

<!-- FOUNDATIONS: record the sans and the mono below. For each font: name, why it was chosen (character, the 0/O and 1/l test for the mono, size), licence, source URL, version, and the file name and SHA-256 of each woff2 file. State the total size. -->

- Sans: not yet recorded.
- Mono: not yet recorded.

### Colour picks

<!-- FOUNDATIONS: record the colour choices below with their reasons. Give the hue and the role of each token group (neutral, surface ramp, text ramp, accent, clay secondary, live, waiting, failed, needs-you) and why it was chosen. Describe values in OKLCH terms; do not write hex values. -->

- Tinted neutral: not yet recorded.
- Surface ramp: not yet recorded.
- Accent and secondary: not yet recorded.
- Status set: not yet recorded.
