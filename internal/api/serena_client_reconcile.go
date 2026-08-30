// serena_client_reconcile.go — dynamic-pool client-reconcile to the
// constant /serena/mcp router endpoint.
//
// Phase 3 of the serena dynamic-pool migrate redesign (design
// docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md
// §5 finding #5; plan docs/superpowers/plans/2026-05-29-serena-migrate-redesign.md
// "Phase 3 — Client-reconcile to /serena/mcp").
//
// Problem: when serena moves to the dynamic-pool model the per-daemon
// localhost:9121 global daemon goes away, but managed clients still point
// their `serena` MCP entry at it. The /serena/mcp router on the GUI server
// is the new routing surface — it resolves the target workspace from a
// tool's path-arg against the live registry, so every client points at ONE
// constant URL, workspace-agnostic. This file rewrites each in-scope
// client's serena entry to that router URL BEFORE the legacy 9121 endpoint
// is removed, so a per-client rewrite failure leaves that client on the
// still-functional legacy endpoint rather than a dead URL.
//
// CRITICAL — this is NOT the G4 hub-resolver path (design claim #8). Serena
// client routing flows exclusively through the registry-driven /serena/mcp
// router. This file does NOT import or invoke
// BuildResolverSnapshotFromManifests / manifestHasScheduledDaemon and does
// NOT touch the G4 binding topology.
//
// This is a NEW, UNWIRED function: Phase 4 (the migrate command) calls it.
// It is not wired into any CLI command or migrate path here.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"mcp-local-hub/internal/clients"
)

// SerenaRouterURLPath is the constant path of the GUI-server router that
// fronts every per-workspace serena daemon. Clients point their serena MCP
// entry here (workspace-agnostic); the router resolves the workspace per
// request from the tool's path-arg (internal/gui/serena_router.go:146
// registers "/serena/mcp").
const SerenaRouterURLPath = "/serena/mcp"

// serenaEntryName is the MCP entry name every client uses for serena. It is
// the manifest server name; clients key their config map on it.
const serenaEntryName = "serena"

// SerenaRouterClientURL is the canonical client MCP URL for the dynamic-pool
// serena server: the constant /serena/mcp router on the LIVE GUI port. It is the
// SINGLE OWNER of serena's client URL — consumed by the write path (migrate) and
// matched by the read path (scan classify) so neither falls back to the legacy
// per-daemon 9121 URL from serena's still-legacy-shaped manifest (the
// serena-client-revert-on-manifest-sync defect). It mirrors the exact shape
// ReconcileSerenaClientsToRouter builds (serena_client_reconcile.go routerURL).
func SerenaRouterClientURL(guiPort int) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", guiPort, SerenaRouterURLPath)
}

// SerenaClientEndpoint is the sole client-facing projection for the dynamic
// Serena pool. WorkspaceEntry.Port is deliberately absent: that persisted port
// belongs to the per-workspace proxy/upstream and must never be offered as an
// MCP client URL.
type SerenaClientEndpoint struct {
	ClientEndpoint string `json:"client_endpoint"`
	EndpointMode   string `json:"endpoint_mode"`
	RouterPort     int    `json:"router_port"`
	Ready          bool   `json:"ready"`
	ReadinessStage string `json:"readiness_stage"`
}

const (
	SerenaEndpointModeMCPFront   = "mcp-front"
	SerenaEndpointModeGUICompat  = "gui-compat"
	SerenaEndpointReadinessReady = "ready"
)

// SerenaClientEndpointUnreadyError preserves the routing target and exact
// readiness substage for CLI and GUI callers. A formatted URL is never a
// successful endpoint projection: the route must first pass the shared MCP
// shape and initialize probes.
type SerenaClientEndpointUnreadyError struct {
	Target ClientRoutingTarget
	Stage  MCPFrontProbeStage
	Cause  error
}

func (e *SerenaClientEndpointUnreadyError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("SERENA_CLIENT_ENDPOINT_UNREADY: mode=%s port=%d stage=%s: %v", e.Target.Mode, e.Target.Port, e.Stage, e.Cause)
}

func (e *SerenaClientEndpointUnreadyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ResolveSerenaClientEndpoint resolves the current routing authority and
// returns a client URL only after the existing bounded router lifecycle proof
// succeeds. It intentionally reads the routing target from its existing owner;
// it does not infer an endpoint from a workspace proxy port or GUI config.
func (a *API) ResolveSerenaClientEndpoint(ctx context.Context) (SerenaClientEndpoint, error) {
	target, err := a.ResolveClientRoutingTarget()
	if err != nil {
		return SerenaClientEndpoint{}, &SerenaClientEndpointUnreadyError{
			Stage: MCPFrontProbeStageInput,
			Cause: err,
		}
	}
	return ResolveSerenaClientEndpointForTarget(ctx, target)
}

// ResolveSerenaClientEndpointForTarget is the target-pinned form used by
// composition roots that already hold routing authority. It is also the test
// seam for a hermetic local router; it makes no state, client, daemon, or
// workspace mutation.
func ResolveSerenaClientEndpointForTarget(ctx context.Context, target ClientRoutingTarget) (SerenaClientEndpoint, error) {
	if err := ValidateClientRoutingTarget(target); err != nil {
		return SerenaClientEndpoint{}, &SerenaClientEndpointUnreadyError{
			Target: target,
			Stage:  MCPFrontProbeStageInput,
			Cause:  err,
		}
	}

	mode := ""
	switch target.Mode {
	case MCPFrontRoutingTargetGUI:
		mode = SerenaEndpointModeGUICompat
	case MCPFrontRoutingTargetFront:
		mode = SerenaEndpointModeMCPFront
	default:
		return SerenaClientEndpoint{}, &SerenaClientEndpointUnreadyError{
			Target: target,
			Stage:  MCPFrontProbeStageInput,
			Cause:  &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("unsupported Serena routing mode %q", target.Mode)},
		}
	}
	if err := AssertSerenaRouterRouteLive(ctx, target.Port); err != nil {
		return SerenaClientEndpoint{}, &SerenaClientEndpointUnreadyError{
			Target: target,
			Stage:  probeStageFromError(err),
			Cause:  err,
		}
	}
	return SerenaClientEndpoint{
		ClientEndpoint: SerenaRouterClientURL(target.Port),
		EndpointMode:   mode,
		RouterPort:     target.Port,
		Ready:          true,
		ReadinessStage: SerenaEndpointReadinessReady,
	}, nil
}

// SerenaWorkspaceProjection is the derived Serena-only read model shared by
// CLI and GUI presenters. It retains the registry's legacy Port field for one
// compatibility window while making its internal-only role explicit through
// WorkspaceProxyPort. ClientEndpoint always comes from SerenaClientEndpoint,
// never from the workspace proxy.
type SerenaWorkspaceProjection struct {
	WorkspaceKey       string              `json:"workspace_key"`
	WorkspacePath      string              `json:"workspace_path"`
	Language           string              `json:"language"`
	Backend            string              `json:"backend"`
	Port               int                 `json:"port"`
	WorkspaceProxyPort int                 `json:"workspace_proxy_port"`
	TaskName           string              `json:"task_name"`
	ClientEntries      map[string]string   `json:"client_entries,omitempty"`
	Lifecycle          string              `json:"lifecycle,omitempty"`
	LastError          string              `json:"last_error,omitempty"`
	Languages          []string            `json:"language_servers,omitempty"`
	ClientEndpoint     string              `json:"client_endpoint"`
	EndpointMode       string              `json:"endpoint_mode"`
	ServiceState       ServiceStateV1      `json:"service_state"`
	ReadinessStage     ReadinessStageV1    `json:"readiness_stage"`
	ReadinessSettled   bool                `json:"readiness_settled"`
	ReadinessFailure   *ReadinessFailureV1 `json:"readiness_failure,omitempty"`
}

// SerenaWorkspaceProjectionMode selects whether an aligned but not-yet-ready
// workspace is returned for observation or rejected for a settled-only caller.
type SerenaWorkspaceProjectionMode string

const (
	SerenaWorkspaceProjectionModeSnapshot       SerenaWorkspaceProjectionMode = "snapshot"
	SerenaWorkspaceProjectionModeRequireSettled SerenaWorkspaceProjectionMode = "require_settled"
)

const (
	SerenaWorkspaceStateMismatchProxyPort          = "proxy_port_mismatch"
	SerenaWorkspaceStateMismatchIntentMissing      = "intent_missing"
	SerenaWorkspaceStateMismatchStatusMissing      = "status_missing"
	SerenaWorkspaceStateMismatchReadinessMissing   = "readiness_missing"
	SerenaWorkspaceStateMismatchAuthorityDuplicate = "authority_duplicate"
	SerenaWorkspaceStateMismatchTask               = "task_mismatch"
	SerenaWorkspaceStateMismatchWorkspace          = "workspace_mismatch"
	SerenaWorkspaceStateMismatchGeneration         = "generation_mismatch"
)

// SerenaWorkspaceStateMismatchError reports an authority contradiction. A
// workspace mismatch includes the three compared canonical path operands so an
// operator can identify the disagreeing authority without another state read.
type SerenaWorkspaceStateMismatchError struct {
	Kind                                                string
	WorkspaceKey, TaskName                              string
	RegistryWorkspacePath, IntentWorkspace              string
	StatusWorkspace                                     string
	RegistryPort, IntentPort, StatusPort, ReadinessPort int
}

