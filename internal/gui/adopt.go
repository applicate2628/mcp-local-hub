package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

type adoptRequest struct {
	ActionTimeout              *string              `json:"action_timeout,omitempty"`
	Entry                      string               `json:"entry"`
	Client                     string               `json:"client"`
	Clients                    []string             `json:"clients"`
	Name                       string               `json:"name"`
	Port                       int                  `json:"port"`
	SymlinkConsent             []adoptSymlinkTarget `json:"symlink_consent"`
	ProjectSourceRoot          string               `json:"project_source_root,omitempty"`
	ProjectSourcePreimageToken string               `json:"project_source_preimage_token,omitempty"`
}

type adoptSymlinkTarget struct {
	Client       string `json:"client"`
	ResolvedPath string `json:"resolved_path"`

	originalPath string
	pinnedPath   string
}

type adoptPlanResponse struct {
	*api.AdoptPlan
	SymlinkTargets             []adoptSymlinkTarget `json:"symlink_targets"`
	ProjectSourcePreimageToken string               `json:"project_source_preimage_token,omitempty"`
}

type adoptExecuteResponse struct {
	ProjectService *api.ProjectServicePlanInfo `json:"project_service,omitempty"`
	Name           string                      `json:"name"`
	Port           int                         `json:"port"`
	AdoptClients   []string                    `json:"adopt_clients"`
	SymlinkTargets []adoptSymlinkTarget        `json:"symlink_targets"`
}

func registerAdoptRoutes(s *Server) {
	s.mux.HandleFunc("/api/adopt/plan", s.requireSameOrigin(s.adoptPlanHandler))
	s.mux.HandleFunc("/api/adopt", s.requireSameOrigin(s.adoptHandler))
	s.mux.HandleFunc("/api/adopt/project-service/plan", s.requireSameOrigin(s.projectServicePlanHandler))
	s.mux.HandleFunc("/api/adopt/project-service/execute", s.requireSameOrigin(s.projectServiceExecuteHandler))
}

type projectServiceAdoptRequest struct {
	adoptRequest
	ProjectService                          api.ProjectServiceKind   `json:"project_service"`
	Intent                                  api.ProjectServiceIntent `json:"intent"`
	MCPProtocolCompatibilityProfile         string                   `json:"mcp_protocol_compatibility_profile,omitempty"`
	MCPProtocolCompatibilityProfileExplicit bool                     `json:"mcp_protocol_compatibility_profile_explicit,omitempty"`
}

func (s *Server) projectServicePlanHandler(w http.ResponseWriter, r *http.Request) {
	s.projectServiceAdoptHandler(w, r, false)
}
func (s *Server) projectServiceExecuteHandler(w http.ResponseWriter, r *http.Request) {
	s.projectServiceAdoptHandler(w, r, true)
}

