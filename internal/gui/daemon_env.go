// Package gui — Task 4.2 daemon-env / discovery-refresh / respawn handlers.
//
// Three POST-only routes serve the Servers-matrix env-overlay surface:
//
//   POST /api/daemon/env         — operator-edit Apply (overlay write only,
//                                  NO supervisor IPC). Validates taskName
//                                  is in current supervisor-intent.json
//                                  AND env keys/values are character-safe;
//                                  writes a `source: operator` row via
//                                  daemon_env_overlay.WriteOverlay.
//                                  Operator clicks Restart separately.
//   POST /api/discovery/refresh  — re-run binary_discovery against the
//                                  installed manifests and overwrite
//                                  non-operator rows in the overlay.
//                                  Useful when the operator installs a
//                                  new SDK / binary into one of the
//                                  hint directories after first install.
//   POST /api/daemon/respawn     — dial supervisor IPC with the canonical
//                                  `respawn` verb; force-bool forwards
//                                  through. Maps supervisor error codes
//                                  to HTTP status: UNKNOWN_TASK → 400,
//                                  QUARANTINED → 409, RESPAWN_NOT_READY
//                                  → 503, RESPAWN_FAILED → 500.
//
// All three wrap requireSameOrigin so cross-origin browser tabs cannot
// reach them (CSRF defense — see internal/gui/csrf.go).
package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/binary_discovery"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/config"
)

// registerDaemonEnvRoutes wires the three Task 4.2 routes onto the
// server's mux. Called from server.go's route-registration init pass.
// Free-function shape matches the existing register*Routes naming so
// the registration order at New() reads consistently.
func registerDaemonEnvRoutes(s *Server) {
	s.mux.HandleFunc("/api/daemon/env", s.requireSameOrigin(s.daemonEnvHandler))
	s.mux.HandleFunc("/api/discovery/refresh", s.requireSameOrigin(s.discoveryRefreshHandler))
	s.mux.HandleFunc("/api/daemon/respawn", s.requireSameOrigin(s.daemonRespawnHandler))
}

// Validation tokens — kept narrow so operators get explicit feedback
// rather than silent truncation downstream.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// daemonEnvRequest is the POST body shape for /api/daemon/env.
type daemonEnvRequest struct {
	TaskName string            `json:"task_name"`
	Env      map[string]string `json:"env"`
}

// daemonEnvHandler accepts a {task_name, env} body and writes a
// source: operator row into the overlay file. Does NOT trigger respawn
// — the operator clicks Restart separately so they can review the
// effective env first.
func (s *Server) daemonEnvHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, fmt.Errorf("method %s not allowed", r.Method), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	defer r.Body.Close()

	var req daemonEnvRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeAPIError(w, fmt.Errorf("decode body: %w", err), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	taskName := daemon_env_overlay.NormalizeOverlayKey(strings.TrimSpace(req.TaskName))
	if taskName == "" {
		writeAPIError(w, fmt.Errorf("task_name is required"), http.StatusBadRequest, "INVALID_ARGS")
		return
	}

	// Validate env entries before any disk write so a malformed pair
	// surfaces as a clean 400 rather than a half-written overlay.
	for k, v := range req.Env {
		if !envKeyPattern.MatchString(k) {
			writeAPIError(w, fmt.Errorf("invalid env key %q: must match [A-Za-z_][A-Za-z0-9_]*", k),
				http.StatusBadRequest, "INVALID_KEY")
			return
		}
		if hasControlChar(v) {
			writeAPIError(w, fmt.Errorf("invalid env value for key %q: contains newline/NUL/control char", k),
				http.StatusBadRequest, "INVALID_VALUE")
			return
		}
	}

	// Known-task validation: a taskName not present in the current
	// supervisor-intent.json is a programmer/UI bug and we refuse the
	// write to avoid accumulating orphan overlay rows.
	intent, err := loadCurrentSupervisorIntent()
	if err != nil {
		writeAPIError(w, fmt.Errorf("read supervisor-intent.json: %w", err), http.StatusInternalServerError, "STATE_READ_FAILED")
		return
	}
	if !intentContainsTask(intent, taskName) {
		writeAPIError(w, fmt.Errorf("task_name %q not in current supervisor-intent.json", req.TaskName),
			http.StatusBadRequest, "UNKNOWN_TASK")
		return
	}

	overlayPath, err := resolveOverlayPath()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "STATE_DIR_FAILED")
		return
	}
	if err := daemon_env_overlay.WriteOverlay(overlayPath, func(ov *daemon_env_overlay.Overlay) error {
		row := ov.Daemons[taskName]
		if row.Env == nil {
			row.Env = make(map[string]string, len(req.Env))
		}
		for k, v := range req.Env {
			row.Env[k] = v
		}
		row.Source = "operator"
		row.ModifiedAt = time.Now().UTC().Format(time.RFC3339Nano)
		ov.Daemons[taskName] = row
		return nil
	}); err != nil {
		writeAPIError(w, fmt.Errorf("write overlay: %w", err), http.StatusInternalServerError, "OVERLAY_WRITE_FAILED")
		return
	}

	changedKeys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		changedKeys = append(changedKeys, k)
	}
	sort.Strings(changedKeys)
	_ = api.LogHubMcpEvent("info", "daemon-env-overlay-applied-via-gui", map[string]any{
		"task_name":    taskName,
		"changed_keys": changedKeys,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task_name":    taskName,
		"changed_keys": changedKeys,
	})
}

