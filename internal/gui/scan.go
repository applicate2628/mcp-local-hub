// internal/gui/scan.go
package gui

import (
	"encoding/json"
	"log"
	"net/http"

	"mcp-local-hub/internal/api"
)

func registerScanRoutes(s *Server) {
	s.mux.HandleFunc("/api/scan", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := s.scanner.Scan()
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "SCAN_FAILED")
			return
		}
		result = sanitizeScanResult(result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}))

	// /api/client-capabilities exposes the backend's per-client capability
	// map (scannable / direct_installable / remote_http_capable) WITHOUT
	// running a full scan, so the Catalog can derive its direct-install client
	// choices (direct_installable) from the same single owner the Servers
	// matrix consumes via the scan result. The body is the static
	// api.ClientCapabilities() projection of clientScanners() + IsRelayStdio
	// + remoteHTTPCapableClients + AdoptSupportedClients — no host I/O, no
	// caching concern.
	s.mux.HandleFunc("/api/client-capabilities", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ClientCapabilities())
	}))
}

// sanitizeScanResult deep-copies a scan result and NILs every Raw config blob
// before serialization, so no client-config internals reach the wire. Raw is a
// field of api.ClientEntry, which appears in EVERY map[string]api.ClientEntry
// on api.ScanEntry — currently both ClientPresence AND LegacyConflict (the
// stdio donor row classifyLSPEntries moves into a hub row, scan.go). Earlier
// this only stripped ClientPresence, so a LegacyConflict entry's Raw leaked
// (codex finding 2). Stripping both is ADDITIVE: the global /api/scan that also
// calls this strips strictly more Raw, which is the intended posture; the only
// observable change is that a previously-leaking LegacyConflict.Raw is now nil
// on the wire. If a new Raw-bearing map[string]api.ClientEntry field is ever
// added to api.ScanEntry, it must be cleared here too.
func sanitizeScanResult(in *api.ScanResult) *api.ScanResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Entries = make([]api.ScanEntry, len(in.Entries))
	for i, entry := range in.Entries {
		entryCopy := entry
		entryCopy.ClientPresence = stripClientEntryRaw(entry.ClientPresence)
		entryCopy.LegacyConflict = stripClientEntryRaw(entry.LegacyConflict)
		out.Entries[i] = entryCopy
	}
	return &out
}

// stripClientEntryRaw returns a copy of a client-entry map with every entry's
// Raw config blob nil'd. A nil input returns nil (preserving the omitempty
// shape of LegacyConflict — an absent field stays absent on the wire rather
// than becoming an empty object).
func stripClientEntryRaw(in map[string]api.ClientEntry) map[string]api.ClientEntry {
	if in == nil {
		return nil
	}
	out := make(map[string]api.ClientEntry, len(in))
	for client, clientEntry := range in {
		clientEntry.Raw = nil
		out[client] = clientEntry
	}
	return out
}

// writeAPIError is the canonical error-envelope shape from spec §4.3.
// Shared by all /api/* handlers added in Tasks 9–15.
func writeAPIError(w http.ResponseWriter, err error, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": err.Error(),
		"code":  code,
	})
}

// writeAPIErrorRedacted is the leak-safe sibling of writeAPIError for
// 500-class sites whose err may wrap an *os.PathError embedding the
// operator's absolute home path (C:\Users\<name>\...), which on
// corp-managed hosts reveals the AD username (G16 P2). It centralizes the
// log-server-side + stable-opaque-client-message pattern the manifest.go
// and marketplace_install.go handlers already apply inline:
//
//   - The raw err (with logCtx for routing) is written to the server log
//     so operators can still diagnose from the host.
//   - The client receives ONLY the stable, code-keyed `code` and a fixed
//     generic "internal error" message — never err.Error().
//
// logCtx is the route/operation tag (e.g. "/api/demigrate") prepended to
// the server-side log line so the leaky error stays diagnosable without
// reaching the wire. Use this instead of writeAPIError at any 500-class
// site that forwards a backend error which may carry a filesystem path.
func writeAPIErrorRedacted(w http.ResponseWriter, err error, status int, code string, logCtx string) {
	writeAPIErrorRedactedFields(w, err, status, code, logCtx, nil)
}

// writeAPIErrorRedactedFields preserves the fixed opaque error envelope while
// allowing explicitly typed, non-sensitive boolean facts to survive an error.
// A bool-only map prevents callers from accidentally forwarding paths, process
// identities, command lines, or other backend error detail.
func writeAPIErrorRedactedFields(w http.ResponseWriter, err error, status int, code string, logCtx string, fields map[string]bool) {
	log.Printf("%s: %v", logCtx, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		body[key] = value
	}
	// Fixed redacted fields win even if a future caller accidentally reuses a
	// reserved key in its bool map.
	body["error"] = "internal error"
	body["code"] = code
	_ = json.NewEncoder(w).Encode(body)
}