// Authorization: the same-origin wrapper owns both explicit control routes.
// Execute freshly receives the source/plan on the original Server API, then
// applies only that operation's private plan under its original finite budget.
func (s *Server) projectServiceAdoptHandler(w http.ResponseWriter, r *http.Request, execute bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req projectServiceAdoptRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	if req.Name != "" || len(req.Clients) != 0 {
		writeAdoptPlanError(w, errors.New("PROJECT_SERVICE_LEGACY_TARGET_SELECTION_FORBIDDEN"))
		return
	}
	if s.api == nil {
		writeAdoptExecuteError(w, errors.New("adopt: missing GUI API lifetime owner"), http.StatusInternalServerError, "ADOPT_FAILED")
		return
	}
	ctx, cancel, err := api.ResolveCodexSharedStdioActionContext(r.Context(), req.ActionTimeout)
	if err != nil {
		if !writeH6ActionBudgetError(w, err) {
			writeAdoptPlanError(w, err)
		}
		return
	}
	defer cancel()
	plan, err := s.api.BuildProjectServicePlan(ctx, api.ProjectServiceOpts{
		Service: req.ProjectService, Intent: req.Intent, EntryName: req.Entry, Client: req.Client, ProjectSourceRoot: req.ProjectSourceRoot, Port: req.Port,
		MCPProtocolCompatibilityProfile: req.MCPProtocolCompatibilityProfile, MCPProtocolCompatibilityProfileExplicit: req.MCPProtocolCompatibilityProfileExplicit,
	})
	if err != nil {
		writeAdoptPlanError(w, err)
		return
	}
	if !execute {
		writeJSON(w, http.StatusOK, adoptPlanResponse{AdoptPlan: plan, SymlinkTargets: []adoptSymlinkTarget{}, ProjectSourcePreimageToken: plan.ProjectSourcePreimageToken()})
		return
	}
	if err := plan.ValidateProjectSourcePreimageToken(req.ProjectSourcePreimageToken); err != nil {
		writeGUIProjectAdoptTokenError(w, err)
		return
	}
	// A stopped definition has no client-config write and requires no symlink
	// consent. Other intents cannot consume this metadata-only execution path.
	var narration bytes.Buffer
	err = s.api.ExecuteAdoptWithOpts(plan, &narration, api.ExecuteAdoptOpts{Context: ctx, LeaseOwner: s.adoptLeaseOwner, ReceivingVerifier: s.adoptReceivingVerifier})
	if err != nil {
		facts := projectServiceExecutionFacts(err)
		if errors.Is(err, clients.ErrCodexProjectSourceChanged) {
			writeAdoptExecuteError(w, err, http.StatusConflict, "CODEX_PROJECT_SOURCE_CHANGED", facts)
			return
		}
		status, code := adoptExecuteErrorStatus(err)
		writeAdoptExecuteError(w, err, status, code, facts)
		return
	}
	s.events.PublishOperatorAction("project-service", api.CurrentOSUser(), map[string]any{"name": plan.ManifestName, "port": plan.Port, "service": req.ProjectService, "intent": req.Intent, "state": plan.ProjectService.State})
	writeJSON(w, http.StatusCreated, adoptExecuteResponse{Name: plan.ManifestName, Port: plan.Port, AdoptClients: plan.AdoptClients, SymlinkTargets: []adoptSymlinkTarget{}, ProjectService: plan.ProjectService})
}

func (s *Server) adoptPlanHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req adoptRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	actionCtx := r.Context()
	if req.ActionTimeout != nil {
		resolved, cancel, err := api.ResolveCodexSharedStdioActionContext(actionCtx, req.ActionTimeout)
		if err != nil {
			if !writeH6ActionBudgetError(w, err) {
				writeAdoptPlanError(w, err)
			}
			return
		}
		defer cancel()
		actionCtx = resolved
	}
	plan, targets, err := s.buildGUIAdoptPlan(actionCtx, &req)
	if err != nil {
		writeAdoptPlanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adoptPlanResponse{
		AdoptPlan:                  plan,
		SymlinkTargets:             targets,
		ProjectSourcePreimageToken: plan.ProjectSourcePreimageToken(),
	})
}

