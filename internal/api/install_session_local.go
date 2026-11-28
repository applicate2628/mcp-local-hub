package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"runtime"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// A session-local provider has no independent Supervisor descriptor or socket.
// This selection borrows the already received route generation. It confers no
// authority to create, replace or retire that shared backend.
type sessionLocalInstallTarget struct {
	Endpoint               HubEndpoint
	Epoch                  MCPFrontRoutingTargetSnapshot
	Route                  SupervisorDaemon
	Supervisor             SupervisorLockOwner
	Aggregate              RouteAggregateStatus
	Tokens                 HubTokenTable
	Providers              []HubStdioEndpoint
	Legacy                 []SupervisorDaemon
	legacyRetired          bool
	legacyRemovalCommitted bool
}

var ErrSessionLocalInstallUnavailable = errors.New("session-local install requires the qualified, live protected front; no shared-daemon fallback")

func selectedSessionLocalDaemons(m *config.ServerManifest, daemonFilter string) []HubStdioEndpoint {
	if m == nil {
		return nil
	}
	var endpoints []HubStdioEndpoint
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		if e := SessionLocalStdioEndpoint(m, &d); e.Configuration != "" {
			endpoints = append(endpoints, e)
		}
	}
	return endpoints
}

// resolveSessionLocalInstallTarget performs only received-owner/read-only
// checks. The provider protocol probe is deferred until apply, before writes.
func (a *API) resolveSessionLocalInstallTarget(ctx context.Context, m *config.ServerManifest, opts InstallOpts) (*sessionLocalInstallTarget, error) {
	providers := selectedSessionLocalDaemons(m, opts.DaemonFilter)
	if len(providers) == 0 {
		return nil, nil
	}
	if runtime.GOOS != "windows" || m.Kind != config.KindGlobal || m.WeeklyRefresh {
		return nil, ErrSessionLocalInstallUnavailable
	}
	for _, d := range m.Daemons {
		if (opts.DaemonFilter == "" || opts.DaemonFilter == d.Name) && d.MCPStdioState != config.MCPStdioStateSessionLocal {
			return nil, ErrSessionLocalInstallUnavailable
		}
	}
	epoch, err := a.MCPFrontRoutingTargetSnapshot()
	if err != nil || epoch.State != MCPFrontRoutingTargetFront {
		return nil, errors.Join(ErrSessionLocalInstallUnavailable, err)
	}
	root, err := DaemonStateDirReadOnly()
	if err != nil {
		return nil, err
	}
	intent, err := ReadSupervisorIntent(filepath.Join(root, supervisorIntentFileLeaf))
	if err != nil {
		return nil, err
	}
	route := findBuiltinRouteDescriptor(intent)
	if route == nil || route.Port != epoch.Port {
		return nil, ErrSessionLocalInstallUnavailable
	}
	legacy, err := sessionLocalLegacySelection(intent, providers)
	if err != nil {
		return nil, err
	}
	owner, err := ReadOccupiedSupervisorLockOwner(root)
	if err != nil {
		return nil, err
	}
	peer, err := ObserveSupervisorProcessIdentityV1(owner.PID)
	if err != nil {
		return nil, err
	}
	check := func(ctx context.Context) error {
		return VerifySupervisorControlGenerationForPeerFromStateDir(ctx, root, owner, peer)
	}
	status, err := QuerySupervisorAggregateStatus(ctx, root, owner, peer, check)
	if err != nil || !status.Valid() || status.Ownership != RouteAggregateOwned || status.Effective != "ready" {
		return nil, errors.Join(ErrSessionLocalInstallUnavailable, err)
	}
	ep, err := LoadHubEndpoint()
	if err != nil || ep.Port <= 0 || ep.InstanceID == "" {
		return nil, errors.Join(ErrSessionLocalInstallUnavailable, err)
	}
	tokens, err := ReloadHubTokens()
	if err != nil {
		return nil, err
	}
	target := &sessionLocalInstallTarget{Endpoint: ep, Epoch: epoch, Route: *route, Supervisor: owner, Aggregate: status, Tokens: tokens, Providers: providers, Legacy: legacy}
	for _, provider := range providers {
		if err := requestHubStdioAdmission(ctx, ep, HubStdioAdmissionRequest{Endpoint: provider}); err != nil {
			return nil, err
		}
	}
	return target, nil
}

