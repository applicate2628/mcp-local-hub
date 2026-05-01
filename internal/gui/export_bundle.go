// Package gui — POST /api/export-config-bundle handler. Memo D11.
package gui

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"mcp-local-hub/internal/api"
)

func registerExportBundleRoutes(s *Server) {
	s.mux.HandleFunc("/api/export-config-bundle",
		s.requireSameOrigin(s.exportConfigBundleHandler))
}

func (s *Server) exportConfigBundleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	filename := fmt.Sprintf("mcphub-bundle-%s.zip", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if err := api.WriteConfigBundle(w); err != nil {
		fmt.Fprintf(os.Stderr, "export-config-bundle: %v\n", err)
	}
}