// discoveryRefreshRequest is the POST body shape for /api/discovery/refresh.
// Currently no fields are required; future versions may scope discovery
// to a specific server or daemon. The empty-body case re-runs full
// discovery across all installed manifests.
type discoveryRefreshRequest struct{}

func (s *Server) discoveryRefreshHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, fmt.Errorf("method %s not allowed", r.Method), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	defer r.Body.Close()

	// Decode-and-discard so a missing/empty body is accepted without
	// 400; future shape changes only need to populate fields here.
	_ = json.NewDecoder(io.LimitReader(r.Body, 4*1024)).Decode(&discoveryRefreshRequest{})

	manifests, err := s.loadAllManifestsForOverlay()
	if err != nil {
		writeAPIError(w, fmt.Errorf("load manifests: %w", err), http.StatusInternalServerError, "MANIFEST_LOAD_FAILED")
		return
	}
	overlayPath, err := resolveOverlayPath()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "STATE_DIR_FAILED")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := seedOverlayFromDiscoveryViaGUI(ctx, manifests, overlayPath); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "DISCOVERY_FAILED")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"manifest_count": len(manifests),
	})
}

// daemonRespawnRequest is the POST body shape for /api/daemon/respawn.
type daemonRespawnRequest struct {
	TaskName string `json:"task_name"`
	Force    bool   `json:"force"`
}

func (s *Server) daemonRespawnHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, fmt.Errorf("method %s not allowed", r.Method), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	defer r.Body.Close()

	var req daemonRespawnRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeAPIError(w, fmt.Errorf("decode body: %w", err), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	taskName := daemon_env_overlay.NormalizeOverlayKey(strings.TrimSpace(req.TaskName))
	if taskName == "" {
		writeAPIError(w, fmt.Errorf("task_name is required"), http.StatusBadRequest, "INVALID_ARGS")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, dialErr := api.DialSupervisorIPCRespawn(ctx, taskName, req.Force, 5000)
	if dialErr != nil {
		writeAPIError(w, dialErr, http.StatusInternalServerError, "IPC_FAILED")
		return
	}
	if result.Success {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_name": taskName,
			"force":     req.Force,
			"state":     "spawned",
		})
		return
	}

	// Map supervisor error codes onto HTTP status. Anything we don't
	// recognize is treated as 500 so the operator sees the raw error
	// rather than a confusing 4xx.
	status := http.StatusInternalServerError
	switch result.Code {
	case "UNKNOWN_TASK", "INVALID_ARGS":
		status = http.StatusBadRequest
	case "QUARANTINED":
		status = http.StatusConflict
	case "RESPAWN_NOT_READY", "SUPERVISOR_UNAVAILABLE":
		status = http.StatusServiceUnavailable
	case "RESPAWN_FAILED":
		status = http.StatusInternalServerError
	}
	writeAPIError(w, fmt.Errorf("%s", result.Message), status, result.Code)
}

// resolveOverlayPath returns the absolute path to daemon-env-overrides.yaml
// under the active state directory. Uses api.DaemonStateDir so the
// MCPHUB_STATE_DIR_OVERRIDE test seam still works from the GUI side.
func resolveOverlayPath() (string, error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(stateDir, "daemon-env-overrides.yaml"), nil
}

// loadCurrentSupervisorIntent reads supervisor-intent.json under the
// active state dir. Missing file → empty intent (legitimate: a fresh
// install before `mcphub install` runs).
func loadCurrentSupervisorIntent() (*api.SupervisorIntentFile, error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return nil, fmt.Errorf("resolve state dir: %w", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &api.SupervisorIntentFile{}, nil
		}
		return nil, err
	}
	return intent, nil
}

// intentContainsTask returns true iff intent.Daemons has a daemon whose
// (normalized) TaskName equals taskName.
func intentContainsTask(intent *api.SupervisorIntentFile, taskName string) bool {
	if intent == nil {
		return false
	}
	for _, d := range intent.Daemons {
		if daemon_env_overlay.NormalizeOverlayKey(d.TaskName) == taskName {
			return true
		}
	}
	return false
}

