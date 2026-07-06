# D-A: Supervisor may reap a verified-own port squatter

- **status:** accepted (text revised per $security-engineer REVISE 2026-07-02 — H1 audit-forensic fix + 3 precision edits; substance approved)
- **filed:** 2026-07-02
- **context:** supervisor lifecycle authority (P2a of `work-items/bugs/2026-07-02-supervisor-lost-child-quarantine-class.md`)
- **design:** `.plans/2026-07/plan(main)-2026-07-02_01-49_supervisor-lost-child-fix-design.md` §2 (Fable-5, code-verified)
- **gate:** `$security-engineer` pass on this text BEFORE P2a implementation; `$security-reviewer` mandatory on the P2a PR.

## Decision

On `port_owner_mismatch`, the supervisor is authorized to `TerminatePIDWithIdentity` (tree-kill) the foreign owner of a daemon's intended port **iff** a strict identity gate proves it is a **disowned mcphub daemon child for THIS task**. Otherwise it must NOT kill: a genuinely foreign process yields an honest `port-squatter-foreign` event + no restart churn; an unverifiable owner (OpenProcess ACCESS_DENIED, lookup failure) fails CLOSED (no kill).

**Adoption rejected** (recorded per design): adopting the squatter (seeding it like `hydrateControllerRunningStates`) would leave a reaper-less PID permanently on the synthesize path with generation bookkeeping that fights P1a. One identity-gated kill + one controller-serialized respawn is simpler and bounded.

## The identity gate (ALL required for `squatterOwnTask`)

> **Gate 5 is the SOLE task-discriminator, not defense-in-depth.** All mcphub daemons — and the GUI, supervisor, and CLI — share the SAME executable (`mcphub.exe`), so gate 3 (exe, handle-truth) cannot distinguish tasks or siblings. The argv gate alone separates THIS task from every other mcphub process; it must also reject sibling subcommands (`gui`, `supervise`, `status`, `daemon recover`) — all must classify Foreign. A substring/prefix bug here is a P0 friendly-fire hole.

1. `ownerPID > 0 && ownerPID != selfPID && ownerPID != entry.CurrentPID`.
2. **Tracked-sibling refusal:** any other task's `CurrentPID` or `OrphanPID` == ownerPID → Foreign (a port collision between two of our daemons must never resolve by killing the sibling).
3. **Exe gate, handle truth:** `process.PIDExecutableMatches(ownerPID, daemonExpectedIdentityExe(d.Command))` — QueryFullProcessImageName on a live handle; argv spoofing cannot beat it; a copied binary at another path mismatches → no kill.
4. **Identity read:** `process.LookupProcessIdentity(ownerPID)`; any error → Unverified (fail closed). Windows-only in v1; POSIX = observe-only.
5. **Argv gate, exact tokens:** global daemon → CommandLine contains `--server <d.Server>` AND `--daemon <d.Daemon>` as adjacent whitespace tokens; runtime-spec proxy → `--task-name <canonical task>` token pair. Exact per-token equality (`serena-b1` must NOT match `serena-b133f336`).
6. **Kill proof from observed values:** `PIDIdentityProof{PID, ExecutablePath, StartedAt}`; `TerminatePIDWithIdentity` re-verifies identity incl. start-time ON THE HELD HANDLE it kills (no PID-reuse window) and errors on ACCESS_DENIED → no kill.

## Security argument

Kill authority over an unspawned PID is gated on the conjunction of: (1) the port comes from operator-authored, owner-only-DACL `supervisor-intent.json`; (2) the process image IS our binary at the daemon's configured path, proven from a live handle; (3) argv names THIS task exactly; (4) the kill primitive re-verifies identity + start-time on the handle it terminates; (5) same-user only — no privilege enablement (`SeDebugPrivilege` forbidden), ACCESS_DENIED fails closed; (6) every verdict/kill audited to supervisor-events.log with full observed identity, with `command_line`/`executable_path` pre-bounded so a hostile squatter cannot force whole-body truncation and erase the record.

**Rate limit** (per goroutine-owned limiter): ≤1 identity lookup/30s and ≤3 reap attempts per failure window per task; beyond → downgrade to observe-only with `rate_limited: true` pointing at the recover verb.

