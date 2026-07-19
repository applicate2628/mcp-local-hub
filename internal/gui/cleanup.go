// internal/gui/cleanup.go
package gui

import (
	"net/http"
	"slices"

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
	s.mux.HandleFunc("/api/cleanup/aggressive", s.requireSameOrigin(s.cleanupAggressiveHandler))
}

// cleanupRequest is the JSON body for POST /api/cleanup/orphans.
//
//   - Apply=false → preview (DRY-RUN); lists candidates, kills nothing.
//                   This is the SAFE default — Go's zero value for
//                   omitted bool is false, so `{}` or any body that
//                   omits `apply` lands on the dry-run path.
//   - Apply=true  → execute; kills the matched processes. Requires
//                   explicit operator opt-in.
//   - MinAgeSec   → ignore processes younger than this. 0 → backend
//                   default (60s). Anti-foot-gun for legitimate
//                   in-flight installs.
//   - Server      → optional manifest-name filter. Empty → all manifests.
//
// Codex Cloud bot P2 on PR #131 / kosyak
// 2026-05-07-destructive-endpoint-with-unsafe-zero-value-default.md:
// the prior `DryRun bool` shape inverted safety polarity — a `{}` body
// triggered the kill path. Renamed to `Apply` so zero value = safe.
type cleanupRequest struct {
	Apply     bool   `json:"apply"`
	MinAgeSec int64  `json:"min_age_sec"`
	Server    string `json:"server"`
	// ScanClients (A6) opts the sweep INTO scanning every installed MCP
	// client's stdio config for additional kill-nomination patterns, so a
	// config-ABSENT dead-parent orphan of a client-direct MCP server (a
	// migrated-out / deleted / uninstalled entry whose child process lives
	// on, re-parented to explorer.exe/svchost) is reaped. Threads to
	// api.CleanupOpts.ScanClientConfigs, whose shipped config-absence gate +
	// identity-verified kill + 600s age floor still apply unchanged — a
	// currently-configured server is nominated then SPARED
	// (ReapVerdictSparedConfigReferenced) by the nomination⊆spare symmetry,
	// so this only widens nomination into the config-absent set.
	//
	// The automatic 5-min ticker sets this true (gui_cleanup_ticker.go); the
	// manual Dashboard "Clean orphans" button omits it, so its zero value
	// (false) keeps the pre-existing manifest-only nomination behavior.
	// Mutually exclusive with Server (see the handler pre-validate): client
	// stdio entries carry no server-name key, so the backend rejects the
	// combination with errOrphanOptsServerScanClientsConflict.
	ScanClients bool `json:"scan_clients"`
	// Expect, on an apply, binds the kill to the exact {pid, started_at} identities
	// the operator confirmed in the GUI modal, so a candidate whose reap verdict
	// drifted to eligible while the dialog was open — or a PID recycled onto a
	// different process before apply (its fresh started_at differs) — is not killed
	// unacknowledged (bot PR #520 P2; architect verdict — identity, not bare PID, is
	// the binding key the kill primitive re-verifies). Omitted / null → no binding
	// (kills every currently-eligible candidate, the pre-existing contract). Ignored
	// on a dry-run preview.
	Expect []api.ProcIdentity `json:"expect"`
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
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		http.Error(w, decodeBodyErrorText(err), decodeBodyStatusCode(err))
		return
	}
	if req.MinAgeSec < 0 {
		http.Error(w, "min_age_sec must be >= 0", http.StatusBadRequest)
		return
	}
	// Defensive pre-validate: scan_clients + server is a backend conflict —
	// client stdio entries carry no server-name key, so mixing the two would
	// expand the kill-pattern set with cmdlines unrelated to the requested
	// server. The backend CleanupOrphans returns
	// errOrphanOptsServerScanClientsConflict for this combination; catch it
	// at the boundary so the caller gets a clean 400 instead of a generic
	// 500. Checked BEFORE the manifest-name lookup so the conflict wins over
	// an "unknown server" error. The auto-ticker never sets server, so this
	// never trips for it.
	if req.ScanClients && req.Server != "" {
		http.Error(w, "scan_clients is incompatible with server (pick one mode)", http.StatusBadRequest)
		return
	}
	if req.Server != "" {
		names, err := api.NewAPI().ManifestList()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "failed to list manifests",
				"code":  "CLEANUP_MANIFEST_LIST_FAILED",
			})
			return
		}
		if !slices.Contains(names, req.Server) {
			http.Error(w, "unknown server", http.StatusBadRequest)
			return
		}
	}

	// Empty ManifestDir → embed-first path (mirrors the convention in
	// internal/gui/extract_manifest.go and migrate.go).
	// api.CleanupOpts still uses DryRun semantics — flip Apply at the
	// boundary so the wire shape is safe-by-default while the backend
	// helper keeps its existing contract.
	// Bind the kill to the confirmed identity set on APPLY only — a dry-run preview
	// must recompute + show the full current candidate set (bot PR #520 P2).
	var expect []api.ProcIdentity
	if req.Apply {
		expect = req.Expect
	}
	orphans, err := s.cleanup.CleanupOrphans(api.CleanupOpts{
		ManifestDir:       "",
		Server:            req.Server,
		DryRun:            !req.Apply,
		MinAgeSec:         req.MinAgeSec,
		ScanClientConfigs: req.ScanClients,
		Expect:            expect,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"code":  "CLEANUP_ORPHANS_FAILED",
		})
		return
	}

	// Normalize nil to empty slice so the JSON wire shape is always
	// `"orphans": []` not `"orphans": null`. Codex Cloud bot P2 on
	// PR #131 commit 1f59a65: `CleanupOrphans` legitimately returns
	// nil on no-candidates (and on non-Windows hosts), and forwarding
	// nil through json.Marshal emits `null`, which TypeScript clients
	// have to defensively check separately from empty-array `[]`.
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

