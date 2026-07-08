// internal/gui/cleanup_aggressive.go
package gui

import (
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
// design memo at cleanup.go:12-18) AND the CLI's recompute-token protocol
// (bot #373 R2 Finding 1): the same-origin/ConfirmModal guard prevents
// CSRF and forces an explicit operator click, but it does NOT pin the
// candidate set across the operator-deliberation window. A client that
// spawns MORE MCP children between Preview and Confirm would otherwise
// get those new processes killed unacknowledged. Token is the CLI's
// recompute-and-compare contract ported here: the dry-run returns a
// token bound to the previewed candidate snapshot, and apply re-resolves
// the CURRENT candidate set and refuses (409) if the token no longer
// matches.
type aggressiveCleanupRequest struct {
	Apply          bool     `json:"apply"`
	Client         string   `json:"client"`
	RootPID        int      `json:"root_pid"`
	MinAgeSec      int64    `json:"min_age_sec"`
	IncludeClasses []string `json:"include_classes"`
	// Token is REQUIRED on apply (apply=true): the confirm token from the
	// prior dry-run's response, bound to the previewed candidate set via
	// api.AggressiveConfirmToken. Ignored on dry-run (apply=false). An
	// empty token on apply is a 400; a stale token (candidate set changed
	// since the preview) is a 409 carrying the fresh candidates + new
	// token so the GUI can re-render and require a fresh Preview.
	Token string `json:"token"`
}

// aggressiveCleanupResponse is the JSON body for POST
// /api/cleanup/aggressive. It mirrors cleanupResponse (orphans + killed +
// skipped) and ADDS a Token field carrying the confirm token bound to the
// returned candidate set. A dedicated struct keeps the orphan handler's
// cleanupResponse free of the aggressive-only token field (bot #373 R2
// Finding 1 — don't pollute the shared shape).
//
//   - On dry-run (apply=false): Token is the preview token over the
//     returned candidates; the GUI captures it for the subsequent apply.
//   - On a token-mismatch 409: Orphans is the FRESH candidate set and
//     Token is the NEW token over it, so the GUI can re-render the drifted
//     set and require a fresh Preview before retrying.
//   - On apply success (apply=true, token matched): Token is omitted
//     (the kill happened; there is no further token to honor).
type aggressiveCleanupResponse struct {
	Orphans []api.OrphanProcess `json:"orphans"`
	Killed  int                 `json:"killed"`
	Skipped int                 `json:"skipped"`
	Token   string              `json:"token,omitempty"`
	// Code is set ONLY on the 409 token-mismatch body
	// (CLEANUP_AGGRESSIVE_TOKEN_MISMATCH) so the GUI can distinguish a
	// drifted-candidate-set refusal from any other non-200 response and
	// reset to require a fresh Preview. Omitted on the 200 paths.
	Code string `json:"code,omitempty"`
}

// cleanupAggressiveTokenMismatchCode is the stable machine-readable code
// in the 409 token-mismatch body. The GUI keys its "re-Preview" reset on
// this exact string (bot #373 R2 Finding 1).
const cleanupAggressiveTokenMismatchCode = "CLEANUP_AGGRESSIVE_TOKEN_MISMATCH"


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
//     exported scope sentinels; empty token
//     on apply.
//   - 409 Conflict           — apply token mismatch (candidate set changed
//     since the preview); body carries the fresh
//     candidates + new token (bot #373 R2 F1).
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
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		http.Error(w, decodeBodyErrorText(err), decodeBodyStatusCode(err))
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

	baseOpts := api.CleanupOpts{
		Aggressive:     true,
		Client:         req.Client,
		RootPID:        req.RootPID,
		MinAgeSec:      req.MinAgeSec,
		IncludeClasses: req.IncludeClasses,
	}

	// runAggressive maps the AggressiveCleanup error to its HTTP status the
	// same way for every call (dry-run preview, apply recompute, real kill).
	// Returns (orphans, true) on success; on error it has already written
	// the response and returns (_, false).
	// expect is nil for a recompute (dry-run preview / token recompute) and the
	// token-validated {PID, StartedAt} identity set for the real kill, so the kill
	// touches ONLY the validated processes and a recycled PID (different StartedAt)
	// is excluded (bot #373 R5; identity-keyed since bug 2026-07-08).
	runAggressive := func(dryRun bool, expect []api.ProcIdentity) ([]api.OrphanProcess, bool) {
		opts := baseOpts
		opts.DryRun = dryRun
		opts.Expect = expect
		orphans, err := s.cleanup.AggressiveCleanup(opts)
		if err != nil {
			// A malformed scope or unknown client is operator error → 400;
			// anything else (process-snapshot failure, etc.) → 500.
			if errors.Is(err, api.ErrAggressiveScopeRequired) || errors.Is(err, api.ErrAggressiveUnknownClient) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return nil, false
			}
			// Redact: the only non-sentinel error here is a process-snapshot
			// failure (wmic/CIM) which can carry a path; log it server-side,
			// return a generic body (security review P3-3, matches scan.go).
			writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "CLEANUP_AGGRESSIVE_FAILED", "cleanup-aggressive")
			return nil, false
		}
		// Normalize nil → [] so the wire shape is always `"orphans": []`,
		// never `null` (same posture as cleanupOrphansHandler).
		if orphans == nil {
			orphans = []api.OrphanProcess{}
		}
		return orphans, true
	}

	if !req.Apply {
		// Dry-run / preview: resolve the candidate set and return it with a
		// confirm token bound to it. The GUI captures the token and replays
		// it on apply (bot #373 R2 Finding 1).
		orphans, ok := runAggressive(true, nil)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, aggressiveCleanupResponse{
			Orphans: orphans,
			Token:   api.AggressiveConfirmToken(orphans),
		})
		return
	}

	// Apply path. The token is REQUIRED and must still match the CURRENT
	// candidate set (recompute-and-compare, mirroring the CLI kill path).
	// An empty token is an operator/contract error → 400.
	if strings.TrimSpace(req.Token) == "" {
		http.Error(w, "token is required on apply", http.StatusBadRequest)
		return
	}
	// Recompute the candidate set NOW (dry-run) and compare its token to
	// the one the operator acknowledged at preview. A mismatch means the
	// set drifted between Preview and Confirm — refuse with 409 + the fresh
	// candidates + new token so the GUI can re-render and force a fresh
	// Preview. (The residual microsecond validate→kill window is accepted;
	// the user-deliberation window is the one this closes.)
	fresh, ok := runAggressive(true, nil)
	if !ok {
		return
	}
	freshToken := api.AggressiveConfirmToken(fresh)
	if freshToken != strings.TrimSpace(req.Token) {
		writeJSON(w, http.StatusConflict, aggressiveCleanupResponse{
			Orphans: fresh,
			Token:   freshToken,
			Code:    cleanupAggressiveTokenMismatchCode,
		})
		return
	}

	// Token matched — execute the real kill, BOUND to the validated {PID, StartedAt}
	// identity set so a process spawned since the validation snapshot, or a PID
	// recycled onto a different process in the validate→kill window, cannot be killed
	// unacknowledged (bot #373 R5; identity-keyed since bug 2026-07-08).
	killed, ok := runAggressive(false, api.IdentitiesOf(fresh))
	if !ok {
		return
	}
	resp := aggressiveCleanupResponse{Orphans: killed}
	for _, o := range killed {
		if o.KillErr == "" {
			resp.Killed++
		} else {
			resp.Skipped++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
