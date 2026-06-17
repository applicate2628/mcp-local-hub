# Backlog: gate-ON hub port-drift — `--reset-port` orphans all client URLs (B2)

Date filed: 2026-06-16 (user: "да" — file the gate-ON review footguns)
Status: fixed (2026-06-17 — refuse-guard shipped)
Source: gate-ON regression review (opus architectural lane) —
`.reports/2026-06/report(main)-2026-06-16_23-30_gate-on-regression-review.md`
Relevance: latent until any client is flipped gate-ON; the most-likely real-world
break of the G4 hub aggregate. NOT a blocker for the dormant-ready state.

## Symptom (VERIFIED by review, file:line below)
After a client is gate-ON (its config points at `http://127.0.0.1:<hubport>/clients/<client>/mcp`,
e.g. 3439), ANY operation that resets the hub port to 0 causes the next hub bind
to grab a NEW OS-assigned ephemeral port. The written client URLs still point at
the OLD port → `connection refused` for ALL aggregated servers at once. InstanceID
survives (it is the long-lived identity), but the port does not. The symptom
("connection refused") misdirects diagnosis toward the daemons, not the config.

## Trigger paths (VERIFIED)
Two code paths set `Port=0`:
1. `mcphub gui --reset-port` — the DOCUMENTED stuck-instance recovery flow the
   operator is most likely to reach for (`hub_mcp_instance.go:252` ResetHubPortContext).
2. The listener-rollback path on a reload-handler failure
   (`internal/gui/hub_listener.go:213` → ResetHubPortContext).

Bind reads the persisted port and re-persists `ln.Addr().Port`
(`hub_mcp_bind.go:157,197`). Client URL is port-bearing:
`HubLoopbackURL(endpoint.Port, ...)` (`install_hub_reconcile.go:230`).

## Why it matters
The hub port is the unstable identity, yet it is baked into every gated client
config. A reset (recovery flow OR rollback) silently invalidates every gated
client until a re-reconcile (`mcphub install --reconcile-hub-mode`) rewrites the
URLs — which the operator may not realize is needed.

## Fix options (pick at adoption time)
- Pin the hub to a stable configured port in `gui-preferences.yaml` (if a
  stable-port setting exists / add one) so a reset re-binds the SAME port rather
  than a random one. Preferred — removes the footgun at the source.
- OR refuse `--reset-port` while any client is gated-ON (detect via managed-entries
  / a live `mcphub-hub` entry) and tell the operator to gate-OFF first.
- OR auto-trigger a re-reconcile after any port change while gated clients exist.
- Document in the stuck-instance-recovery runbook (CLAUDE.md) that a port reset
  REQUIRES `mcphub install --reconcile-hub-mode` when clients are gated-ON.

## Next steps
- Decide adoption posture first (this is moot if gate-ON is never used).
- If gate-ON is adopted: implement stable-port pinning + the runbook note before
  flipping any non-pilot client.

## Resolution (2026-06-17 — refuse-guard shipped)

No stable-port pin setting exists in `gui-preferences.yaml` (checked
`settings_registry.go` — only `gui_server.hub_endpoint_enabled`), so the
bounded SAFEST option from "Fix options" was implemented: **`mcphub gui
--reset-port` now REFUSES (exit 8) while any client is gate-ON.**

- Detection helper `api.GatedOnClients()` /
  `api.AnyClientGatedOn()` (`internal/api/hub_gate_detect.go`): scans
  every supported client's on-disk config for the reserved `mcphub-hub`
  aggregate entry (the positive signal the gate-ON reconciler writes).
  Relay-stdio clients are skipped (the aggregate is never written to
  them); per-client read errors are skipped (best-effort advisory probe).
- CLI guard (`internal/cli/gui.go`, in the `--reset-port` block AFTER the
  single-instance lock proves the GUI is not running): on any gate-ON
  client, prints a refusal naming the gated clients + the two recovery
  paths (gate OFF first, OR re-run `mcphub install --reconcile-hub-mode`
  after) and returns exit 8. Endpoint state is left UNTOUCHED.
- Runbook: CLAUDE.md gained a "Hub aggregate (gate-ON) mode + port reset"
  subsection documenting gate-ON itself + that a port reset REQUIRES
  `mcphub install --reconcile-hub-mode` when clients are gate-ON, and the
  exit-code table gained code 8.
- Tests: `TestGatedOnClients*` (api), `TestGuiResetPortRefusedWhenClientGatedOn`
  (cli); the pre-existing `--reset-port` happy-path/hub-running tests were
  made HOME-hermetic so the new guard reads sandbox configs, not the
  developer's real ones.

NOT done (deferred, larger): stable-port pinning in `gui-preferences.yaml`
(removes the footgun at the source) and auto-re-reconcile after a port
change. The refuse-guard is the bounded fix; pinning remains the
preferred long-term option if/when gate-ON is broadly adopted.
