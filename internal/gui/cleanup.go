// internal/gui/cleanup.go
package gui

import (
	"encoding/json"
	"net/http"

	"mcp-local-hub/internal/api"
)

// registerCleanupRoutes wires the cleanup endpoints behind the same-origin
// gate. Per design memo Q5=E and the existing secrets D5 escalation
// pattern (`internal/api/secrets.go:500`), the destructive guard is a
// boolean `confirm`-style flag (here `dry_run`) plus the existing CSRF
// + DNS-rebind + loopback-only chain. The frontend Maintenance modal
// is responsible for the typed-confirmation UX before flipping
// `dry_run` to false.
func registerCleanupRoutes(s *Server) {
	s.mux.HandleFunc("/api/cleanup/orphans", s.requireSameOrigin(s.cleanupOrphansHandler))
	s.mux.HandleFunc("/api/cleanup/log-watchers", s.requireSameOrigin(s.cleanupLogWatchersHandler))
}

// cleanupRequest is the JSON body for POST /api/cleanup/orphans.
//
//   - DryRun=true  → preview; lists candidates, kills nothing.
//   - DryRun=false → execute; kills the matched processes.
//   - MinAgeSec    → ignore processes younger than this. 0 → backend
//                    default (60s). Anti-foot-gun for legitimate
//                    in-flight installs.
//   - Server       → optional manifest-name filter. Empty → all manifests.
type cleanupRequest struct {
	DryRun    bool   `json:"dry_run"`
	MinAgeSec int64  `json:"min_age_sec"`
	Server    string `json:"server"`
}

// cleanupResponse is the JSON body returned for both dry-run and apply
// modes. Mirrors the CLI shape from `internal/cli/cleanup.go` so a
// future operator-CLI bridge can format either output identically.
type cleanupResponse struct {
	Orphans []api.OrphanProcess `json:"orphans"`
	Killed  int                 `json:"killed"`
	Skipped int                 `json:"skipped"`
}

// cleanupOrphansHandler handles POST /api/cleanup/orphans.
//
// Method-restricted to POST so a CSRF probe via `<img src=...>` cannot
// trigger a kill: the same-origin wrapper handles CSRF on POST, and a
// browser cannot issue a JSON-bodied POST cross-origin without a
// preflight that the wrapper also rejects.
//
// Errors map to status:
//   - 400 BadRequest      — JSON parse failure.
//   - 405 MethodNotAllowed — non-POST.
//   - 500 InternalServerError — backend error.
//   - 200 OK              — both dry-run and apply success paths.
func (s *Server) cleanupOrphansHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Codex Cloud bot P2 on PR #131 (commit 99938e7): the wrapped
	// API.CleanupOrphans is Windows-only and returns (nil, nil) on
	// POSIX, so an empty 200 indistinguishable from "no orphans
	// found" misleads operators. Surface the unsupported state
	// explicitly so the frontend can render "Not supported on this
	// OS yet" instead of a misleading empty list. Mirrors the
	// pattern at internal/gui/force_kill.go:54 for macOS lock
	// recovery.
	//
	// Codex Cloud bot P1 on PR #131 (commit 460e7ff) / kosyak
	// `2026-05-06-os-gate-bypassed-test-seam.md`: the platform check
	// must read from the s.cleanup interface seam, NOT runtime.GOOS
	// directly, so tests on non-Windows can inject a fake that
	// returns true and exercise the full handler (JSON validation,
	// auth, response shape). realCleanupAPI's impl checks runtime.GOOS;
	// fakeCleanupAPI's stub returns true for cross-platform tests.
	if !s.cleanup.CleanupOrphansSupported() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":  "not_supported_on_this_os",
			"detail": "Orphan MCP-server cleanup is currently Windows-only. POSIX support is on the roadmap — track via docs/superpowers/specs/2026-05-06-cleanup-buttons-design.md.",
		})
		return
	}

	var req cleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Empty ManifestDir → embed-first path (mirrors the convention in
	// internal/gui/extract_manifest.go and migrate.go).
	orphans, err := s.cleanup.CleanupOrphans(api.CleanupOpts{
		ManifestDir: "",
		Server:      req.Server,
		DryRun:      req.DryRun,
		MinAgeSec:   req.MinAgeSec,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"code":  "CLEANUP_ORPHANS_FAILED",
		})
		return
	}

	resp := cleanupResponse{Orphans: orphans}
	if !req.DryRun {
		for _, o := range orphans {
			if o.KillErr == "" {
				resp.Killed++
			} else {
				resp.Skipped++
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// logWatcherRequest is the JSON body for POST /api/cleanup/log-watchers.
//
//   - DryRun=true     → preview; lists candidates, kills nothing.
//   - DryRun=false    → kills matched processes (subject to IncludeLive).
//   - IncludeLive     → also kill path-matched processes whose parent
//                       is still alive. Default false: those almost
//                       always represent CURRENT active agent sessions.
//   - PathRegex       → optional override for the path-shape match.
//   - OrphanGrepRegex → optional override for the orphan-grep token list.
type logWatcherRequest struct {
	DryRun          bool   `json:"dry_run"`
	IncludeLive     bool   `json:"include_live"`
	PathRegex       string `json:"path_regex"`
	OrphanGrepRegex string `json:"orphan_grep_regex"`
}

// logWatcherResponse mirrors cleanupResponse — same shape so the GUI
// can use one rendering pipeline for both Maintenance buttons.
type logWatcherResponse struct {
	Watchers []api.LogWatcher `json:"watchers"`
	Killed   int              `json:"killed"`
	Skipped  int              `json:"skipped"`
}

// cleanupLogWatchersHandler handles POST /api/cleanup/log-watchers.
// Same method/auth/error contract as cleanupOrphansHandler, but the
// detection target is orphan tail/grep/bash watcher pipelines spawned
// by agent shell-snapshot launchers (Claude Code, codex CLI etc.) per
// the brief at d:/dev/orphaned-log-watchers-report.md.
func (s *Server) cleanupLogWatchersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req logWatcherRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	watchers, err := s.cleanup.CleanupLogWatchers(api.LogWatcherCleanupOpts{
		PathRegex:       req.PathRegex,
		OrphanGrepRegex: req.OrphanGrepRegex,
		IncludeLive:     req.IncludeLive,
		DryRun:          req.DryRun,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"code":  "CLEANUP_LOG_WATCHERS_FAILED",
		})
		return
	}

	resp := logWatcherResponse{Watchers: watchers}
	if !req.DryRun {
		for _, wch := range watchers {
			if wch.KillErr == "" {
				// Mirror PS1 script semantics: a process whose parent
				// is alive is SKIPPED unless IncludeLive is set, even
				// in apply mode. The empty KillErr in that case means
				// "we deliberately did not call kill", not "kill
				// succeeded" — the API.CleanupLogWatchers function
				// honors that distinction.
				if wch.ParentAlive && !req.IncludeLive {
					resp.Skipped++
				} else {
					resp.Killed++
				}
			} else {
				resp.Skipped++
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
