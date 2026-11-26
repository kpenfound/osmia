# Foundations records

Each entry gives the pick and the reasons. The choice is a token change, so a later change updates the record here and the matching token in `internal/web/style.css`.

## Contents

- Font picks
- Colour picks

## Font picks

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

## Colour picks

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