func (e *SerenaWorkspaceStateMismatchError) Error() string {
	if e == nil {
		return ""
	}
	prefix := fmt.Sprintf("SERENA_WORKSPACE_STATE_MISMATCH/%s: workspace=%q task=%q", e.Kind, e.WorkspaceKey, e.TaskName)
	if e.Kind == SerenaWorkspaceStateMismatchWorkspace {
		prefix += fmt.Sprintf(" registry_workspace_path=%q intent_workspace=%q status_workspace=%q", e.RegistryWorkspacePath, e.IntentWorkspace, e.StatusWorkspace)
	}
	return fmt.Sprintf("%s registry_port=%d intent_port=%d status_port=%d readiness_port=%d", prefix, e.RegistryPort, e.IntentPort, e.StatusPort, e.ReadinessPort)
}

// SerenaWorkspaceProxyUnreadyError is the settled-only result for an aligned
// proxy that has not reached a running, complete, settled readiness state.
type SerenaWorkspaceProxyUnreadyError struct {
	WorkspaceKey string
	TaskName     string
	ServiceState ServiceStateV1
	Stage        ReadinessStageV1
	Settled      bool
	Failure      *ReadinessFailureV1
}

func (e *SerenaWorkspaceProxyUnreadyError) Error() string {
	if e == nil {
		return ""
	}
	failureID := ""
	if e.Failure != nil {
		failureID = e.Failure.FailureID
	}
	return fmt.Sprintf("SERENA_WORKSPACE_PROXY_UNREADY: workspace=%q task=%q service_state=%s readiness_stage=%s settled=%t failure_id=%s", e.WorkspaceKey, e.TaskName, e.ServiceState, e.Stage, e.Settled, failureID)
}

// SerenaWorkspaceProjectionInputV1 is one already-held authority snapshot.
// ProjectSerenaWorkspaceSnapshot is pure: it does not read registry/intent
// files, dial supervisor IPC, or probe routing; acquisition stays with its API
// composition owner so every workspace observes the same command snapshot.
type SerenaWorkspaceProjectionInputV1 struct {
	Mode             SerenaWorkspaceProjectionMode
	RegistryRows     []WorkspaceEntry
	SupervisorIntent SupervisorIntentFile
	StatusRows       []DaemonStatus
	Readiness        ReadinessSnapshotV1
	Endpoint         SerenaClientEndpoint
}

// SerenaWorkspaceProjectionOutputV1 contains the task-sorted Serena rows and
// a cloned, readiness-enriched status snapshot. A mismatch returns its zero
// value so a presenter can never render a mixture of rejected and healthy rows.
type SerenaWorkspaceProjectionOutputV1 struct {
	Workspaces []SerenaWorkspaceProjection
	StatusRows []DaemonStatus
}

// ProjectSerenaWorkspaceSnapshot is the single API-owned, batch authority join
// for Serena workspace rows. It compares registry, supervisor intent, held IPC
// status, the readiness reduction derived from that same status snapshot, and
// the already-observed client routing endpoint. It deliberately performs no
// acquisition or mutation.
func ProjectSerenaWorkspaceSnapshot(in SerenaWorkspaceProjectionInputV1) (SerenaWorkspaceProjectionOutputV1, error) {
	if in.Mode != SerenaWorkspaceProjectionModeSnapshot && in.Mode != SerenaWorkspaceProjectionModeRequireSettled {
		return SerenaWorkspaceProjectionOutputV1{}, &SerenaWorkspaceStateMismatchError{Kind: "projection_mode_invalid"}
	}

	statusRows := cloneDaemonStatusRows(in.StatusRows)
	intentByTask, intentDup := indexSerenaIntentByTask(in.SupervisorIntent.Daemons)
	statusByTask, statusDup := indexDaemonStatusByTask(statusRows)
	readinessByTask, readinessDup := indexReadinessByTask(in.Readiness.Daemons)
	workspaces := make([]SerenaWorkspaceProjection, 0)

	for _, entry := range in.RegistryRows {
		if entry.Language != SerenaLanguageSentinel || entry.Backend != SerenaServerName {
			continue
		}
		taskName := normalizeSerenaAuthorityTaskName(entry.TaskName)
		mismatch := func(kind string, intent *SupervisorDaemon, status *DaemonStatus, readiness *DaemonReadinessV1) error {
			err := &SerenaWorkspaceStateMismatchError{Kind: kind, WorkspaceKey: entry.WorkspaceKey, TaskName: taskName, RegistryWorkspacePath: entry.WorkspacePath, RegistryPort: entry.Port}
			if intent != nil {
				err.IntentWorkspace = intent.Workspace
				err.IntentPort = intent.Port
			}
			if status != nil {
				err.StatusWorkspace = status.Workspace
				err.StatusPort = status.Port
			}
			if readiness != nil {
				err.ReadinessPort = readiness.Port
			}
			return err
		}
		if taskName == "" || entry.Port <= 0 {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchTask, nil, nil, nil)
		}
		if intentDup[taskName] || statusDup[taskName] || readinessDup[taskName] {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchAuthorityDuplicate, nil, nil, nil)
		}
		intent, intentOK := intentByTask[taskName]
		if !intentOK {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchIntentMissing, nil, nil, nil)
		}
		statusIndex, statusOK := statusByTask[taskName]
		if !statusOK {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchStatusMissing, &intent, nil, nil)
		}
		status := &statusRows[statusIndex]
		readiness, readinessOK := readinessByTask[taskName]
		if !readinessOK {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchReadinessMissing, &intent, status, nil)
		}
		if normalizeSerenaAuthorityTaskName(intent.TaskName) != taskName || normalizeSerenaAuthorityTaskName(status.TaskName) != taskName || normalizeSerenaAuthorityTaskName(readiness.TaskName) != taskName {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchTask, &intent, status, &readiness)
		}
		if intent.Workspace != entry.WorkspacePath || status.Workspace != entry.WorkspacePath {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchWorkspace, &intent, status, &readiness)
		}
		if intent.Port <= 0 || status.Port <= 0 || readiness.Port <= 0 || entry.Port != intent.Port || entry.Port != status.Port || entry.Port != readiness.Port {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchProxyPort, &intent, status, &readiness)
		}
		if status.ReadinessObservation == nil {
			legacySnapshotUnready := in.Mode == SerenaWorkspaceProjectionModeSnapshot && readiness.PIDGeneration == 0 && readiness.ServiceState == ServiceStateStarting && readiness.Stage == ReadinessStageWrapperStart && !readiness.Settled
			if !legacySnapshotUnready {
				return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchGeneration, &intent, status, &readiness)
			}
		} else if status.ReadinessObservation.CurrentPIDGeneration == 0 || status.ReadinessObservation.ObservedPIDGeneration == 0 || readiness.PIDGeneration == 0 || status.ReadinessObservation.CurrentPIDGeneration != status.ReadinessObservation.ObservedPIDGeneration || status.ReadinessObservation.ObservedPIDGeneration != readiness.PIDGeneration {
			return SerenaWorkspaceProjectionOutputV1{}, mismatch(SerenaWorkspaceStateMismatchGeneration, &intent, status, &readiness)
		}

		applyDaemonReadiness(status, readiness)
		projection := projectSerenaWorkspaceAuthorityRow(entry, readiness)
		if readiness.ServiceState != ServiceStateRunning || readiness.Stage != ReadinessStageComplete || !readiness.Settled {
			if in.Mode == SerenaWorkspaceProjectionModeRequireSettled {
				return SerenaWorkspaceProjectionOutputV1{}, &SerenaWorkspaceProxyUnreadyError{WorkspaceKey: entry.WorkspaceKey, TaskName: taskName, ServiceState: readiness.ServiceState, Stage: readiness.Stage, Settled: readiness.Settled, Failure: cloneReadinessFailure(readiness.Failure)}
			}
			workspaces = append(workspaces, projection)
			continue
		}

		if !serenaClientEndpointMatchesRouter(in.Endpoint) || in.Endpoint.RouterPort == entry.Port {
			if in.Mode == SerenaWorkspaceProjectionModeRequireSettled {
				return SerenaWorkspaceProjectionOutputV1{}, &SerenaClientEndpointUnreadyError{Stage: serenaEndpointUnreadyStage(in.Endpoint), Cause: errors.New("routing observation is not ready")}
			}
			projection.ServiceState = ServiceStateDegraded
			status.ServiceState = ServiceStateDegraded
			workspaces = append(workspaces, projection)
			continue
		}
		projection.ClientEndpoint = in.Endpoint.ClientEndpoint
		projection.EndpointMode = in.Endpoint.EndpointMode
		workspaces = append(workspaces, projection)
	}

	sort.SliceStable(workspaces, func(i, j int) bool { return workspaces[i].TaskName < workspaces[j].TaskName })
	return SerenaWorkspaceProjectionOutputV1{Workspaces: workspaces, StatusRows: statusRows}, nil
}

