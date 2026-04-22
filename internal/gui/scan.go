// internal/gui/scan.go
package gui

import (
	"encoding/json"
	"net/http"
)

func registerScanRoutes(s *Server) {
	s.mux.HandleFunc("/api/scan", func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
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
