// internal/gui/servers.go
package gui

import (
	"encoding/json"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// registerServerRoutes wires POST /api/servers/:name/{restart,stop} onto
// the mux. URL pattern: /api/servers/<name>/<action>. Both actions share
// the same per-task RestartResult shape and the same 200/207/500 contract;
// only the response key (restart_results vs stop_results) and the error
// code differ.
func registerServerRoutes(s *Server) {
	s.mux.HandleFunc("/api/servers/", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/servers/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		name, action := parts[0], parts[1]
		switch action {
		case "restart":
			writeServerActionResult(w, r, func() ([]api.RestartResult, error) {
				return s.restart.Restart(name)
			}, "restart_results", "RESTART_FAILED")
		case "stop":
			writeServerActionResult(w, r, func() ([]api.RestartResult, error) {
				return s.stop.Stop(name)
			}, "stop_results", "STOP_FAILED")
		default:
			http.NotFound(w, r)
		}
	}))
}

// writeServerActionResult is the shared response writer for the restart and
// stop actions. Both run a per-task fan-out that returns the same
// RestartResult slice; the only differences across actions are the JSON
// response key and the error code, so they pass those in.
func writeServerActionResult(
	w http.ResponseWriter, r *http.Request,
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
