# Design — A2: adopt npx-stdio MCP servers into the hub + harden the orphan reaper

Architect design pass 2026-07-05 (gate PASS). Source bug:
`work-items/bugs/2026-07-04-npx-stdio-mcp-orphan-accumulation-bypasses-hub.md`
(root-cause CONFIRMED, fable-authored, file:line-backed). All seams re-verified vs HEAD.

Queued AFTER the A1 F-package (per the locked bug-backlog order): A2 impl is PR4/PR5.
This design is the Lane-β prefetch; a $security-engineer review + the two blocker
resolutions below must land before implementation.

## Problem
Direct `npx`-stdio MCP servers (mui-mcp, chrome-devtools, playwright, flowbite,
repomix, raindrop, …) are configured as DIRECT stdio entries in external client
configs (codex `config.toml`, `~/.claude.json`), NOT as mcphub daemons → they bypass
the hub (no Job Object, no supervisor), re-spawn per session, and orphan-accumulate
(~360 node.exe observed, 200+ a single server). Defeats mcphub's raison d'être. The
fix machinery already ships (`transport: stdio-bridge` runs any stdio command as a
supervised Job-owned HTTP-bridged daemon — how memory/fetch/time already run); what's
missing is an adoption seam + secret-safe env + a hardened reaper.

## Approach (3 parts)
- **P1 — `mcphub adopt <entry> [--client X|--all-clients|--all-unknown] [--yes]`** (dry-run plan by default; GUI "Adopt into hub" button on Discovery `unknown` rows → `POST /api/adopt`). Composes the EXISTING pipeline: `ExtractManifestFromClient` (scan.go:2208) → `ManifestCreate` → **`InstallParsedManifest` (additive MERGE path)** → rewrite client configs to the hub route (adapter AddReplace) + confirm-gated Remove of signature-matching cross-client duplicates. Manifest kind = `global`/`stdio-bridge` (existing; no new kind). Dedicated port range (e.g. 9200-9299) — the shared 9121-9139 pool is near-exhausted.
- **P0 — env→vault routing (security must-fix).** New `adopt_secret_route.go` between extract+persist: for each drafted env, if `api.IsSensitiveEnvName` (import_vscode.go:489) AND value is a literal → `secrets.Vault.Set(key,value)` (vault.go:156) + replace with `secret:<key>`; vault-unavailable → REFUSE (never persist the literal into `servers/<name>/manifest.yaml`).
- **P2 — harden the EXISTING reaper.** `CleanupOrphans` (cleanup.go:753) already signature-matches (T1 manifest / T2 client-stdio / T3 heuristic) but kills with raw `taskkill /F` (cleanup.go:864, no identity re-verify) and has no pipe-peer gate. Harden: kill EXCLUSIVELY via `TerminatePIDWithIdentity` (proof captured at scan); reap iff (1) exact T1/T2 signature (T3 report-only), (2) parent dead / recycle-guarded, (3) age>~10min, (4) **pipe-peer-liveness: no OTHER live process holds the candidate's stdio pipe** (SystemHandleInformation correlation; enumeration-fail → observe-only), (5) exclude supervisor-state PIDs + transient_pids + Job.HasMember + mcphub.exe. Audit every verdict (orphan-reaped/-skipped-live/-unverified). Distinct classifier — MUST NOT overload `classifyPortSquatter` (it answers "is this MY disowned daemon child", would classify orphans Foreign→never-kill).

## Security constraints ($security-engineer gates ALL three; $security-reviewer mandatory on P0+P2)
1. Config-mutation consent (P1): dry-run default; mutate only on --yes/GUI-confirm; plan enumerates every AddReplace/Remove/secret-route; no auto-adopt.
2. Secret-vault routing (P0): no sensitive literal to manifest; vault-unavailable refuses.
3. Kill-authority identity gate (P2): reap only via TerminatePIDWithIdentity; unverifiable/enum-fail → observe-only; foreign/live-owned → never; hub-owned/self excluded; verdicts audited. Highest-risk surface (kill authority over never-spawned PIDs).

## Hazards
- **H1** adopt reaps serena dynamic pool → mitigated by pinning the `InstallParsedManifest` MERGE path (never full-reconcile prune, never DaemonFilter=serena). H1b: adopt takes no supervisor lock / no migration.lock.
- **H2 (BLOCKS PR-1 — needs a probe):** codex `config.toml` is a symlink into dotfiles; `SecureWriteClientConfig`'s atomic rename at the destination NAME may replace the symlink with a regular file, silently detaching it. Must verify current behavior + decide resolve-target-vs-refuse.
- **H4** P2 must not overload classifyPortSquatter (distinct classifier).
- **H5 (BLOCKS PR-3 kill path — needs a spike):** pipe-peer SystemHandleInformation correlation reliability; fail-closed (enum-fail → observe-only).

## Phasing
- **PR4 = P0+P1 (prevention)** — adopt verb + BuildAdoptPlan + secret routing + GUI + dedicated ports. $security-engineer MANDATORY. **Blocked on H2.**
- **PR4b = anti-drift signal + S1 catalog rows** (mui-mcp/next-devtools/chrome-devtools/flowbite/raindrop/repomix) — additive; fold into PR4 or right after.
- **PR5 = P2 (reaper hardening)** — taskkill→TerminatePIDWithIdentity + pipe-peer + T3-report-only + exclusions + audit. $security-reviewer MANDATORY. AFTER A1's F-package. **Blocked on pipe-peer spike** (report-only tiers can ship without it; the raw-taskkill→identity fix is standalone-shippable — see `work-items/bugs/2026-07-05-cleanuporphans-raw-taskkill-no-identity-reverify.md`).

## Decisions to register (work-items/decisions/, status: proposed)
1. adopt-verb-and-intent-merge-path · 2. adopt-env-vault-routing · 3. orphan-reaper-kill-authority (sibling to A1 D-A) · 4. adopt-symlinked-client-config-policy (pending H2).

## Investigation-required before implementation-ready
- H2 symlink write behavior (blocks PR4). · pipe-peer spike (blocks PR5 kill path). · confirm F1/F3 coupling with A1 owner (sequencing).

Full architect package (14 sections incl. Change-Surface Contract + 10 falsifiable Claims + all file:line seams) captured in the 2026-07-05 architect session; reproduce from there for the planner when A2's turn comes.
