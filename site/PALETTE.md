# manzanas brand palette — "big juicy apple"

Single source of truth for the red/yellow/orange apple palette used across the
site, brand assets, and the launch video. Defined as CSS variables in
`app/globals.css` and as the `manzanas` color scale in `tailwind.config.ts`.

| Token            | Hex       | Role                                          |
| ---------------- | --------- | --------------------------------------------- |
| `apple-red-700`  | `#a01c0f` | Deep red — dark ends of text/hero gradients    |
| `apple-red-600`  | `#c22214` | Primary red — links, primary buttons, focus    |
| `apple-red-550`  | `#d4361f` | Hover state for primary red                    |
| `apple-red-500`  | `#e0301e` | Bright accent — carets, dots, glows, icon body |
| `apple-orange`   | `#ff7f11` | Orange accent — icon blush, secondary accents  |
| `apple-amber`    | `#ffb02e` | Yellow-orange blush — glows, stem, highlights  |

Supporting tints/shades:

| Value     | Role                                             |
| --------- | ------------------------------------------------ |
| `#fdf1e7` | Warm cream tint (light diagram/panel fills)      |
| `#2b1410` / `#200d09` / `#130705` | Warm dark gradient for phone/code panes |
| `rgba(224, 48, 30, 0.10–0.12)` | Red radial glows              |
| `rgba(255, 176, 46, 0.08–0.10)` | Amber radial blush glows     |

Neutrals (`#1d1d1f`, `#6e6e73`, `#f5f5f7`, `#d2d2d7`, …) are unchanged from
the Apple-style light theme.
