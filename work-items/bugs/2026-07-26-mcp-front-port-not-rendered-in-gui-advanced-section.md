---
title: mcp_front.port is declared in the "advanced" settings section but no GUI surface renders it
severity: low
found-by: codex bot review of PR #588 (P2, internal/api/settings_registry.go:214)
affected-surface: internal/gui/frontend/src/components/settings/SectionAdvanced.tsx (no registry-field rendering), internal/api/settings_registry.go (mcp_front.port entry)
context: adjacent-finding
status: open
---

## Symptom

`mcp_front.port` is declared with `Section: "advanced"` in `SettingsRegistry`,
but an operator cannot view or edit it in the GUI Settings screen. It is only
reachable through `mcphub settings {list,get,set}`.

## Cause

`SectionAdvanced` (`internal/gui/frontend/src/components/settings/SectionAdvanced.tsx`)
destructures its snapshot prop as `_` and renders only hard-coded controls
(open-app-data-folder, export-config-bundle, force-kill diagnose/apply, the
state-read-relax toggle, and `SectionAdvancedDiagnostics`). It contains no
`FieldRenderer` usage and no registry-field iteration, so declaring a key in
the `advanced` section does not surface it anywhere.

Note that `FieldRenderer` — the component the original (now corrected)
registry comment named — is currently referenced by NO section at all.
`SectionGuiServer` hand-rolls its own control markup against a hard-coded key
list, so `gui_server.port` is not rendered by `FieldRenderer` either.

## What was fixed on PR #588, and what was not

FIXED: the registry comment's false claim that the "advanced" placement "keeps
it visible in the GUI via the generic FieldRenderer". It is retracted and
replaced with an accurate description of the real (CLI-only) surface, plus two
guards in `internal/api/mcp_front_port_gui_visibility_pr588_test.go`:

- `TestMCPFrontPortSetting_IsManageableThroughTheSettingsCLI` pins the surface
  that DOES work (registry listing + set/get round-trip + range validation +
  resolver observation).
- `TestSectionAdvanced_StillHasNoGenericRegistryFieldRendering` is a
  cross-language drift gate that FAILS as soon as `SectionAdvanced.tsx`
  references `FieldRenderer`, `useSectionSaveFlow`, or `mcp_front.port`, so
  the corrected comment cannot rot a second time.

NOT FIXED (this item): the GUI control itself.

## Why the GUI control was deliberately not implemented in that change

PR #588 is a backend branch and the change was a bot-finding fix pass. Adding
an editable field to `SectionAdvanced` means wiring `onDirtyChange` +
`useSectionSaveFlow` into a component that currently has neither, then
regenerating and committing `internal/gui/assets/` via `go generate
./internal/gui/...`.

The blocking problem is verification, not effort. The Settings screen's
dirty-guard behaviour is covered by the Playwright E2E suite (per-section Save
isolation, the dirty-guard navigation prompt, and the discard-key remount on
intra-Settings confirmed-discard navigation — 16 settings smoke tests). That
suite spawns real `mcphub gui` binaries and could not be run in the
fix-pass environment, so a dirty-state-affecting change to that exact surface
would have shipped unverified against the tests that cover it. Landing an
unverifiable change to a heavily-E2E-covered surface is a worse outcome than
an accurately-documented gap.

## Suggested fix

Preferred: give `SectionAdvanced` a generic registry-field block (reuse
`FieldRenderer` + `useSectionSaveFlow`, as `SectionGuiServer` does for its own
keys), so that EVERY present and future `advanced` registry key renders without
a per-key frontend edit. That is the "right abstraction level" shape — a new
concrete setting should not force an edit to the section component.

Then, in the same change:

1. delete `TestSectionAdvanced_StillHasNoGenericRegistryFieldRendering`;
2. update the `mcp_front.port` comment in `internal/api/settings_registry.go`
   to describe the new GUI surface;
3. run `go generate ./internal/gui/...` and commit the regenerated
   `internal/gui/assets/`;
4. run the frontend unit tests, the typecheck, and the Playwright settings E2E
   specs (the dirty-guard cases in particular);
5. close this item.
