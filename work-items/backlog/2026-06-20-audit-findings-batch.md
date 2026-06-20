---
status: in-progress
date: 2026-06-20
slug: audit-findings-batch
source: opus subsystem-audit Workflow (wpaephnqp, 6 areas) + 2 codex audits (CLI/lock-order, cold-restart/IPC)
---

## RESUME POINT (2026-06-20 ~18:50) — gated on Codex-limit refresh ~19:00

MERGED tonight (8): #381 #383 #382 #378 #384 #385 #387 + #386 (architect-refactored serena gate-seam).
3 architect decisions filed (gate-uniform-seam, marketplace-proxy-ssrf-residual, moved-binding-B′ [inline]).
Commission Workflow (wse1jj1o2): all 4 security PRs = MERGE, zero confirmed blockers.

IN-FLIGHT — 3 PRs CLEAN (inline=0, structural fixes landed) AWAITING bot PASS:
- #388 marketplace-security @3ccbe75 — SSRF/Trojan-Source, 10 rounds; structural `urlredact.ScrubParseError`
  single-owner ended the redaction whack-a-mole. commission=MERGE.
- #389 state-file-read-hardening @e0b66b7 — inode-anchor + relax-posture + secret-file-read-refuse. commission=MERGE.
- #391 hub-aggregator-hardening @7107943 — B′ (deleted moved-binding wrapper) + detached-reinit-never-orphans.
  architect+commission signed.

GATE: the Codex BOT hit its usage limit ("You have reached your Codex usage limits for code review") —
the bot IS Codex; refreshes ~19:00 (user instruction: don't run codex till 19:00). HOLDING all codex/bot
activity (no @codex retrigger, no codex lanes) to preserve the budget.

NEXT (at 19:00, in order): (1) retrigger bot on the 3 clean PRs → PASS → squash-merge each;
(2) redeploy full fleet (build.sh + full supervisor restart + `claude mcp list` verify + serena 404-contract
probe); (3) NEXT-BATCH below (CLI strict-mode P1 first); (4) work-items doc traceability — commit the
session's decision/bug docs (commission flagged dangling refs in #386 commit; they live untracked in main tree).

# Audit-findings batch — mined bugs from the 3-source subsystem audit (2026-06-20)

A concurrent Claude opus Workflow (6 subsystems) + 2 codex audit lanes mined the live/merged
subsystems for real bugs. Full reports: tasks/wpaephnqp.output (Claude) + the codex
audit-* .out files. Triaged below.

## BEING FIXED NOW (codex lanes / open PRs)

**state-file read hardening** (fix/state-file-read-hardening, lane bxzwpyp1m):
- P1 — ReadSupervisorIntent/ReadSupervisorState: separate-path parent gate then symlink-following
  os.ReadFile (read-side TOCTOU + symlink-follow) on the live boot + 60s re-read path → inode-anchor.
- P2 — daemon-intent.json (ReadDaemonIntentFile) bypasses the hardened reader → inode-anchor.
- P2 — workspaces.yaml (Registry.Load) raw os.ReadFile, no read gate → inode-anchor.
- P2 — secrets vault.save() non-atomic (os.WriteFile, no temp+rename) → atomic temp+rename+fsync.

**marketplace security** (fix/marketplace-security, lane ba3oe5brj):
- P2 — sanitizeCatalogField misses Unicode bidi/Trojan-Source controls (U+200E/200F, 202A-202E,
  2066-2069, 2028/2029, 0085) → strip them.
- P2 — readMarketplaceCache trusts the cached catalog without re-validation → re-run ParseMarketplaceCatalog.
- P3 — http-entry URL is a HasPrefix("https://") byte check → url.Parse (scheme/host/no-creds).

**cold-restart upgrade hardening** (fix/cold-restart-upgrade-hardening, lane bypyw191y):
- P1 — `install --upgrade` reports success before the old supervisor releases the lock + the
  successor may fail to start → verify handoff (lock release + new-supervisor IPC readiness).
- P3 — SweepOldBinaries is dead code (defined, never wired) → wire into upgrade + supervisor startup.
- P3 — SweepOldBinaries deletes by mtime without validating the `.old-<timestamp>` suffix → time.Parse gate.

**serena idle-stop races** (PR #386, fix/serena-idle-stop-gate): the 2 idle-stop subsystem races
(per-workspace stop-gate). MERGED-pending bot + codex review.

## NEXT BATCH (filed — not yet fixed)

**hub-aggregator:**
- P2 — tools_hidden enforcement STALE on the LISTING surface (hiddenToolsForScope reads
  SnapshotAtInit not the live snapshot; tools/call already revalidates per #374) → read live snapshot.
- P2 — production hub http.Server has only ReadHeaderTimeout (no ReadTimeout/WriteTimeout/IdleTimeout)
  → slow-loris pins a handler goroutine; add timeouts.
- P2 — per-tools/list-request inode-anchored DISK READ for instance_id (handler already has it cached;
  empty-on-transient-failure) → thread the cached InstanceID.
- P3 — daemon-side MCP session leak when initialize ok but notifications/initialized fails (not tracked
  in InitSuccesses, handleDelete never DELETEs it) → best-effort delete on the notification-failure path.
- P3 — staleDaemonPorts sync.Map never pruned (small, port-keyed, unbounded within a session).

**CLI:**
- P1 — strict-mode assumes AutostartBackend.Enable is atomic, but the OS backends mutate the shim
  before erroring (Windows delete-before-create, Linux write-before-enable, macOS write-before-bootstrap);
  on a step-2 error + intent-revert-success, strict-mode deletes the breadcrumb and reports no torn
  state → intent+shim desync. Make autostart Enable transactional, or strict-mode snapshot/restore the shim.
- P3 — docs/cli-reference.md omits migrate-legacy/autostart/strict-mode + the 0/1/8/9/10 exit-code contract.

**supervisor-reaper:**
- P3 — crashCh non-blocking send drops a real EvChildExit if 64+ exits are pending (buffered 64).
- P3 — RestartHistory/BackoffUntil/QuarantineSince/QueuedAction are vestigial (no production writer);
  either delete + update the docs, or wire the persistence the SM docstrings claim.

**secure-write** (otherwise CLEAN — auth/handle-relative pipeline verified solid):
- P3 — post-rename failure leaves a complete owner-only file at path, contradicting the "no file on
  error" contract → best-effort delete on post-rename error.

**groups:**
- P3 — DELETE token-prune-failure leaves the g:<name> token row; a re-created group reuses the stale
  token (ensureHubTokensLocked never rotates) → rotate on (re)create.

**cold-restart (deferred from that lane):**
- P2 — crash/power-loss between the rename steps leaves the binary missing with no next-run recovery
  → durable recovery (detect missing-target + valid .old-* → restore).
- P3 — the post-ListenPipe DACL "smoke assert" returns the requested SDDL, not the live handle's
  effective DACL → query the actual pipe handle SD (or add negative connection tests).
