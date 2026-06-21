# GUI button style contract (G5)

The single source of truth for button chrome in the mcphub GUI is the
`.btn` base + modifier set defined near the top of
[`style.css`](./style.css) (search for "Button design-system foundation").
This document is the **target** the rest of the screens migrate **to**;
when the `.btn` block in `style.css` changes, update this file in the same
edit so the two never drift.

Everything below rides the existing app design tokens (`--link`,
`--danger`, `--border`, `--sidebar-bg`, `--text`, the density-keyed
`--button-padding-*` / `--input-padding-*` scale). There are **no new
colors or spacing magic numbers** — the contract is purely a de-duplication
of the chrome that was copied into ~8 per-screen selectors
(`.dashboard-bulk-actions button`, `.card-actions button`,
`.catalog-card-actions button`, `.scan-rescan-btn`, the add-server toolbar,
the migration screen, the secrets table, the logs controls).

## Usage

A button opts in by carrying `btn` plus one variant class and (optionally)
one size class:

```tsx
<button class="btn btn-primary">Save</button>
<button class="btn btn-secondary">Cancel</button>
<button class="btn btn-danger">Delete</button>
<button class="btn btn-ghost btn-sm">Edit</button>
```

`btn` alone renders as the secondary/outline look; `btn-secondary` is the
explicit, documented name for that same default (prefer naming it at the
call site).

## Variants

| Class | Look | Use for |
|---|---|---|
| `btn-primary` | Accent-filled (`--link` bg, white text) | The ONE primary call-to-action per group: Save, Apply, Install. |
| `btn-secondary` (= bare `btn`) | Outline on `--sidebar-bg`, `--text` | Neutral / secondary actions: Cancel, Reset, Restart, Run all. |
| `btn-danger` | Outline, `--danger` text + softened danger border | Destructive actions: Stop, Delete, Uninstall, Force-kill. Stays outline (not filled) so it reads "careful", not "primary". |
| `btn-ghost` | Borderless, transparent until hover | Low-emphasis inline actions that should not draw a box. |

Pick exactly one variant per button. Do not stack `btn-primary` with
`btn-danger`.

## Sizes

| Class | Padding | Font | Use for |
|---|---|---|---|
| `btn-md` (default — omit) | `--button-padding-y` / `--button-padding-x` | 14px | Standard buttons. |
| `btn-sm` | `--input-padding-y` / `--input-padding-x` | 13px | Dense toolbars, compact card rows. |

`md` is the default; you do not need to add `btn-md` (it exists for the rare
case you want to name the size explicitly).

## Spacing scale

Button padding rides the density-keyed tokens so every button follows the
operator's compact / comfortable / spacious choice automatically:

- `md` padding = `var(--button-padding-y) var(--button-padding-x)`
  (comfortable: `6px 12px`; compact: `4px 8px`; spacious: `8px 16px`).
- `sm` padding = `var(--input-padding-y) var(--input-padding-x)`
  (comfortable: `4px 8px`).
- Inner icon/label gap = `var(--gap-xs)`.
- Border radius = `4px` (the app's button radius; the Settings-card buttons
  intentionally use `8px` — see "Out of scope" below).

A per-container delta (e.g. the Dashboard header's wider
`--cell-padding-y / --gap-md` padding, or `flex: 1` so two card actions
split a row) is allowed as a single extra rule scoped to
`.<container> button.btn { … }`, NOT by re-declaring the chrome.

## States

| State | Behavior |
|---|---|
| `:hover:not(:disabled)` | Secondary/ghost: border/background shift toward `--link` / `--border`. Primary: background → `--link-hover`. Danger: faint `--danger`-tinted background + full `--danger` border. |
| `:focus-visible` | `2px solid var(--focus, #0969da)` ring, `2px` offset — keyboard-only (matches the app-wide focus ring; pointer clicks draw no outline). |
| `:disabled` | `opacity: 0.5`, `cursor: not-allowed`. Transition narrows to opacity-only so the busy/disabled change fades rather than flickers. |
| transition | `background-color / border-color / color / opacity`, `.12s ease`. Never on the focus outline (the ring must snap in for accessibility). |

## Migrated so far (proof)

These three screens were converted to the unified base as the G5 proof.
Each kept its testids, button text, document order, and per-card button
count identical — the change is additive `btn …` classes plus removal of
the now-redundant per-container chrome:

- **Dashboard** bulk actions (Run all / Stop all) — `btn btn-secondary` /
  `btn btn-danger`.
- **Dashboard** per-card actions (Restart / Stop) — `btn btn-secondary` /
  `btn btn-danger` (the `flex: 1` split is the only per-container delta).
- **Catalog** card actions (Install / Cancel / Uninstall / Confirm
  uninstall) — `btn btn-secondary` / `btn btn-danger`.

## Still to migrate (target)

Convert these the same way (add `btn` + variant, delete the duplicated
chrome from the per-screen rule, keep only genuine layout deltas):

- Add-server toolbar (`.screen.add-server .toolbar button`, including the
  `.primary` save action → `btn-primary`).
- Migration screen (`.screen.migration button`, `.danger` → `btn-danger`).
- Secrets table actions (`.secrets-table button`, `.danger` → `btn-danger`).
- Logs controls (`#logs-controls button`).
- Scan / rescan control (`.scan-rescan-btn`).
- Marketplace install block (`.catalog-marketplace-install button`, already
  has a local `.btn-primary` convention to fold in).

## Out of scope (intentionally not migrated)

- **Settings-card buttons** (`section[data-section] button:not(.infotip-trigger)`)
  already have their own coherent, unified rule with `.btn-primary` /
  `.btn-danger` modifiers, an `8px` radius, and a `--card-surface` base
  (they sit on the elevated card, not `--sidebar-bg`). They are a separate,
  already-clean family; folding them into `.btn` would change their radius
  and surface and is a deliberate non-goal here.
