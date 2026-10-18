package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// SetGUIQuitHandler composes the original GUI lifetime's cooperative Quit
// operation. The composition root sets it before the server serves requests.
func (s *Server) SetGUIQuitHandler(handler GUIQuitHandler) {
	s.guiQuitHandler = handler
}

func registerGUIQuitRoutes(s *Server) {
	s.mux.HandleFunc("/api/gui/quit", s.requireSameOrigin(s.guiQuitHTTPHandler))
}

type guiQuitHTTPResponse struct {
	GUIQuitResult
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

func (s *Server) guiQuitHTTPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	// Reuse the common control-body size and single-value/trailing-data
	// checks, then apply this operation's strict request-field contract.
	var body json.RawMessage
	if err := decodeJSONBodyLimited(w, r, &body, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	var request GUIQuitRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeAPIError(w, errors.New("invalid GUI Quit request"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(request.ExpectedGeneration) == "" {
		writeAPIError(w, errors.New("expected_generation is required"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	if s.guiQuitHandler == nil {
		writeAPIError(w, errors.New("GUI Quit is unavailable"), http.StatusServiceUnavailable, "GUI_QUIT_UNAVAILABLE")
		return
	}

	result, err := s.guiQuitHandler(r.Context(), request)
	response := guiQuitHTTPResponse{GUIQuitResult: result}
	status := http.StatusAccepted
	switch {
	case err != nil && result.Accepted:
		status = http.StatusInternalServerError
		response.Error, response.Code = "GUI Quit intent accepted with an incomplete outcome", "GUI_QUIT_PARTIAL"
	case errors.Is(err, ErrGUIQuitGeneration):
		status = http.StatusConflict
		response.Error, response.Code = "GUI generation changed or is unavailable for Quit", "GUI_QUIT_GENERATION_CONFLICT"
	case errors.Is(err, api.ErrGUIControlConflict):
		status = http.StatusConflict
		response.Error, response.Code = "GUI control state changed", "GUI_CONTROL_CONFLICT"
	case errors.Is(err, api.ErrGUIStoppedByUser):
		status = http.StatusConflict
		response.Error, response.Code = "GUI is stopped; run mcphub gui --resume to start it", "GUI_STOPPED_BY_USER"
	case err != nil:
		status = http.StatusInternalServerError
		response.Error, response.Code = "GUI Quit failed", "GUI_QUIT_FAILED"
	case !result.Accepted:
		status = http.StatusServiceUnavailable
		response.Error, response.Code = "GUI Quit was not accepted", "GUI_QUIT_REFUSED"
	}
	if err != nil {
		// Preserve the actual callback cause only in the existing server log.
		log.Printf("/api/gui/quit: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("/api/gui/quit response: %v", err)
	}
	// The callback owns shutdown after durable intent. Neither response
	// encoding nor flushing is permission to perform or cancel that shutdown.
}
