package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/api"
)

// extractor is the narrow interface the /api/extract-manifest handler needs.
// realExtractor is the production adapter; tests inject their own.
//
// The client+server pair is what A1's Create-manifest button sends as query
// parameters. The opts argument is ignored by realExtractor (it builds its
// own ScanOpts from the user's home dir, mirroring internal/cli/manifest.go's
// `manifest extract` command) — it exists so tests can redirect the four
// client config paths without faking HOME.
type extractor interface {
	ExtractManifestFromClient(client, server string, opts api.ScanOpts) (string, error)
}

type realExtractor struct{}

// ExtractManifestFromClient delegates to api.ExtractManifestFromClient with
// ScanOpts fully populated from the user's home dir — the four client config
// paths and an empty ManifestDir (embed-first resolution). This mirrors
// internal/cli/manifest.go:newManifestExtractCmd so behavior is identical
// between the CLI and the GUI wrapper.
//
// The opts argument is intentionally ignored by the production path; tests
// inject their own extractor stub to redirect paths under a temp home.
func (realExtractor) ExtractManifestFromClient(client, server string, _ api.ScanOpts) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return api.NewAPI().ExtractManifestFromClient(client, server, api.ScanOpts{
		ClaudeConfigPath:      filepath.Join(home, ".claude.json"),
		CodexConfigPath:       filepath.Join(home, ".codex", "config.toml"),
		GeminiConfigPath:      filepath.Join(home, ".gemini", "settings.json"),
		AntigravityConfigPath: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		ManifestDir:           "", // embed-first; matches CLI scanManifestDir() in prod
	})
}

type extractManifestResponse struct {
	YAML string `json:"yaml"`
}

// registerExtractManifestRoutes wires GET /api/extract-manifest
// ?client=<name>&server=<name> -> 200 {yaml: string} | 400 | 500.
//
// Shape mirrors /api/manifest/validate: JSON response body, writeAPIError
// for failures. Called by the GUI AddServer prefill effect when the user
// arrives via A1 Migration's Create-manifest button.
func registerExtractManifestRoutes(s *Server) {
	s.mux.HandleFunc("/api/extract-manifest", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		client := r.URL.Query().Get("client")
		server := r.URL.Query().Get("server")
		if client == "" || server == "" {
			writeAPIError(w, fmt.Errorf("client and server required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		yaml, err := s.extractor.ExtractManifestFromClient(client, server, api.ScanOpts{})
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "EXTRACT_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(extractManifestResponse{YAML: yaml})
	}))
}
