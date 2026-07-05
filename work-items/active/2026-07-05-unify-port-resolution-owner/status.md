# Unify supervisor port-resolution owner

Template: research → delivery (requiresLead: false)
Orchestrator: main conversation
State: **IMPLEMENTED (P1–P4 committed) → commission-fix phase (fable REVISE, F1 blocker open)**
Branch: `refactor/port-resolution-owner` (NOT yet pushed; NO PR yet)
Opened: 2026-07-05
Last synced: 2026-07-05 (recovered after a prior session died mid-commission — this file
was stale at "design pass dispatched"; the plan was in fact already implemented + reviewed)

## Problem (unchanged — full detail in design.md / plan.md)
The supervisor answered "what port does daemon X use" via TWO owners (persisted
`SupervisorDaemon.Port` + the status path's manifest read), so a `Port=0` legacy
descriptor structurally disabled the liveness bind-check, P1b deadline, P2a squatter
reap, and `mcphub daemon recover`. F5 (`BackfillIntentDaemonPorts`) was a write-pass that
warmed the cache and accreted special-cases (`Port>0||RuntimeSpec`, `server=="serena"`).
**Accepted fix:** one lazy owner `api.EffectiveDaemonPort` + `EffectiveStartupBindDeadlineSeconds`;
delete F5 and both special-cases. ADR `work-items/decisions/2026-07-05-daemon-port-resolution-single-owner.md` = **accepted**.

## Implementation ledger — ALL PHASES LANDED on the branch (git log)
| Phase | Commit | Subject |
|---|---|---|
| P1 | `0c36d02` | introduce the single port/deadline resolution owner |
| P2 | `017f019` | serena first-bind deadline via the owner, drop argv arm |
| P3a | `6e9980f` | liveness sweep + startup-scan resolve effective port |
| P3a-fix | `7f93a2a` | keep the startup running-scan port-only (P3a correction) |
| P3b | `21ab3a7` | squatter classifier resolves identity via the owner |
| P3c | `2d48c23` | recover CLI resolves effective port, drop 3-way hint |
| P3d | `ca4b736` | status path resolves via the owner, drop private memo |
| P4 | `d9d35f8` | **delete F5 backfill call site + dead ResolveManifestDaemonPort** |

Branch base = `d98e657` (#504 serena-guard, on master) — superseded by P4's F5 deletion.
Net: single owner in `internal/api/supervisor_port_owner.go`; F5 gone; ~−555 lines.

## Uncommitted working-tree changes (on disk — survive reboot; NOT in git history)
- `internal/api/supervisor_port_owner.go` (+9/−5) — arch-review F1+F2 fix
- `internal/cli/supervise_liveness.go` (+9/−2) — sweep uses the memoized `effDeadlineSecs`
  from the single `portResolver.Resolve(d)` call (no re-parse every 5s); docstring at
  `:313-315` references "arch review F1"
- `cmd/mcphub/resource.syso` — Windows build artifact, ignore
⚠️ **Worktree safety:** do NOT `git reset --hard` / `git checkout` these — the arch fixes
are only here, not committed. Commit them together with the fable fixes (below) next session.

## Commission (2026-07-05, on the branch) — ALL LANES IN
- **security-reviewer (opus): PASS** — verified-own-only kill invariant holds; the refactor
  TIGHTENS the trust boundary (new fail-closed field/argv-mismatch rule → `ok=false`).
- **architecture-reviewer (opus): REVISE → resolved.** F1 (sweep re-parsed manifest every 5s)
  + F2 (docstring) FIXED (the uncommitted working-tree changes above). Charter PASS on all 6.
  Its own F3/F4 (event logs port=0 for legacy rows; status-vs-sweep identity-threading
  asymmetry) → BACKLOG, non-blocking per reviewer.
- **codex lane: overflowed** (small context, no verdict) — angles covered by fable.
- **fable-5 (opus/xhigh deep): REVISE.** Findings below. This is the last lane; verdict in.

## Fable-5 findings — verified against HEAD this session (disposition)
Source file: `.scratch/fable-refactor-port-owner.out.md` — **UTF-16; direct `Read` triggers a
policy-kick (session drop). Read it ONLY via a cheap subagent or codex, never directly.**

- **F1 — P2, BLOCKER — CONFIRMED.** The post-force-kill orphan port-verify builds
  `expectedPorts` with `if d.Port > 0` at `internal/cli/install_migration_wiring_windows.go:87-91`
  and `internal/cli/migrate_serena_restart_windows.go:89-93`. F5's deletion leaves legacy
  intent rows `Port=0` forever → they drop out of the verify → a surviving orphan child still
  holding e.g. 9121 is never proven-unbound → the new supervisor spawns a duplicate →
  **EADDRINUSE**. On master F5 had backfilled `Port>0` so these verified. **Fix:** resolve each
  row via `api.EffectiveDaemonPort(d)` (append when `ok && port>0`); keep every fail-closed
  branch intact.
- **F2 — P3 — ALREADY FIXED** by the uncommitted arch change (sweep uses memoized deadline).
  Fable reviewed pre-fix code. No-op.
- **F3 — P3, minor — CONFIRMED.** `internal/cli/supervise_liveness.go:319-327` emits
  `daemon-port-unresolved` every sweep tick per unresolved row (no latch) → log spam vs F5's
  once-per-startup. **Fix:** once-per-(taskName, supervisor-session) latch, threaded through the
  same caller that owns `bindLatch` (persistent map; NOT function-local).
- **F4 — P3, CONFIRM-INTENT (open).** F5-deletion + serena-skip removal means legacy-unified
  serena@9121 (`Port=0`) is now under the bind-check with a 120s deadline. Fable notes a cold
  `uvx` + git-fetch can exceed 120s on a cold-cache host → `daemon-bind-timeout` → restart →
  quarantine, where master never touched that row. **Action:** confirm ADR §4b intended this
  (plan Phase 2 gave serena 120s exactly for this); decide whether 120s suffices for a cold uvx
  or needs a larger legacy-unified deadline / sweep-skip. NOT a code fix until intent confirmed.
- **F5 — P4, minor, display-only — CONFIRMED.** `internal/gui/daemon_env.go:266` uses raw
  `d.Port` → GUI `/api/daemon/env` shows Port=0 for legacy rows. **Fix:** `api.EffectiveDaemonPort(d)`.

## NEXT ACTION (resume anchor — post-reboot)
Codex prompt for F1/F3/F5 is **pre-written and ready**: `.scratch/codex-fable-fixes.md`
(file-based, xhigh, neutral; verifies then fixes the 3 confirmed findings; F2 marked do-not-touch).

1. Read the fable file via a **cheap subagent or codex** (never direct `Read` — policy-kick).
2. Apply **F1 (blocker)** + **F3** + **F5**. Confirm **F4** vs the ADR before deciding its fix.
3. Commit the uncommitted arch F1+F2 changes **together with** the fable F1/F3/F5 fixes
   (one clean commit, or arch-fix commit then fable-fix commit).
4. Pre-push local gate (CLAUDE.md): **back up live `%LOCALAPPDATA%\mcp-local-hub\supervisor-intent.json`
   first** (MEMORY `feedback_kosyak_subagent_test_wiped_live_supervisor_intent`), then
   `go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...` +
   `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`.
   Sweep `mcphub.exe` after (MEMORY `feedback_clean_test_processes`).
5. Re-verify the changed sites (fable was REVISE); commission was full — a targeted re-review of
   the F1 force-kill sites is enough, not a fresh full commission, unless surface grew.
6. `git push -u origin refactor/port-resolution-owner` → open PR → **Codex bot PASS loop**
   (mandatory final gate) → deep-sec agents → `gh pr merge --squash --delete-branch`.
7. **E2E per plan.md §Final end-to-end verification** (the genuine `Port=0` path is the
   load-bearing proof) + deploy discipline (build.sh → rename-aside → FULL supervisor restart;
   verify via `claude mcp get mcphub-hub`).

## Reboot-safety summary
- Committed impl safe at `d9d35f8`. Uncommitted arch fixes are on disk (survive reboot) — see
  Worktree-safety warning above.
- plan.md (620-line 6-phase delivery plan) + design.md (33KB) + ADR (accepted) all intact.
- HELD for reboot per user; no codex/fix/commit started this session.

## Optional follow-up (OUT of scope — needs re-admission)
Force-stop `Port=0` gap: `work-items/bugs/2026-07-05-stop-force-supervisor-port-zero-gap.md`
(`internal/api/stop_force_supervisor.go:155-182`). One-line `api.EffectiveDaemonPort(d)` fix once
admitted; `$security-reviewer`-gated (kill authority).
