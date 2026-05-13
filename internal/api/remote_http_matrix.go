// internal/api/remote_http_matrix.go — single source of truth for the
// list of client adapters that support transport=remote-http.
//
// G6 ships header round-trip via clients/*.go for six MCP clients;
// antigravity is stdio-relay only and is rejected at install plan
// time. Every place that needs to enumerate "which clients can take
// a remote-http binding" must read from remoteHTTPCapableClients to
// stay consistent. Pre-centralization, the install/test surface and
// the draft-emission surface kept the list separately and drifted
// (cumulative G6 review caught qwen-cli missing from drafts).
//
// Keep this list sorted alphabetically so the emitted YAML is stable
// across runs and diffs.

package api

// remoteHTTPCapableClients is the canonical adapter list for the
// transport=remote-http install/draft path. Adding a new adapter
// here requires three confirmations:
//
//  1. The adapter's AddEntry serializes Headers to its client config.
//  2. The adapter's GetEntry round-trips Headers (so rollback's
//     priorEntry snapshot is lossless).
//  3. Header round-trip is pinned by a test in
//     internal/clients/headers_roundtrip_test.go.
var remoteHTTPCapableClients = []string{
	"claude-code",
	"codex-cli",
	"cursor",
	"gemini-cli",
	"qwen-cli",
	"vscode",
}

// isRemoteHTTPCapableClient reports whether the named client adapter
// supports a transport=remote-http binding. Used by the install plan
// gate so a manifest declaring a remote-http binding for a non-
// capable client (antigravity today) fails at plan build with a
// clear error, before any client config is touched.
func isRemoteHTTPCapableClient(name string) bool {
	for _, c := range remoteHTTPCapableClients {
		if c == name {
			return true
		}
	}
	return false
}

// displayURLOf returns u.DisplayURL when set, falling back to u.URL.
// Plan + install stdout sites route every URL print through this
// helper so the operator's terminal sees the pre-expansion form for
// remote-http (no leaked secret bytes) while local manifests keep
// the existing URL display.
func displayURLOf(u ClientUpdatePlan) string {
	if u.DisplayURL != "" {
		return u.DisplayURL
	}
	return u.URL
}