func normalizeSerenaAuthorityTaskName(taskName string) string {
	return strings.TrimPrefix(taskName, `\`)
}

func indexSerenaIntentByTask(rows []SupervisorDaemon) (map[string]SupervisorDaemon, map[string]bool) {
	out, duplicate := make(map[string]SupervisorDaemon, len(rows)), map[string]bool{}
	for _, row := range rows {
		key := normalizeSerenaAuthorityTaskName(row.TaskName)
		if _, exists := out[key]; exists {
			duplicate[key] = true
		}
		out[key] = row
	}
	return out, duplicate
}

func indexDaemonStatusByTask(rows []DaemonStatus) (map[string]int, map[string]bool) {
	out, duplicate := make(map[string]int, len(rows)), map[string]bool{}
	for i := range rows {
		key := normalizeSerenaAuthorityTaskName(rows[i].TaskName)
		if _, exists := out[key]; exists {
			duplicate[key] = true
		}
		out[key] = i
	}
	return out, duplicate
}

func indexReadinessByTask(rows []DaemonReadinessV1) (map[string]DaemonReadinessV1, map[string]bool) {
	out, duplicate := make(map[string]DaemonReadinessV1, len(rows)), map[string]bool{}
	for _, row := range rows {
		key := normalizeSerenaAuthorityTaskName(row.TaskName)
		if _, exists := out[key]; exists {
			duplicate[key] = true
		}
		out[key] = row
	}
	return out, duplicate
}

func cloneDaemonStatusRows(in []DaemonStatus) []DaemonStatus {
	out := make([]DaemonStatus, len(in))
	copy(out, in)
	for i := range out {
		out[i].ReadinessFailure = cloneReadinessFailure(in[i].ReadinessFailure)
		if in[i].ReadinessObservation != nil {
			observation := *in[i].ReadinessObservation
			observation.Failures = append([]ReadinessFailureV1(nil), in[i].ReadinessObservation.Failures...)
			out[i].ReadinessObservation = &observation
		}
		if in[i].Health != nil {
			health := *in[i].Health
			out[i].Health = &health
		}
		if in[i].JobProtection != nil {
			jobProtection := *in[i].JobProtection
			out[i].JobProtection = &jobProtection
		}
	}
	return out
}

func cloneReadinessFailure(in *ReadinessFailureV1) *ReadinessFailureV1 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func applyDaemonReadiness(status *DaemonStatus, readiness DaemonReadinessV1) {
	status.ServiceState = readiness.ServiceState
	status.ReadinessStage = readiness.Stage
	status.ReadinessSettled = readiness.Settled
	status.ReadinessFailure = cloneReadinessFailure(readiness.Failure)
}

func projectSerenaWorkspaceAuthorityRow(entry WorkspaceEntry, readiness DaemonReadinessV1) SerenaWorkspaceProjection {
	return SerenaWorkspaceProjection{
		WorkspaceKey: entry.WorkspaceKey, WorkspacePath: entry.WorkspacePath,
		Language: entry.Language, Backend: entry.Backend, Port: entry.Port,
		WorkspaceProxyPort: entry.Port, TaskName: entry.TaskName,
		ClientEntries: cloneSerenaClientEntries(entry.ClientEntries), Lifecycle: entry.Lifecycle,
		LastError: entry.LastError, Languages: append([]string(nil), entry.Languages...),
		ServiceState: readiness.ServiceState, ReadinessStage: readiness.Stage,
		ReadinessSettled: readiness.Settled, ReadinessFailure: cloneReadinessFailure(readiness.Failure),
	}
}

func serenaClientEndpointMatchesRouter(endpoint SerenaClientEndpoint) bool {
	return endpoint.Ready && endpoint.RouterPort > 0 && endpoint.EndpointMode != "" && endpoint.ReadinessStage == SerenaEndpointReadinessReady && endpoint.ClientEndpoint == SerenaRouterClientURL(endpoint.RouterPort) && IsSerenaRouterURL(endpoint.ClientEndpoint)
}

func serenaEndpointUnreadyStage(endpoint SerenaClientEndpoint) MCPFrontProbeStage {
	if endpoint.ReadinessStage != "" && endpoint.ReadinessStage != SerenaEndpointReadinessReady {
		return MCPFrontProbeStage(endpoint.ReadinessStage)
	}
	return MCPFrontProbeStageInput
}

func cloneSerenaClientEntries(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// IsSerenaRouterURL reports whether an endpoint is serena's /serena/mcp router
// URL (loopback host + the router path). PORT-AGNOSTIC on purpose: the GUI
// re-binds its listener port on each start, so the path is the stable
// discriminator. The scan classifier uses it to recognize a correctly-routed
// serena client entry as via-hub instead of misreading it as a stale/foreign
// loopback.
func IsSerenaRouterURL(endpoint string) bool {
	if !clients.IsHubHTTPURL(endpoint) {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	// IsHubHTTPURL is a string-prefix check, so a userinfo URL
	// (http://127.0.0.1:9125@evil.example/serena/mcp) passes it while the PARSED
	// host is evil.example. Reject userinfo and re-validate the parsed hostname is
	// loopback so a remote URL cannot masquerade as the local router (#379 r4).
	if u.User != nil {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
	default:
		return false
	}
	return u.Path == SerenaRouterURLPath
}

// IsHubOwnedSerenaRouterEntry reports whether a client entry is a HUB-OWNED serena
// /serena/mcp router entry: the URL-native shape (only mcphub writes /serena/mcp),
// OR the relay shape (entry.RelayURL) WHEN the relay command is the mcphub binary.
// The relay binary guard stops uninstall/demigrate from removing a user-owned relay
// that merely points its --url at a /serena/mcp endpoint (#379 r4). Port-agnostic
// (cleanup removes a stale-port entry too).
func IsHubOwnedSerenaRouterEntry(e *clients.MCPEntry) bool {
	if e == nil {
		return false
	}
	if IsSerenaRouterURL(e.URL) {
		return true
	}
	return IsSerenaRouterURL(e.RelayURL) && clients.IsMcphubBinary(e.RelayExePath)
}

// IsLiveSerenaRouterURL reports whether endpoint is the /serena/mcp router on
// the LIVE GUI port. guiPort<=0 means the caller has no live port, so degrade to
// the port-agnostic shape check and never claim staleness without proof.
func IsLiveSerenaRouterURL(endpoint string, guiPort int) bool {
	return IsLiveSerenaRouterURLAnyPort(endpoint, guiPort)
}

// IsLiveSerenaRouterURLAnyPort is the multi-port generalization of
// IsLiveSerenaRouterURL: an entry counts as live if its port matches ANY of
// the caller's known-live ports (sub-increment 2a — a serena router entry
// may legitimately point at the GUI's live port OR the settings-owned
// mcp_front.port). A naive `IsLiveSerenaRouterURL(e, a) ||
// IsLiveSerenaRouterURL(e, b)` is WRONG here: IsLiveSerenaRouterURL degrades
// to "true" whenever its OWN single port argument is <=0 (the CLI-unknown-
// port case), so OR-ing two such calls would make a genuinely stale entry
// match via-hub as soon as EITHER port happens to be unknown — exactly the
// class of bug TestClassify's "stale GUI port -> external" cases catch.
//
// Correct semantics: if NONE of the supplied ports are known (all <=0),
// degrade to the port-agnostic shape check (matches IsLiveSerenaRouterURL's
// single-argument behavior with guiPort<=0 exactly). If AT LEAST ONE port is
// known (>0), the entry must match one of the KNOWN ports — an unknown port
// contributes no vote either way.
func IsLiveSerenaRouterURLAnyPort(endpoint string, ports ...int) bool {
	if !IsSerenaRouterURL(endpoint) {
		return false
	}
	anyKnown := false
	for _, port := range ports {
		if port <= 0 {
			continue
		}
		anyKnown = true
		if p, ok := loopbackEntryPort(endpoint); ok && p == port {
			return true
		}
	}
	return !anyKnown
}

// IsSerenaServer reports whether a server name is the dynamic-pool serena server
// (THE router-fronted server). The write + read paths special-case it by name
// because serena's client URL is the /serena/mcp router, not the manifest's
// legacy per-daemon port.
func IsSerenaServer(server string) bool {
	return server == serenaEntryName
}

// serenaReconcileClientSet is the O2 client set for the serena router
// rewrite — the clients the legacy serena manifest bound
// (servers/serena/manifest.yaml client_bindings) intersected, at call
// time, with the clients actually installed on the host. The set is fixed
// (not hard-coded per workstation): it mirrors the legacy serena binding
// surface. Order is stable for deterministic reports.
//
// Antigravity is in the set but takes the stdio-relay shape (relay → router)
// rather than a direct URL, per the descriptor-proxy design §5.
//
// This fixed set excludes the other relay-stdio adapter (zed) by
// construction — it mirrors only the legacy serena binding surface, which
// predates zed. The relay-stdio classification itself is owned by
// clients.IsRelayStdio (antigravity is correctly classified true there); the
// per-client relay handling below keys off that shape, not off this list.
func serenaReconcileClientSet() []string {
	return []string{
		"claude-code",
		"codex-cli",
		"cursor",
		"vscode",
		"gemini-cli",
		"qwen-cli",
		"antigravity",
	}
}

// readPidportFn is the test seam for parsing the GUI pidport file. It
// mirrors internal/gui.ReadPidport's "<PID> <PORT>\n" parse exactly so the
// runtime fail-closed contract is identical; the api package cannot import
// internal/gui directly (internal/gui imports internal/api, so the reverse
// would be an import cycle). Phase 4's caller in internal/cli — which CAN
// import both — may inject gui.ReadPidport via SerenaReconcileOpts.ReadPidport.
//
// Default: parseGUIPidportFile below.
var readPidportFn = parseGUIPidportFile

// parseGUIPidportFile reads "<PID> <PORT>\n" from path. Returns (0,0,err) on
// a missing file or any parse failure — byte-identical semantics to
// internal/gui.ReadPidport (single_instance.go:92-110).
func parseGUIPidportFile(path string) (pid, port int, err error) {
	b, err := readStateFileInodeAnchored(path)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed pidport %q", string(b))
	}
	pid, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse pid: %w", err)
	}
	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse port: %w", err)
	}
	return pid, port, nil
}