// logWatcherRequest is the JSON body for POST /api/cleanup/log-watchers.
//
//   - Apply=false     → preview (DRY-RUN); lists candidates, kills
//                       nothing. SAFE default — Go's zero value for
//                       omitted bool is false.
//   - Apply=true      → kills matched processes (subject to IncludeLive).
//                       Requires explicit operator opt-in.
//   - IncludeLive     → also kill path-matched processes whose parent
//                       is still alive. Default false: those almost
//                       always represent CURRENT active agent sessions.
//   - PathRegex       → optional override for the path-shape match.
//   - OrphanGrepRegex → optional override for the orphan-grep token list.
//
// Codex Cloud bot P2 on PR #131 / kosyak
// 2026-05-07-destructive-endpoint-with-unsafe-zero-value-default.md:
// renamed from DryRun bool so the zero-value path is safe.
type logWatcherRequest struct {
	Apply           bool   `json:"apply"`
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
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		http.Error(w, decodeBodyErrorText(err), decodeBodyStatusCode(err))
		return
	}

	// api.LogWatcherCleanupOpts still uses DryRun semantics — flip
	// Apply at the boundary so the wire shape is safe-by-default.
	watchers, err := s.cleanup.CleanupLogWatchers(api.LogWatcherCleanupOpts{
		PathRegex:       req.PathRegex,
		OrphanGrepRegex: req.OrphanGrepRegex,
		IncludeLive:     req.IncludeLive,
		DryRun:          !req.Apply,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"code":  "CLEANUP_LOG_WATCHERS_FAILED",
		})
		return
	}

	// Same nil → [] normalization as cleanupOrphansHandler above so
	// the wire shape never emits `"watchers": null`.
	if watchers == nil {
		watchers = []api.LogWatcher{}
	}
	resp := logWatcherResponse{Watchers: watchers}
	if req.Apply {
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
