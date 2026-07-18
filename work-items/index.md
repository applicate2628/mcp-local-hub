# Work items index

> Canonical open-bug truth: **`bugs/TRIAGE-2026-07-09.md`** (HEAD-reconciled against master
> `63b6a008`, amended for #525 at master `7898c148`, then amended for #526 at master
> `b5e6f6f3`). Individual bug-doc frontmatter used to lag reality; the top-level bug registry
> has now been normalized. Trust the TRIAGE + the "Truly open" list below, NOT a raw count of
> files in `bugs/` (33 files = 0 open + 28 fixed + 1 duplicate + 1 wontfix + 3 triage records —
> the last open bug was disposed open→fixed on 2026-07-10; see TRIAGE Amendment 3).

## Active (work-items/active/)

| Item | Status |
|---|---|
| [2026-07-16-productization-gui-solidify](active/2026-07-16-productization-gui-solidify/status.md) | Item 3 Unit B gated group: Phases D/E/F/I committed (default-OFF, not deployed); Option B/#562 integrated; Phase G parent coordinator implementation in progress at documentation-amended `beadf474` |

## Reaper kill-authority hardening — COMPLETE (2026-07-08, all bot-PASS + deployed)

| Fix | PR | master | Status |
|---|---|---|---|
| A2 PR5 auto-reaper (config-absence gate + snapshot fail-closed + T3 source-fix + reap_verdict presentation + identity-bind + specificity) | #520 | c53d874a | deployed + live-verified |
| Walk-uncertainty P0 (3-state fail-closed walk: tri-state QueryPIDState + self-loop + depth-cap) | #521 | 509afa31 | deployed + live-verified |
| Aggressive-token identity binding (token+started_at + {pid,started_at} kill-bind both surfaces) | #522 | 669951e3 | deployed + live-verified |

## Truly-open bugs (everything else in bugs/ is fixed/closed — status just not flipped)