func (s *Server) adoptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req adoptRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	actionCtx := r.Context()
	if req.ActionTimeout != nil {
		resolved, cancel, err := api.ResolveCodexSharedStdioActionContext(actionCtx, req.ActionTimeout)
		if err != nil {
			if !writeH6ActionBudgetError(w, err) {
				writeAdoptPlanError(w, err)
			}
			return
		}
		defer cancel()
		actionCtx = resolved
	}
	plan, err := s.buildGUIAdoptAPIPlan(actionCtx, &req)
	if err != nil {
		writeAdoptPlanError(w, err)
		return
	}
	if err := plan.ValidateProjectSourcePreimageToken(req.ProjectSourcePreimageToken); err != nil {
		writeGUIProjectAdoptTokenError(w, err)
		return
	}
	targets, err := adoptSymlinkTargetsForPlan(&req, plan)
	if err != nil {
		writeAdoptPlanError(w, err)
		return
	}
	missingConsent := adoptSymlinkTargetsMissingConsent(targets, req.SymlinkConsent)
	if len(missingConsent) > 0 {
		writeAPIError(w, fmt.Errorf("symlink consent required for %s", formatAdoptSymlinkTargets(missingConsent)), http.StatusConflict, "SYMLINK_CONSENT_REQUIRED")
		return
	}

	var narration bytes.Buffer
	if s.api == nil {
		writeAdoptExecuteError(w, fmt.Errorf("adopt: missing GUI API lifetime owner"), http.StatusInternalServerError, "ADOPT_FAILED")
		return
	}
	err = s.api.ExecuteAdoptWithOpts(plan, &narration, api.ExecuteAdoptOpts{
		Context:           actionCtx,
		SymlinkConsents:   adoptResolvedSymlinkConsents(targets),
		LeaseOwner:        s.adoptLeaseOwner,
		ReceivingVerifier: s.adoptReceivingVerifier,
	})
	if err != nil {
		if out := strings.TrimSpace(narration.String()); out != "" {
			log.Printf("/api/adopt execution output before failure:\n%s", out)
		}
		status, code := adoptExecuteErrorStatus(err)
		writeAdoptExecuteError(w, err, status, code)
		return
	}
	// gui-events.log audit row: adopt is a committed operator mutation (it
	// creates a manifest and rewrites client configs), so it must publish an
	// operator action like the sibling migrate/demigrate routes do (bot PR
	// #516 P3). Emitted only here, AFTER ExecuteAdopt committed. Identifiers
	// only (manifest name, port, repointed client names — non-sensitive);
	// routed secret KEY names are deliberately NOT included.
	s.events.PublishOperatorAction("adopt", api.CurrentOSUser(), map[string]any{
		"name":    plan.ManifestName,
		"port":    plan.Port,
		"clients": plan.AdoptClients,
	})
	writeJSON(w, http.StatusCreated, adoptExecuteResponse{
		Name:           plan.ManifestName,
		Port:           plan.Port,
		AdoptClients:   plan.AdoptClients,
		SymlinkTargets: targets,
	})
}

