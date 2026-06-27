# Decision: area-5 (trusted-folders-ux) design — closes install-and-it-works-ux epic

status: accepted
date: 2026-06-27
owners: analyst (a1049ab3) + architect (a32ed42a, PASS) + lead (gap-a option B accepted)
epic: work-items/epics/2026-06-19-install-and-it-works-ux.md (area 5 — LAST child)
sibling decision: work-items/decisions/2026-06-27-trusted-roots-shared-owner.md (keep-not-rename, proposed→arch-reviewer promotes)

## Shipped surface (REUSE, don't rebuild)
lsp-trusted-roots.json store + canonicalizeTrustedRoot + the predicate LSPWorkspaceRootTrusted (lsp_trusted_roots.go:288) + Bless/RemoveDefaultTrustedRoot; setup --trusted-root (cli/setup.go:419); LSP auto-bless-on-register (register.go:272 registerBlessTrustedRootFn); /api/lsp/trusted-roots CRUD + SectionTrustedRoots.tsx. THE LSP GATE: lsp_router.go:448, LSP-only.

## Scope corrections (verified, narrow the work)
- C-1: only ONE serena auto-register seam = attemptSerenaAutoRegister→AutoRegisterSerenaWorkspace (serena_router.go:1322). The :1678/:1968 sites are withSerenaWorkspaceGate RACE-gate forwards (decision 2026-06-20), NOT auto-register — untouched.
- C-2: the serena canonical root resolves INSIDE AutoRegisterSerenaWorkspace (resolveSerenaProjectRoot); the router only has the untrusted pathArg. So the serena trust gate MUST live in the backend (AutoRegisterSerenaWorkspace step 2.5), NOT the router.

## Gap-b — `mcphub trust` verb (additive, no security boundary)
NEW internal/cli/trust.go: `mcphub trust <path>` (→BlessDefaultTrustedRoot) + `mcphub untrust <path>` (→RemoveDefaultTrustedRoot) + `mcphub trust list` (→LoadDefaultLSPTrustedRoots + store path). Validation mirrors setup --trusted-root (validateTrustedRootArgs: non-empty+absolute for trust; non-empty for untrust). Pure cobra adapter over existing owners — zero new store logic. Server-neutral name (gates BOTH servers after gap-c).

## Gap-c — serena trust gate + bless co-design + keep-not-rename
- **Store: KEEP lsp-trusted-roots.json + LSPTrustedRootsFile UNCHANGED** (rename = orphaned operator stores + migration shim >> cosmetic benefit). Add ONE neutral predicate alias `WorkspaceRootTrusted(root)` == LSPWorkspaceRootTrusted, so serena's call reads server-neutral.
- **Gate placement:** inside AutoRegisterSerenaWorkspace, AFTER step-2 (root resolved + languages read) and BEFORE step-3 (per-key mutex / port / registry Save / interlock / reap-start). New `var serenaTrustedRootCheckFn = func(root)(bool,error){ return api.WorkspaceRootTrusted(root) }`. FAIL-CLOSED (nil seam / error / false → refuse). New sentinel ErrSerenaRootNotTrusted. Runs AFTER the DoS-bound marker check (ErrNotASerenaProject preserved — composes: no marker→not-found; marker+untrusted→ErrSerenaRootNotTrusted). Runs BEFORE any side effect → refused root writes nothing, can't perturb the §7.1 supervisor interlock.
- **Co-design (REGRESSION-SAFETY — gate+bless ship TOGETHER):** serena auto-bless on `mcphub workspace register --backend serena` (workspace_cmd.go after reg.Save() :291) via NEW `serenaRegisterBlessTrustedRootFn`→BlessDefaultTrustedRoot (best-effort/warn-only, mirror register.go:272). Else out-of-box serena auto-introduce (epic area 4) regresses. Bless reachable ONLY from explicit ops (register/trust/setup/GUI), NEVER the router auto-register path (a self-blessing router = the original vulnerability).
- **Router refusal mapping:** serena_router.go:1400 switch — ErrSerenaRootNotTrusted → 503/-32002 with actionable message ("workspace <path> not a trusted folder; run `mcphub trust <path>` or GUI Settings→Trusted Roots, then retry").

## Gap-a — DESCOPE prompt; take option B (lead-accepted)
Router serves MCP agents, not humans → no interactive prompt hook. Take **B**: fold a machine-readable `code:"NEEDS_TRUST"` + candidate path into the serena/LSP refusal JSON-RPC error `data` (cheap, additive, future-proofs one-click trust; sanitize path per catalog C0/C1/ESC strip — no unsanitized attacker path into logs/UI). The existing GUI panel + actionable error IS the add-UX. NOT building the pending-trust GUI inbox (option C) — it re-opens a durable attacker-influenced-write/DoS surface; available as a future P5c IF the user wants first-touch-prompting UX.

## Security (security-reviewer MANDATORY — trust-boundary EXTENSION)
The gate protects: arbitrary-path serena daemon spawn (uvx), supervisor-intent/registry mutation from untrusted input, supervisor-cutover abuse — all from a `.serena/project.yml`-marked attacker path. Stronger than the marker-only DoS bound (today a marked attacker path mutates state). Invariants: gate-before-mutation; bless-not-on-router-path; fail-closed-on-error; NEEDS_TRUST path sanitized.

## Protected: LSP gate (lsp_router.go:448) unchanged · the trust predicate/containment reused not re-derived · store format/filename unchanged (no migration) · withSerenaWorkspaceGate race-gate untouched · the serena DoS marker bound preserved · the GUI CRUD+panel reused unchanged · scan.go 0-diff.

## Tests
gap-b: trust/untrust/list seam-stubbed + validation parity. gap-c: untrusted-root→refused+ZERO side effects (the security test) / trusted→proceeds (existing tests default seam true, pass unchanged) / gate-error→fail-closed / ordering (no interlock-acquire on refusal) / serena-register-blesses-once / router maps ErrSerenaRootNotTrusted / REGRESSION-GUARD (bless via serena register → sibling auto-introduce succeeds). State-safe (back up live supervisor-intent before api tests).

## Phasing: ONE cohesive area-5 PR (lead decision — gap-b + gap-c gate+bless [together, never split] + gap-a-B metadata). security-MANDATORY + arch + qa + bot. Closing area 5 closes the install-and-it-works-ux epic.
