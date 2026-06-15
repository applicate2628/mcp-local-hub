---
title: Servers.test.tsx expects "Columns (N/15)" but MatrixColumnsMenu renders "Clients (N/15)"
severity: low
found-by: frontend-engineer (adjacent finding during Discovery-view slice)
found-on: 2026-06-16
project: mcp-local-hub
context: adjacent-finding
status: open
related-pr: Discovery view (external remotes + Managed-by-hub flag)
---

# Two pre-existing Servers.test.tsx failures: button label text drifted

## Symptom

`npx vitest run` (and any run including `src/screens/Servers.test.tsx`)
fails exactly two tests:

- `ServersScreen — manual column visibility > renders the Columns button
  labelled with the visible/total count`
- `ServersScreen — manual column visibility > hiding a detected core column
  removes it from the matrix header and persists`

Both fail with:

```
AssertionError: expected 'Clients (7/15)' to contain 'Columns (7/15)'
```

(and the `6/15` variant).

## Root cause

`internal/gui/frontend/src/components/MatrixColumnsMenu.tsx:71` renders the
trigger button text as:

```tsx
Clients ({visibleSet.size}/{ALL_CLIENTS.length})
```

but `internal/gui/frontend/src/screens/Servers.test.tsx:628` and `:663`
assert the text contains `"Columns (7/15)"` / `"Columns (6/15)"`. The
component label and the test string have drifted apart — one of them was
renamed ("Columns" ↔ "Clients") without updating the other. The component's
own doc comment at `MatrixColumnsMenu.tsx:20` still says `"Columns (N/15)"`,
which suggests the COMPONENT text was changed to "Clients" and the test +
doc comment were left stale (so the test is the correct intent and the
runtime label regressed), but that needs the owning author to confirm.

## Evidence it is pre-existing (NOT introduced by the Discovery slice)

The Discovery slice touched none of these files — `git diff HEAD --name-only`
does not list `MatrixColumnsMenu.tsx`, `matrix-columns.ts`, or
`Servers.test.tsx`. The full vitest run is `2 failed | 557 passed`; the two
failures are exactly these and exist on `HEAD` independent of the slice.

## Fix proposal (deferred — out of slice scope)

Decide the intended label, then align the other two surfaces:

- If "Columns" is intended (matches the doc comment + test): change
  `MatrixColumnsMenu.tsx:71` from `Clients (...)` to `Columns (...)`.
- If "Clients" is intended: update the two `Servers.test.tsx` assertions and
  the `MatrixColumnsMenu.tsx:20` doc comment to "Clients".

Add no behavior change either way — this is a one-word label + matching test
fix. The orchestrator decides priority; not fixed here to keep the Discovery
diff scoped.
