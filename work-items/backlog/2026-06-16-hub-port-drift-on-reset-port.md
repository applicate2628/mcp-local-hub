# Backlog: gate-ON hub port-drift — `--reset-port` orphans all client URLs (B2)

Date filed: 2026-06-16 (user: "да" — file the gate-ON review footguns)
Status: backlog
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