// SerenaReconcileOpts configures one client-reconcile run. All discovery
// inputs are injectable so the function is testable without a real GUI and
// so Phase 4 can wire the gui-package primitives across the
// api->gui-can't-import-api boundary.
type SerenaReconcileOpts struct {
	// RoutingTarget is the frozen, typed decision used by production writers.
	// When nil, Port/PidportPath retain their legacy source-compatible behavior.
	RoutingTarget *ClientRoutingTarget

	// Port, when non-zero, is used DIRECTLY as the client-URL port instead
	// of discovering it via the GUI pidport file — still liveness-probed
	// via Ping before any client write (fail-closed), just against a
	// caller-supplied port rather than a pidport-discovered one. PidportPath
	// / ReadPidport / VerifyIdentity are ignored when Port is set.
	//
	// This is the seam sub-increment 2a's `mcphub install
	// --reconcile-mcp-front` command uses to point clients at the
	// settings-owned mcp_front.port (internal/api.ResolveMCPFrontPort)
	// instead of the GUI's ephemeral, pidport-discovered port — the whole
	// point of the front-daemon decision is that the client-facing port no
	// longer needs the GUI process to be alive to be known, only to be
	// PROVEN live via the same readiness probe. Zero (the default) preserves
	// every existing caller's behavior (migrate_serena.go's GUI-pidport
	// discovery) unchanged.
	Port int

	// PidportPath is the absolute path to the GUI pidport file. Phase 4
	// supplies gui.PidportPath(); tests supply a temp path. Required — an
	// empty path fails closed (we never guess the router port) — UNLESS
	// Port is set, in which case pidport discovery is skipped entirely.
	PidportPath string

	// ReadPidport parses the pidport file at PidportPath into (pid, port).
	// nil → the package default parseGUIPidportFile (byte-identical to
	// gui.ReadPidport). Phase 4 may inject gui.ReadPidport directly.
	ReadPidport func(path string) (pid, port int, err error)

	// VerifyIdentity binds the pidport to the listener before any router URL is
	// trusted. nil → defaultGUIPidportIdentityCheck, which PROVES at the OS
	// level that the recorded PID owns the loopback router socket: the recorded
	// PID must be alive, must be the OS-reported owner of the 127.0.0.1:<port>
	// LISTENING socket (netstat -ano), and that owner's image must be the
	// mcphub binary. It does NOT trust any PID the listener self-reports over
	// HTTP (that is forgeable from the world-readable pidport file — bot PR
	// #252 P1). Tests may inject a no-op when they are not exercising discovery.
	VerifyIdentity func(ctx context.Context, pid, port int) error

	// Ping confirms the GUI router is actually serving on the discovered port.
	// nil → defaultRouterReadinessPing (a loopback HEAD + initialize probe). A nil
	// return means live; any error fails the reconcile closed. Mirrors the
	// G4 reconcile's live-probe-before-rewrite posture
	// (internal/cli/install.go:348-374).
	Ping func(ctx context.Context, port int) error

	// Clients is the {name -> adapter} map to reconcile. nil →
	// clients.AllClients() (production). Tests inject hermetic adapters
	// (their config paths resolve under a redirected HOME/USERPROFILE).
	Clients map[string]clients.Client

	// ClientsInclude optionally narrows the in-scope set. Empty → the full
	// serenaReconcileClientSet() intersected with installed adapters.
	ClientsInclude []string

	// McphubExePath is the absolute path written into Antigravity's relay
	// `command` field. "" → canonicalMcphubPath() (the installed binary,
	// never a throwaway %TEMP%/dev-checkout path — same rationale as
	// MigrateFrom).
	McphubExePath string

	// LegacyPort is retained for source compatibility with callers from the
	// pre-row-journal reconcile. Every supported adapter owns exactly one
	// "serena" entry, so the conditional router rewrite replaces that entry
	// in place and no second cleanup mutation is legal.
	LegacyPort int

	// RemoveLegacy is retained for source compatibility. It is intentionally
	// a no-op: every in-scope client uses the same "serena" entry name, so a
	// successful conditional add already replaced the legacy endpoint. A
	// later GetEntry/RemoveEntry cleanup would have no distinct legacy entry
	// to target and could delete a concurrent operator edit.
	RemoveLegacy bool

	// BackupKeepN bounds the per-adapter rolling backup count. 0 → no
	// pruning (Backup semantics). Phase 4 supplies effectiveBackupKeepN().
	BackupKeepN int

	// OnBackupCaptured, when non-nil, is the caller's WRITE-AHEAD hook: it is
	// invoked for each in-scope client immediately AFTER that client's
	// pre-rewrite backup lands on disk and STRICTLY BEFORE its config is
	// mutated, with the client name and the backup path.
	//
	// WHY IT EXISTS. The recovery record a caller persists for this run is
	// what its rollback restores from, and a record written AFTER the
	// mutations it protects has a window in which the mutation is committed
	// and the record is not: successful client rewrites followed by a failed
	// state-file write leave every rewritten client on the new endpoint while
	// rollback refuses, because no record exists. Handing the caller the
	// backup path before the mutation lets it close that window — it can make
	// the record durable first, and only then allow the write.
	//
	// CONTRACT. A non-nil return ABORTS that client's rewrite: its config is
	// NOT touched, and it is recorded as a Failed row (retryable, exactly like
	// a backup or AddEntry failure). Prevention, not compensation, is the
	// whole point — there is nothing to undo for a mutation that never
	// happened. The hook is called at most once per client per run, is never
	// called for a DryRun row (nothing is backed up or written), and MUST NOT
	// mutate any client config itself.
	//
	// nil (every pre-existing caller) preserves the previous behavior exactly.
	OnBackupCaptured func(client, backupPath string) error

	// OnAttemptPrepared persists the exact pre/intended tuple after the
	// backup hand-off and immediately before AddEntry. A non-nil error aborts
	// the client without mutation.
	OnAttemptPrepared func(SerenaReconcileAttemptResult) error

	// OnAttemptFinished is the total post-attempt hand-off used by durable
	// recovery callers. It runs exactly once after AddEntry returns, on both
	// success and error, and before this function advances to another client.
	// A callback error stops the whole run: the caller could not make the
	// observed ownership result durable, so later writes must not overtake it.
	OnAttemptFinished func(SerenaReconcileAttemptResult) error

	// DryRun reports the intended rewrites without touching any config
	// file. Discovery (pidport + ping) still runs so a dry-run surfaces the
	// "start the GUI first" failure too.
	DryRun bool

	// classifyClientMutation is the per-run settlement seam. Production leaves
	// it nil and uses clients.ClassifyClientMutation.
	classifyClientMutation func(error) clients.ClientMutationSettlement
}

// SerenaReconcileAttemptResult is the complete observable result of one
// prepared Serena rewrite. Fingerprints use the same stable projection as
// SerenaClientEntryFingerprint; an empty fingerprint means an absent entry.
type SerenaReconcileAttemptResult struct {
	Client               string
	BackupPath           string
	PreFingerprint       string
	IntendedFingerprint  string
	ObservedFingerprint  string
	Invoked              bool
	PreconditionConflict bool
	PreparationErr       error
	AdapterErr           error
	ObservationErr       error
}

type SerenaOwnedRestoreStatus string

const (
	SerenaOwnedRestoreRestored SerenaOwnedRestoreStatus = "restored"
	SerenaOwnedRestoreConflict SerenaOwnedRestoreStatus = "skipped-conflict"
	SerenaOwnedRestoreFailed   SerenaOwnedRestoreStatus = "failed"
)

// SerenaOwnedRestoreResult is one front-reconcile rollback outcome.
type SerenaOwnedRestoreResult struct {
	Client string
	Status SerenaOwnedRestoreStatus
	Err    error
}

// SerenaOwnedRestoreRequest is one row-owned front-reconcile inverse. It is
// deliberately independent of MigrateReport: version-3 Rows are the persisted
// authority, and BaselinePresent selects the exact restore-vs-remove inverse.
type SerenaOwnedRestoreRequest struct {
	Client                     string
	BaselineBytes              []byte
	ExpectedAppliedFingerprint string
	BaselinePresent            bool
}

