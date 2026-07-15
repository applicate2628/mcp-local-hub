package gui

import (
	"bytes"
	"log"
	"net/http"

	"mcp-local-hub/internal/api"
)

type deAdoptRequest struct {
	Server                string   `json:"server"`
	AcceptConflictClients []string `json:"accept_conflict_clients"`
}

type deAdoptEligibilityResponse struct {
	Eligible      bool     `json:"eligible"`
	AdoptOwned    bool     `json:"adopt_owned"`
	GateOn        bool     `json:"gate_on"`
	GateOnClients []string `json:"gate_on_clients"`
	BlockedReason string   `json:"blocked_reason"`
}

func registerDeAdoptRoutes(s *Server) {
	s.mux.HandleFunc("/api/deadopt/plan", s.requireSameOrigin(s.deAdoptPlanHandler))
	s.mux.HandleFunc("/api/deadopt", s.requireSameOrigin(s.deAdoptHandler))
	s.mux.HandleFunc("/api/deadopt/eligible", s.requireSameOrigin(s.deAdoptEligibleHandler))
	s.mux.HandleFunc("/api/deadopt/recoverable", s.requireSameOrigin(s.deAdoptRecoverableHandler))
}

func (s *Server) deAdoptPlanHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req deAdoptRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	plan, err := api.NewAPI().BuildDeAdoptPlan(req.Server)
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusBadRequest, "DEADOPT_PLAN_FAILED", "/api/deadopt/plan")
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) deAdoptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req deAdoptRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	var narration bytes.Buffer
	report, err := api.NewAPI().ExecuteDeAdoptWithOpts(req.Server, &narration, api.ExecuteDeAdoptOpts{
		AcceptConflictClients: req.AcceptConflictClients,
	})
	if err != nil {
		if narration.Len() != 0 {
			log.Printf("/api/deadopt execution output before failure:\n%s", narration.String())
		}
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "DEADOPT_FAILED", "/api/deadopt")
		return
	}

	// The executor owns the deadopt-* lifecycle events. This distinct GUI audit
	// row records only the operator action and redaction-safe report fields.
	s.events.PublishOperatorAction("deadopt", api.CurrentOSUser(), map[string]any{
		"server":       req.Server,
		"restored":     report.Restored,
		"accepted":     report.Accepted,
		"failed_count": len(report.Failed),
	})
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) deAdoptEligibleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plan, err := api.NewAPI().BuildDeAdoptPlan(r.URL.Query().Get("server"))
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusBadRequest, "DEADOPT_ELIGIBILITY_FAILED", "/api/deadopt/eligible")
		return
	}
	eligibility := plan.Eligibility
	writeJSON(w, http.StatusOK, deAdoptEligibilityResponse{
		Eligible:      eligibility.Eligible,
		AdoptOwned:    eligibility.AdoptOwned,
		GateOn:        eligibility.GateOn,
		GateOnClients: eligibility.GateOnClients,
		BlockedReason: eligibility.BlockedReason,
	})
}

func (s *Server) deAdoptRecoverableHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	names, err := api.ListDeAdoptRecoverableManifestNames()
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "DEADOPT_RECOVERABLE_FAILED", "/api/deadopt/recoverable")
		return
	}
	writeJSON(w, http.StatusOK, names)
}