// loadAllManifestsForOverlay returns every parsed manifest visible to
// the running mcphub binary. Uses (*api.API).ManifestList + ManifestGet
// so the embed-first source-of-truth (compiled-in manifests) wins over
// stale on-disk copies. The discovery seeder needs the parsed structs
// to read each manifest's RequiredBinaries declarations.
func (s *Server) loadAllManifestsForOverlay() ([]*config.ServerManifest, error) {
	if s == nil || s.api == nil {
		return nil, fmt.Errorf("gui server has no api handle")
	}
	names, err := s.api.ManifestList()
	if err != nil {
		return nil, fmt.Errorf("list manifests: %w", err)
	}
	manifests := make([]*config.ServerManifest, 0, len(names))
	for _, name := range names {
		yaml, err := s.api.ManifestGet(name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("get manifest %q: %w", name, err)
		}
		m, err := config.ParseManifest(strings.NewReader(yaml))
		if err != nil {
			return nil, fmt.Errorf("parse manifest %q: %w", name, err)
		}
		manifests = append(manifests, m)
	}
	return manifests, nil
}

// seedOverlayFromDiscoveryViaGUI mirrors the install-time seeder logic
// (cli/install_overlay_seed.go) for the GUI's refresh path. It would be
// ideal to share one implementation, but the install-side helper lives
// in package cli and depending on cli from gui would create a cycle. The
// duplication is intentional + small.
func seedOverlayFromDiscoveryViaGUI(ctx context.Context, manifests []*config.ServerManifest, overlayPath string) error {
	binaryToServers := map[string][]string{}
	for _, m := range manifests {
		if m == nil {
			continue
		}
		for _, b := range m.RequiredBinaries {
			binaryToServers[b] = append(binaryToServers[b], m.Name)
		}
		for _, lang := range m.Languages {
			for _, b := range lang.RequiredBinaries {
				binaryToServers[b] = append(binaryToServers[b], m.Name)
			}
		}
	}
	if len(binaryToServers) == 0 {
		return nil
	}
	allBinaries := make([]string, 0, len(binaryToServers))
	for b := range binaryToServers {
		allBinaries = append(allBinaries, b)
	}
	sort.Strings(allBinaries)

	start := time.Now()
	found, _ := binary_discovery.Discover(ctx, allBinaries, binary_discovery.DefaultHints())
	_ = api.LogHubMcpEvent("info", "binary-discovery-ran", map[string]any{
		"scan_duration_ms": time.Since(start).Milliseconds(),
		"manifest_count":   len(manifests),
		"binary_count":     len(allBinaries),
		"trigger":          "gui-refresh",
	})

	type assignment struct {
		taskName, binDir, binary, server string
	}
	var assignments []assignment
	for _, m := range manifests {
		if m == nil {
			continue
		}
		dnames := manifestDaemonNamesForGUI(m)
		for _, b := range m.RequiredBinaries {
			abs := found[b]
			if abs == "" {
				continue
			}
			binDir := filepath.Dir(abs)
			for _, dn := range dnames {
				assignments = append(assignments, assignment{
					taskName: fmt.Sprintf(`\mcp-local-hub-%s-%s`, m.Name, dn),
					binDir:   binDir,
					binary:   b,
					server:   m.Name,
				})
			}
		}
	}
	if len(assignments) == 0 {
		return nil
	}
	return daemon_env_overlay.WriteOverlay(overlayPath, func(o *daemon_env_overlay.Overlay) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, a := range assignments {
			key := daemon_env_overlay.NormalizeOverlayKey(a.taskName)
			existing, present := o.Daemons[key]
			if present && existing.Source == "operator" {
				_ = api.LogHubMcpEvent("info", "daemon-env-overlay-skipped-operator-override", map[string]any{
					"task_name": key,
					"binary":    a.binary,
					"server":    a.server,
					"trigger":   "gui-refresh",
				})
				continue
			}
			env := map[string]string{}
			if existing.Env != nil {
				for k, v := range existing.Env {
					env[k] = v
				}
			}
			env["Path"] = a.binDir + string(os.PathListSeparator) + "${parent_path}"
			o.Daemons[key] = daemon_env_overlay.DaemonRow{
				Env:          env,
				Source:       "auto-discovery",
				DiscoveredAt: now,
			}
		}
		return nil
	})
}

func manifestDaemonNamesForGUI(m *config.ServerManifest) []string {
	if m == nil {
		return nil
	}
	if len(m.Daemons) == 0 {
		return []string{"default"}
	}
	out := make([]string, 0, len(m.Daemons))
	seen := map[string]struct{}{}
	for _, d := range m.Daemons {
		name := d.Name
		if name == "" {
			name = "default"
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// hasControlChar returns true if s contains a newline, NUL, or any
// non-tab control character. Used as a defense-in-depth gate against
// env values that would corrupt logs or process environment blocks.
func hasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}