// ErrSerenaReconcileGUINotLive is the fail-closed sentinel returned when the
// GUI pidport is absent/stale or the readiness ping fails. Callers must NOT
// write any client entry when this fires — a guessed/spoofed router URL must
// never reach a client config (security: the loopback-only address + the
// readiness ping bound the URL to a live, local GUI).
var ErrSerenaReconcileGUINotLive = errors.New("serena client-reconcile: GUI not live (start `mcphub gui` first); refusing to write a guessed router URL")

// ErrSerenaReconcileRouteNotLive is the Port-path counterpart of
// ErrSerenaReconcileGUINotLive: returned when SerenaReconcileOpts.Port is
// set but the readiness ping against it fails. Kept as a distinct sentinel
// (rather than reusing ErrSerenaReconcileGUINotLive) because its message
// names the correct remediation — `mcphub supervise`, not `mcphub gui` — for
// the mcp-front reconcile command's own fail-closed gate.
//
// The remediation names ONLY `mcphub supervise` on purpose. This is a
// LIVENESS sentinel, and liveness alone is not sufficient for the cutover: a
// hand-started `mcphub route` satisfies this probe while
// AssertMCPFrontPortSupervisorOwned still (correctly) refuses the run,
// because nothing would restart it. Advertising it here would send the
// operator to a remedy that cannot make the command succeed.
var ErrSerenaReconcileRouteNotLive = errors.New("serena client-reconcile: mcp front route daemon not live (start `mcphub supervise`, or enable autostart, so the supervisor spawns its built-in route daemon); refusing to write a guessed router URL")

// ReconcileSerenaClientsToRouter rewrites each in-scope client's serena MCP
// entry to the constant /serena/mcp router URL on the live GUI port, then
// (optionally) removes the legacy localhost:9121 entry — but ONLY after the
// router rewrite for that client has succeeded.
//
// GUI-port discovery is live-pidport + readiness ping, fail-closed: it reads
// the actual bound port from the pidport file (written only AFTER the
// listener is up — the persisted setting is WRONG for --port 0 / explicit
// flag launches), pings it, and on absent/stale pidport OR ping failure
// returns ErrSerenaReconcileGUINotLive with NO client writes.
//
// The result is a MigrateReport: Applied rows are actual (or dry-run intended)
// router rewrites; Failed rows carry per-client lifecycle errors. Ordinary add
// failures leave that client on its still-functional legacy endpoint. When an
// add/remove applied but lock release was unconfirmed, both rows are present
// and the in-process rollback owner skips only that unsafe client leaf. The
// returned error is non-nil only for a whole-run blocker (GUI not live).
//
// This function does NOT restart the supervisor, does NOT touch the disk
// manifest, and does NOT flow through the G4 hub resolver (claim #8). It is
// inert until Phase 4 wires it into the migrate command.
func ReconcileSerenaClientsToRouter(ctx context.Context, opts SerenaReconcileOpts) (*MigrateReport, error) {
	request := StableGUICompatibility()
	if opts.RoutingTarget != nil {
		request = ExactTarget(*opts.RoutingTarget)
	}
	var report *MigrateReport
	err := NewAPI().WithClientRoutingAuthorityLease(ctx, request, func(canonical ClientRoutingTarget) error {
		if opts.RoutingTarget != nil {
			opts.RoutingTarget = &canonical
		}
		var reconcileErr error
		report, reconcileErr = reconcileSerenaClientsToRouter(ctx, opts)
		return reconcileErr
	})
	return report, err
}

// ReconcileSerenaClientsToRouterForMCPFrontTransaction is the explicit
// transaction lane used only after durable state has entered front-preparing.
// Ordinary callers use ReconcileSerenaClientsToRouter and cannot bypass the
// routing lease with a pre-frozen target.
func ReconcileSerenaClientsToRouterForMCPFrontTransaction(ctx context.Context, opts SerenaReconcileOpts) (*MigrateReport, error) {
	if opts.RoutingTarget == nil {
		return nil, &MCPFrontTargetInvalidError{Detail: "front transaction Serena reconcile requires a frozen routing target"}
	}
	return reconcileSerenaClientsToRouter(ctx, opts)
}

func reconcileSerenaClientsToRouter(ctx context.Context, opts SerenaReconcileOpts) (*MigrateReport, error) {
	report := &MigrateReport{}

	// 1. Resolve + prove the client-URL port live (fail-closed). This MUST
	//    happen before any client write so a stale/absent pidport, an unproven
	//    Port override, or a dead listener never results in a guessed URL
	//    being persisted.
	port, err := resolveSerenaReconcilePort(ctx, opts)
	if err != nil {
		return nil, err
	}

	routerURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, SerenaRouterURLPath)

	allClients := opts.Clients
	if allClients == nil {
		allClients = clients.AllClients()
	}
	classify := opts.classifyClientMutation
	if classify == nil {
		classify = clients.ClassifyClientMutation
	}

	// 2. Antigravity's relay `command` field needs the canonical installed
	//    mcphub path. Resolve once. A failure here is NOT fatal to the whole
	//    run — only Antigravity needs it, and its per-client attempt will
	//    surface the error.
	relayExePath := opts.McphubExePath
	if relayExePath == "" {
		if canonical, cerr := canonicalMcphubPath(); cerr == nil {
			relayExePath = canonical
		}
	}

	for _, clientName := range inScopeReconcileClients(opts.ClientsInclude) {
		adapter := allClients[clientName]
		if adapter == nil {
			// No adapter constructed on this host (e.g. UserHomeDir
			// failed). Silently skip — a Failed row would add noise
			// without a repairable cause, mirroring MigrateFrom.
			continue
		}

		entry := clients.MCPEntry{
			Name: serenaEntryName,
			URL:  routerURL,
			// Relay fields are consumed only by the Antigravity adapter;
			// URL adapters ignore them. RelayURL routes the relay at the
			// router via its --url escape hatch (the /serena/mcp router has
			// no per-daemon manifest port for the --server/--daemon form).
			RelayServer:  serenaEntryName,
			RelayDaemon:  "claude",
			RelayExePath: relayExePath,
			RelayURL:     routerURL,
		}

		if opts.DryRun {
			report.Applied = append(report.Applied, AppliedMigration{
				Server: serenaEntryName, Client: clientName, URL: routerURL,
			})
			continue
		}

		if !adapter.Exists() {
			// Client not installed on this machine — nothing to reconcile.
			// Skip quietly (mirrors MigrateFrom / Install).
			continue
		}

		preFingerprint, preErr := serenaEntryFingerprint(adapter, serenaEntryName)
		if preErr != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName, Err: preErr.Error(),
			})
			continue
		}
		// Fingerprint the adapter's actual persisted/readback projection, not
		// the generic write DTO — see intendedEntryReadbackProjection, the
		// shared owner of that rule.
		intendedEntry := intendedEntryReadbackProjection(adapter, entry)
		intendedFingerprint, intendedErr := fingerprintSerenaEntry(&intendedEntry)
		if intendedErr != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName, Err: intendedErr.Error(),
			})
			continue
		}

		mutator, ok := adapter.(clients.ConditionalEntryMutator)
		if !ok {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName,
				Err: "adapter lacks conditional entry mutation capability",
			})
			continue
		}
		var mutationReleaseErr error
		var prepareCallbackErr error
		observed := mutator.ConditionalEntryMutation(clients.ConditionalEntryMutationRequest{
			EntryName: serenaEntryName,
			ExpectedLive: func(live *clients.MCPEntry) bool {
				fingerprint, err := fingerprintSerenaEntry(live)
				return err == nil && fingerprint == preFingerprint
			},
			BackupKeepN: &opts.BackupKeepN,
			Operation:   clients.EntryMutationAdd,
			Entry:       entry,
			BeforeMutation: func(preparation clients.EntryMutationPreparation) error {
				exactPre, fingerprintErr := fingerprintSerenaEntry(preparation.Before)
				if fingerprintErr != nil {
					return fingerprintErr
				}
				if opts.OnBackupCaptured != nil {
					if callbackErr := opts.OnBackupCaptured(clientName, preparation.BackupPath); callbackErr != nil {
						prepareCallbackErr = callbackErr
						return callbackErr
					}
				}
				if opts.OnAttemptPrepared != nil {
					prepareCallbackErr = opts.OnAttemptPrepared(SerenaReconcileAttemptResult{
						Client:              clientName,
						BackupPath:          preparation.BackupPath,
						PreFingerprint:      exactPre,
						IntendedFingerprint: intendedFingerprint,
					})
					return prepareCallbackErr
				}
				return nil
			},
		})
		observedFingerprint := ""
		if observed.ObservationErr == nil {
			var fingerprintErr error
			observedFingerprint, fingerprintErr = fingerprintSerenaEntry(observed.After)
			if fingerprintErr != nil {
				observed.ObservationErr = fingerprintErr
			}
		}
		if opts.OnAttemptFinished != nil && prepareCallbackErr == nil {
			finishErr := opts.OnAttemptFinished(SerenaReconcileAttemptResult{
				Client:               clientName,
				BackupPath:           observed.BackupPath,
				PreFingerprint:       preFingerprint,
				IntendedFingerprint:  intendedFingerprint,
				ObservedFingerprint:  observedFingerprint,
				Invoked:              observed.Invoked,
				PreconditionConflict: observed.PreconditionConflict,
				PreparationErr:       observed.PreparationErr,
				AdapterErr:           observed.MutationErr,
				ObservationErr:       observed.ObservationErr,
			})
			if finishErr != nil {
				report.Failed = append(report.Failed, FailedMigration{
					Server: serenaEntryName, Client: clientName,
					Err: fmt.Sprintf("persist post-attempt recovery result: %v", finishErr),
				})
				return report, fmt.Errorf("serena client-reconcile: post-attempt result for %s was not durable: %w", clientName, finishErr)
			}
		}
		if prepareCallbackErr != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName,
				Err: fmt.Sprintf("prepare recovery attempt: %v (this client's config was NOT modified)", prepareCallbackErr),
			})
			return report, fmt.Errorf("serena client-reconcile: attempt for %s was not durable: %w", clientName, prepareCallbackErr)
		}
		if observed.PreconditionConflict {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName,
				Err: clients.ErrEntryMutationPreconditionConflict.Error(),
			})
			continue
		}
		if observed.PreparationErr != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName, Err: observed.PreparationErr.Error(),
			})
			continue
		}
		if observed.MutationErr != nil {
			if classify(observed.MutationErr) != clients.ClientMutationAppliedReleaseUnconfirmed {
				report.Failed = append(report.Failed, FailedMigration{
					Server: serenaEntryName, Client: clientName, Err: observed.MutationErr.Error(),
				})
				continue
			}
			mutationReleaseErr = observed.MutationErr
		}
		if observed.ObservationErr != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName,
				Err: fmt.Sprintf("post-write readback: %v", observed.ObservationErr),
			})
			if opts.OnAttemptFinished != nil {
				return report, fmt.Errorf("serena client-reconcile: post-write state for %s is unreadable: %w", clientName, observed.ObservationErr)
			}
			continue
		}

		// 4. Record the managed-entries marker so a later demigrate can tell
		//    a mcphub-installed entry from an operator-owned one (demigrate
		//    symmetry — same RecordManagedEntry discipline MigrateFrom uses
		//    at migrate.go:175). Best-effort: a marker-write failure must NOT
		//    roll back the successful router rewrite (the operator's config is
		//    the load-bearing artifact; the marker is observability). The row
		//    is still Applied; the marker error is a soft warning.
		if recErr := RecordManagedEntry(clientName, serenaEntryName); recErr != nil {
			_ = LogHubMcpEvent("warn", "managed-entries-record-failed", map[string]any{
				"server": serenaEntryName,
				"client": clientName,
				"err":    recErr.Error(),
				"note":   "serena router-reconcile demigrate fallback for this entry will fail-closed until the marker is repopulated",
			})
		}

		applied := AppliedMigration{
			Server: serenaEntryName, Client: clientName, URL: routerURL, BackupPath: observed.BackupPath,
		}
		if mutationReleaseErr != nil {
			applied.restoreUnsafe = true
			report.Applied = append(report.Applied, applied)
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName,
				Err: fmt.Sprintf("router rewrite applied; lock release unconfirmed: %v", mutationReleaseErr),
			})
			continue
		}
		report.Applied = append(report.Applied, applied)
	}

	return report, nil
}

