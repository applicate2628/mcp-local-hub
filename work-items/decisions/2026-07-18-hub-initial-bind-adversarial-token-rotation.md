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
Implement Option B on a branch off master (Phase C is live/ungated), its own PR → bot re-review (Option B
is demonstrably STRONGER than the bot's own fix against the raised threat, so it should satisfy the
re-review) → merge → deploy. Then CLOSE PR #561 as non-canon with a comment pointing at this decision +
the Option B PR. Sequenced AFTER the gated Phase F/I commit to avoid a working-tree/branch conflict.