**ACCEPTED deviation — two SEPARATE single-goroutine-owned limiters (OBS-1, PR-3 codex-P2 + security).** The F1 automatic trigger (the off-loop port-gate worker) and the liveness sweep (P2a `port_owner_mismatch` + F3 quarantine self-heal) each own a DISTINCT `squatterReapLimiter` instance, because they run on DIFFERENT goroutines (the controller's dedicated port-gate worker vs the liveness-monitor goroutine) and the limiter is deliberately lock-free — sharing one instance across goroutines would require adding a mutex the limiter's single-owner contract forbids. Consequence: the *combined* fleet ceiling for one task is ≤2 identity lookups/30s and ≤6 reap attempts per failure window (≤3 from each limiter), not the ≤1/≤3 a single shared limiter would impose. This is accepted as security-neutral: (a) the rate limit is a runaway-loop bound, not the friendly-fire boundary — the 5-gate identity check is (every reap, from either limiter, still targets a VERIFIED-OWN disowned child of THIS task, gates 1-2 exclude every tracked PID); (b) the two triggers are mutually exclusive in time for a given task (F1 fires while the daemon is in backoff/respawn; F3 fires only while it is StQuarantined); (c) `pruneWindow` is time-based (a sliding `respawnFailureWindow`), so oscillation cannot accumulate reaps beyond the per-window cap on either side. The alternative (one locked shared limiter) trades a real lock-free-single-writer invariant for a 2× tighter bound on an already-verified-own kill — not worth it. The pre-P1 design ran F1's classify+reap inline on the controller event loop with F1's limiter loop-owned; the codex-P1 fix moved that work to the dedicated worker goroutine, and the limiter's ownership moved with it (still exactly one owner, still lock-free).

## $security-reviewer checklist (revised per $security-engineer memo 2026-07-02)

- Argv tokenizer cannot be confused into cross-task matches (test with prefix-overlapping task names, e.g. `serena-b1` vs `serena-b133f336`) AND rejects sibling subcommands (`mcphub gui` / `supervise` / `status` / `daemon recover` → Foreign).
- No `SeDebugPrivilege` anywhere; no retry-after-ACCESS_DENIED.
- Relax-lane threat delta is security-neutral: the reap's ONLY new primitive is "kill an existing same-user process matching our exe+argv," which is strictly subsumed by the intent-swapper's pre-existing arbitrary-spawn authority (`exec.Command(d.Command, d.Args...)` = arbitrary code execution as the user). The reap's trigger sits on the SAME trust boundary (`supervisor-intent.json`) as spawn, so it grants nothing that actor lacked.
- **H1 (load-bearing):** Reap-event bodies pre-bound every attacker-influenceable observed string (`command_line` AND `executable_path`, e.g. ≤2 KB each) BEFORE emit, so the forensic scalars (`squatter_pid`, `verdict`, `executable_path`, `started_at`) can never be evicted. NOTE: `supervisor_events.go:401-428` truncates the ENTIRE Body to a `_truncated_note` sentinel at 16 KB — it does NOT field-truncate — so an unbounded CommandLine/exe-path would let an attacker erase the killed-process identity from the audit. Field-level pre-bounding is mandatory; the event-log's own truncation is not a substitute. Test: oversized-CommandLine squatter still yields a body carrying `squatter_pid` + bounded `executable_path`.
- Sibling gate covers `OrphanPID`, not just `CurrentPID`.

## MUST-FIX implementation constraints ($security-engineer §4 — implementer + reviewer hold these)

1. Pre-bound attacker-influenceable strings before emit (H1 above).
2. Kill exclusively via `process.TerminatePIDWithIdentity` (never raw TerminateProcess/taskkill by bare PID); no ACCESS_DENIED retry.
3. Kill proof's `StartedAt`/`ExecutablePath` sourced from THIS classification pass's `LookupProcessIdentity` result — structurally impossible to build a proof without a fresh read.
4. Gate 5 exact per-whitespace-token match; prefix-overlap + sibling-subcommand tests; false-positive here = P0.
5. Gate 2 iterates BOTH `CurrentPID` and `OrphanPID` of every other tracked task.
6. POSIX/non-Windows → `squatterUnverified` (observe-only, no kill).
7. `port_owner_self` handled before the mismatch arm; classifier still asserts gate 1 defensively.
8. SHOULD: distinguish OpenProcess-failure (→ Unverified) from genuine exe-mismatch (→ Foreign) for accurate forensics.
9. SHOULD: global sweep-level reap counter in addition to per-task cap.
10. Regression: every existing sweep test asserting `port_owner_mismatch → EvManualRestart` must be updated — mismatch is no longer an unconditional restart; foreign/unverified post NO loop event.
