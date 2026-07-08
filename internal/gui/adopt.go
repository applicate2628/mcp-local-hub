package gui

import (
	"bytes"
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
	Entry          string               `json:"entry"`
	Client         string               `json:"client"`
	Clients        []string             `json:"clients"`
	Name           string               `json:"name"`
	Port           int                  `json:"port"`
	SymlinkConsent []adoptSymlinkTarget `json:"symlink_consent"`
}

type adoptSymlinkTarget struct {
	Client       string `json:"client"`
	ResolvedPath string `json:"resolved_path"`

	originalPath string
	pinnedPath   string
}

type adoptPlanResponse struct {
	*api.AdoptPlan
	SymlinkTargets []adoptSymlinkTarget `json:"symlink_targets"`
}

type adoptExecuteResponse struct {
	Name           string               `json:"name"`
	Port           int                  `json:"port"`
	AdoptClients   []string             `json:"adopt_clients"`
	SymlinkTargets []adoptSymlinkTarget `json:"symlink_targets"`
}

func registerAdoptRoutes(s *Server) {
	s.mux.HandleFunc("/api/adopt/plan", s.requireSameOrigin(s.adoptPlanHandler))
	s.mux.HandleFunc("/api/adopt", s.requireSameOrigin(s.adoptHandler))
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
	plan, targets, err := buildGUIAdoptPlan(&req)
	if err != nil {
		writeAdoptPlanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adoptPlanResponse{
		AdoptPlan:      plan,
		SymlinkTargets: targets,
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
	plan, targets, err := buildGUIAdoptPlan(&req)
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
	if err := api.NewAPI().ExecuteAdoptWithOpts(plan, &narration, api.ExecuteAdoptOpts{
		SymlinkConsents: adoptResolvedSymlinkConsents(targets),
	}); err != nil {
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

func buildGUIAdoptPlan(req *adoptRequest) (*api.AdoptPlan, []adoptSymlinkTarget, error) {
	plan, err := api.NewAPI().BuildAdoptPlan(api.AdoptOpts{
		EntryName:    req.Entry,
		Client:       req.Client,
		ManifestName: req.Name,
		Port:         req.Port,
		Clients:      req.Clients,
	})
	if err != nil {
		return nil, nil, err
	}
	targets, err := adoptSymlinkTargets(plan.AdoptClients)
	if err != nil {
		return nil, nil, err
	}
	return plan, targets, nil
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

func writeAdoptPlanError(w http.ResponseWriter, err error) {
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
	return adoptActionablePlanErrorRe.MatchString(msg)
}

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
	msg := err.Error()
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "collides with a shipped") {
		return http.StatusConflict, "NAME_CONFLICT"
	}
	return http.StatusBadRequest, "BAD_INPUT"
}

func adoptExecuteErrorStatus(err error) (int, string) {
	msg := err.Error()
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "collides with a shipped") {
		return http.StatusConflict, "NAME_CONFLICT"
	}
	if isAdoptSymlinkWriteRefusal(msg) {
		return http.StatusConflict, "SYMLINK_CONSENT_REQUIRED"
	}
	return http.StatusInternalServerError, "ADOPT_FAILED"
}

func writeAdoptExecuteError(w http.ResponseWriter, err error, status int, code string) {
	// SYMLINK_CONSENT_REQUIRED deliberately names the resolved symlink target —
	// the operator already sees resolved_path in the plan, so surfacing it here
	// is consistent, not a leak.
	if code == "SYMLINK_CONSENT_REQUIRED" {
		writeAPIError(w, err, status, code)
		return
	}
	// Everything else path-bearing or 5xx is redacted. NAME_CONFLICT is
	// normally a path-free "manifest X already exists" message, but a wrapped OS
	// error ("... already exists: C:\path") would ride this lane, so redact it
	// too when a path is present (fable PR #516 P3-B).
	if status >= http.StatusInternalServerError || code == "BAD_INPUT" || adoptErrorMessageHasPath(err.Error()) {
		writeAPIErrorRedacted(w, err, status, code, "/api/adopt")
		return
	}
	writeAPIError(w, err, status, code)
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
