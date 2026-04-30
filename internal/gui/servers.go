// internal/gui/servers.go
package gui

import (
	"encoding/json"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// registerServerRoutes wires POST /api/servers/:name/{restart,stop} onto
// the mux. URL pattern: /api/servers/<name>/<action>[?daemon=<daemon>].
//
// The optional ?daemon query narrows the action to a single daemon of a
// multi-daemon server (serena ships claude + codex). Without it the
// action targets every daemon of the server — existing single-daemon
// contract. Validation matrix (per Codex CLI consult, 2026-04-30):
//
//	?daemon absent              → all daemons
//	?daemon=name                → that daemon only; unknown → 404
//	?daemon=  (empty)           → 400
//	?daemon=a&daemon=b          → 400
//
// Both actions share the same per-task RestartResult shape and the same
// 200/207/500 contract; only the response key (restart_results vs
// stop_results) and the error code differ.
func registerServerRoutes(s *Server) {
	s.mux.HandleFunc("/api/servers/", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/servers/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		name, action := parts[0], parts[1]
		daemon, ok := parseDaemonQuery(w, r)
		if !ok {
			return // parseDaemonQuery wrote the 400 response
		}
		switch action {
		case "restart":
			writeServerActionResult(w, r, daemon, func() ([]api.RestartResult, error) {
				return s.restart.Restart(name, daemon)
			}, "restart_results", "RESTART_FAILED")
		case "stop":
			writeServerActionResult(w, r, daemon, func() ([]api.RestartResult, error) {
				return s.stop.Stop(name, daemon)
			}, "stop_results", "STOP_FAILED")
		default:
			http.NotFound(w, r)
		}
	}))
}

// parseDaemonQuery extracts the optional ?daemon=<name> query value.
// Empty string ("?daemon=") and repeated values ("?daemon=a&daemon=b")
// are user errors that earn a 400 — they almost always indicate a
// frontend bug, and silently treating them as "all daemons" would mask
// the bug behind a multi-daemon mass restart.
//
// Returns ok=false after writing the 400 response; callers should
// return immediately. Returns daemon="" (with ok=true) when the query
// is absent — the caller forwards "" to api.Restart/Stop, which is the
// "all daemons" path.
func parseDaemonQuery(w http.ResponseWriter, r *http.Request) (daemon string, ok bool) {
	values, present := r.URL.Query()["daemon"]
	if !present {
		return "", true
	}
	if len(values) > 1 {
		http.Error(w, `multiple "daemon" query parameters not allowed`, http.StatusBadRequest)
		return "", false
	}
	if values[0] == "" {
		http.Error(w, `"daemon" query parameter must not be empty`, http.StatusBadRequest)
		return "", false
	}
	return values[0], true
}

// writeServerActionResult is the shared response writer for the restart and
// stop actions. Both run a per-task fan-out that returns the same
// RestartResult slice; the only differences across actions are the JSON
// response key and the error code, so they pass those in.
//
// daemonFilter is the value parsed from ?daemon=. When non-empty, an
// empty results slice means "no scheduler task matched the filter" =
// unknown daemon → 404 (per Codex CLI consult, "unknown daemon must
// not silently fall back to all"). When empty, an empty results slice
// just means "no daemons configured for this server"; that is normal
// for newly-installed servers and stays 200.
func writeServerActionResult(
	w http.ResponseWriter, r *http.Request, daemonFilter string,
	run func() ([]api.RestartResult, error),
	resultsKey, errCode string,
) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	results, err := run()
	if results == nil {
		results = []api.RestartResult{}
	}
	body := map[string]any{resultsKey: results}
	if err != nil {
		body["error"] = err.Error()
		body["code"] = errCode
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	if daemonFilter != "" && len(results) == 0 {
		// Filter targeted a daemon that does not exist for this server.
		// Surface as 404 so the frontend can show a real error instead
		// of "Restarted" on a no-op.
		body["error"] = "daemon not found"
		body["code"] = "DAEMON_NOT_FOUND"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	status := http.StatusOK
	for _, row := range results {
		if row.Err != "" {
			status = http.StatusMultiStatus // 207
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