func buildSessionLocalInstallPlan(m *config.ServerManifest, opts BuildPlanOpts) (*Plan, error) {
	target := opts.sessionLocal
	if target == nil || target.Epoch.State != MCPFrontRoutingTargetFront || target.Endpoint.Port <= 0 || target.Endpoint.InstanceID == "" {
		return nil, ErrSessionLocalInstallUnavailable
	}
	include, err := installClientPredicate(opts)
	if err != nil {
		return nil, err
	}
	p := &Plan{Server: m.Name, FullInstall: opts.DaemonFilter == "", sessionLocal: target}
	for _, binding := range m.ClientBindings {
		if (opts.DaemonFilter != "" && opts.DaemonFilter != binding.Daemon) || !include(binding.Client) {
			continue
		}
		d, found := findDaemon(m, binding.Daemon)
		if !found || d.MCPStdioState != config.MCPStdioStateSessionLocal || clients.IsRelayStdio(binding.Client) || binding.Client == "openhands" {
			return nil, ErrSessionLocalInstallUnavailable
		}
		path, err := clientConfigPath(binding.Client)
		if err != nil {
			return nil, err
		}
		token := target.Tokens.Tokens[binding.Client]
		if !isLowerHex64(token) {
			return nil, ErrSessionLocalInstallUnavailable
		}
		url := clients.HubLoopbackURL(target.Endpoint.Port, HubClientPrefix+binding.Client+"/mcp")
		if opts.ProjectClientCreate != nil {
			group := opts.ProjectClientCreate.Group
			if validateGroupName(group) != nil {
				return nil, errHubProjectAuthority
			}
			url = clients.HubLoopbackURL(target.Endpoint.Port, HubGroupPrefix+group+"/mcp")
			token = projectCredentialPlanMarker
		}
		p.ClientUpdates = append(p.ClientUpdates, ClientUpdatePlan{Client: binding.Client, Path: path, Action: ClientUpdateAddReplace,
			EntryName: m.Name, DaemonName: binding.Daemon, URL: url, ToolTimeoutSec: binding.ToolTimeoutSec,
			Headers: map[string]string{"X-Mcphub-Hub-Token": token, "X-Mcphub-Instance-Id": target.Endpoint.InstanceID}})
	}
	p.Notices = append(p.Notices, "session-local stdio: reuse the protected front; launch one provider per authenticated front session")
	return freezeCodexInstallTargetNames(p, opts)
}

func (a *API) verifySessionLocalInstallTarget(ctx context.Context, m *config.ServerManifest, plan *Plan, opts InstallOpts, probe bool) error {
	if plan == nil || plan.sessionLocal == nil || !plan.supervisorIntentHashesBound {
		return ErrSessionLocalInstallUnavailable
	}
	current, err := a.resolveSessionLocalInstallTarget(ctx, m, opts)
	if err != nil {
		return err
	}
	prior := plan.sessionLocal
	if current == nil || current.Endpoint != prior.Endpoint || current.Epoch != prior.Epoch || current.Supervisor != prior.Supervisor ||
		!reflect.DeepEqual(current.Aggregate, prior.Aggregate) || !reflect.DeepEqual(current.Route, prior.Route) || !reflect.DeepEqual(current.Providers, prior.Providers) {
		return ErrSessionLocalInstallUnavailable
	}
	if prior.legacyRetired {
		if len(current.Legacy) != 0 {
			return ErrSessionLocalInstallUnavailable
		}
	} else if !reflect.DeepEqual(current.Legacy, prior.Legacy) {
		return ErrSessionLocalInstallUnavailable
	}
	for _, update := range plan.ClientUpdates {
		if opts.ProjectClientCreate == nil && current.Tokens.Tokens[update.Client] != update.Headers["X-Mcphub-Hub-Token"] {
			return ErrSessionLocalInstallUnavailable
		}
	}
	if probe {
		if len(prior.Legacy) != 0 && !prior.legacyRetired {
			return ErrSessionLocalInstallUnavailable
		}
		for _, provider := range prior.Providers {
			// The registered scope must already declare this provider. No new
			// membership or credential is minted to perform admission.
			scope := ""
			for _, binding := range m.ClientBindings {
				if binding.Daemon == provider.Daemon && isLowerHex64(current.Tokens.Tokens[binding.Client]) {
					scope = binding.Client
					break
				}
			}
			if scope == "" {
				return ErrSessionLocalInstallUnavailable
			}
			if err := requestHubStdioAdmission(ctx, prior.Endpoint, HubStdioAdmissionRequest{Endpoint: provider, Scope: scope, Probe: true}); err != nil {
				return err
			}
		}
	}
	return nil
}

// HubStdioAdmissionRequest is consumed only after the existing control-token
// gate. It selects an already prepared descriptor; it carries no launch input.
type HubStdioAdmissionRequest struct {
	Endpoint HubStdioEndpoint `json:"endpoint"`
	Scope    string           `json:"scope,omitempty"`
	Probe    bool             `json:"probe,omitempty"`
}

