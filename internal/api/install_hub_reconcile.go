// install_hub_reconcile.go — Phase 5 Task 5.1 (G4 unified hub MCP).
//
// Full-reconcile planner + applier that owns the bidirectional gate
// transition (ON: aggregate `mcphub-hub` entry + remove per-daemon
// entries; OFF: restore per-daemon entries + remove `mcphub-hub`).
//
// Per-server install paths (`mcphub install --server X`) do NOT call
// this — they emit only their own per-(server, client) bindings via
// BuildPlan / BuildPlanWithOpts. The per-server planner skips any
// Remove of `mcphub-hub` per codex r3 general F2 closure.
//
// Spec:
//   - docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
//     §"Bidirectional install reconciler"
//   - same spec §"Crash-safe reconcile ordering" — within one client
//     config rewrite, all AddReplace ops apply before any Remove op so
//     a mid-sequence crash never leaves a client with no entry at all.
//
// Plan:
//   - docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 5.1.

package api

import (
	"fmt"
	"sort"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// HubReconcileOpts toggles the gate direction.
type HubReconcileOpts struct {
	// GateOn=true emits the ON-transition plan (one AddReplace
	// `mcphub-hub` per client with non-empty bindings + a Remove for
	// every previously-installed per-(server, client) entry).
	//
	// GateOn=false emits the OFF-transition plan (AddReplace each
	// per-(server, client) entry pointing at the per-daemon HTTP URL
	// + Remove the `mcphub-hub` aggregate).
	GateOn bool
}

// HubReconcileReport summarizes ApplyHubReconcileInOrder's outcome.
// Succeeded lists the client ids whose entire batch (adds + removes)
// applied without error. Failed records per-client failures, scoped to
// the phase that errored ("add/replace" or "remove") and the wrapped
// error message. The reconcile continues with the next client when a
// per-client phase fails — operators rerun to converge.
type HubReconcileReport struct {
	Succeeded []string
	Failed    []HubReconcileFailure
}

// HubReconcileFailure is one row in HubReconcileReport.Failed. Phase
// uses the same wire strings as ClientUpdateAction ("add/replace" or
// "remove") so log consumers can correlate without dereferencing the
// typed enum.
type HubReconcileFailure struct {
	Client string
	Phase  string
	Err    string
}

// BuildHubReconcilePlan emits the full-reconcile plan AS A WHOLE per
// spec §"Bidirectional install reconciler". The plan covers every
// (manifest, client) tuple discovered across `manifests` and uses
// `clients.ConfigPathForName` to resolve the on-disk path each
// adapter expects.
//
// On GateOn=true: for every client with at least one binding in
// `manifests`, emit one AddReplace `mcphub-hub` op (URL +
// X-Mcphub-Hub-Token + X-Mcphub-Instance-Id headers) plus one Remove
// for every per-(server, client) binding. The hub URL is
// `http://127.0.0.1:<port>/clients/<id>/mcp` per spec §"Hub MCP
// endpoint contract".
//
// On GateOn=false: for every client with at least one binding, emit
// one AddReplace per per-(server, client) binding pointing at
// `http://localhost:<port>/mcp` plus one Remove of the `mcphub-hub`
// aggregate.
//
// Clients with no resolvable config path (e.g. an unknown id) are
// skipped silently — the same forgiving behavior as the per-server
// planner.
// reconcileBindingRef is the planning-internal record for one
// (client, server, daemon) tuple that the gate-off path needs. It
// extends canonicalDaemonRef with the binding's URLPath so the
// gate-off URL rebuild matches the URL the canonical per-server
// install path would have written (codex bot phase5 r1 P2 closure
// on PR #160 — gate-OFF reconcile MUST honor non-default url_path).
type reconcileBindingRef struct {
	Server   string
	Daemon   string
	Port     int
	URLPath  string // binding's url_path; empty defaults to "/mcp" per validateClientURLPath
}

func BuildHubReconcilePlan(
	manifests []config.ServerManifest,
	endpoint HubEndpoint,
	tokens HubTokenTable,
	opts HubReconcileOpts,
) ([]ClientUpdatePlan, error) {
	// 1. Compute per-client union of (server, daemon, port, url_path)
	//    bindings across ALL manifests. The map key is the canonical
	//    client id (per `clients.SupportedClientNames()`), but we
	//    don't require every shipped client to appear — only clients
	//    with at least one binding produce ops.
	perClient := map[string][]reconcileBindingRef{}
	for i := range manifests {
		m := &manifests[i]
		for _, b := range m.ClientBindings {
			d, ok := findDaemon(m, b.Daemon)
			if !ok {
				return nil, fmt.Errorf("manifest %q: binding references unknown daemon %q", m.Name, b.Daemon)
			}
			perClient[b.Client] = append(perClient[b.Client], reconcileBindingRef{
				Server: m.Name, Daemon: b.Daemon, Port: d.Port, URLPath: b.URLPath,
			})
		}
	}

	// Walk clients in a stable order so plan output is deterministic
	// across runs (map iteration in Go is randomized; deterministic
	// output matters for both test assertions and human-readable
	// dry-run output).
	clientNames := make([]string, 0, len(perClient))
	for c := range perClient {
		clientNames = append(clientNames, c)
	}
	sort.Strings(clientNames)

	var plan []ClientUpdatePlan
	for _, client := range clientNames {
		refs := perClient[client]
		if len(refs) == 0 {
			continue
		}
		// codex bot phase5 r3 P2 closure on PR #160: do NOT sort refs.
		// Manifest order is the canonical replace-by-name precedence
		// the per-server install path uses. Sorting here would make
		// the persisted-on-disk winner be sort-order instead of
		// manifest-order, diverging from BuildPlanWithOpts semantics
		// when a manifest legitimately has multiple bindings under
		// one (server, client). Determinism across runs is preserved
		// by the manifest order itself (manifests slice is the
		// caller's responsibility; we read it as-is) + the
		// clientNames sort above.

		path, err := clients.ConfigPathForName(client)
		if err != nil {
			// Unknown client id — skip rather than fail the whole
			// plan. The per-server planner uses the same forgiving
			// rule via installClientPredicate.
			continue
		}
		if opts.GateOn {
			// AddReplace the aggregate FIRST so applier ordering
			// stays "adds before removes" without an extra sort.
			plan = append(plan, ClientUpdatePlan{
				Client:    client,
				Path:      path,
				Action:    ClientUpdateAddReplace,
				EntryName: "mcphub-hub",
				URL:       fmt.Sprintf("http://127.0.0.1:%d/clients/%s/mcp", endpoint.Port, client),
				Headers: map[string]string{
					"X-Mcphub-Hub-Token":   tokens.Tokens[client],
					"X-Mcphub-Instance-Id": endpoint.InstanceID,
				},
			})
			// Remove every previously-installed per-(server, client)
			// entry. codex bot phase5 r2 P2 closure on PR #160:
			// dedupe by server (one Remove per server per client is
			// enough; the on-disk entry name IS the server name, so
			// multiple bindings produce one entry on disk that
			// requires one Remove).
			seenServer := map[string]bool{}
			for _, ref := range refs {
				if seenServer[ref.Server] {
					continue
				}
				seenServer[ref.Server] = true
				plan = append(plan, ClientUpdatePlan{
					Client:     client,
					Path:       path,
					Action:     ClientUpdateRemove,
					EntryName:  ref.Server,
					DaemonName: ref.Daemon,
				})
			}
		} else {
			// Gate OFF: AddReplace each per-(server, client) entry,
			// then Remove the aggregate.
			//
			// codex bot phase5 r1 P2 closure on PR #160: reuse
			// binding url_path (defaults to /mcp).
			//
			// codex bot phase5 r2 P2 closure on PR #160: preserve
			// last-wins manifest precedence. The pre-r2 dedup kept
			// the FIRST binding per server (after sort-by-daemon),
			// which diverged from the per-server install path where
			// AddEntry is replace-by-name and the LAST binding wins.
			// For manifests with multiple bindings under one
			// (server, client) — e.g. different daemon or url_path —
			// the gate-OFF restore would have emitted a different URL
			// than the canonical install would, causing routing
			// mismatch after toggling off. Now: emit every binding
			// in manifest order; the applier's replace-by-name
			// semantics make the LAST AddReplace the one that
			// persists. The Remove for the aggregate runs after, so
			// "adds before removes" ordering is unchanged.
			for _, ref := range refs {
				p := ref.URLPath
				if p == "" {
					p = "/mcp"
				}
				plan = append(plan, ClientUpdatePlan{
					Client:     client,
					Path:       path,
					Action:     ClientUpdateAddReplace,
					EntryName:  ref.Server,
					URL:        fmt.Sprintf("http://localhost:%d%s", ref.Port, p),
					DaemonName: ref.Daemon,
				})
			}
			plan = append(plan, ClientUpdatePlan{
				Client:    client,
				Path:      path,
				Action:    ClientUpdateRemove,
				EntryName: "mcphub-hub",
			})
		}
	}
	return plan, nil
}

// ApplyHubReconcileInOrder applies the plan per spec §"Crash-safe
// reconcile ordering": within one client config rewrite, all
// AddReplace ops execute before any Remove op. Per-client failures
// (either phase) are collected and the reconcile continues with the
// next client; operators rerun to converge.
//
// Each adapter write inside `applyOpsForClient` flows through
// SecureWriteClientConfig (the per-adapter rewrite in
// internal/clients/*.go), so the handle-relative + DACL-bound write
// pipeline applies uniformly across JSON / TOML adapters.
func ApplyHubReconcileInOrder(plan []ClientUpdatePlan) HubReconcileReport {
	byClient := groupReconcileByClient(plan)
	report := HubReconcileReport{}

	// Walk clients in a stable order so the report's Succeeded /
	// Failed slices are deterministic across runs.
	clientNames := make([]string, 0, len(byClient))
	for c := range byClient {
		clientNames = append(clientNames, c)
	}
	sort.Strings(clientNames)

	for _, client := range clientNames {
		ops := byClient[client]
		adds, removes := partitionByAction(ops)
		// AddReplace first.
		if err := callApplyOpsForClient(client, adds); err != nil {
			report.Failed = append(report.Failed, HubReconcileFailure{
				Client: client, Phase: string(ClientUpdateAddReplace), Err: err.Error(),
			})
			continue
		}
		// THEN Remove.
		if err := callApplyOpsForClient(client, removes); err != nil {
			report.Failed = append(report.Failed, HubReconcileFailure{
				Client: client, Phase: string(ClientUpdateRemove), Err: err.Error(),
			})
			continue
		}
		report.Succeeded = append(report.Succeeded, client)
	}
	return report
}

// groupReconcileByClient groups plan ops by Client. The returned map
// preserves the input ordering within each client slice so adapter
// behavior remains stable across reruns.
func groupReconcileByClient(plan []ClientUpdatePlan) map[string][]ClientUpdatePlan {
	out := map[string][]ClientUpdatePlan{}
	for _, op := range plan {
		out[op.Client] = append(out[op.Client], op)
	}
	return out
}

// partitionByAction splits a per-client op slice into (adds, removes).
// AddReplace ops keep their input order; Remove ops keep their input
// order. Other actions are dropped (currently impossible — the type is
// a closed enum — but the check is defensive against a future widening).
func partitionByAction(ops []ClientUpdatePlan) (adds, removes []ClientUpdatePlan) {
	for _, op := range ops {
		switch op.Action {
		case ClientUpdateAddReplace:
			adds = append(adds, op)
		case ClientUpdateRemove:
			removes = append(removes, op)
		}
	}
	return adds, removes
}

// applyOpsForClientForTest is a test seam: when non-nil, the applier
// uses it instead of the production implementation. Tests that want to
// assert ORDERING (rather than on-disk effects) install a recorder via
// this seam. Production callers leave it nil; the runtime path is
// applyOpsForClient.
var applyOpsForClientForTest func(client string, ops []ClientUpdatePlan) error

// callApplyOpsForClient routes through the test seam when set,
// otherwise delegates to the production applyOpsForClient.
func callApplyOpsForClient(client string, ops []ClientUpdatePlan) error {
	if applyOpsForClientForTest != nil {
		return applyOpsForClientForTest(client, ops)
	}
	return applyOpsForClient(client, ops)
}

// applyOpsForClient executes the ordered batch against the adapter
// for `client`. Adapter selection uses `clients.AllClients()` so any
// adapter that fails to construct on the current host (e.g. missing
// $HOME) is silently absent from the map and the corresponding op
// surfaces as "unknown client" — same behavior as the per-server
// install path.
//
// Each op flows through the matching MCPEntry shape:
//
//   - AddReplace `mcphub-hub`: Name="mcphub-hub", URL+Headers; relay
//     fields stay empty (the aggregate is HTTP-only, never relayed).
//   - AddReplace `<server>`: Name=<server>, URL set; relay fields stay
//     empty for HTTP-native adapters. Antigravity is intentionally
//     excluded from the reconcile path because it needs relay metadata
//     the reconcile planner does not have (RelayDaemon + RelayExePath
//     come from the per-server install path).
//   - Remove: name = EntryName; calls RemoveEntry on the adapter.
//
// Idempotent: re-running with the same plan against a converged config
// is a no-op (AddEntry replaces in place; RemoveEntry no-ops on a
// missing entry).
func applyOpsForClient(client string, ops []ClientUpdatePlan) error {
	if len(ops) == 0 {
		return nil
	}
	allClients := clients.AllClients()
	c, ok := allClients[client]
	if !ok {
		return fmt.Errorf("unknown client %q", client)
	}
	if !c.Exists() {
		// Match the per-server install path's tolerance: a client not
		// installed on this host produces no error, the reconcile
		// simply skips its ops. The caller logs Succeeded so the
		// operator sees the run-as-no-op outcome.
		return nil
	}
	for _, op := range ops {
		switch op.Action {
		case ClientUpdateAddReplace:
			entry := clients.MCPEntry{
				Name:    op.EntryName,
				URL:     op.URL,
				Headers: op.Headers,
			}
			if err := c.AddEntry(entry); err != nil {
				return fmt.Errorf("add %s in %s: %w", op.EntryName, client, err)
			}
		case ClientUpdateRemove:
			if err := c.RemoveEntry(op.EntryName); err != nil {
				return fmt.Errorf("remove %s in %s: %w", op.EntryName, client, err)
			}
		}
	}
	return nil
}
