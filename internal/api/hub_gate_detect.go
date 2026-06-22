// hub_gate_detect.go — gate-ON detection helper (B2 footgun guard).
//
// Background (work-items/backlog/2026-06-16-hub-port-drift-on-reset-port.md):
// the hub port is baked into every gated client URL
// (`http://127.0.0.1:<hubport>/clients/<client>/mcp`, written by the
// gate-ON reconciler in install_hub_reconcile.go). `--reset-port` (and
// the listener-rollback path) clear the persisted Port to 0, so the
// NEXT hub bind grabs a fresh OS-assigned ephemeral port — orphaning
// every gated client URL at once (connection refused for ALL aggregated
// servers). The symptom misdirects diagnosis toward the daemons, not
// the config.
//
// This helper lets the `--reset-port` CLI guard detect whether any
// supported client is currently gate-ON (its config carries the reserved
// `mcphub-hub` aggregate entry) so the CLI can REFUSE the reset and tell
// the operator to gate-OFF or re-run `mcphub install --reconcile-hub-mode`
// after — the bounded fix vs the larger stable-port-pin feature.
//
// Detection is by the SAME positive signal the reconciler writes: a live
// `mcphub-hub` entry in a client's on-disk config (Client.GetEntry of the
// reserved entry name). We do NOT infer gate-ON from the managed-entries
// marker — that file records per-(client, server) tuples, never the
// aggregate entry. Reading the actual config is the authoritative source.

package api

import (
	"sort"

	"mcp-local-hub/internal/clients"
)

// GatedOnClients returns the sorted list of supported client ids whose
// on-disk config currently carries the reserved `mcphub-hub` aggregate
// entry — i.e. the clients that are gate-ON and whose URLs are baked to
// the current hub port.
//
// A client is reported gate-ON iff:
//   - its adapter constructs on this host (clients.AllClients drops
//     unconstructable adapters), AND
//   - its config file exists (Exists()), AND
//   - GetEntry("mcphub-hub") returns a non-nil entry with no error, AND
//   - that entry is NOT disabled (entry.Disabled == false).
//
// The disabled exclusion (bot PR #420 finding 5): a multi-layer adapter
// (MiMoCode) can return a non-nil aggregate entry that is in a DISABLED state
// (enabled:false) the client will NOT load. A disabled aggregate orphans no live
// URL, so a hub port reset against it would break nothing — counting it as
// gate-ON would FALSELY block `mcphub gui --reset-port`. GetEntry stamps
// MCPEntry.Disabled for such an entry (additive, default false), so this gate
// skips it while read-membership callers still see the entry. This mirrors the
// scan path (shapeMimoCodeEntry classifies enabled:false as Transport "absent").
//
// Read errors per client (corrupt config, DACL violation) are
// SKIPPED, not fatal: this is a best-effort advisory probe whose only
// consumer is a refuse-guard. A client whose config cannot be parsed is
// treated as "not detectably gate-ON" rather than aborting the whole
// probe — the operator still gets the guard for every client we COULD
// read, and a corrupt config surfaces its own error on the next install.
//
// Relay-stdio clients (Antigravity, Zed, ...) are skipped: the gate-ON
// reconciler never writes a `mcphub-hub` aggregate to them (they require
// relay context the aggregate entry has no shape for — see
// install_hub_reconcile.go applyOpsForClient), so they can never be
// gate-ON via the hub aggregate.
func GatedOnClients() []string {
	var gated []string
	all := clients.AllClients()
	for name, c := range all {
		if c == nil {
			continue
		}
		if c.IsRelayStdio() {
			// The hub aggregate is never written to a relay-stdio client.
			continue
		}
		if !c.Exists() {
			continue
		}
		entry, err := c.GetEntry(hubReconcileAggregateEntryName)
		if err != nil || entry == nil {
			// Skip on read error (best-effort) or absent entry.
			continue
		}
		if entry.Disabled {
			// A disabled aggregate (enabled:false) is one the client never loads,
			// so it orphans no live URL on a hub port reset — not gate-ON (bot PR
			// #420 finding 5).
			continue
		}
		gated = append(gated, name)
	}
	sort.Strings(gated)
	return gated
}

// AnyClientGatedOn reports whether at least one supported client is
// gate-ON. Thin convenience wrapper over GatedOnClients for the CLI
// guard's boolean decision; callers that need the client list for the
// operator message use GatedOnClients directly.
func AnyClientGatedOn() bool {
	return len(GatedOnClients()) > 0
}