func (s *Server) buildGUIAdoptPlan(ctx context.Context, req *adoptRequest) (*api.AdoptPlan, []adoptSymlinkTarget, error) {
	plan, err := s.buildGUIAdoptAPIPlan(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	targets, err := adoptSymlinkTargetsForPlan(req, plan)
	if err != nil {
		return nil, nil, err
	}
	return plan, targets, nil
}

func (s *Server) buildGUIAdoptAPIPlan(ctx context.Context, req *adoptRequest) (*api.AdoptPlan, error) {
	if s.api == nil {
		return nil, errors.New("adopt: missing GUI API lifetime owner")
	}
	plan, err := s.api.BuildAdoptPlan(api.AdoptOpts{
		Context:                ctx,
		EntryName:              req.Entry,
		Client:                 req.Client,
		ManifestName:           req.Name,
		Port:                   req.Port,
		Clients:                req.Clients,
		CodexProjectSourceRoot: req.ProjectSourceRoot,
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func adoptSymlinkTargetsForPlan(req *adoptRequest, plan *api.AdoptPlan) ([]adoptSymlinkTarget, error) {
	clientNames := plan.AdoptClients
	if req.ProjectSourceRoot != "" {
		clientNames = make([]string, 0, len(plan.AdoptClients))
		for _, clientName := range plan.AdoptClients {
			if clientName != "codex-cli" {
				clientNames = append(clientNames, clientName)
			}
		}
	}
	return adoptSymlinkTargets(clientNames)
}

func adoptSymlinkTargets(clientNames []string) ([]adoptSymlinkTarget, error) {
	targets := make([]adoptSymlinkTarget, 0)
	for _, client := range clientNames {
		configPath, err := clients.ConfigPathForName(client)
		if err != nil {
			return nil, fmt.Errorf("resolve client config path for %q: %w", client, err)
		}
		resolved, pinned, isSymlink := api.ResolveClientConfigSymlink(configPath)
		if !isSymlink {
			continue
		}
		targets = append(targets, adoptSymlinkTarget{
			Client:       client,
			ResolvedPath: resolved,
			originalPath: filepath.Clean(configPath),
			pinnedPath:   filepath.Clean(pinned),
		})
	}
	return targets, nil
}

// writeH6ActionBudgetError projects only the API-owned input sentinels.
// Cancellation after admission remains in the existing operation-failure channel.
func writeH6ActionBudgetError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, api.ErrCodexSharedStdioActionBudgetRequired):
		writeAPIError(w, api.ErrCodexSharedStdioActionBudgetRequired, http.StatusBadRequest, "H6_ACTION_BUDGET_REQUIRED")
	case errors.Is(err, api.ErrCodexSharedStdioActionBudgetInvalid):
		writeAPIError(w, api.ErrCodexSharedStdioActionBudgetInvalid, http.StatusBadRequest, "H6_ACTION_BUDGET_INVALID")
	default:
		return false
	}
	return true
}

func writeAdoptPlanError(w http.ResponseWriter, err error) {
	if writeH6ActionBudgetError(w, err) {
		return
	}
	status, code := adoptPlanErrorStatus(err)
	// Fail closed: forward the error text ONLY when it is a recognized,
	// path-free adopt validation / name-conflict message the operator can act
	// on (unknown/unsupported client, disabled entry, invalid --name, name
	// collision, ...); redact everything else. A default-redact posture means a
	// path can never evade to the wire and an unrecognized or wrapped-OS error
	// degrades to a safe generic body instead of leaking (fable PR #516
	// P3-A/P3-B: the earlier path-only heuristic left both a quoted/rooted-path
	// evasion and the "... already exists: <path>" NAME_CONFLICT lane
	// un-redacted; the blanket-redact before THAT lost every actionable message
	// — this allowlist keeps the actionable ones while failing closed).
	if adoptPlanErrorIsActionable(err.Error()) {
		writeAPIError(w, err, status, code)
		return
	}
	writeAPIErrorRedacted(w, err, status, code, "/api/adopt/plan")
}

// adoptPlanErrorIsActionable reports whether a BuildAdoptPlan error is a known
// operator-actionable validation / name-conflict message carrying no filesystem
// path. It is deliberately an ALLOWLIST with a default-redact fallback: any
// detected path forces redaction, and an unrecognized or newly-wrapped error
// shape is redacted (safe degradation) rather than forwarded (potential leak).
func adoptPlanErrorIsActionable(msg string) bool {
	if adoptErrorMessageHasPath(msg) {
		return false
	}
	return adoptActionablePlanErrorRe.MatchString(msg) || projectServiceActionablePlanErrorRe.MatchString(msg)
}

var projectServiceActionablePlanErrorRe = regexp.MustCompile(`^PROJECT_SERVICE_[A-Z0-9_]+$`)

// adoptActionablePlanErrorRe is the allowlist of stable, path-free phrases the
// BuildAdoptPlan validation / name-conflict errors carry (see internal/api/
// adopt.go). A new validation message not listed here is redacted, not leaked —
// the fail-safe direction for a redaction gate.
var adoptActionablePlanErrorRe = regexp.MustCompile(strings.Join([]string{
	`adopt entry name is required`,
	`--client must be one of`,
	`must include source`,
	`unknown adopt client`,
	`requires --name to equal`,
	`is disabled; enable it`,
	`is not a valid manifest name`,
	`already exists`,
	`collides with a shipped`,
	// adopt Area-3 fail-loud: a requested client's config is unreadable. Provably
	// path-free — the client id is a member of adoptSupportedClients and the reason
	// is one of the fixed adoptExtractionErrorClass strings; adoptErrorMessageHasPath
	// still runs first as the fail-closed backstop.
	`cannot adopt into client`,
}, "|"))

// adoptErrorPathRe matches a Windows drive-absolute path (`C:\` / `C:/`), a
// Windows path segment (`\host\`, `\Users\` — UNC or rooted), or a POSIX
// absolute path (a `/`-led segment after start, whitespace, or a quote /
// delimiter) embedded anywhere in a message. Adopt's validation errors carry
// only ids/names and never a path separator, so a match reliably flags a
// path-bearing read/parse failure whose absolute path must not reach the wire.
// Broadened for fable PR #516 P3-A (quoted `"/home/x"` and rooted `\Users\x`
// shapes the prior `(?:^|\s)/\S` + `\\\\` form missed).
var adoptErrorPathRe = regexp.MustCompile(`[A-Za-z]:[\\/]|\\[A-Za-z][^\s\\]*\\|(?:^|[\s"'(=:,])/\S`)

func adoptErrorMessageHasPath(msg string) bool {
	return adoptErrorPathRe.MatchString(msg)
}

func adoptPlanErrorStatus(err error) (int, string) {
	if errors.Is(err, api.ErrManifestNameExists) || errors.Is(err, api.ErrManifestNameEmbedded) {
		return http.StatusConflict, "NAME_CONFLICT"
	}
	msg := err.Error()
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "collides with a shipped") {
		return http.StatusConflict, "NAME_CONFLICT"
	}
	return http.StatusBadRequest, "BAD_INPUT"
}

func writeGUIProjectAdoptTokenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, clients.ErrCodexProjectSourcePreviewRequired):
		writeAPIError(w, err, http.StatusConflict, "CODEX_PROJECT_SOURCE_PREVIEW_REQUIRED")
	case errors.Is(err, clients.ErrCodexProjectSourceChanged):
		writeAPIError(w, err, http.StatusConflict, "CODEX_PROJECT_SOURCE_CHANGED")
	case errors.Is(err, api.ErrProjectAdoptSourcePreimageUnexpected):
		writeAPIError(w, err, http.StatusBadRequest, "PROJECT_ADOPT_SOURCE_PREIMAGE_UNEXPECTED")
	case errors.Is(err, api.ErrProjectAdoptRepeatStateInconsistent):
		writeAPIError(w, err, http.StatusConflict, "PROJECT_ADOPT_REPEAT_STATE_INCONSISTENT")
	default:
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "ADOPT_FAILED", "/api/adopt")
	}
}

func adoptExecuteErrorStatus(err error) (int, string) {
	if errors.Is(err, api.ErrManifestNameExists) || errors.Is(err, api.ErrManifestNameEmbedded) {
		return http.StatusConflict, "NAME_CONFLICT"
	}
	msg := err.Error()
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "collides with a shipped") {
		return http.StatusConflict, "NAME_CONFLICT"
	}
	if errors.Is(err, api.ErrSecureWriteReparsePointRefused) {
		return http.StatusConflict, "SYMLINK_CONSENT_REQUIRED"
	}
	if isAdoptSymlinkWriteRefusal(msg) {
		return http.StatusConflict, "SYMLINK_CONSENT_REQUIRED"
	}
	return http.StatusInternalServerError, "ADOPT_FAILED"
}

func projectServiceExecutionFacts(err error) map[string]bool {
	var stage *api.AdoptStageError
	if !errors.As(err, &stage) || stage == nil {
		return nil
	}
	switch stage.CommitState {
	case "uncommitted":
		return map[string]bool{"project_service_uncommitted": true}
	case "pending":
		return map[string]bool{"project_service_publication_pending": true}
	case "committed_unverified":
		return map[string]bool{"project_service_committed_unverified": true}
	default:
		return nil
	}
}

