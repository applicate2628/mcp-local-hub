// internal/gui/secrets.go
package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

func registerSecretsRoutes(s *Server) {
	s.mux.HandleFunc("/api/secrets/init", s.requireSameOrigin(s.secretsInitHandler))
	s.mux.HandleFunc("/api/secrets", s.requireSameOrigin(s.secretsListOrAddHandler))
	s.mux.HandleFunc("/api/secrets/", s.requireSameOrigin(s.secretsByKeyHandler))
}

// writeJSON is the shared helper that sets Content-Type, writes the
// status code, and encodes body as JSON. All secrets handlers use this
// instead of duplicating the three-line pattern inline.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// secretsInitHandler handles POST /api/secrets/init.
//
// Four outcomes per memo §5.1 D2:
//   - 200 {vault_state:"ok"}                    — idempotent or fresh init
//   - 200 {code:"SECRETS_INIT_FAILED", cleanup_status:"ok", vault_state:"missing"} — init failed, cleanup succeeded
//   - 500 {code:"SECRETS_INIT_FAILED", cleanup_status:"failed", orphan_path:…}     — init + cleanup both failed
//   - 409 {code:"SECRETS_INIT_BLOCKED"}          — pre-existing files
func (s *Server) secretsInitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res, err := s.secrets.Init()
	if err == nil {
		writeJSON(w, http.StatusOK, res)
		return
	}
	var blocked *api.SecretsInitBlocked
	if errors.As(err, &blocked) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(),
			"code":  "SECRETS_INIT_BLOCKED",
		})
		return
	}
	var initFailed *api.SecretsInitFailed
	if errors.As(err, &initFailed) {
		body := map[string]any{
			"error":          err.Error(),
			"code":           "SECRETS_INIT_FAILED",
			"cleanup_status": initFailed.CleanupStatus,
		}
		if initFailed.OrphanPath != "" {
			body["orphan_path"] = initFailed.OrphanPath
		}
		if initFailed.CleanupStatus == "ok" {
			body["vault_state"] = "missing"
			writeJSON(w, http.StatusOK, body)
			return
		}
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": err.Error(),
		"code":  "SECRETS_INIT_FAILED",
	})
}

