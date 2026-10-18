// internal/gui/secrets.go
package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

const (
	secretsInitCapabilityKey   = "init-capabilities"
	secretsInitCapabilityPath  = "/api/secrets/" + secretsInitCapabilityKey
	secretsInitCapabilityAllow = "GET, PUT, DELETE"
)

func registerSecretsRoutes(s *Server) {
	s.mux.HandleFunc("/api/secrets/init", s.requireSameOrigin(s.secretsInitHandler))
	s.mux.HandleFunc("GET "+secretsInitCapabilityPath, s.requireSameOrigin(s.secretsInitCapabilitiesHandler))
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
//   - 500 {code:"SECRETS_INIT_FAILED", cleanup_status:"failed"} — init + cleanup both failed
//   - 409 {code:"SECRETS_INIT_BLOCKED"}          — pre-existing files
//   - 500 {code:"SECRETS_INIT_FAILED", recovery_status:…} — evidence persistence failed
//
// Authorization: the registered route retains the Server Host and same-origin
// guards. Headerless local callers remain supported; no private report/action
// authority is granted by this projection.
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
	var persistence *api.SecretsRecoveryPersistenceError
	if errors.As(err, &persistence) {
		body := map[string]any{
			"error": "Recovery evidence could not be verified. Reload or update the application and contact local support.",
			"code":  "SECRETS_INIT_FAILED",
		}
		addSecretsInitRecoveryReference(body, persistence.RecoveryReference())
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	var blocked *api.SecretsInitBlocked
	if errors.As(err, &blocked) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "Initialization is blocked by existing files. Reload or update the application and contact local support.",
			"code":  "SECRETS_INIT_BLOCKED",
		})
		return
	}
	var initFailed *api.SecretsInitFailed
	if errors.As(err, &initFailed) {
		body := map[string]any{
			"error":          "Initialization failed. Reload or update the application and contact local support.",
			"code":           "SECRETS_INIT_FAILED",
			"cleanup_status": initFailed.CleanupStatus,
		}
		addSecretsInitRecoveryReference(body, initFailed.RecoveryReference())
		if initFailed.CleanupStatus == "ok" {
			body["vault_state"] = "missing"
			writeJSON(w, http.StatusOK, body)
			return
		}
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": "Initialization failed. Reload or update the application and contact local support.",
		"code":  "SECRETS_INIT_FAILED",
	})
}

func addSecretsInitRecoveryReference(body map[string]any, reference api.PublicRecoveryReference) {
	if reference.ID != "" {
		body["recovery_id"] = reference.ID
	}
	if reference.Status != "" {
		body["recovery_status"] = reference.Status
	}
}

// Authorization: the canonical registration applies the existing Host/Origin
// guards. This static compatibility declaration carries no private data,
// operator capability or recovery action, and invokes no secrets backend.
func (s *Server) secretsInitCapabilitiesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", secretsInitCapabilityAllow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		SchemaVersion  int    `json:"schema_version"`
		Workflow       string `json:"workflow"`
		PathFreeErrors bool   `json:"path_free_errors"`
	}{1, "cli_support_readonly", true})
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
		if err := decodeJSONBodyLimited(w, r, &body, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "SECRETS_INVALID_JSON")
			return
		}
		if err := s.secrets.Set(body.Name, body.Value); err != nil {
			writeSecretsOpError(w, err)
			return
		}
		// gui-events.log audit row (deep-review P3 finding): emit only
		// after the vault write committed. KEY NAME ONLY — body.Value
		// (the credential material) never reaches this call or any log;
		// mirrors the discipline emitSecretAuditEvent already applies to
		// the hub-mcp.log audit trail (P2.4, PR #468) for the same
		// mutation.
		s.events.PublishOperatorAction("secret-set", api.CurrentOSUser(), map[string]any{
			"key": body.Name,
		})
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
//
//	/api/secrets/<key>          → secretsKeyRoot  (PUT, DELETE)
//	/api/secrets/<key>/restart  → secretsKeyRestart (POST)
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
		if err := decodeJSONBodyLimited(w, r, &body, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "SECRETS_INVALID_JSON")
			return
		}
		res, err := s.secrets.Rotate(key, body.Value, body.Restart)
		// gui-events.log audit row (deep-review P3 finding): the credential
		// mutation is COMMITTED the moment res.VaultUpdated is true, even
		// when the subsequent restart orchestration fails (PR #476 bot P2:
		// the old emit sat AFTER the orchestration-error return, so a
		// rotate that changed the vault but failed the restart left NO
		// audit row for a committed credential change). Emit on the
		// committed-rotation fact, independent of the restart outcome —
		// matching the API-side emitSecretAuditEvent which audits right
		// after the vault commit, before the restart phase. A pre-commit
		// failure (*SecretsOpError: invalid name / empty value / vault not
		// initialized) returns res.VaultUpdated==false, so no row fires.
		// KEY NAME ONLY — body.Value never reaches this call or any log.
		if res.VaultUpdated {
			s.events.PublishOperatorAction("secret-rotate", api.CurrentOSUser(), map[string]any{
				"key":               key,
				"restart_requested": body.Restart,
			})
		}
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
		// gui-events.log audit row (deep-review P3 finding): emit only
		// after the vault delete committed. KEY NAME ONLY.
		s.events.PublishOperatorAction("secret-delete", api.CurrentOSUser(), map[string]any{
			"key": key,
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		allow := "PUT, DELETE"
		if key == secretsInitCapabilityKey {
			allow = secretsInitCapabilityAllow
		}
		w.Header().Set("Allow", allow)
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