func writeAdoptExecuteError(w http.ResponseWriter, err error, status int, code string, optionalFacts ...map[string]bool) {
	if writeH6ActionBudgetError(w, err) {
		return
	}
	var facts map[string]bool
	for _, selected := range optionalFacts {
		for key, value := range selected {
			if facts == nil {
				facts = make(map[string]bool)
			}
			facts[key] = value
		}
	}
	writePublic := func(base map[string]string) {
		body := make(map[string]any, len(facts)+len(base))
		for key, value := range facts {
			body[key] = value
		}
		for key, value := range base {
			body[key] = value
		}
		writeJSON(w, status, body)
	}
	var leaseFailure *api.LeaseFailure
	if errors.As(err, &leaseFailure) && leaseFailure != nil {
		if leaseFailure.ReasonID != "" {
			writePublic(map[string]string{
				"error":     leaseFailure.FailureID,
				"code":      code,
				"reason_id": string(leaseFailure.ReasonID),
				"action":    string(leaseFailure.Action),
			})
			return
		}
		if leaseFailure.FailureID == "E_ADOPT_LEASE_CLEANUP" {
			writePublic(map[string]string{"error": leaseFailure.FailureID, "code": code})
			return
		}
	}
	// The public lease cleanup identifier is already redacted and actionable.
	// Preserve it for the local GUI while every wrapped owner cause stays out of
	// the wire body and renderer.
	if err.Error() == "E_ADOPT_LEASE_CLEANUP" {
		writePublic(map[string]string{"error": "E_ADOPT_LEASE_CLEANUP", "code": code})
		return
	}
	if errors.Is(err, api.ErrSecureWriteReparsePointRefused) {
		writePublic(map[string]string{"error": api.ErrSecureWriteReparsePointRefused.Error(), "code": code})
		return
	}
	// SYMLINK_CONSENT_REQUIRED deliberately names the resolved symlink target —
	// the operator already sees resolved_path in the plan, so surfacing it here
	// is consistent, not a leak.
	if code == "SYMLINK_CONSENT_REQUIRED" {
		writePublic(map[string]string{"error": err.Error(), "code": code})
		return
	}
	// Everything else path-bearing or 5xx is redacted. NAME_CONFLICT is
	// normally a path-free "manifest X already exists" message, but a wrapped OS
	// error ("... already exists: C:\path") would ride this lane, so redact it
	// too when a path is present (fable PR #516 P3-B).
	if status >= http.StatusInternalServerError || code == "BAD_INPUT" || adoptErrorMessageHasPath(err.Error()) {
		writeAPIErrorRedactedFields(w, err, status, code, "/api/adopt", facts)
		return
	}
	writePublic(map[string]string{"error": err.Error(), "code": code})
}

func isAdoptSymlinkWriteRefusal(msg string) bool {
	return strings.Contains(msg, "pre-existing symlink refused") ||
		strings.Contains(msg, "pre-existing reparse point refused") ||
		strings.Contains(msg, "symlink may have been repointed after consent") ||
		strings.Contains(msg, "refuse to initialize through symlink")
}

func adoptSymlinkTargetsMissingConsent(fresh, consented []adoptSymlinkTarget) []adoptSymlinkTarget {
	if len(fresh) == 0 {
		return nil
	}
	consentedSet := make(map[string]struct{}, len(consented))
	for _, target := range consented {
		consentedSet[adoptSymlinkConsentKey(target.Client, target.ResolvedPath)] = struct{}{}
	}
	missing := make([]adoptSymlinkTarget, 0)
	for _, target := range fresh {
		if _, ok := consentedSet[adoptSymlinkConsentKey(target.Client, target.ResolvedPath)]; !ok {
			missing = append(missing, target)
		}
	}
	return missing
}

func adoptSymlinkConsentKey(client, resolvedPath string) string {
	cleanResolved := filepath.Clean(resolvedPath)
	if runtime.GOOS == "windows" {
		cleanResolved = strings.ToLower(cleanResolved)
	}
	return client + "\x00" + cleanResolved
}

func adoptResolvedSymlinkConsents(targets []adoptSymlinkTarget) []api.ResolvedSymlinkConsent {
	if len(targets) == 0 {
		return nil
	}
	consents := make([]api.ResolvedSymlinkConsent, 0, len(targets))
	for _, target := range targets {
		consents = append(consents, api.ResolvedSymlinkConsent{
			Client:             target.Client,
			OriginalPath:       target.originalPath,
			PinnedResolvedPath: target.pinnedPath,
		})
	}
	return consents
}

func formatAdoptSymlinkTargets(targets []adoptSymlinkTarget) string {
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		parts = append(parts, fmt.Sprintf("%s -> %s", target.Client, target.ResolvedPath))
	}
	return strings.Join(parts, ", ")
}
