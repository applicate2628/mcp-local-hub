---
status: accepted
---

# Decision: hub initial-bind-failure keeps Phase C auto-recovery + rotates the hub InstanceID ONLY on a foreign/unverifiable port holder — reject the bot's fail-closed revert (PR #561)

Date: 2026-07-18
Decided by: $lead on a fable security review (`security-reviewer`, model fable), convened at operator
request ("его исправление не является каноном ... проведи fable").
Relates: PR #561 (`codex/fix-hub-auto-rebind-token-leak-vulnerability`), Phase C of item-3 Unit B
(shipped in `a7d05fd3`, live), item-1 honest hub health (`hub_health.go`), the daemon-recover squatter
identity gate.

## The claim under review
A Codex Cloud bot (PR #561) claims Phase C — which made a gate-on INITIAL hub-listener bind failure
recover (`HubHealthRecovering` + same-port retry through the existing driver) instead of terminal
`HubHealthDown` — is a hub-token-leak vulnerability, and reverts it to fail-closed.

## fable security verdict: threat OVERSTATED; bot revert rejected
- **Harvest is mechanically real but PRE-EXISTING, not Phase-C-created.** Gate-on clients POST both
  `X-Mcphub-Hub-Token` and `X-Mcphub-Instance-Id` to `127.0.0.1:<hubport>` on every request
  (`install_hub_reconcile.go:249-250`, read at `hub_mcp_handler.go:215-242`); a foreign process bound to
  the hub port first receives both. `EnsureHubTokens` never rotates (`hub_mcp_tokens.go:78-85`) and
  `EnsureHubEndpoint` preserves `InstanceID` across every rebind (`hub_mcp_instance.go:80-118`). This
  loopback-port-squat + preserved-credentials class is ALREADY documented in-tree
  (`hub_listener.go:29-32,577-581`) with the intended mitigation being the operator credential-rotation
  WARNING, not fail-closed-forever. Phase C changes none of it.
- **The bot's fail-closed revert closes NOTHING.** The harvest happens BEFORE mcphub binds; the revert's
  "operator recovery" is a plain GUI relaunch → the same `EnsureHubTokens`(preserve) +
  `EnsureHubEndpoint`(preserve instance-id) path → serves with the SAME possibly-harvested credentials.
  Security delta ≈ nil; the revert only trades away Phase C's transient-failure robustness (a benign
  momentary self-conflict / own stale instance now permanently downs the hub) for zero gain.