**None — truly-open = 0.** All 30 top-level `bugs/*.md` docs are fixed/closed at master
`66b80ece`. The last open bug,
[2026-07-07-lsp-router-relay-entries-ignore-per-client-disable](bugs/2026-07-07-lsp-router-relay-entries-ignore-per-client-disable.md),
was disposed open→fixed by `$lead` on 2026-07-10 (backend #512 `c3fc1801` + GUI Servers-tab
per-client disable #524 `22c91cab`; see `bugs/TRIAGE-2026-07-09.md` Amendment 3).

Three triage batch files (`bugs/TRIAGE-2026-05-28.md`, `bugs/TRIAGE-2026-07-08.md`,
`bugs/TRIAGE-2026-07-09.md`) are
reconciliation records, not open tasks.

## Epics

| Epic | Status | Closed | Outcome |
|---|---|---|---|
| [2026-06-23-desktop-app-mcp-catalog](epics/2026-06-23-desktop-app-mcp-catalog.md) | closed | 2026-06-29 | DONE — desktop/creative-app MCP catalog, 52 rows, shipped npm v0.4.9. |
| [2026-06-19-install-and-it-works-ux](epics/2026-06-19-install-and-it-works-ux.md) | closed | 2026-06-28 | DONE — clean-install + hub-launch + per-project UX (#377/#407/#408/#437/#428–#435/#439). |

## Reference / parked (NOT open tasks)

- [2026-06-18-clean-install-ux-vision](active/2026-07-16-productization-gui-solidify/2026-06-18-clean-install-ux-vision.md) — source material for the active productization initiative.
- `.scratch/roadmap-scans/roadmap-scan-2026-06-14.md` — the local-only raw-inventory scan the
  `ROADMAP.md` audit header cites. Moved out of this tree 2026-07-16: it is gitignored working
  material, and `/.scratch/` is where local-only raw artifacts belong — a `.gitignore` rule hiding a
  file inside `work-items/` was papering over the wrong location. Superseded by `ROADMAP.md` for all
  live tracking; regenerate if absent.

## Archived (work-items/archive/<YYYY-MM>/)

| Item | Closed | Outcome |
|---|---|---|
| [2026-06-17-phase1-audit-findings](archive/2026-07/2026-06-17-phase1-audit-findings.md) | 2026-07-15 | Phase 1 arch+security audit. **HEAD-reconciled 2026-07-15 (the inline fixed-markers were stale):** 6/6 findings resolved-or-exempt (P1 state-fanout, config-path |
| | | double-encode, dual state-enum, hand-rolled atomic writers, path-validate raw-error — all FIXED with backing tests; backups path-echo EXEMPT-by-design). Finding 4's residual (4 GUI state-dir-error POSIX path-leak sites) |
| | | **FIXED 2026-07-15** — the 4 sites now route through `writeAPIErrorRedacted` + a `state_dir_error_redaction_test.go` structural guard; the backlog tracker is removed on delivery. See the doc's |
| | | HEAD-RECONCILIATION header for per-finding evidence. |
| [2026-06-18-vendor-init-uninstalled-clients](archive/2026-07/2026-06-18-vendor-init-uninstalled-clients.md) | 2026-06-21 | RESOLVED — secure-parent-create stack. |
| [2026-07-05-adopt-npx-orphans](archive/2026-07/2026-07-05-adopt-npx-orphans/closure.md) | 2026-07-16 | DELIVERED — absorb unmanaged direct-stdio MCP entries into the hub: `mcphub adopt` (CLI/API + GUI "Adopt into hub" surface, #513/#516), auto-reaper hardening (#520 + kill-authority bugs #521/#522), anti-drift "unmanaged detected" GUI signal (#523), and Phase-2 `mcphub de-adopt` (the separate `2026-07-09-deadopt-hub-to-native` item, #539-#550). All shipped + deployed. Post-delivery consilium qualification (2026-07-16, #554) closed the two open gate-ON bugs + added the adopt→de-adopt lifecycle round-trip test. The status-header "D P2a/P2b GUI" residual is a cross-ref to the per-project-GUI approval-surface initiative (decision `2026-06-24-per-project-gui-design.md` + backlog `2026-06-25-p2b-approval-surface-claude-json-only.md`), not this item. |
| [2026-07-09-deadopt-hub-to-native](archive/2026-07/2026-07-09-deadopt-hub-to-native/closure.md) | 2026-07-15 | DELIVERED — `mcphub de-adopt` reverses `mcphub adopt`: restores every adopt-owned client entry to its exact pre-adopt config (original secret-literal spellings from pinned snapshots) + removes the hub manifest, atomically + roll-forward-recoverable. All 12 phases merged (#539/#540/#542/#544/#543/#545/#546/#547/#548/#549/#550, master `837fe95c`). Spine: per-manifest lease E1→E6 + CLOSE-READY single terminal gate + roll-forward resume + under-lease re-read/re-classify authority + `--accept-conflict` mutation-point proof. Gates: FULL-COMMISSION (Sol+Terra+fable) on 3/4/6/8; Phase 8 executor took 16 fixes (commission + 6 bot rounds + fable final-gate catching a fail-open the bot missed). v1 = all-clients-only, gate-OFF-only, atomic; subset/gate-ON/`--reconstruct-legacy` DEFERRED (backlog `2026-07-11-deadopt-subset-and-gate-on-followup.md`). |
| [2026-07-13-daemon-port-ephemeral-self-heal](archive/2026-07/2026-07-13-daemon-port-ephemeral-self-heal/closure.md) | 2026-07-13 | DELIVERED — supervisor self-heals a dynamic-pool proxy (serena/LSP) whose loopback pool port is stolen by a foreign process (WSL2/Hyper-V-widened OS ephemeral range over the pools) — hub no longer "partial". L1 off-loop atomic realloc + respawn (covers bind-fail-after-spawn AND port-held-at-pre-spawn), L2 setup detect + `--fix-ephemeral-range`, L3 redacted events. #535 squash `b351a5cc`, 7-round commission (Sol+Terra+fable arbiter) + bot PASS, deployed + fleet-verified. P3 follow-ups in `backlog/2026-07-13-daemon-port-self-heal-followups.md`. |
| [2026-07-09-adopt-side-durable-pre-adopt-provenance](archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/closure.md) | 2026-07-10 | DELIVERED — durable, non-destructive pre-adopt provenance store (`adopted-entries.json` + owner-only snapshot dir) capturing per-client pre-adopt state before the config rewrite; #528 squash `16dba601`, bot PASS (5 rounds, 16 real bugs) + security re-verify. Coherent crash-consistency model (per-manifest flock lease + single `classifyDeadAdoptingRow` + row-first anchor + snapshot-dir GC). Unblocks de-adopt. Both decisions `accepted`; follow-ups in `backlog/2026-07-10-adopt-provenance-lease-hygiene.md`. |
| [2026-07-09-test-leftover-reaper](archive/2026-07/2026-07-09-test-leftover-reaper/closure.md) | 2026-07-10 | DELIVERED — v1 preview/diagnostics-only `mcphub cleanup test-leftovers`; #527 squash `436e4f58`, bot PASS (2 rounds). Non-destructive: enumerate + classify + hypothetical-refusal labels, no process termination. Destructive apply deferred to v2 (blocked on round-3 P2/P3 + value case); standalone-`supervise` stays manual-reap-only. |
| [2026-07-09-lsp-relay-per-client-disable-gui](archive/2026-07/2026-07-09-lsp-relay-per-client-disable-gui/closure.md) | 2026-07-09 | DELIVERED — per-client LSP-router enable/disable in the Servers matrix; #524 squash `22c91cab`, deployed + live-verified. |
| [2026-07-09-intent-collapse-stop-resurrection](archive/2026-07/2026-07-09-intent-collapse-stop-resurrection/closure.md) | 2026-07-09 | DELIVERED — absent-only legacy stop watermarks; #525 squash `5d8ab063`, deployed `7898c148`. |
| [2026-07-05-unify-port-resolution-owner](archive/2026-07/2026-07-05-unify-port-resolution-owner/closure.md) | 2026-07-05 | DELIVERED — daemon port+identity single-owned; #505, deployed. |
| [2026-06-15-dynamic-mcp-discovery](archive/2026-06/2026-06-15-dynamic-mcp-discovery/closure.md) | 2026-06-16 | DELIVERED — Discovery view + demigrate + marketplace mark-installed. |
| [2026-06-15-workspace-daemon-prune](archive/2026-06/2026-06-15-workspace-daemon-prune/closure.md) | 2026-06-16 | DELIVERED — auto-prune Phase 1 + idle + GUI toggle. |
| [2026-06-15-settings-redesign](archive/2026-06/2026-06-15-settings-redesign/closure.md) | 2026-06-15 | DELIVERED — Settings card redesign + tooltips + UX-audit fixes. |
| [2026-06-11-pr288-cumulative-review](archive/2026-06/2026-06-11-pr288-cumulative-review/closure.md) | 2026-06-14 | CLOSED — review-only; findings landed via #300–#303. |