// intendedEntryReadbackProjection reduces a generic WRITE-REQUEST MCPEntry to
// the subset the given adapter actually persists, i.e. the shape a subsequent
// GetEntry will return. It is the SINGLE OWNER of that rule for every surface
// that compares an intended post-state against a readback.
//
// A write request is a command object, not a state: it deliberately carries
// BOTH URL and relay fields so each adapter can pick its own shape. A
// URL-native adapter stores `url` and reads RelayURL back empty; a relay-stdio
// adapter stores `relay --url <target>` in args and reads URL back empty.
// Comparing the raw write request against the readback therefore makes every
// SUCCESSFUL write look like an unknown third state, which downstream owners
// report as a conflict (serena: an unknown fingerprint; the mcp-front v3
// journal: `forward-ownership-unknown`).
//
// Both consumers must use this one owner — the serena forward reconcile
// (fingerprint of the intended entry) and the LSP router plan's IntendedState
// snapshot. Re-typing the family rule at either call site is the defect this
// owner exists to prevent; the round-trip shape it predicts is pinned for both
// families by TestLSPRouterPlan_IntendedStateMatchesAdapterReadback.
func intendedEntryReadbackProjection(adapter clients.Client, entry clients.MCPEntry) clients.MCPEntry {
	if adapter.IsRelayStdio() {
		return clients.MCPEntry{
			Name:         entry.Name,
			RelayExePath: entry.RelayExePath,
			RelayURL:     entry.RelayURL,
		}
	}
	return clients.MCPEntry{
		Name:    entry.Name,
		URL:     entry.URL,
		Headers: entry.Headers,
	}
}

// RestoreSerenaReconcileApplied undoes a partially-successful
// ReconcileSerenaClientsToRouter run by restoring each safely-reacquirable
// Applied client's serena entry from the per-client backup captured immediately
// before its rewrite. An Applied row marked with unconfirmed lock release is
// reported and skipped so rollback never reacquires that poisoned leaf. It is
// the outer-rollback compensator the serena migrate
// driver runs when the reconcile reports per-client failures (report.Failed
// non-empty): the migrate must NOT proceed to the irreversible supervisor reap
// while only SOME clients point at the router, so the ones that succeeded are
// reverted to their pre-rewrite (legacy) entry and the whole run is aborted.
//
// allClients is the {name -> adapter} map to restore against (the same
// surface the reconcile rewrote); nil → clients.AllClients(). Restore is
// best-effort per client: a client whose adapter is missing, whose backup
// path was not recorded (dry-run / empty), or whose restore errors is
// collected into the returned joined error, but every other client is still
// attempted so one failure does not strand the rest on the router.
//
// CRITICAL — this restore uses RestoreEntryFromBackupForRollback, NOT the
// plain RestoreEntryFromBackup. The per-client backup captured before the
// reconcile rewrite is the client's PRE-RECONCILE state, which for a normal
// pre-cutover serena client IS the legacy hub entry (loopback
// http://localhost:9121/mcp for URL clients, or the `mcphub relay` form for
// Antigravity). RestoreEntryFromBackup defends the demigrate flow by
// REFUSING to write a hub-managed-shaped backup entry
// (ErrBackupEntryAlreadyMigrated) — which would make this abort-rollback
// FAIL and strand the already-rewritten clients on /serena/mcp even though
// the migration aborted (no dynamic-pool intent, no daemons). The rollback
// variant bypasses that guard to put the exact pre-reconcile bytes back; the
// demigrate guard stays in force for the normal demigrate flow.
func RestoreSerenaReconcileApplied(report *MigrateReport, allClients map[string]clients.Client) error {
	return restoreSerenaReconcileAppliedWithClassifier(report, allClients, clients.ClassifyClientMutation)
}

// RestoreSerenaReconcileAppliedOwned is the persisted front-reconcile inverse.
// Unlike the synchronous migrate compensator above, each row supplies the
// exact fingerprint written by the forward generation. The compare and restore
// occur under the adapter's existing config lock through CASEntryMutator.
func RestoreSerenaReconcileAppliedOwned(
	requests []SerenaOwnedRestoreRequest,
	allClients map[string]clients.Client,
) ([]SerenaOwnedRestoreResult, error) {
	if allClients == nil {
		allClients = clients.AllClients()
	}
	var errs []error
	var results []SerenaOwnedRestoreResult
	for _, request := range requests {
		adapter := allClients[request.Client]
		if adapter == nil {
			err := fmt.Errorf("restore serena/%s: no adapter on this host", request.Client)
			errs = append(errs, err)
			results = append(results, SerenaOwnedRestoreResult{Client: request.Client, Status: SerenaOwnedRestoreFailed, Err: err})
			continue
		}
		if !adapter.Exists() {
			err := fmt.Errorf("restore serena/%s: client config is unavailable; retry when the client is installed", request.Client)
			errs = append(errs, err)
			results = append(results, SerenaOwnedRestoreResult{Client: request.Client, Status: SerenaOwnedRestoreFailed, Err: err})
			continue
		}
		if request.ExpectedAppliedFingerprint == "" {
			err := fmt.Errorf("restore serena/%s: missing expected applied fingerprint", request.Client)
			errs = append(errs, err)
			results = append(results, SerenaOwnedRestoreResult{Client: request.Client, Status: SerenaOwnedRestoreFailed, Err: err})
			continue
		}
		mutator, ok := clients.AsCASEntryMutator(adapter)
		if !ok {
			err := fmt.Errorf("restore serena/%s: adapter lacks rollback CAS capability", request.Client)
			errs = append(errs, err)
			results = append(results, SerenaOwnedRestoreResult{Client: request.Client, Status: SerenaOwnedRestoreFailed, Err: err})
			continue
		}
		match := func(live *clients.MCPEntry) bool {
			sum, err := fingerprintSerenaEntry(live)
			return err == nil && sum == request.ExpectedAppliedFingerprint
		}
		var inverseErr error
		if request.BaselinePresent {
			if request.BaselineBytes == nil {
				inverseErr = errors.New("missing verified baseline bytes")
			} else {
				inverseErr = mutator.CASRestoreEntryFromBytesForRollback(serenaEntryName, match, request.BaselineBytes)
			}
		} else {
			inverseErr = mutator.CASGuardedRemoveEntry(serenaEntryName, match)
		}
		if inverseErr != nil {
			if errors.Is(inverseErr, clients.ErrCASConflict) {
				results = append(results, SerenaOwnedRestoreResult{Client: request.Client, Status: SerenaOwnedRestoreConflict, Err: inverseErr})
				continue
			}
			err := fmt.Errorf("restore serena/%s: %w", request.Client, inverseErr)
			errs = append(errs, err)
			results = append(results, SerenaOwnedRestoreResult{Client: request.Client, Status: SerenaOwnedRestoreFailed, Err: err})
			continue
		}
		results = append(results, SerenaOwnedRestoreResult{Client: request.Client, Status: SerenaOwnedRestoreRestored})
	}
	return results, errors.Join(errs...)
}

