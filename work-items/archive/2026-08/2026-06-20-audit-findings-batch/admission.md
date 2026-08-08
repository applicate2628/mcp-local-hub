---
status: candidate
date: 2026-06-20
slug: audit-findings-batch
source: opus subsystem-audit Workflow (wpaephnqp, 6 areas) + 2 codex audits (CLI/lock-order, cold-restart/IPC)
---

# Audit-findings batch — mined bugs from the 3-source subsystem audit (2026-06-20)

A concurrent Claude opus subsystem-audit Workflow (6 areas) + 2 codex audit lanes (CLI/lock-order,
cold-restart/IPC) mined the live/merged subsystems for real bugs; the raw audit outputs are session
artifacts under the local `.reports/` tree (not committed). The triaged findings are below.

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

**cold-restart upgrade hardening** — ✅ MERGED as PR #387 (0a574b80):
- P1 — `install --upgrade` reports success before the old supervisor releases the lock + the
  successor may fail to start → verify handoff (lock release + new-supervisor IPC readiness). FIXED.
- P3 — SweepOldBinaries is dead code (defined, never wired) → wire into upgrade + supervisor startup. FIXED.
- P3 — SweepOldBinaries deletes by mtime without validating the `.old-<timestamp>` suffix → time.Parse gate. FIXED.

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

_Closed 2026-06-21: NEXT-BATCH fully shipped across tonight's 20 PRs._
