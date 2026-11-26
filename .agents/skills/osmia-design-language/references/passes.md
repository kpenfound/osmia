# Quieter, distill and polish passes

Run these in order on a finished view, looking at screenshots at a desktop width and a phone width. Each pass leaves the view working and its test hooks intact.

## Contents

- Quieter
- Distill
- Polish

## Quieter

1. List what competes for attention on the screen: bright fills, borders, icons, badges, bold text.
2. Keep one primary action per region and one accent-coloured thing at a time. Demote the rest to secondary or quiet.
3. Remove borders that a surface step or a gap can replace. Lower the contrast of anything that is not the current task.
4. Replace colour-only emphasis with weight or position, and keep status colour for status.
5. Check that the owner's next action is still the first thing the eye lands on.

## Distill

1. For each element ask what the owner does with it. If nothing, remove it or move it to Activity, Settings or a disclosure.
2. Merge duplicates: one place to create a thing, one place for a setting, one way to show a state.
3. Cut copy to what is needed to act. Replace labels that restate the control. Prefer a specific verb to a generic one.
4. Replace separator strings and stacked tags with a laid-out pair of label and value, or with the nest tube.
5. Delete the CSS that no longer applies, and make sure no detail was dropped, only moved.

## Polish

1. Spacing and alignment: everything sits on the spacing scale, baselines line up and nothing is a pixel off.
2. States: hover, active, focus-visible, disabled, loading, empty and error exist for every interactive element.
3. Type and copy: sentence case, the right ramp step, the mono for identifiers, no all-caps, no joined meta strings, no ellipsis typed as three dots.
4. Tokens: no literal colour in a component, no `transition: all`, reduced motion honoured, status never carried by colour alone.
5. Touch and size: 44px targets on phones, 16px inputs, no horizontal page scroll at narrow widths, and a check at short heights.
6. Run the `web-design-guidelines` checklist over the files and fix what it finds.
