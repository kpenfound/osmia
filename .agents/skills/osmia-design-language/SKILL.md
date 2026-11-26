---
name: osmia-design-language
description: Defines Osmia's design language for the web page in internal/web, covering tokens, type, buttons and components, decision cards, the nest tube, the Conversation and Activity split, banned defaults, and the quieter, distill and polish passes. Use when building, changing or reviewing any page in internal/web, including CSS, markup and page copy.
---

# Osmia design language

The page is a calm, dense tool for an owner who keeps it open all day. The conversation, decision cards and whatever needs the owner lead. Everything else recedes. Structure is felt, not seen: a surface step or a gap does the work a border used to do.

Boldness is spent in one place that comes from the product: the nest tube of units, and an Osmia-blue accent used sparingly. Everything around those stays quiet.

The page is dark only, in vanilla HTML, CSS and JS with no framework, build step or Node dependency.

## How to use this skill

1. Building: follow `frontend-design` for the process (plan, check the plan for generic defaults, build, critique), and take every visual and structural decision from the files below.
2. Reviewing: apply the `web-design-guidelines` checklist, then check the change against this skill.
3. Finish a view with the passes in [references/passes.md](references/passes.md), in order.

Read only the file that matches the change:

| Change | Read |
| --- | --- |
| Colour, surfaces, text ramp, status colours, borders, radius, shadow, fonts, sizes, density, copy style | [references/tokens-and-type.md](references/tokens-and-type.md) |
| Buttons, focus, motion, forms, dialogs, empty and error states, decision cards, state display, the nest tube | [references/components.md](references/components.md) |
| The Conversation and Activity tabs, milestones, unread markers, workgroup events | [references/conversation-and-activity.md](references/conversation-and-activity.md) |
| Quieter, distill and polish passes on a finished view | [references/passes.md](references/passes.md) |
| The chosen fonts, files, hashes, and the OKLCH values of every colour | [references/records.md](references/records.md) |

## Rules that always apply

- A component never holds a literal colour; it names a token from the palette block at the top of `internal/web/style.css`.
- Colours are OKLCH. Every grey shares one low-chroma hue. No pure black or pure white.
- Depth is a ladder of surface steps, not shadows. Shadow only for things that float.
- One accent, used sparingly. Status colour is for status, and status never rides on colour alone: pair it with a glyph or a word.
- Sentence case everywhere. No all-caps.
- Units are shown as the nest tube, never as counts or tags.
- Every interactive element has a visible `:focus-visible` state, and motion honours `prefers-reduced-motion`.
- Keep every existing test hook (ids, `data-field`, `data-kind`, `aria-label`) under its original name when markup changes.

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
