package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"mcp-local-hub/internal/api"
)

type clientEntryRemover interface {
	RemoveClientEntry(context.Context, api.ClientEntryRemovalRequest) (*api.ClientEntryRemovalResult, error)
}

const clientEntryRemovalRequestTimeout = 15 * time.Second

func registerClientEntryRemovalRoutes(s *Server) {
	// Authorization: existing loopback same-origin operator control boundary;
	// exact selection and positive apply are enforced by the API owner.
	s.mux.HandleFunc("/api/client-entry/remove", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req api.ClientEntryRemovalRequest
		if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "BAD_REQUEST")
			return
		}
		if err := api.ValidateClientEntryRemovalRequest(req); err != nil {
			writeAPIErrorRedacted(w, err, http.StatusBadRequest, "BAD_REQUEST", "/api/client-entry/remove")
			return
		}
		if s.clientEntryRemover == nil {
			writeAPIErrorRedacted(w, errors.New("removal service unavailable"), http.StatusInternalServerError, "REMOVAL_SERVICE_FAILED", "/api/client-entry/remove")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), clientEntryRemovalRequestTimeout)
		defer cancel()
		result, err := s.clientEntryRemover.RemoveClientEntry(ctx, req)
		if err != nil || result == nil {
			if err == nil {
				err = errors.New("missing removal result")
			}
			writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "REMOVAL_SERVICE_FAILED", "/api/client-entry/remove")
			return
		}
		status := http.StatusMultiStatus
		if (result.Outcome == "removed" || result.Outcome == "absent") && result.EffectiveAbsent && result.Code == "REMOVAL_SETTLED" && result.Receipt != nil {
			status = http.StatusOK
		}
		if result.Outcome == "removed" && s.events != nil {
			s.events.PublishOperatorAction("remove-client-entry", api.CurrentOSUser(), map[string]any{"server": result.Server, "client": result.Client})
		}
		if s.events != nil && (result.Outcome == "removed" || result.Outcome == "applied-unverified" || result.Outcome == "rollback-conflict") {
			s.events.Publish(Event{Type: "clients-rescan", Body: map[string]any{"server": result.Server, "client": result.Client}})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(result)
	}))
}
