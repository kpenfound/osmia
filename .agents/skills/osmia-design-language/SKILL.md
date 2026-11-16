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

Both fonts are variable woff2 files, subset to Latin (Basic Latin, Latin-1, Latin Extended-A and B, Latin Extended Additional, general punctuation, the arrows and triangles the page uses). They live in `internal/web/fonts/<family>/` beside their `OFL.txt`, are embedded in `web.go`, and are served as `font/woff2` under `/fonts/`. The content-security policy allows `font-src 'self'`. Together they weigh about 100 KB, inside the 200 KB budget. `@font-face` uses `font-display: swap`, and each stack ends in the generic family only for the instant before the file arrives.

- **Sans: Instrument Sans.** A grotesque with a narrow, slightly technical cut and a width axis, so it reads as a tool rather than a marketing page. It stays legible at 14px in dense rows, has real semibold and tabular figures, and is not one of the reflex families. Licence: SIL Open Font License 1.1.
  - Source: `https://raw.githubusercontent.com/google/fonts/cc39c4c2c304bc368c9b7c311ea2bcc6013299fe/ofl/instrumentsans/InstrumentSans%5Bwdth%2Cwght%5D.ttf` (google/fonts at commit `cc39c4c2c304bc368c9b7c311ea2bcc6013299fe`, upstream project `https://github.com/Instrument/instrument-sans`, font file as published there, copyright 2022). Upstream TTF SHA-256 `b24f1812584816958afcf22e22d08e44318c5e51651e25d2438efdde389b33b1`.
  - Shipped file: `fonts/instrument-sans/InstrumentSans-Variable.woff2`, 66,424 bytes, SHA-256 `a31b722ba8808d930152087b16a164f047a26608cf49eb515d86f3aac8323b93`. Axes kept: weight 400 to 700 and width 75 to 100. Features kept: kern, liga, tnum, case, locl.
  - Licence file: `fonts/instrument-sans/OFL.txt`, SHA-256 `9e27a72ed30eb49a08678f6a5d6ed98ec7ba5368f541637ee0683ec9134ef966`.
- **Mono: JetBrains Mono.** Built for reading code at small sizes, with tall x-height and open letterforms. The 0/O and 1/l test passes: with the `zero` feature on, the zero is slashed and the capital O is plain; the 1 has a flag and a foot, the l has a curved tail, and the capital I has serifs, so each pair is different at 14px. Every mono rule in `style.css` sets `font-feature-settings: "zero" 1` and turns ligatures off, so IDs and hashes read as typed. Licence: SIL Open Font License 1.1.
  - Source: `https://github.com/JetBrains/JetBrainsMono/releases/download/v2.304/JetBrainsMono-2.304.zip`, version 2.304, file `fonts/variable/JetBrainsMono[wght].ttf`. Upstream TTF SHA-256 `662a196d58f1183bf2d77428b6d5283fe3f45161ab021bea4036bc98e5cac016`; release zip SHA-256 `6f6376c6ed2960ea8a963cd7387ec9d76e3f629125bc33d1fdcd7eb7012f7bbf`.
  - Shipped file: `fonts/jetbrains-mono/JetBrainsMono-Variable.woff2`, 33,340 bytes, SHA-256 `9e6a4ced86e830a95b95b83bd4a89f87b023da102bc9cf53e0a8a472f9fd244d`. Axis kept: weight 400 to 700. Features kept: zero, calt, ccmp, locl.
  - Licence file: `fonts/jetbrains-mono/OFL.txt`, SHA-256 `30f0c136e3c88e422d0791acd97238870f9054a9729bc34cf2ff0d4ed8cac4ad`.
- **How the files were made.** fontTools 4.68 with brotli: `fonttools varLib.instancer <ttf> wdth=75:100 wght=400:700` (mono: `wght=400:700` only), then `pyftsubset --unicodes="U+0020-007E,U+00A0-024F,U+0259,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+1E00-1EFF,U+2010-2015,U+2018-201E,U+2020-2022,U+2026,U+2030,U+2039-203A,U+20AC,U+2122,U+2190-2193,U+2212,U+2215,U+25B8,U+25BE" --flavor=woff2` with the layout features listed above. The sans has no triangle glyphs, so the disclosure markers in `style.css` are set in the mono.
- Inline `style` attributes are blocked by the page's policy (`style-src 'self'`), so a test that needs a size sets it through the CSS object model.

### Colour picks

Every colour is an OKLCH custom property in the one palette block at the top of `style.css`. Components use only the semantic tokens, and a test fails if a component rule holds a literal colour. A second palette overrides that block.

- **Tinted neutral.** Hue 258, a cool blue-slate, at chroma 0.012 to 0.02 for every surface, text and line. The hue echoes the dark blue-black of the orchard mason bee and keeps the accent from fighting the grounds. One hue means one family: nothing is pure black or white.
- **Surface ramp.** Lightness 0.175 (sunken: sidebar, header, code and input wells), 0.21 (base: the main area), 0.245 (raised: cards and controls), 0.285 (overlay: menus and dialogs), then 0.30 and 0.345 for secondary buttons and their hover. Steps are about 0.035 apart, enough to see and small enough to stay calm. Hover and selected are translucent light tints so they work on any step.
- **Text ramp.** Primary at 0.935, muted at 0.73 and faint at 0.60 lightness. Muted clears 4.5:1 on every surface step; faint is for placeholders and disabled text only. Lines are 0.33 (hairline) and 0.44 (an input's edge).
- **Accent.** Hue 245, lightness 0.74, chroma 0.115: a cool, slightly steel blue, calm rather than electric, after the metallic blue of the blue orchard mason bee. It marks the primary button, the selected tab, links, the focus ring and the unread dot. Text on it is a near-black blue (lightness 0.20). A soft variant (0.34 lightness) is the selection colour.
- **Clay secondary.** Hue 52, lightness 0.73, chroma 0.105: a warm clay, the colour of the nest's mud partitions. It is for rare emphasis that is not an action, such as the "before you ship" block and the "settings changed" dot. It never fills a button. It sits on a dark blue-slate, so it does not make the cream and terracotta look.
- **Status set.** Four tokens with soft backgrounds, each on its own hue and separate from the accent:
  - Live: hue 165, lightness 0.78, a settled green-teal.
  - Waiting: hue 85, lightness 0.80, chroma 0.07, a low-chroma sand, neutral to warm, used for pauses, a full capacity meter and saved-but-not-applied results.
  - Failed: hue 25, lightness 0.70, chroma 0.17, the one hard red, also the danger button.
  - Needs you: hue 95, lightness 0.86, chroma 0.15, a bright honey, the brightest colour on the page and used only for things the owner must answer. It is far lighter and stronger than the clay and the sand, so the two warm tokens do not blur.
  Status carries a glyph or a word as well as colour, never colour alone.
