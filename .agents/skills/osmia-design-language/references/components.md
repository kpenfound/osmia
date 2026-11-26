# Components, decisions and the nest tube

## Contents

- Buttons
- Component rules
- Showing decisions, state and units
- The nest tube

## Buttons

Four button styles, one hierarchy per view:

- **Primary.** Accent fill. The one action the view wants. At most one per region, and in a decision card it is the recommended option.
- **Secondary.** A raised surface with a quiet edge. The other real choices.
- **Quiet.** Text with a hover surface. Navigation, dismiss, copy, expand and the long tail.
- **Danger.** The failed colour. Destroy, Abandon, Archive permanently and Remove project, always behind a confirmation that names the consequence. Danger is never the default button.

## Component rules

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
