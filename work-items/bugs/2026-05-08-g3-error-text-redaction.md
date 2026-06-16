---
title: G3 failure banner renders raw probe/capability err text (workspace paths, token-like values possible)
severity: low
found-by: codex deep-sec r10 security lane on PR #144
found-on: 2026-05-08
project: mcp-local-hub
related-pr: #144 (G3 capability display)
---

# Capability error rendering is raw-text — not redacted

## Reproduction

1. Configure an MCP daemon whose `tools/list` response carries a
   workspace path or token-like value in its error message
   (e.g., `tools/list: stat C:\Users\<you>\secret\token.json: ...`).
2. Open `#/capabilities`. Observe the failure banner / inline
   probe-error pill renders the raw err text into the DOM.
3. Operator screenshot or browser dev-tools session captures the
   sensitive content.

Preact's text rendering escapes HTML so this is NOT an XSS vector.
The risk is information leakage via screen captures, browser
dev-tools, or shared support sessions.

## Current rendering surfaces

Three places render raw err strings:

1. `Capabilities.tsx::renderSectionErrors()` — `<strong>{e.scope}</strong>: {e.err}`
2. `Capabilities.tsx` failure-empty / partial-failures banners — same shape.
3. `CapabilityCard.tsx` — `<span class="capability-card-probe-err">{probeErr}</span>`
4. `CapabilitySection.tsx` — `<p class="capability-section-err">{sub.err}</p>`

All four pass the backend `err` string verbatim.

## Risk

Low. Operator-facing in a localhost-only tool. But:

- Workspace paths leak when err message includes file paths.
- Token-like values can leak if a misconfigured daemon includes
  them in error contexts (e.g., `auth failed: token sk-... rejected`).
- Cleanup-6 already addressed similar concern for cmdline rendering
  via basename redaction (PR #143).

## Fix proposal

Two-layer defense:

1. **Backend** (preferred): expose a separate `safe_err` summary
   alongside `err` in the JSON wire. `safe_err` carries an
   operator-friendly category (e.g., `"network"`, `"parse"`,
   `"timeout"`) without raw paths/tokens. Keep `err` for
   debug-only consumers.
2. **Frontend**: if backend stays as-is, apply a redaction filter
   client-side. Patterns:
   - `C:\Users\<name>\...` → `<homedir>/...`
   - `[A-Za-z0-9]{32,}` → `<token>`
   - workspace-name-like prefixes if detectable

Approach #1 is cleaner — single source of truth on the Go side.
Defer to G4 + threat-model gate (the same gate the spec deferred
tool-execution to); G3 ships as-is with raw err for consistency
with `mcphub cleanup` CLI which also shows raw cmdline.

## Plan

- Defer to G4 or a dedicated security-hardening PR.
- Coordinate with PR #143 cmdline-redaction pattern: the same
  helper-style redaction (apply at server boundary, expose
  display-safe field on wire) would work here.
- Add a regression test once the redaction lands: probe.err with
  workspace path + token → wire `safe_err` carries category only,
  not the raw value.

## Constraints

User explicitly scoped G3 as read-only consumer of G2 backend.
This redaction is a backend-side change (G4 or G3 follow-up) +
frontend update — out of G3 scope.