- **Reachability is narrow.** The token is only a secret the attacker lacks on a MULTI-TENANT host against
  a DIFFERENT-user process (owner-only DACL denies cross-user file read). On the single-user solo-dev host
  (this project's primary target) a same-user attacker reads the token/endpoint files directly and reaches
  the loopback daemon ports without the hub at all — the harvest path is moot. The multi-tenant residual is
  the same class governed by `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` + DACL.

## The decision: Option B (adversarial-gated InstanceID rotation)
KEEP Phase C's `signalInitialHubBindFailure` auto-recovery. On an initial-bind failure where the failing
`restartPort` is held by a **FOREIGN or unverifiable** process (classified via the EXISTING verified-own-
vs-foreign `lookupProcessIdentity` squatter identity gate already shipped for `mcphub daemon recover`),
call `api.RotateHubInstanceID()` (`hub_mcp_instance.go:177`) ONCE before the eventual successful rebind.
Fail-safe: an unverifiable owner is treated as foreign → rotate. Benign own-stale-instance case keeps the
silent same-port retry (no `needs-reconcile` churn).

**Single deciding reason:** rotation is the ONLY option that actually invalidates the possibly-harvested
credentials before the hub serves again. Fail-closed (bot's A) closes nothing while destroying robustness;
document-only (D) leaves the narrow multi-tenant reuse open. Gating rotation to the FOREIGN signal keeps
Phase C's robustness for the benign case.

**Why it's minimal — the downstream is already wired:** the successful bind's
`loadHubListenerRestartInstanceID()` will then differ from `previousInstanceID` → the existing
`instanceIDChanged` branch (`hub_listener.go:444-463`) fires `hub-listener-restart-instance-id-changed` →
`markReconcilePending()` (`hub_health.go:194-195`) → `HubHealthNeedsReconcile` + operator action
`mcphub install --reconcile-hub-mode`. No new health-state code. The ONLY new code: the port-owner probe +
one conditional `RotateHubInstanceID` call in the `preview == nil` + `initial-bind-failed` branch
(`hub_listener.go:306-327`).

## Blast radius (fable-confirmed)
- Gated D-J self-restart group: NO coupling. Phase C's symbols are referenced only in `hub_listener.go` +
  `server.go:1084`; the gated restart-v3 machinery references none. `GUIListenerOwner` (GUI-listener seam)
  is orthogonal to the hub-listener restart cause.
- Item-1 honest health: Option B preserves `recovering → down` AND lights the already-reachable
  `recovering → needs-reconcile` edge on the adversarial branch via existing `markReconcilePending`. No
  item-1 invariant broken. (The bot's revert would skip the `recovering` surface — a regression.)

## Secondary (verify during implementation, non-blocking)
Confirm the credential-rotation WARNING still fires on a foreign-held-port bind failure under Phase C
(emitted from `api.BindHubMcpListener`, `hub_mcp_bind.go` — fable did not read it this pass). That operator
alert is the human-in-the-loop the design relies on and must not have been silenced by routing the failure
into the auto-retry driver.

## Consequence
Implement Option B on a branch off master (Phase C is live/ungated), its own PR → bot re-review → merge →
deploy. Then CLOSE PR #561 as non-canon with a comment pointing at this decision + the Option B PR.
Sequenced AFTER the gated Phase F/I commit to avoid a working-tree/branch conflict.

## Amendment 1 (2026-07-18) — bot review of PR #562: what the adversarial-gate can and cannot close
PR #562 went to the Codex bot over two fix rounds. Round 1 raised two REAL P1s (both fixed): the owner
classifier used the DAEMON-descriptor gate (`daemonrecovery.ClassifyPortOwner`), which misclassifies an
own mcphub-GUI hub-port holder as FOREIGN and rotates on every benign same-port recovery → switched to a
GUI-process identity gate (`processID`+`matchBasename`+`cmdlineIsGui`, the `--force --kill` gate); and
`RotateHubInstanceID` was a blocking non-cancellable lock → added `RotateHubInstanceIDContext`.

Round 2 raised three more, and they are DECISIVE about the limits of the whole approach:
- **P1-3 (fixed in round 3):** rotation-then-cancel (driver cancelled after the InstanceID persisted but
  before a successful retry) left clients on the old ID. The round-2 in-memory health latch covered only
  the current process; round 3 makes `reconcile_pending` part of the atomically-written endpoint record,
  restores `needs-reconcile` on every later startup, and clears it only after successful
  `mcphub install --reconcile-hub-mode` application.
- **P1-1 + P1-2 (ACCEPTED as bounded residual — operator decision 2026-07-18):** reliable adversarial
  detection at rebind time is IMPOSSIBLE — the owner probe is asynchronous to the bind failure (a foreign
  holder can DROP the port before the probe and evade rotation, P1-1), and process identity is FORGEABLE
  (a different-user attacker runs a copied binary named `mcphub` with `gui` argv, P1-2). BOTH require a
  DIFFERENT-USER (multi-tenant) attacker; on the single-user solo-dev host (this product's primary target)
  a same-user actor already owns the credential files, so the attack is moot. This is the SAME bounded
  multi-tenant residual this decision already accepted, governed by `MCPHUB_REQUIRE_SINGLE_USER_HOME` + the
  owner-only DACL. Documented inline at the classifier; NOT fixed (cannot be, robustly).

**Operator decision (2026-07-18):** fix P1-3; accept + document P1-1/P1-2 as the bounded multi-tenant
residual; the gate stays for the single-user common case (own-GUI → no rotation, no needs-reconcile churn).
Because the bot may keep flagging P1-1/P1-2 (they are genuinely un-closable), the operator explicitly
authorized MERGING #562 on this documented-residual justification — this specific PR only — rather than
requiring a clean bot PASS. Option B still strictly improves security over both Phase-C-as-was (no
rotation) and the bot's fail-closed revert (which reuses the same preserved credentials on manual relaunch).

## Amendment 2 (2026-07-18) — bot round 3 + full commission on the P1-3-completion fix

Round 3 (on `d9cabcb4`, the first P1-3 attempt) proved the round-2 fix INCOMPLETE and added a scoping bug:
- **P1 (durability, FIXED):** the round-2 `markReconcilePending()` was process-scoped in-memory; a
  shutdown after the InstanceID rotation persisted but before a successful rebind lost the signal, so the
  next process bound the new (durable) InstanceID with no needs-reconcile surface. Fixed by a DURABLE
  `HubEndpoint.ReconcilePending` field set in the SAME endpoint write as the new InstanceID
  (`rotateHubInstanceIDLocked`), hydrated at `Server.Start`, and cleared by `ClearHubReconcilePendingLocked`
  from `runReconcileHubMode` under a continuously-held `hub-mcp.lock` (the same lock rotation takes → no
  interleaving rotation can lose a just-set marker).
- **P2 (scoping, FIXED):** `Server.Start` emitted `initial-bind-failed` for EVERY hub startup error. Now a
  new `hubMcpListenerBindError` wraps ONLY a confirmed WSAEADDRINUSE(10048)/WSAEACCES(10013) refusal on the
  positive persisted port; non-bind startup errors take a new `initial-startup-failed` retry cause with NO
  port probe and NO rotation.

**Full commission before the round-4 bot push (fable mandatory member):**
- **fable (Claude fable-5) — PASS.** No in-diff blocker. Verified CLEAN: the channel-type reshape, the
  wrap-chain `errors.As` reachability, durable-marker preservation across all endpoint writers, idempotency,
  and — decisively — the **Skipped-clients-clear** question: `report.Skipped` is populated ONLY for
  `clients.IsRelayStdio` adapters (antigravity/aider), which never receive a `mcphub-hub` aggregate entry
  and carry no `X-Mcphub-Instance-Id`, so clearing the marker with skips present un-signals nothing stale.
  Raised 4 P2/P3 follow-ups (all pre-existing / out-of-#561-scope), filed as
  `backlog/2026-07-18-hub-restart-path-adversarial-rotation-followups.md`; the one substantive item is F1
  (P2) — the mid-run RESTART path has the SAME foreign-holder capture window with NO rotation (the
  initial-bind fix's mirror gap).
- **Terra (codex gpt-5.6-terra) — REVISE, 2×P1.** Terra-A (a non-missing endpoint load error at startup
  hydration silently skipped the durable signal) was a REAL distinct gap and is FIXED: hydration now fails
  safe — a non-missing read error surfaces needs-reconcile (a spurious idempotent prompt beats a dropped
  security signal), while first-run absence still skips; covered by
  `TestServerHubHealthReconcileHydrationLoadErrorFailsSafe`. Terra-B (clear-despite-Skipped) was
  EMPIRICALLY DISPROVEN by $lead (Skipped = relay-stdio only, no hub identity) — same conclusion fable
  reached independently; recorded as a false positive.
- **Sol (codex gpt-5.6-sol) — output misfired** (emitted a stray marker instead of its verdict after
  123k tokens of reasoning; read-only review, no diff to recover). Not re-run: fable's deep PASS + Terra's
  live finding + $lead's own trace of the P1-race closure and the Windows-classifier coverage already
  cover the substantive surface. F4 (a stale burn-down doc line) was fixed in-PR.

Net for the round-4 push: durable-reconcile (P1) + confirmed-bind (P2) + Terra-A load-error fail-safe, all
$lead-verified green; F1-F3 filed as follow-ups; F4 doc corrected. The accepted P1-1/P1-2 residual is
unchanged and still governs the merge authorization above.
