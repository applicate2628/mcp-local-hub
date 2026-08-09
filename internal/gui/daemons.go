// Package gui — daemon-lifecycle HTTP routes. Memo §4.
package gui

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

const (
	// manualRecoveryHint is surfaced in the 5xx response body when the
	// rollback path leaves the Task Scheduler trigger in a degraded or
	// failed state — i.e. the operator must intervene manually because
	// neither the new task nor the prior task is currently registered.
	manualRecoveryHint = "Run `mcphub workspace-weekly-refresh-restore` or restart mcphub to re-create the task."

	weeklyScheduleAppliedReleaseUnconfirmedHint  = "The weekly schedule was committed, but its lock release could not be confirmed. Restart the running mcp-local-hub process before making another schedule change."
	weeklyScheduleReleaseUnconfirmedRecoveryHint = "The weekly schedule transaction did not commit because its lock release could not be confirmed. Restart the running mcp-local-hub process before retrying."
)

func registerDaemonsRoutes(s *Server) {
	s.mux.HandleFunc("/api/daemons/weekly-refresh-membership",
		s.requireSameOrigin(s.weeklyRefreshMembershipHandler))
	s.mux.HandleFunc("/api/daemons/weekly-schedule",
		s.requireSameOrigin(s.weeklyScheduleHandler))
}

// writeRegistryError is the leak-safe sibling of writeJSON for the
// daemons.go registry/membership 500-class sites, whose backend errors
// wrap an *os.PathError embedding the absolute registry/lock path (which on
// corp-managed hosts reveals the operator's AD username — G16 P2). It is
// the {error, detail}-envelope counterpart of writeAPIErrorRedacted in
// scan.go: it log.Printf's the raw err + logCtx server-side, then emits a
// 500 carrying the stable error token plus a fixed opaque `detail` — never
// err.Error(). errorToken is the stable, code-keyed signal the frontend
// already switches on ("registry_path", "registry_load",
// "membership_failed"); only the leaky detail is redacted.
func writeRegistryError(w http.ResponseWriter, errorToken string, err error, logCtx string) {
	log.Printf("%s: %v", logCtx, err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error":  errorToken,
		"detail": "internal error",
	})
}

// membershipRowDTO is the on-the-wire shape of one row in the weekly-refresh
// membership snapshot returned by GET /api/daemons/weekly-refresh-membership
// (memo D6). It is intentionally a strict subset of WorkspaceEntry: the GUI
// only needs key + path + language + the boolean enrollment flag to render
// the membership table.
type membershipRowDTO struct {
	WorkspaceKey  string `json:"workspace_key"`
	WorkspacePath string `json:"workspace_path"`
	Language      string `json:"language"`
	WeeklyRefresh bool   `json:"weekly_refresh"`
}