- The `.infotip-trigger` (ⓘ) keeps its own pill style.

# Responsive layout contract (G14)

The GUI is launched by the tray at arbitrary window sizes — frequently a
half-screen / narrow window. The desktop shell (a fixed 220px sidebar +
the up-to-6-column `.servers-matrix` capped at `960px` + the Settings
two-column layout) does NOT reflow below those widths, so a narrow window
horizontally scrolls and clips. This contract defines the breakpoints and
the per-region narrow behaviour. Everything is **CSS-first** (media
queries); there is no JS layout switch — the existing `appearance.layout`
user setting is untouched.

## Breakpoints

| Token | Range | Name | Triggers |
|---|---|---|---|
| (none) | `> 768px` | **desktop** | The unchanged shipped layout. |
| `--bp-narrow` = `768px` | `<= 768px` | **narrow** | Sidebar collapses to a top bar; matrices become horizontal-scroll regions with a sticky first column; Settings collapses to one column. |
| `600px` | `<= 600px` | **xs** | Pre-existing env-kv / daemon-env-kv grid single-column collapse (kept as-is). |

`768px` is the single new narrow breakpoint. It is expressed as one
`@media (max-width: 768px)` block (search "G14 responsive" in
[`style.css`](./style.css)) so the desktop rules above the block are the
default and the narrow rules are purely additive overrides. The desktop
layout is never altered by this contract — at `> 768px` not one of these
rules applies.

## Scroll-ownership invariant (single scroll container)

The shell chrome NEVER scrolls. `html`, `body`, and `#app` carry
`overflow: hidden`, and the content track is a `minmax(0, 1fr)` grid
column with `min-width: 0` (and `#app` / `main` carry `min-height: 0`).
**`#screen-root` (the `<main>` element) is the ONLY scroll container on
every screen and at every breakpoint** — it owns `overflow: auto`. This
holds for all three layouts (sidebar, tabs, narrow): each one's `#app`
grid uses `minmax(0, 1fr)` for the content track so a non-shrinkable
child (a fixed-width control, a long unbroken token) can never blow the
column past the viewport.

Why `minmax(0, 1fr)` (not bare `1fr`): a grid `1fr` track has an
implicit `min-width: min-content`, so a wide child forces the track —
and the whole page — wider than the viewport, producing BOTH a
horizontal scrollbar AND a second (body) vertical scrollbar. `minmax(0,
1fr)` lets the track shrink below its content; the overflow then stays
inside `#screen-root`. Settings is the worst offender (fixed-width
`.field-ctl w-64` / `w-56` / `w-20` controls in `justify-between` rows),
so `[data-section] .field-ctl` / `.settings-section .field-ctl` are also
capped at `max-width: 100%` to shrink instead of widening the card.

The ONLY permitted secondary scrollers are these bounded-height widget
panes (each has its own `max-height` + `overflow`, scoped to its widget,
and never participates in page layout overflow):

- `#logs-body` (`max-height: 70vh; overflow: auto`) — the log viewport.
- `.add-server-preview pre` (`overflow: auto`) — the YAML preview block.
- `.secret-picker-dropdown` (`overflow-y: auto`) — the secret-picker
  combobox listbox.

Any new scroll container is a contract change: it must be a bounded-height
widget pane (not the page), justified here, and must not let the
horizontal/vertical page overflow escape `#screen-root`.

## Sidebar → top bar (CSS-only, layout setting preserved)

Below the narrow breakpoint the `#app` grid switches from
`220px 1fr` (two columns) to `auto 1fr` (two **rows**), and the
`.sidebar` is restyled in place as a horizontal top bar: brand inline on
the left, the `nav` flowing as a horizontal wrapping row of links. This is
the lowest-risk responsive move — it reuses the existing markup and the
existing topbar visual idiom (the same horizontal-nav look the
`data-layout="tabs"` setting produces) **without** mutating the user's
persisted `appearance.layout` and without any JS. The sidebar's
`border-right` becomes a `border-bottom`. The already-tabs layout
(`:root[data-layout="tabs"]`) is independent and unaffected — the narrow
block only restyles the default `.sidebar` shell, and the tabs `.topbar`
nav already wraps.

## Matrices → horizontal scroll + sticky first column

`.servers-matrix` (and `.servers-matrix.lsp-matrix`) on narrow:

- The `max-width: 960px` cap is dropped so the table keeps its natural
  width instead of squeezing the client columns to illegibility.
- The screen body (`#screen-root`, which already carries `overflow: auto`
  via the `main` rule) is the horizontal-scroll boundary — no DOM wrapper
  is added (keeps every existing testid + the test DOM intact).
- The **first column** (`th:first-child` / `td:first-child` — the Server
  name) is pinned with `position: sticky; left: 0` plus an opaque
  background and a right separator, so the operator scrolls the client
  columns while the row label stays visible. `position: sticky` on table
  cells needs `border-collapse: separate` to paint the sticky cell
  background reliably across browsers, so the narrow block switches the
  matrices to `border-collapse: separate; border-spacing: 0` and restores
  the single-border look with explicit cell borders (desktop keeps
  `border-collapse: collapse`).

## Settings + cards

- `.settings-layout` collapses from `110px 1fr` to a single column; the
  sticky section nav (`.settings-section-nav`) becomes a non-sticky,
  horizontally-wrapping row above the body so it never overlaps content in
  a short window.
- `.cards` already uses `repeat(auto-fit, minmax(240px, 1fr))` and the
  Settings cards are `max-width`-bounded, so they reflow to one column on
  their own; the narrow block only guarantees `#screen-root` padding
  shrinks so the cards are not clipped at the right edge.