func requestHubStdioAdmission(ctx context.Context, ep HubEndpoint, request HubStdioAdmissionRequest) error {
	return requestHubControlReceived(ctx, ep, "stdio-admission-v1", request)
}

func requestHubControlReceived(ctx context.Context, ep HubEndpoint, operation string, request any) error {
	control, err := ReadHubMcpControlToken()
	if err != nil {
		return err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, clients.HubLoopbackURL(ep.Port, "/internal/reload-tokens"), bytes.NewReader(data))
	if err != nil {
		return err
	}
	r.Header.Set("X-Mcphub-Control-Token", control)
	r.Header.Set("X-Mcphub-Control-Operation", operation)
	r.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(r)
	if err != nil {
		return errors.Join(ErrSessionLocalInstallUnavailable, err)
	}
	_, drainErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.Join(ErrSessionLocalInstallUnavailable, drainErr, closeErr)
	}
	return errors.Join(drainErr, closeErr)
}

func (a *API) materializeSessionLocalInstall(ctx context.Context, m *config.ServerManifest, plan *Plan, filter string, dryRun bool, w io.Writer, consents []ResolvedSymlinkConsent) (resultErr error) {
	if dryRun {
		return printPlanTo(w, plan)
	}
	if len(plan.SupervisorIntent) != 0 || len(plan.SchedulerTasks) != 0 || plan.ownedBackendInstall != nil {
		return ErrSessionLocalInstallUnavailable
	}
	if err := a.retireSessionLocalLegacy(ctx, plan); err != nil {
		return err
	}
	release, err := AcquireHubMcpMutationContext(ctx)
	if err != nil {
		return err
	}
	defer ReleaseAndJoin(&resultErr, release, "release session-local install Hub selection")
	if err := a.verifySessionLocalInstallTarget(ctx, m, plan, InstallOpts{Server: m.Name, DaemonFilter: filter}, true); err != nil {
		return err
	}
	return a.installPlan(ctx, m, plan, installPlanOpts{Writer: w, DaemonFilter: filter,
		SkipSchedulerTasks: true, SkipSchedulerPrune: true, SymlinkConsents: consents})
}

// A selected descriptor is either still capturable from its original creator,
// or absent without any pending stop/tombstone. Absence with a stop watermark
// is not proof of original receiving, including after restart or ClearStop.
func sessionLocalLegacySelection(intent *SupervisorIntentFile, providers []HubStdioEndpoint) ([]SupervisorDaemon, error) {
	var selected []SupervisorDaemon
	for _, provider := range providers {
		task := canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(provider.Server, provider.Daemon))
		found := false
		for _, row := range intent.Daemons {
			if canonicalIntentTaskKey(row.TaskName) != task {
				continue
			}
			if found || row.Server != provider.Server || row.Daemon != provider.Daemon {
				return nil, ErrSessionLocalInstallUnavailable
			}
			selected, found = append(selected, row), true
		}
		if !found {
			_, stopped := intent.Stops[task]
			_, watermark := intent.LegacyStopWatermarks[task]
			if stopped || watermark {
				return nil, fmt.Errorf("%w: original shared-provider closure remains unreceived", ErrSessionLocalInstallUnavailable)
			}
		}
	}
	return selected, nil
}

// SessionLocalInstalledServers reuses the settled registration projection as
// the install signal. Gate-ON may select only a user-global config; a project
// attachment must not grant writes to a global config. Gate-OFF also considers
// project attachments so their private front cannot be silently discarded.
// The config-path identity is read by the existing snapshot owner. Its full
// content hash is deliberately not an install signal: reconciliation itself
// replaces per-provider entries with the aggregate entry.
func SessionLocalInstalledServers(ctx context.Context, globalOnly bool) (map[string]bool, error) {
	entries, err := readManagedEntries()
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool)
	var globalClients map[string]clients.Client
	for _, entry := range entries.Entries {
		if entry.Owner != "hub-supervisor" || !entry.Exists || entry.Transport != clients.ManagedRegistrationTransportHTTP || entry.IdentityState != clients.ManagedRegistrationIdentityManagedHTTP {
			continue
		}
		if globalOnly {
			if globalClients == nil {
				globalClients = clients.AllClients()
			}
			owner, ok := clients.AsManagedRegistrationClient(globalClients[entry.Client])
			if !ok {
				return nil, ErrSessionLocalInstallUnavailable
			}
			snapshot, err := owner.SnapshotManagedRegistration(ctx, entry.Server)
			if err != nil {
				return nil, err
			}
			if snapshot.ConfigPathSHA256 != entry.ConfigPathSHA256 {
				continue
			}
		}
		result[entry.Server] = true
	}
	return result, nil
}
