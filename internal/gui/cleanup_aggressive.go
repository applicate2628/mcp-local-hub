// internal/gui/cleanup_aggressive.go
package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// aggressiveCleanupRequest is the JSON body for
// POST /api/cleanup/aggressive — the GUI surface over the
// operator-confirmed override that kills the live-rooted MCP-stdio
// processes the default safe sweep (POST /api/cleanup/orphans) correctly
// refuses to touch (the per-subagent stdio fan-out under a live client).
//
//   - Apply=false → preview (DRY-RUN); resolves + lists the candidates,
//     kills nothing. SAFE default — Go's zero value for an
//     omitted bool is false, so `{}` lands on dry-run
//     (same safe-polarity contract as cleanupRequest).
//   - Apply=true  → execute; kills the matched processes. Requires
//     explicit operator opt-in (the frontend gates this
//     behind a ConfirmModal that surfaces the danger-class
//     opt-ins).
//   - Client      → scope to descendants of this recognized client
//     launcher basename. Mutually exclusive with RootPID.
//   - RootPID     → scope to descendants of this process id. Mutually
//     exclusive with Client.
//   - MinAgeSec   → ignore processes younger than this. 0 → backend
//     default (60s). Anti-foot-gun for just-spawned
//     in-flight children mid-handshake.
//   - IncludeClasses → dangerous process classes the operator explicitly
//     opted back into the kill set (cmd / conhost / pwsh /
//     powershell / chrome). Empty → every dangerous class
//     stays excluded.
//
// The GUI uses the same-origin + CSRF + ConfirmModal guard (per the
// design memo at cleanup.go:12-18), NOT the CLI's recompute-token
// protocol — so there is no confirm-token field on the wire.
type aggressiveCleanupRequest struct {
	Apply          bool     `json:"apply"`
	Client         string   `json:"client"`
	RootPID        int      `json:"root_pid"`
	MinAgeSec      int64    `json:"min_age_sec"`
	IncludeClasses []string `json:"include_classes"`
}

// cleanupAggressiveHandler handles POST /api/cleanup/aggressive. It
// mirrors cleanupOrphansHandler's method/OS-gate/error contract:
//
//   - 405 MethodNotAllowed   — non-POST (a destructive op must not be
//     triggerable by a bare <img> / navigation).
//   - 501 NotImplemented     — OS gate (Windows-only), via the
//     s.cleanup seam (NOT runtime.GOOS) so the
//     fake can exercise the handler on POSIX.
//   - 400 BadRequest         — JSON parse failure; negative min_age_sec;
//     malformed scope (neither / both of
//     client + root_pid) mapped from the
//     exported scope sentinels.
//   - 500 InternalServerError — any other backend error.
//   - 200 OK                 — both dry-run and apply success paths.
func (s *Server) cleanupAggressiveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// OS gate through the seam (same posture as cleanupOrphansHandler).
	if !s.cleanup.CleanupAggressiveSupported() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":  "not_supported_on_this_os",
			"detail": "Aggressive MCP-stdio cleanup is currently Windows-only. POSIX support is on the roadmap — track via docs/superpowers/specs/2026-05-06-cleanup-buttons-design.md.",
		})
		return
	}

	var req aggressiveCleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.MinAgeSec < 0 {
		http.Error(w, "min_age_sec must be >= 0", http.StatusBadRequest)
		return
	}

	// Pre-validate the scope here too so a malformed request is a clean
	// 400 before the seam runs a process snapshot. AggressiveCleanup
	// re-validates and returns the same sentinels (single owner of the
	// rule); the errors.Is mapping below keeps the 400 if the backend
	// is the one that catches it.
	hasClient := strings.TrimSpace(req.Client) != ""
	hasRoot := req.RootPID != 0
	if hasClient == hasRoot {
		http.Error(w, api.ErrAggressiveScopeRequired.Error(), http.StatusBadRequest)
		return
	}

	// DryRun=!Apply at the boundary so the wire shape is safe-by-default
	// while the backend helper keeps its existing DryRun contract.
	orphans, err := s.cleanup.AggressiveCleanup(api.CleanupOpts{
		Aggressive:     true,
		Client:         req.Client,
		RootPID:        req.RootPID,
		MinAgeSec:      req.MinAgeSec,
		IncludeClasses: req.IncludeClasses,
		DryRun:         !req.Apply,
	})
	if err != nil {
		// A malformed scope or unknown client is operator error → 400;
		// anything else (process-snapshot failure, etc.) → 500.
		if errors.Is(err, api.ErrAggressiveScopeRequired) || errors.Is(err, api.ErrAggressiveUnknownClient) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"code":  "CLEANUP_AGGRESSIVE_FAILED",
		})
		return
	}

	// Normalize nil → [] so the wire shape is always `"orphans": []`,
	// never `null` (same posture as cleanupOrphansHandler).
	if orphans == nil {
		orphans = []api.OrphanProcess{}
	}
	resp := cleanupResponse{Orphans: orphans}
	if req.Apply {
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
