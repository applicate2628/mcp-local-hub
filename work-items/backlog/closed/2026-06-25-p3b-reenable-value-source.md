# P3b — design the object-member re-enable value-source SECRET-SAFELY

- status: closed (settled — see ## Resolution)
- created: 2026-06-25
- origin: PR #433 (per-project-GUI P3a, write phase) bot finding 4 — lead adjudication
- area: per-project-GUI, internal/gui (projects scan/aggregate), internal/api (ScanResult/ClientEntry)

## Context

P3a's `/api/projects/toggle` ENABLE for an OBJECT-MEMBER project substrate
(cursor `.cursor/mcp.json`, vscode `.vscode/mcp.json`, claude-code `.mcp.json`
Project scope) requires the caller to supply the full object-member `value` (the
config shape to set). P3a does NOT synthesize that value (synthesizing it would
absorb the migrate machinery — out of scope).

The r2 attempt added a `ClientEntry.ToggleValue` field (a verbatim copy of the
entry's `Raw` config fragment) to the project read API so the frontend could echo
the value back on a re-enable. That field was **REMOVED in P3a r3** (this finding)
because it re-exposed secret-bearing config that `sanitizeScanResult` strips
DELIBERATELY: `Raw` may hold hand-written literal secrets (`headers.Authorization`,
`env.TOKEN`, etc.). Even over a same-origin localhost transport, surfacing the raw
value bypasses an existing security control — the secret then lands in devtools,
network logs, screenshots, and bug reports. P3a's aggregate therefore stays
NAMES-only (the original security-clean shape).

This resolves the r1-finding-5 ↔ r2-finding-4 flip-flop: the value-source is a
P3b concern, NOT a P3a read-API field.

## P3b scope (to design)

Design the object-member re-enable value-source SECRET-SAFELY:

- **Redact raw secrets** per the same sanitize-Raw convention the global/project
  scan already applies — a literal `headers.Authorization` / `env.TOKEN` value
  must NOT reach the wire.
- **`secret:<key>` references MAY pass** — they are references, not resolved
  secrets (the scan never resolves them), so echoing a `secret:<key>` ref back is
  safe.
- **Design WITH the frontend flow.** The two re-enable cases differ:
  - *re-enable-in-session* — the value may be held client-side (the frontend
    still has the value it disabled, never round-tripped through a read API), so
    no backend value-source is needed.
  - *cold-enable* (fresh page / different session) — needs a redacted/structural
    value source from the backend, OR a different UX (e.g. operator re-enters
    secret values, or the structural skeleton is provided with secret fields
    blanked/marked).

## Backend already-built seam

The `/api/projects/toggle` backend ALREADY accepts a caller-supplied value
(`projectToggleRequest.Value`, dispatched to `clients.ToggleProjectObjectMember`).
P3b only has to source that value securely — the write side is done.

## Out of scope for P3b admission to reconsider

- claude-code LOCAL scope (array-move) needs NO value source — a re-enable moves
  the name between the enabled/disabled arrays; the `mcpServers` definition is
  untouched.

## Resolution (settled 2026-06-27)

Settled by the P3b UX design decision
(`work-items/decisions/2026-06-27-per-project-gui-p3b-uxdesign.md`, status: accepted),
whose frontmatter `settles:` this backlog item, in combination with PR #434's
warm-replay removal:

- The warm value-replay machinery (the proposed in-session client-held value path)
  was **REMOVED in #434** — the aggregate NILs every `raw` (`stripClientEntryRaw`),
  so the warm path was always a no-op. There is therefore no backend value-source to
  build, and the security constraint (never re-send secret-bearing `Raw`) is preserved
  by construction.
- **Cold object-member re-enable routes to the existing Add/Catalog flow** (value
  sourced from marketplace/manifest + vault `secret:<key>` refs), NOT a backend-echoed
  value — the aggregate stays NAMES-only.

Residual: **cold object-member re-enable via the per-row toggle is DEFERRED (D2)**,
tracked in the p3b-uxdesign decision's `## Deferrals from P3b v1` (D2). The claude
Project scope (array-move) needs no value source at all. No live work remains in this
item; it is closed/settled.
