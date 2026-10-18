// internal/gui/dismiss.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// dismissRequest is the /api/dismiss POST and DELETE body. Matches the
// Migration screen's row actions: one server name per click.
type dismissRequest struct {
	Server string `json:"server"`
}

// dismissedResponse is the /api/dismissed GET body shape. The single
// "unknown" key is deliberately future-proof: A4 Settings may later
// add per-entry metadata (timestamp, per-client granularity) as
// additional keys alongside it, and the frontend can ignore fields
// it doesn't understand.
type dismissedResponse struct {
	Unknown []string `json:"unknown"`
}

func registerDismissRoutes(s *Server) {
	s.mux.HandleFunc("/api/dismiss", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req dismissRequest
		if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "BAD_REQUEST")
			return
		}
		// Normalize before both the empty-check and the persist call so
		// " server " doesn't pass validation and then store a key that
		// won't match scan entries during filtering. (PR #4 Codex R3.)
		name := strings.TrimSpace(req.Server)
		if name == "" {
			writeAPIError(w, fmt.Errorf("server must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if r.Method == http.MethodDelete {
			if err := s.dismisser.UndismissUnknown(name); err != nil {
				writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "UNDISMISS_FAILED", "/api/dismiss DELETE")
				return
			}
		} else {
			if err := s.dismisser.DismissUnknown(name); err != nil {
				writeAPIError(w, err, http.StatusInternalServerError, "DISMISS_FAILED")
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	s.mux.HandleFunc("/api/dismissed", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		set, err := s.dismisser.ListDismissedUnknown()
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "DISMISSED_LIST_FAILED")
			return
		}
		// Always emit a non-nil slice so the frontend sees `[]`
		// instead of `null` (see TestDismissedHandler_EmptyListReturnsEmptyArray).
		names := make([]string, 0, len(set))
		for n := range set {
			names = append(names, n)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dismissedResponse{Unknown: names})
	}))
}