func restoreSerenaReconcileAppliedWithClassifier(
	report *MigrateReport,
	allClients map[string]clients.Client,
	classify func(error) clients.ClientMutationSettlement,
) error {
	if report == nil {
		return nil
	}
	if allClients == nil {
		allClients = clients.AllClients()
	}
	if classify == nil {
		classify = clients.ClassifyClientMutation
	}
	var errs []error
	for _, app := range report.Applied {
		if app.BackupPath == "" {
			// No snapshot to restore from (dry-run row, or a producer that
			// does not capture a backup). Nothing to undo for this client.
			continue
		}
		if app.restoreUnsafe {
			errs = append(errs, fmt.Errorf("restore %s/%s skipped: prior mutation was applied but config-lock release was unconfirmed", app.Server, app.Client))
			continue
		}
		adapter := allClients[app.Client]
		if adapter == nil {
			err := fmt.Errorf("restore %s/%s: no adapter on this host", app.Server, app.Client)
			errs = append(errs, err)
			continue
		}
		if rerr := adapter.RestoreEntryFromBackupForRollback(app.BackupPath, serenaEntryName); rerr != nil {
			if classify(rerr) == clients.ClientMutationAppliedReleaseUnconfirmed {
				errs = append(errs, fmt.Errorf("restore %s/%s from %s applied; lock release unconfirmed: %w", app.Server, app.Client, app.BackupPath, rerr))
				continue
			}
			errs = append(errs, fmt.Errorf("restore %s/%s from %s: %w", app.Server, app.Client, app.BackupPath, rerr))
		}
	}
	return errors.Join(errs...)
}

// SerenaClientEntryFingerprint returns a stable content hash of ONE client's
// live `serena` entry, for use as a divergence baseline across two separate
// command invocations.
//
// WHY (codex bot PR #588). RestoreSerenaReconcileApplied overwrites the live
// entry from the recorded backup unconditionally. A recovery record can stay
// active indefinitely — the forward run and its `--rollback` are separate
// operator actions with no bound between them — so by rollback time the
// operator may have edited that entry themselves (repointed it at a remote
// serena, added headers, disabled it). Overwriting it silently discards their
// work with no warning and no way back. The LSP side of this same command
// already refuses that (RestoreLSPRouterClientEntriesSnapshot's ownership and
// exact-match guards); the serena side had no equivalent, and this is the
// primitive that gives it one: record the fingerprint of what the forward run
// LEFT, re-compute it before restoring, and refuse when they differ.
//
// Returns ("", nil) when the entry is ABSENT — a real, distinguishable state
// (the operator deleted it), never conflated with a hash.
//
// The hash covers the adapter's whole projected entry, not just its URL, so a
// changed header / env / relay shape is caught too. encoding/json emits map
// keys in sorted order, which is what makes this stable across processes.
func SerenaClientEntryFingerprint(clientName string, allClients map[string]clients.Client) (string, error) {
	if allClients == nil {
		allClients = clients.AllClients()
	}
	adapter := allClients[clientName]
	if adapter == nil {
		return "", fmt.Errorf("serena entry fingerprint %s: no adapter on this host", clientName)
	}
	if !adapter.Exists() {
		return "", nil
	}
	return serenaEntryFingerprint(adapter, serenaEntryName)
}

func serenaEntryFingerprint(adapter clients.Client, entryName string) (string, error) {
	entry, err := adapter.GetEntry(entryName)
	if err != nil {
		return "", err
	}
	if entry == nil {
		return "", nil
	}
	return fingerprintSerenaEntry(entry)
}

