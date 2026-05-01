// Package gui — daemon-lifecycle HTTP routes. Memo §4.
package gui

import (
	"encoding/json"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

func registerDaemonsRoutes(s *Server) {
	s.mux.HandleFunc("/api/daemons/weekly-refresh-membership",
		s.requireSameOrigin(s.weeklyRefreshMembershipHandler))
}

func (s *Server) weeklyRefreshMembershipHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body []api.MembershipDelta
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "bad_json",
			"detail": err.Error(),
		})
		return
	}
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":  "registry_path",
			"detail": err.Error(),
		})
		return
	}
	updated, err := api.UpdateWeeklyRefreshMembership(regPath, body)
	if err != nil {
		// Validation errors (unknown pair) → 400; storage errors → 500.
		status := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "save registry") ||
			strings.HasPrefix(err.Error(), "load registry") ||
			strings.HasPrefix(err.Error(), "acquire lock") {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]string{
			"error":  "membership_failed",
			"detail": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":  updated,
		"warnings": []string{},
	})
}
