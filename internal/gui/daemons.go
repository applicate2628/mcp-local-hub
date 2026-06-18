// Package gui — daemon-lifecycle HTTP routes. Memo §4.
package gui

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// manualRecoveryHint is surfaced in the 5xx response body when the
// rollback path leaves the Task Scheduler trigger in a degraded or
// failed state — i.e. the operator must intervene manually because
// neither the new task nor the prior task is currently registered.
const manualRecoveryHint = "Run `mcphub workspace-weekly-refresh-restore` or restart mcphub to re-create the task."

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
	var body []api.MembershipDelta
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "bad_json",
			"detail": err.Error(),
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
	updated, err := api.UpdateWeeklyRefreshMembership(regPath, body)
	if err != nil {
		// Validation errors (unknown pair) → 400; storage errors → 500.
		status := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "save registry") ||
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

// weeklyScheduleHandler owns the full memo-D8 transactional update of
// daemons.weekly_schedule. Steps:
//
//  1. Method-check (PUT only).
//  2. Decode JSON body {schedule string}; bad JSON → 400 bad_json.
//  3. ParseSchedule(body.Schedule); reject → 400 parse_error.
//     IMPORTANT: parse_error 400 carries ONLY {error, detail, example} —
//     no `updated`, no `restore_status` field. The 5xx envelope is
//     reserved for transactional failures that already crossed the
//     destructive boundary.
//  4. Preflight ExportXML for WeeklyRefreshTaskName. ErrTaskNotFound is
//     fresh-install (priorXML=nil); any other error → 500
//     snapshot_unavailable, abort BEFORE Delete.
//  5. Snapshot prior settings YAML value, write new value. Settings
//     write failure → 500 settings_write_failed (no scheduler mutation
//     attempted yet).
//  6. Call SwapWeeklyTrigger (or test seam). Success → 200 with
//     restore_status="n/a".
//  7. On swap failure: roll the YAML back (only — the helper already
//     re-imported priorXML where it could). Combine the helper's
//     restoreStatus with the YAML rollback result via the truth table:
//
//     helper restoreStatus | YAML rollback | response.restore_status
//     "n/a"                | ok            | "n/a"
//     "ok"                 | ok            | "ok"
//     "degraded"           | ok            | "degraded"
//     any                  | failed        | "failed"
//
//     manual_recovery is surfaced ONLY when the final restore_status is
//     "degraded" or "failed".
func (s *Server) weeklyScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Schedule string `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "bad_json",
			"detail":  err.Error(),
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

	// Preflight: capture the prior task XML BEFORE any destructive
	// step. ErrTaskNotFound is the fresh-install case (priorXML stays
	// nil; SwapWeeklyTrigger handles it without rollback). Any other
	// error must abort the transaction immediately so we don't Delete
	// without a snapshot.
	exportFn := s.exportXMLForRoute
	if exportFn == nil {
		exportFn = realExportXML
	}
	priorXML, exportErr := exportFn(api.WeeklyRefreshTaskName)
	if exportErr != nil && !errors.Is(exportErr, scheduler.ErrTaskNotFound) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":          "snapshot_unavailable",
			"detail":         exportErr.Error(),
			"updated":        false,
			"restore_status": "n/a",
		})
		return
	}

	// Normalize the input to canonical form (title-case day, zero-padded
	// HH:MM) before persisting. This ensures round-trips like
	// "  weekly mon 14:30  " are stored and returned as "weekly Mon 14:30".
	canonical := spec.Canonical()

	// Settings YAML phase: snapshot the prior value (best-effort —
	// even on read error we proceed with empty string as the rollback
	// target, which is acceptable because SettingsSet validates and
	// rejects invalid values uniformly), then write the new value.
	priorScheduleValue, _ := api.NewAPI().SettingsGet("daemons.weekly_schedule")
	if err := api.NewAPI().SettingsSet("daemons.weekly_schedule", canonical); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":          "settings_write_failed",
			"detail":         err.Error(),
			"updated":        false,
			"restore_status": "n/a",
		})
		return
	}

	// Scheduler phase: hand the parsed spec + prior XML to the swap
	// helper. The helper owns Delete → Create → optional ImportXML
	// rollback for the scheduler XML side; on failure it returns a
	// restoreStatus describing the scheduler outcome only.
	swapFn := s.swapForRoute
	if swapFn == nil {
		swapFn = api.SwapWeeklyTrigger
	}
	helperStatus, swapErr := swapFn(spec, priorXML)
	if swapErr == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"updated":        true,
			"schedule":       canonical,
			"restore_status": "n/a",
		})
		return
	}

	// Swap failed. Helper already attempted (or skipped) its scheduler
	// rollback; we now own the YAML rollback and the truth-table
	// combination.
	settingsRollbackFailed := false
	if rerr := api.NewAPI().SettingsSet("daemons.weekly_schedule", priorScheduleValue); rerr != nil {
		settingsRollbackFailed = true
	}
	finalStatus := helperStatus
	if settingsRollbackFailed {
		finalStatus = "failed"
	}
	resp := map[string]any{
		"error":          "scheduler_swap_failed",
		"detail":         swapErr.Error(),
		"updated":        false,
		"restore_status": finalStatus,
	}
	if finalStatus == "degraded" || finalStatus == "failed" {
		resp["manual_recovery"] = manualRecoveryHint
	}
	writeJSON(w, http.StatusInternalServerError, resp)
}

// realExportXML is the production ExportXML adapter. It loads a real
// scheduler via scheduler.New() and forwards ExportXML. Tests inject a
// closure into s.exportXMLForRoute to bypass this and avoid touching
// Task Scheduler.
func realExportXML(taskName string) ([]byte, error) {
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	return sch.ExportXML(taskName)
}