// secretsListOrAddHandler handles GET /api/secrets and POST /api/secrets.
func (s *Server) secretsListOrAddHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		env, err := s.secrets.List()
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "SECRETS_LIST_FAILED")
			return
		}
		writeJSON(w, http.StatusOK, env)
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "SECRETS_INVALID_JSON")
			return
		}
		if err := s.secrets.Set(body.Name, body.Value); err != nil {
			writeSecretsOpError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// writeSecretsOpError maps a *api.SecretsOpError to the correct HTTP
// status using opErrorStatus, falling back to 500 for unexpected types.
// Named "SecretsOpError" to match the renamed type (was SecretsSetError
// in the original plan draft; renamed in commit 7a6378e).
func writeSecretsOpError(w http.ResponseWriter, err error) {
	var opErr *api.SecretsOpError
	if errors.As(err, &opErr) {
		status := opErrorStatus(opErr.Code)
		writeAPIError(w, err, status, opErr.Code)
		return
	}
	writeAPIError(w, err, http.StatusInternalServerError, "SECRETS_SET_FAILED")
}

// opErrorStatus maps a SecretsOpError.Code to an HTTP status code per
// memo §5.7. Cases not listed here fall through to 500.
func opErrorStatus(code string) int {
	switch code {
	case "SECRETS_INVALID_NAME", "SECRETS_EMPTY_VALUE":
		return http.StatusBadRequest
	case "SECRETS_KEY_EXISTS", "SECRETS_VAULT_NOT_INITIALIZED":
		return http.StatusConflict
	case "SECRETS_KEY_NOT_FOUND":
		return http.StatusNotFound
	case "SECRETS_LIST_FAILED":
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// secretsByKeyHandler handles all /api/secrets/<key>[/<action>] paths.
// Routing:
//   /api/secrets/<key>          → secretsKeyRoot  (PUT, DELETE)
//   /api/secrets/<key>/restart  → secretsKeyRestart (POST)
//
// The /api/secrets/init pattern is registered more specifically and wins
// over this handler in Go's ServeMux.
func (s *Server) secretsByKeyHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/secrets/")
	if rest == "" || rest == "init" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	key := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		s.secretsKeyRoot(w, r, key)
	case "restart":
		s.secretsKeyRestart(w, r, key)
	default:
		http.NotFound(w, r)
	}
}

// secretsKeyRoot handles PUT and DELETE on /api/secrets/<key>.
func (s *Server) secretsKeyRoot(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Value   string `json:"value"`
			Restart bool   `json:"restart"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "SECRETS_INVALID_JSON")
			return
		}
		res, err := s.secrets.Rotate(key, body.Value, body.Restart)
		if err != nil {
			// Validation / vault-access errors carry *api.SecretsOpError.
			var opErr *api.SecretsOpError
			if errors.As(err, &opErr) {
				writeSecretsOpError(w, err)
				return
			}
			// Orchestration failure: vault was updated but restart aborted.
			// Codex plan-R2 P2: normalize nil restart_results to empty
			// array so the wire contract always carries a JSON array.
			results := res.RestartResults
			if results == nil {
				results = []api.RestartResult{}
			}
			full := map[string]any{
				"vault_updated":   res.VaultUpdated,
				"restart_results": results,
				"error":           err.Error(),
				"code":            "RESTART_FAILED",
			}
			writeJSON(w, http.StatusInternalServerError, full)
			return
		}
		// res.RestartResults may be nil when restart=false; ensure non-nil for JSON.
		if res.RestartResults == nil {
			res.RestartResults = []api.RestartResult{}
		}
		anyFailed := false
		for _, rr := range res.RestartResults {
			if rr.Err != "" {
				anyFailed = true
				break
			}
		}
		status := http.StatusOK
		if anyFailed {
			status = http.StatusMultiStatus
		}
		writeJSON(w, status, res)
	case http.MethodDelete:
		confirm := r.URL.Query().Get("confirm") == "true"
		if err := s.secrets.Delete(key, confirm); err != nil {
			writeSecretsDeleteError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// secretsKeyRestart handles POST /api/secrets/<key>/restart.
// Runs the restart phase only — does NOT modify the vault (memo §5.4a).
func (s *Server) secretsKeyRestart(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	results, err := s.secrets.Restart(key)
	if results == nil {
		results = []api.RestartResult{}
	}
	if err != nil {
		var opErr *api.SecretsOpError
		if errors.As(err, &opErr) {
			writeSecretsOpError(w, err)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":           err.Error(),
			"code":            "RESTART_FAILED",
			"restart_results": results,
		})
		return
	}
	anyFailed := false
	for _, rr := range results {
		if rr.Err != "" {
			anyFailed = true
			break
		}
	}
	status := http.StatusOK
	if anyFailed {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{"restart_results": results})
}

// writeSecretsDeleteError maps *api.SecretsDeleteError (409 with
// used_by or manifest_errors) and *api.SecretsOpError (404/409/500)
// to the correct response. Unknown errors fall back to 500.
func writeSecretsDeleteError(w http.ResponseWriter, err error) {
	var de *api.SecretsDeleteError
	if errors.As(err, &de) {
		body := map[string]any{
			"error": de.Message,
			"code":  de.Code,
		}
		if de.Code == "SECRETS_HAS_REFS" {
			body["used_by"] = de.UsedBy
		}
		if de.Code == "SECRETS_USAGE_SCAN_INCOMPLETE" {
			body["manifest_errors"] = de.ManifestErrors
		}
		writeJSON(w, http.StatusConflict, body)
		return
	}
	var opErr *api.SecretsOpError
	if errors.As(err, &opErr) {
		writeSecretsOpError(w, err)
		return
	}
	// Defensive fallback: SecretsDelete in the api layer should always
	// return either *SecretsDeleteError, *SecretsOpError, or nil. This
	// branch only fires if a future caller introduces an untyped error
	// path that bypasses the catalog. Stable label so a stray unknown
	// failure surfaces as something operators can grep for.
	writeAPIError(w, err, http.StatusInternalServerError, "SECRETS_DELETE_FAILED")
}
