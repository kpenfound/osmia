# Tokens, type and density

## Contents

- Tokens
- Type and density

## Tokens

All colour lives in custom properties at the top of `internal/web/style.css`. A component never holds a literal colour; it names a token. A second palette must be able to override the tokens without touching a component rule. This skill states no hex values: colours are OKLCH, and the chosen values live in `style.css` and in [records.md](records.md).

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
