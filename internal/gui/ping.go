// internal/gui/ping.go
package gui

import (
	"encoding/json"
	"errors"
	"net/http"
)

const activationNoTargetReasonHeader = "X-Mcphub-Activation-No-Target-Reason"

func registerPingRoutes(s *Server) {
	s.mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"pid":     s.cfg.PID,
			"version": s.cfg.Version,
		})
	})
	// Authorization: public by design to native loopback clients; browser
	// callers must pass the production allowed-Host and same-origin checks.
	s.mux.HandleFunc("/api/activate-window", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.onActivateWindow == nil {
			// No callback wired — historic behavior was 204 (the test
			// shape Server-with-defaults). Preserve it so a minimal
			// Server (no GUI app wired) still answers cleanly.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		err := s.onActivateWindow()
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, ErrActivationNoTarget):
			// Incumbent reachable but cannot focus or relaunch a
			// window. 503 + reason header + diagnostic body let the
			// second-instance handshake surface a useful message
			// instead of "activated existing mcphub gui" — which would
			// be a lie in this case. Codex bot review on PR #26 P2.
			w.Header().Set(activationNoTargetReasonHeader, string(activationNoTargetReason(err)))
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
}