// weeklyRefreshMembershipHandler is the method multiplexer for
// /api/daemons/weekly-refresh-membership:
//
//   - GET → list current membership rows in registry order (memo D6).
//   - PUT → idempotent partial update of (workspace_key, language) toggles
//     (memo D5).
//
// All other methods return 405 with an Allow header. The GET handler exists
// to feed the SectionDaemons WeeklyMembershipTable on mount; the PUT handler
// is op 3 of the multi-op save flow described in memo D4.
func (s *Server) weeklyRefreshMembershipHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.weeklyRefreshMembershipList(w, r)
	case http.MethodPut:
		s.weeklyRefreshMembershipPut(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// weeklyRefreshMembershipList serves the GET snapshot. Loads the registry
// fresh on each request — there is no in-memory cache. Empty registries
// yield {"rows": []} with status 200 (the GUI distinguishes loading vs
// empty by HTTP status, not row count).
func (s *Server) weeklyRefreshMembershipList(w http.ResponseWriter, _ *http.Request) {
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		// err can embed the absolute LOCALAPPDATA registry path; log
		// server-side + return a stable opaque detail (G16 P2).
		writeRegistryError(w, "registry_path", err, "/api/daemons/weekly-refresh-membership GET registry path")
		return
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		// reg.Load wraps an *os.PathError embedding the absolute registry
		// path; log server-side + return a stable opaque detail (G16 P2).
		writeRegistryError(w, "registry_load", err, "/api/daemons/weekly-refresh-membership GET registry load")
		return
	}
	// B.1: the weekly-refresh membership table is LSP-only; serena
	// (sentinel) rows must not appear in the GUI table because the
	// "Language" column would render "@serena" which has no operator
	// meaning. Workspace-level serena visibility lives in a different
	// route (B.2+).
	rows := make([]membershipRowDTO, 0, len(reg.Workspaces))
	for _, e := range reg.Workspaces {
		if e.Language == api.SerenaLanguageSentinel {
			continue
		}
		rows = append(rows, membershipRowDTO{
			WorkspaceKey:  e.WorkspaceKey,
			WorkspacePath: e.WorkspacePath,
			Language:      e.Language,
			WeeklyRefresh: e.WeeklyRefresh,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// weeklyRefreshMembershipPut applies the structured-array delta body to
// workspaces.yaml under registryMu via api.UpdateWeeklyRefreshMembership.
// Memo D5 contract: idempotent partial update; entries not in body unchanged.
func (s *Server) weeklyRefreshMembershipPut(w http.ResponseWriter, r *http.Request) {
	weeklyRefreshMembershipPutWithUpdate(w, r, api.UpdateWeeklyRefreshMembership)
}

// weeklyRefreshMembershipPutWithUpdate keeps the wire owner local while making
// the storage boundary injectable for deterministic response-projection tests.
func weeklyRefreshMembershipPutWithUpdate(w http.ResponseWriter, r *http.Request, update func(string, []api.MembershipDelta) (int, error)) {
	var body []api.MembershipDelta
	// Membership deltas can carry one entry per (workspace, client) across a
	// large registry, so they use the generous manifest-class cap rather than
	// the tight control cap a small key/value control body uses.
	if err := decodeJSONBodyLimited(w, r, &body, maxManifestBodyBytes); err != nil {
		writeJSON(w, decodeBodyStatusCode(err), map[string]string{
			"error":  decodeBodyErrorCode(err),
			"detail": decodeBodyErrorText(err),
		})
		return
	}
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		// err can embed the absolute LOCALAPPDATA registry path; log
		// server-side + return a stable opaque detail (G16 P2).
		writeRegistryError(w, "registry_path", err, "/api/daemons/weekly-refresh-membership PUT registry path")
		return
	}
	updated, err := update(regPath, body)
	if err != nil {
		// Validation errors (unknown pair) → 400; storage errors → 500.
		status := http.StatusBadRequest
		if errors.Is(err, api.ErrLockReleaseUnconfirmed) ||
			strings.HasPrefix(err.Error(), "save registry") ||
			strings.HasPrefix(err.Error(), "load registry") ||
			strings.HasPrefix(err.Error(), "acquire lock") {
			status = http.StatusInternalServerError
		}
		if status == http.StatusInternalServerError {
			// Storage errors (save/load registry, acquire lock) wrap an
			// *os.PathError embedding the absolute registry/lock path; log
			// server-side + return a stable opaque detail (G16 P2). The
			// 400 validation branch below keeps its client-facing
			// "unknown pair" detail — that is an intentional caller signal,
			// not a path leak.
			writeRegistryError(w, "membership_failed", err, "/api/daemons/weekly-refresh-membership PUT update membership")
			return
		}
		writeJSON(w, status, map[string]string{
			"error":  "membership_failed",
			"detail": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":  updated,
		"warnings": []string{},
	})
}

var applyWeeklyScheduleForRoute = func(spec *api.ScheduleSpec) (string, error) {
	return api.NewAPI().ApplyWeeklyRefreshSchedule(spec)
}

// weeklyScheduleHandler owns HTTP parse and response projection only. It
// delegates the combined settings/task update to ApplyWeeklyRefreshSchedule;
// runWeeklyRefreshTaskTransaction owns the scheduler lifecycle. The handler
// maps their existing result into the established response and selects a
// truthful recovery hint without changing the response schema.
func (s *Server) weeklyScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Schedule string `json:"schedule"`
	}
	if err := decodeJSONBodyLimited(w, r, &body, maxControlBodyBytes); err != nil {
		writeJSON(w, decodeBodyStatusCode(err), map[string]string{
			"error":   decodeBodyErrorCode(err),
			"detail":  decodeBodyErrorText(err),
			"example": "weekly Sun 03:00",
		})
		return
	}
	spec, err := api.ParseSchedule(body.Schedule)
	if err != nil {
		// Memo D8: parse-error 400 envelope carries ONLY
		// {error, detail, example}. No `updated`, no
		// `restore_status` — the transaction never crossed the
		// destructive boundary, so the rollback envelope does not
		// apply.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "parse_error",
			"detail":  err.Error(),
			"example": "weekly Sun 03:00",
		})
		return
	}

	// Normalize the input to canonical form (title-case day, zero-padded
	// HH:MM) before persisting. This ensures round-trips like
	// "  weekly mon 14:30  " are stored and returned as "weekly Mon 14:30".
	canonical := spec.Canonical()

	helperStatus, applyErr := applyWeeklyScheduleForRoute(spec)
	if applyErr == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"updated":        true,
			"schedule":       canonical,
			"restore_status": "n/a",
		})
		return
	}
	if api.IsAppliedLockReleaseUnconfirmed(applyErr) {
		log.Printf("/api/daemons/weekly-schedule committed with an unconfirmed lock release: %v", applyErr)
		writeJSON(w, http.StatusOK, map[string]any{
			"updated":         true,
			"schedule":        canonical,
			"restore_status":  helperStatus,
			"warning":         "lock_release_unconfirmed",
			"manual_recovery": weeklyScheduleAppliedReleaseUnconfirmedHint,
		})
		return
	}

	errorCode := "scheduler_swap_failed"
	if errors.Is(applyErr, api.ErrWeeklyRefreshSnapshotUnavailable) {
		errorCode = "snapshot_unavailable"
	} else if errors.Is(applyErr, api.ErrWeeklyScheduleSettingsWrite) {
		errorCode = "settings_write_failed"
	}
	log.Printf("/api/daemons/weekly-schedule apply failed: %v", applyErr)
	resp := map[string]any{
		"error":          errorCode,
		"detail":         "internal error",
		"updated":        false,
		"restore_status": helperStatus,
	}
	if errors.Is(applyErr, api.ErrLockReleaseUnconfirmed) {
		resp["manual_recovery"] = weeklyScheduleReleaseUnconfirmedRecoveryHint
	} else if helperStatus == "degraded" || helperStatus == "failed" {
		resp["manual_recovery"] = manualRecoveryHint
	}
	writeJSON(w, http.StatusInternalServerError, resp)
}
