# status — native Mathcad Prime MCP server

Template: full-delivery. Orchestrator: main conversation. Branch: feat/mathcad-mcp.
State: DESIGN done + acceptance panel RUNNING. User priority (surfaced 2026-07-08 after a 2-week gap;
was consulted 2026-06-23 + deferred 2026-06-24 PR#426 for 3 blockers the native design closes).

## Decision
Native Go + go-ole late-binding COM, in-hub `internal/mathcad` subcommand (like godbolt/gdb). NOT the GPL
Python fork, NOT C#/.NET (evidence F1-F3: full surface is late-binding IDispatch → Go viable; C# would add a
.NET runtime against the hub's Python→Go trajectory). Full design: design.md (this dir).

## User's REAL requirements (2026-07-08) — the value is COMPUTATION, not worksheet CRUD
- CORE: `eval_expression` (units OFF, COMPLEX mandatory), `integrate_2d_region` (adaptive, TRIANGLE region, TOL→1e-12).
- Fail-loud on solver non-convergence (NEVER a silent number). TOL to the 1e-12 class.
- Desired: `sweep` (param-table → CSV, one call), `find_root` (bracket), special fns (elliptic K/E, Bessel, spherical Bessel, erf).
- Per-call timeout + cancel. WARM Prime instance (cold start kills sweeps).
- Mechanism: Prime has NO eval-expr API → template `calc.mcdx` with alias contract (in_expr/in_pN/in_tol/out_val/out_err/out_converged).
- FEASIBILITY CRUX (host-smoke gate before full build): does Prime give adaptive-2D-triangle-to-1e-12 WITH a
  convergence signal via string-expr into an alias? Acceptance test = ∫∫1/R³ unit triangle, obs (1/3,1/3,1e-3)
  → **6265.5263429603** @ TOL 1e-9, converged=true.

## Plan
Acceptance panel (fable+sonnet+codex) → host feasibility-smoke (the acceptance test) → phased build P0-P6.

## MATLAB MCP — QUICK-CONNECTED 2026-07-08 (direct, for immediate work) + hub-catalog DEFERRED to plan
- Host: MATLAB **R2025b** confirmed at `C:\Program Files\MATLAB\R2025b` (registry MathWorks\MATLAB 25.2; NOT on PATH).
- **DONE (quick):** official MathWorks server `matlab-mcp-server-windows-x64.exe` v0.11.1 downloaded to
  `C:\Users\dima_\.local\bin\matlab-mcp-server.exe`; added to CLAUDE user-config (`claude mcp add -s user matlab`)
  with `--matlab-root "C:\Program Files\MATLAB\R2025b" --matlab-display-mode nodesktop --disable-telemetry true`.
  `claude mcp get matlab` = ✔ Connected. Server starts MATLAB lazily on first tool call. (go install is broken —
  the module go.mod has exclude directives; release binary is the supported path.)
- NOT done (deliberate, user: "в магазин хаба в план потом, чтобы не распыляться"): the **hub-catalog/Store
  integration** — a `servers/matlab/manifest.yaml` (kind:global, stdio-bridge to the matlab-mcp-server binary) +
  a MATLAB-install file_glob probe (`C:\Program Files\MATLAB\R*\bin\matlab.exe`) so it's hub-managed + appears in
  the GUI Store like the built-in servers. DEFERRED to the plan (small separate PR). Also optional: add to codex config.
- License: official MathWorks OSS (LICENSE.md in the repo) — driven directly, not redistributed by us; clean for direct-connect.

## ============ DURABLE QUEUE (do NOT lose on compaction) — the pre-MCP work still pending ============
After Mathcad (commission-ranked 2026-07-08, fable+sonnet+codex consensus):
1. **E — #517 F1** (security): wrong-owner-relax class survives in 3 sibling client-config CREATE lanes
   (secureCreateClientConfigIfMissingWithOperatorOpt / SecureCreateClientConfigParentDir / SecureCreateParentDirForConfigLock,
   internal/api/client_write_init.go). Their inner-verify wraps use %v not %w → ErrWrongOwner FLATTENED. Fix = %v→%w
   in the create-lane wraps + predicate adoption. Surgical, near-zero risk. (fable+sonnet both confirmed at HEAD.)
2. **D — adopt Area-3** (correctness): an explicitly-requested client whose config EXTRACTION errors survives the
   mismatch/disabled filter (adopt.go:303-306 continue → none of Found/Matching/Mismatched/Disabled); harden to an
   explicit `Errored` exclusion bucket. Surgical. Bundle with E (both tiny, "don't split tiny PRs").
3. **A — adopt auto-reaper PR5** (the big raison-d'être lever): kill orphaned bypass npx-stdio servers; accepted gate
   = config-file-absence AND parent-dead(STILL_ACTIVE) AND age AND identity-reverify (decision 2026-07-08; pipe-peer
   rejected). Kill-authority primitives (TerminatePIDWithIdentity) shipped + proven. Dry-run-first + operator-confirm.
   Design accepted (work-items/active/2026-07-05-adopt-npx-orphans/). fable+codex top pick.

Also parked (documented): adopt de-adopt/revert-to-native phase-2 (needs-design; interim `mcphub uninstall --server`);
adopt anti-drift GUI surface (sonnet: already shipped by #516 — close stale status line); #517 5×P3 pagination/redaction
hardening follow-ups (TRIAGE-2026-07-08.md); §11.3 metrics (G, needs-design+prereq-blocked) + E2 daemon-intent deletion
(H, upgrade-floor-gated 0.5.0/1.0) — both DO-NOT-DO-NOW per commission.

Sources of truth: work-items/bugs/TRIAGE-2026-07-08.md (committed master), work-items/active/2026-07-05-adopt-npx-orphans/status.md
(committed master), work-items/decisions/2026-07-08-pipe-peer-unreliable-reaper-gate.md.