func fingerprintSerenaEntry(entry *clients.MCPEntry) (string, error) {
	if entry == nil {
		return "", nil
	}
	raw, merr := json.Marshal(entry)
	if merr != nil {
		return "", merr
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// inScopeReconcileClients returns the client names to reconcile: the full
// serenaReconcileClientSet() unless include narrows it. Narrowing preserves
// the canonical order and drops names not in the set (so a caller cannot
// widen the surface past the legacy serena bindings).
func inScopeReconcileClients(include []string) []string {
	full := serenaReconcileClientSet()
	if len(include) == 0 {
		return full
	}
	want := make(map[string]bool, len(include))
	for _, c := range include {
		want[c] = true
	}
	out := make([]string, 0, len(full))
	for _, c := range full {
		if want[c] {
			out = append(out, c)
		}
	}
	return out
}

// resolveSerenaReconcilePort resolves the client-URL port for this
// reconcile run. opts.Port>0 takes the direct path (used by the mcp-front
// reconcile command): the port is already known (a settings value, not
// discovered), so it is proven live via the SAME readiness ping
// discoverLiveGUIPort uses (opts.Ping, default defaultRouterReadinessPing)
// before being trusted — a bare "the setting says 9137" is never enough to
// write a client config, only "9137 answered the MCP initialize lifecycle
// like the real router" is. opts.Port==0 (every existing caller) is
// unchanged: pidport-based GUI discovery.
func resolveSerenaReconcilePort(ctx context.Context, opts SerenaReconcileOpts) (int, error) {
	if opts.RoutingTarget != nil {
		if err := ValidateClientRoutingTarget(*opts.RoutingTarget); err != nil {
			return 0, err
		}
		switch opts.RoutingTarget.Mode {
		case MCPFrontRoutingTargetFront:
			return proveSerenaDirectPortLive(ctx, opts, opts.RoutingTarget.Port)
		case MCPFrontRoutingTargetGUI:
			port, err := discoverLiveGUIPort(ctx, opts)
			if err != nil {
				return 0, err
			}
			if port != opts.RoutingTarget.Port {
				return 0, fmt.Errorf("%w: pidport resolved port %d but durable GUI target is %d", ErrSerenaReconcileGUINotLive, port, opts.RoutingTarget.Port)
			}
			return port, nil
		}
	}
	if opts.Port > 0 {
		return proveSerenaDirectPortLive(ctx, opts, opts.Port)
	}
	return discoverLiveGUIPort(ctx, opts)
}

func proveSerenaDirectPortLive(ctx context.Context, opts SerenaReconcileOpts, port int) (int, error) {
	ping := opts.Ping
	if ping == nil {
		ping = defaultRouterReadinessPing
	}
	if perr := ping(ctx, port); perr != nil {
		return 0, fmt.Errorf("%w: port %d did not answer the readiness ping: %v", ErrSerenaReconcileRouteNotLive, port, perr)
	}
	return port, nil
}

// discoverLiveGUIPort reads the GUI pidport, confirms the GUI is live via a
// readiness ping, and returns the bound port. Fail-closed: an empty
// PidportPath, an unreadable/stale pidport, an out-of-range port, or a ping
// failure all return ErrSerenaReconcileGUINotLive (wrapped) so callers never
// write a guessed URL.
func discoverLiveGUIPort(ctx context.Context, opts SerenaReconcileOpts) (int, error) {
	if opts.PidportPath == "" {
		return 0, fmt.Errorf("%w: no pidport path provided", ErrSerenaReconcileGUINotLive)
	}
	readPidport := opts.ReadPidport
	if readPidport == nil {
		readPidport = readPidportFn
	}
	pid, port, err := readPidport(opts.PidportPath)
	if err != nil {
		return 0, fmt.Errorf("%w: read pidport %s: %v", ErrSerenaReconcileGUINotLive, opts.PidportPath, err)
	}
	// A 0 port is the well-known "auto-assign placeholder" the GUI writes
	// BEFORE its listener binds (internal/cli/gui.go) — it is never a usable
	// router port. Out-of-range values are corrupt metadata. Either way the
	// GUI is not yet serving a real port: fail closed.
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%w: pidport %s has no usable bound port (%d) — the GUI listener is not up", ErrSerenaReconcileGUINotLive, opts.PidportPath, port)
	}
	verifyIdentity := opts.VerifyIdentity
	if verifyIdentity == nil {
		verifyIdentity = defaultGUIPidportIdentityCheck
	}
	if ierr := verifyIdentity(ctx, pid, port); ierr != nil {
		return 0, fmt.Errorf("%w: pidport %s identity check failed for PID %d on port %d: %v", ErrSerenaReconcileGUINotLive, opts.PidportPath, pid, port, ierr)
	}
	ping := opts.Ping
	if ping == nil {
		ping = defaultRouterReadinessPing
	}
	if perr := ping(ctx, port); perr != nil {
		return 0, fmt.Errorf("%w: GUI on port %d did not answer the readiness ping: %v", ErrSerenaReconcileGUINotLive, port, perr)
	}
	return port, nil
}

// loopbackPortOwnerFn is the test seam for the OS-level port→owner-PID
// resolution. Production: loopbackPortOwnerPID (Windows: `netstat -ano` scan
// for the 127.0.0.1:<port> LISTENING owner; POSIX: fail-closed stub). Tests
// override it to inject a synthetic owner without a real listener, mirroring
// the file's existing readPidportFn / Ping / VerifyIdentity seam style.
var loopbackPortOwnerFn = loopbackPortOwnerPID

// guiImageForPIDFn is the test seam for the owner-PID → image-basename
// resolution. Production: guiImageForPID (Windows: procNameAndParent via
// wmic/PowerShell; POSIX: fail-closed stub). Tests override it to assert the
// foreign-image rejection without a real process table.
var guiImageForPIDFn = guiImageForPID

// defaultGUIPidportIdentityCheck PROVES, at the OS level, that the recorded
// pidport PID is the process that actually owns the loopback router socket —
// it does NOT trust any value the listener self-reports over HTTP.
//
// Why the self-reported path was insufficient (bot PR #252 P1): the GUI
// pidport file is world-readable and intentionally left on disk after the GUI
// exits. A local attacker that binds the stale loopback port can read the
// pidport file and echo its PID back from /api/ping, while processAlive(pid)
// only proves SOME process with that PID exists — not that it owns the socket.
// The HTTP-reported PID is therefore forgeable. The fix replaces it with an
// unforgeable OS binding:
//
//  1. processAlive(pid) — cheap pre-check: fail fast if the recorded PID is
//     already dead (the common stale-pidport case) before shelling out.
//  2. loopbackPortOwnerFn(port) — ask the OS who owns the 127.0.0.1:<port>
//     LISTENING socket. Require ownerPID == the recorded pidport pid. A
//     mismatch means a DIFFERENT process holds the port (stale GUI gone, port
//     reused by an attacker) → fail closed.
//  3. guiImageForPIDFn(ownerPID) — require the owning process's image basename
//     to be the mcphub binary (clients.IsMcphubBinary). A foreign image owning
//     the port → fail closed.
//
// The separate router-readiness probe (opts.Ping / defaultRouterReadinessPing)
// still runs AFTER this proof to confirm the router is actually serving; it is
// no longer the identity authority. Every failure path here is fail-closed:
// any uncertainty refuses the reconcile rather than trusting a guess.
func defaultGUIPidportIdentityCheck(ctx context.Context, pid, port int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pidport pid %d", pid)
	}
	// (1) Cheap liveness pre-check — fail fast on a dead recorded PID.
	alive, err := processAlive(pid)
	if err != nil {
		return fmt.Errorf("check recorded process liveness: %w", err)
	}
	if !alive {
		return fmt.Errorf("recorded process is not alive")
	}
	// (2) OS-level port-owner proof. The OWNER of the loopback LISTENING
	//     socket must be the recorded PID — not a value the listener reports
	//     about itself.
	ownerPID, ok, err := loopbackPortOwnerFn(port)
	if err != nil {
		// netstat failure OR the POSIX fail-closed stub. Refuse — never fall
		// back to a self-reported PID.
		return fmt.Errorf("resolve OS owner of loopback port %d: %w", port, err)
	}
	if !ok || ownerPID <= 0 {
		return fmt.Errorf("no process owns loopback LISTENING port %d (pidport is stale or the GUI listener is down)", port)
	}
	if ownerPID != pid {
		return fmt.Errorf("loopback port %d is owned by PID %d, not the recorded pidport PID %d (stale pidport / port reused by another process)", port, ownerPID, pid)
	}
	// (3) The owning process's image must be the mcphub binary. A foreign
	//     image holding the port fails closed even if it somehow matched the
	//     recorded PID number.
	image, ok := guiImageForPIDFn(ownerPID)
	if !ok {
		return fmt.Errorf("could not resolve the image of port-%d owner PID %d (OS-level identity proof unavailable)", port, ownerPID)
	}
	if !clients.IsMcphubBinary(image) {
		return fmt.Errorf("loopback port %d owner PID %d image %q is not the mcphub GUI binary (foreign process owns the router port)", port, ownerPID, image)
	}
	return nil
}

// defaultRouterReadinessPing confirms a live local GUI is serving on port by
// issuing a loopback HEAD request to the /serena/mcp router path. Any
// transport error fails closed. The router answers a HEAD on the same-origin
// path (a 4xx/5xx status still proves SOMETHING local is listening and
// serving that route); a connection-refused on a dead/stale port returns a
// transport error. This mirrors the G4 reconcile precedent of probing the
// bound port before rewriting client configs
// (internal/cli/install.go:348-374) and keeps the address loopback-only so a
// remote/spoofed endpoint can never satisfy it.
// ErrSerenaRouterRouteNotLive is the fail-closed sentinel for the exported
// serena route assertion below. It mirrors ErrLSPRouterRouteNotLive so a
// caller that gates on route liveness treats both surfaces alike.
var ErrSerenaRouterRouteNotLive = errors.New("the /serena/mcp router route is not live at mcp_front.port")

// AssertSerenaRouterRouteLive is the exported, side-effect-free form of the
// serena route proof (defaultRouterReadinessPing), so a caller can establish
// route liveness BEFORE any mutating call rather than relying on the probe
// embedded inside ReconcileSerenaClientsToRouter.
//
// That embedded probe stays exactly where it is — it is the serena reconcile's
// own guarantee and must not depend on callers remembering to pre-check. This
// is the second, explicit user of the same proof, for a caller that also
// rewrites OTHER surfaces and therefore cannot use one surface's internal gate
// as its whole-command gate (codex bot PR #588).
func AssertSerenaRouterRouteLive(ctx context.Context, port int) error {
	if port <= 0 {
		return fmt.Errorf("%w: refusing to probe non-positive port %d", ErrSerenaRouterRouteNotLive, port)
	}
	if err := defaultRouterReadinessPing(ctx, port); err != nil {
		return fmt.Errorf("%w: %w", ErrSerenaRouterRouteNotLive, err)
	}
	return nil
}

func defaultRouterReadinessPing(ctx context.Context, port int) error {
	// Verify this is actually the mcphub serena router, NOT just any local HTTP
	// server that happened to reuse a stale pidport's port (bot PR #248 P1). The
	// router answers a non-POST request (our HEAD) with 405 + `Allow: POST, DELETE`
	// (internal/gui/serena_router.go:479-488) — a signature a random reused-port
	// server would not produce. A 200/404/other status means something ELSE is
	// listening here; fail closed so the reconcile never rewrites client configs
	// to an unrelated service (the prior "any HTTP response = live GUI" check
	// broke the fail-closed discovery guarantee).
	//
	// The signature check itself lives in routerRouteShapeProbe
	// (lsp_router_readiness.go) because the LSP route assertion added in the
	// same round must apply the IDENTICAL definition of "this is an mcphub MCP
	// router"; two copies of that predicate would be free to drift.
	if err := routerRouteShapeProbe(ctx, port, SerenaRouterURLPath); err != nil {
		return fmt.Errorf("%w — the GUI may not be up, or the pidport is stale and the port was reused by another service", err)
	}
	// Step 2 (bot PR #248 P2): verify the router actually SERVES the MCP session
	// lifecycle before we point any client at it. The HEAD/405 check above only
	// proves the serena route exists; a real client's FIRST request is an MCP
	// `initialize` (no workspace path-arg), and the current router rejects any
	// POST without params.name — so a client pointed at it would fail at session
	// setup. This probe fails CLOSED until the router-completion phase synthesizes
	// the non-tool lifecycle, so the reconcile never rewrites a client to a router
	// that cannot complete `initialize`.
	return mcpInitializeProbe(ctx, port)
}

// mcpInitializeProbe POSTs a minimal MCP `initialize` request to the serena
// router and returns nil only if the router answers with a JSON-RPC RESULT
// (i.e. it serves the session lifecycle). A non-200 status, a JSON-RPC error, or
// a missing result means the router does not yet handle the non-tool lifecycle
// (the pre-router-completion state, where it only routes tool calls by
// params.name) → fail closed. Loopback-only; same 2s budget as the HEAD ping.
func mcpInitializeProbe(ctx context.Context, port int) error {
	// Shared owner — see routerInitializeLifecycleProbe (lsp_router_readiness.go)
	// for why the lifecycle assertion is not duplicated per route.
	if err := routerInitializeLifecycleProbe(ctx, port, SerenaRouterURLPath); err != nil {
		return fmt.Errorf("%w — the router must synthesize/handle the non-tool lifecycle before clients are pointed at it", err)
	}
	return nil
}
