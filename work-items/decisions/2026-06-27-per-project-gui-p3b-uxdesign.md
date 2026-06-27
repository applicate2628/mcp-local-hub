# Decision: per-project-GUI P3b UX design (frontend toggle UI)

status: accepted
date: 2026-06-27
owners: ux-designer (a324f87e, PASS) + lead (O-1 accepted)
parent: work-items/decisions/2026-06-25-per-project-gui-p3-design.md
partially-settles: work-items/backlog/2026-06-25-p3b-reenable-value-source.md (security/value-source question settled; D2 cold-re-enable UX still OPEN — the Re-add CTA links to a bare #/add-server, not a pre-filled restore)
live binary at design time: 12990df1 (P3a deployed — /api/projects aggregate + /api/projects/toggle present, no frontend consumer)

> **SHIPPED-REALITY UPDATE (#434 r2, live 8c359a1c):** the WARM re-enable path
> described below was a design-time assumption that was **REMOVED before merge**.
> The aggregate NILs every `raw` (`stripClientEntryRaw`), so the client never
> receives a value to hold and the warm replay was dead on arrival. Object-member
> re-enable (cursor/vscode) is therefore **ALWAYS COLD** in the shipped build —
> the per-row toggle only ever DISABLES; re-enable is exclusively the "Re-add…"
> CTA into the Add-server/Catalog flow. The original 3-step ruling is kept below
> for design-history honesty; read step 2 (warm) as superseded/never-shipped.

## CORE RULING — object-member re-enable value-source (SHIPPED = cold-only)
Per-row toggle for an OBJECT-MEMBER substrate (cursor `.cursor/mcp.json` + vscode `.vscode/mcp.json`, scope `project-object-member`). NOTE: the claude Project `.mcp.json` row is NOT an object-member toggle — it uses the approval ARRAY-MOVE (`claude-local-membership`), so it needs no value and never deletes the `.mcp.json` definition (see "KEY CORRECTION" below, shipped per #434 r2). The ruling here applies ONLY to cursor/vscode.

**SHIPPED behavior:** Disable is always available and needs no value; re-enable is ALWAYS COLD — a NON-toggle CTA "Re-add…" routing to the existing Add-server/Catalog flow (value sourced from marketplace/manifest + vault secret:<key> refs), NOT a backend-echoed value. **Cold object-member re-enable via the per-row toggle is DEFERRED (D2).** Rationale: honors the dropped-toggle_value security constraint (backend never re-sends secret-bearing Raw); the value comes only from where the secret already legitimately lives (manifest/marketplace via the Re-add flow). The aggregate stays NAMES-only.

**Design-time 3-step ruling (steps 1 + 2 SUPERSEDED — NOT SHIPPED, kept for provenance):** the original design assumed the UI could hold the just-disabled value client-side and replay it warm. #434 r2 removed that machinery (the aggregate NILs `raw` via `stripClientEntryRaw`, so the client never receives a value to hold), leaving cold-only.
1. **Disable** [SUPERSEDED — NOT SHIPPED part: the client-side value-hold was removed in #434 r2; disable still works but holds no value]: always available; in the original design the UI held the just-disabled member value CLIENT-SIDE. In the shipped build disable needs no value AND keeps none.
2. **Warm re-enable** (same session, value held): replay the client-held value into POST /api/projects/toggle {enable:true, value:<held>}. **[SUPERSEDED — NOT SHIPPED: removed in #434 r2; the aggregate nils `raw`, so no value is ever held.]**
3. **Cold re-enable** (the ONLY shipped re-enable path for object-member): the "Re-add…" CTA described under SHIPPED behavior above. **DEFERRED (D2)** as a per-row toggle.

## KEY CORRECTION (verified vs code)
claude-code dual substrate:
- **Project (.mcp.json, checked-in):** toggled via the APPROVAL ARRAY-MOVE — scope `claude-local-membership` moves the name between enabled/disabledMcpjsonServers in ~/.claude.json; the .mcp.json DEFINITION is never deleted → re-enable trivial, NO value needed.
- **Local (~/.claude.json projects.<key>.mcpServers defs):** has NO P3a write owner → **READ-ONLY in P3b v1 (D1 deferred).** Toggling it would mean delete/restore a member on the catastrophic ~/.claude.json corruption surface.
So the frontend picks SCOPE not owner: claude Project rows → `claude-local-membership`; cursor/vscode → `project-object-member`; workspace → `workspace-lsp`; group → `group-servers`. The GUI never re-derives ownership (decision 5).

## §10.2 both-scopes (claude-code card = two labeled subsections)
- **Project (.mcp.json — checked-in, shared):** live toggles, ON/OFF from ScanEntry.ProjectEnabled (*bool); write `claude-local-membership`. ProjectEnabled==null = anomaly → disabled toggle + "state unknown", never guess ON.
- **Local (~/.claude.json — private):** READ-ONLY list (no write owner). Raw approve/disable arrays in a collapsible "Advanced: raw approval state".
- **Shadow** (ProjectShadowedByLocal==true): rendered ONCE Local-owned. Project subsection shows a muted non-interactive ⊘ row "shadows the .mcp.json entry → see Local below" (anchor); Local subsection shows the authoritative row "shadows the Project .mcp.json '<name>' entry". Never two competing rows. Frontend reads the two *bool flags, never re-derives precedence.
- cursor/vscode = own cards, flat live-toggle lists (single substrate).

## Per-row IMMEDIATE toggle (decision 8 — NOT matrix dirty/Apply)
State machine: idle → toggling(optimistic flip + per-row spinner, that control disabled, others interactive) → on 200: reconcile to response.Enabled (NOT intent — idempotent/clamp self-corrects) + ✓ flash + surface Warnings[] quietly → idle; on non-2xx: REVERT optimistic flip + row-scoped inline error (§ code map) + Retry. mountedRef-guard every post-await setState. No auto-poll while any row toggling. Debounce double-click (disabled control).

## §3.1 stable-code → plain-copy (no raw codes on screen; code in tooltip)
- 400 PROJECT_TOGGLE_INVALID → "Couldn't change this — a required field was missing." revert+Retry
- 400 PROJECT_TOGGLE_UNSUPPORTED → "This client has no project-local config here — manage it in the client." revert; hide toggle
- 400 PROJECT_ROOT_INVALID → "This project's root could not be read — may have moved/deleted." revert+Retry+reload
- 400 PROJECT_TOGGLE_UNKNOWN_SERVER → "That server isn't a known routable server." revert; NO Retry
- 404 PROJECT_TOGGLE_GROUP_NOT_FOUND → "That group no longer exists — refresh." revert+Reload-section
- 500 PROJECT_TOGGLE_FAILED → "The change couldn't be saved. Retry, or check the app log." revert+Retry

## Deps-consent on ENABLE (decision 7 — reuse, no fork)
GET /api/server/readiness → if ready, proceed; if blockers, ConsentGate: ReadinessPanel(report) + per-secret "Set <key>" → AddSecretModal + "Open Secrets"; Confirm-Enable disabled while readinessBlockerCount>0 (the SAME single-owner predicate Catalog uses); AddSecretModal save → re-fetch readiness (bump reload token). Readiness-fetch error = advisory, not blocking. Disable never gates.
**O-1 (ACCEPTED, lead):** for a bare project-only object-member with NO global manifest, getServerReadiness returns no report → SKIP the gate (proceed; backend toggle stays authoritative). Honest: don't promise a check that can't run. ux-reviewer to confirm.

## Screen = extend P1 ProjectDetail (Projects.tsx), data source = the single /api/projects aggregate
3 cards: [A] Workspace tools (Entries + Enabled toggle col, scope workspace-lsp) · [B] Project MCP config (per-client sub-cards: Claude Code dual-scope §10.2 / Cursor / VS Code flat) · [C] Group lens (per-server toggle scope group-servers; keep P1 "tools_hidden not a security fence" note + "not yet project-bound" copy until P3c). Keep MechanismBadge "backed by <file>" provenance everywhere. No Apply/dirty language anywhere.

## Acceptance criteria (frontend-engineer) — 11
single-data-source(aggregate) · single-owner-dispatch({client,scope}, never branch on client name) · immediate-per-row state machine(reconcile-to-response) · both-scopes claude card(Project toggle/Local read-only/shadow once) · consent-on-enable(O-1 skip-when-no-report) · object-member cold-readd-only [SHIPPED #434 r2: the warm-replay criterion was dropped — the aggregate nils `raw`, so object-member re-enable is cold-only; never enable-POST without value] · code→copy map · data-testids(projects-toggle-<scope>-<server>, projects-consent-gate-<server>, projects-shadow-<name>, projects-readd-<server>) · protected(Servers.tsx byte-unchanged, scan.go 0-diff, no new Client method) · mountedRef+req-id seq · `go generate ./internal/gui/...` after frontend changes (embed bundle).

## Deferrals from P3b v1 (D1 + D2 are OPEN residuals, not closed)
D1 claude Local-scope toggle (no write owner; SHIPPED read-only — `Projects.tsx` Local subsection is read-only) · D2 object-member cold re-enable via toggle — the shipped Re-add CTA links to a BARE `#/add-server` (`Projects.tsx:843`), NOT a pre-filled restore, so the member is not actually restorable from the toggle; tracked OPEN in `work-items/backlog/2026-06-25-p3b-reenable-value-source.md` · D3 group↔project binding filter (shipped in P3c #435) · D4 project-scoped Servers view (would touch protected Servers.tsx — not built).

## Gates: ux-reviewer (cold-readd honesty, shadow legibility, O-1, checked-in-vs-private, copy quality) + architecture-reviewer + qa-engineer (Playwright E2E on a Windows runner per CLAUDE.md — toggle happy/error-revert/reconcile-to-response/both-scopes/array-move-scope/cold-reenable/consent/group-name-gate/per-row-isolation/code-copy/section-scoped-ScanError). [SHIPPED #434 r2: warm-reenable dropped — object-member re-enable is cold-only.]
