// internal/gui/version.go
package gui

import (
	"encoding/json"
	"net/http"
	"runtime"

	"mcp-local-hub/internal/buildinfo"
)

// versionDTO is the JSON payload of GET /api/version. Mirrors the
// fields rendered by `mcphub version` so the About screen can show
// the same metadata. Homepage / issues / license / author links are
// static strings owned by this layer because they're presentational
// constants, not build inputs.
type versionDTO struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	Homepage  string `json:"homepage"`
	Issues    string `json:"issues"`
	License   string `json:"license"`
	Author    string `json:"author"`
}

func registerVersionRoutes(s *Server) {
	// Read-only metadata, but still gate behind requireSameOrigin to
	// match the rest of /api/* — the version page is reachable from
	// the GUI's own origin and shouldn't leak to a malicious page that
	// happened to reach the listener.
	s.mux.HandleFunc("/api/version", s.requireSameOrigin(s.versionHandler))
}

func (s *Server) versionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	v, c, d := buildinfo.Get()
	dto := versionDTO{
		Version:   v,
		Commit:    c,
		BuildDate: d,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Homepage:  "https://github.com/applicate2628/mcp-local-hub",
		Issues:    "https://github.com/applicate2628/mcp-local-hub/issues",
		License:   "Apache-2.0",
		Author:    "Dmitry Denisenko (@applicate2628)",
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(dto); err != nil {
		// Same pattern as other handlers: writing the JSON header
		// already committed the response, so a marshal error here is
		// best-effort logged via http.Error and otherwise dropped.
		http.Error(w, "version encode failed", http.StatusInternalServerError)
	}
}
