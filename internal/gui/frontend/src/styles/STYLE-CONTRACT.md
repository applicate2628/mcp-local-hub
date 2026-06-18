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
