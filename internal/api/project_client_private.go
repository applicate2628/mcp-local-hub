package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

const projectPrivatePlanSchema = "project-client-private-plan-v3"
const projectPrivateCapsuleSchema = "project-client-private-v3"

// This is a binding on the existing project attachment transaction, not a
// second lifecycle owner. The same backup/capsule and original route receipt
// govern forward publication, rebind and rollback.
type projectPrivateAttachment struct {
	target     *sessionLocalInstallTarget
	enrollment hubProjectEnrollment
	token      string
}

type projectPrivateCapsule struct {
	Enrollment hubProjectEnrollment `json:"enrollment"`
	Endpoint   HubEndpoint          `json:"endpoint"`
	Provider   HubStdioEndpoint     `json:"provider"`
}

type projectPrivateBackendBinding struct {
	Server              string           `json:"server"`
	Provider            HubStdioEndpoint `json:"provider"`
	RouteManifestSHA256 string           `json:"route_manifest_sha256"`
}

func prepareProjectPrivateBinding(opts InstallOpts, plan *Plan, identity clients.CodexProjectIdentity) (*projectPrivateAttachment, string, error) {
	if plan.sessionLocal == nil {
		if opts.ProjectClientCreate.Group != "" {
			return nil, "", errHubProjectAuthority
		}
		hash, err := projectClientValueHash(plan.SupervisorIntent)
		return nil, hash, err
	}
	target := plan.sessionLocal
	if len(target.Providers) != 1 || opts.ProjectClientCreate.Group == "" || target.Route.TaskName != BuiltinRouteTaskName {
		return nil, "", errHubProjectAuthority
	}
	groups, err := LoadGroups()
	if err != nil {
		return nil, "", err
	}
	var group *Group
	for i := range groups.Groups {
		if groups.Groups[i].Name == opts.ProjectClientCreate.Group {
			group = &groups.Groups[i]
			break
		}
	}
	if group == nil || !slices.Contains(group.Servers, opts.Server) {
		return nil, "", errHubProjectAuthority
	}
	groupHash, err := projectClientValueHash(group)
	if err != nil {
		return nil, "", err
	}
	authority, err := readHubProjectAuthority()
	if err != nil {
		return nil, "", err
	}
	prior := authority.Groups[hubProjectGroupKey(group.Name)]
	if prior.Revision != "" && prior.AttachmentReceipt == "" && !prior.Revoked {
		return nil, "", errHubProjectAuthority
	}
	if prior.Revision != "" && !prior.matchesEpoch(target.Epoch) {
		return nil, "", errHubProjectAuthority
	}
	projection := struct {
		Endpoint   HubEndpoint
		Epoch      MCPFrontRoutingTargetSnapshot
		Route      SupervisorDaemon
		Supervisor SupervisorLockOwner
		Aggregate  RouteAggregateStatus
		Provider   HubStdioEndpoint
		Group      Group
		Prior      hubProjectEnrollment
		Legacy     []SupervisorDaemon
	}{target.Endpoint, target.Epoch, target.Route, target.Supervisor, target.Aggregate, target.Providers[0], *group, prior, target.Legacy}
	hash, err := projectClientValueHash(projection)
	if err != nil {
		return nil, "", err
	}
	private := &projectPrivateAttachment{target: target, enrollment: hubProjectEnrollment{
		Group: group.Name, Project: identity.ProjectIdentitySHA256(), PriorRevision: prior.Revision,
		Generation: target.Epoch.Generation, Port: target.Epoch.Port, GroupSHA256: groupHash,
		ProjectRoot: identity.CanonicalRoot(),
	}}
	return private, hash, nil
}

// The V3 digest binds the issuance action. An actual token may replace ONLY
// the exact marker at the auth-header slot; every other desired field remains
// covered by DesiredEntrySHA256 and the accepted public plan.
func privateDesiredPlanHash(raw map[string]any) (string, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	var copy map[string]any
	if json.Unmarshal(encoded, &copy) != nil {
		return "", clients.ErrProjectClientInvalid
	}
	headers, ok := copy["http_headers"].(map[string]any)
	if !ok {
		return "", clients.ErrProjectClientInvalid
	}
	value, ok := headers["X-Mcphub-Hub-Token"].(string)
	if !ok || (value != projectCredentialPlanMarker && !isLowerHex64(value)) {
		return "", clients.ErrProjectClientInvalid
	}
	headers["X-Mcphub-Hub-Token"] = projectCredentialPlanMarker
	return projectClientValueHash(copy)
}

func bindProjectPrivateIssuance(p *projectClientAttachmentPlan, plan *Plan, opts InstallOpts) error {
	if p.private == nil {
		return nil
	}
	revision, err := generateHexToken()
	if err != nil {
		return err
	}
	token, err := generateHexToken()
	if err != nil {
		return err
	}
	update := plan.ClientUpdates[0]
	update.Headers = cloneManagedRegistrationStringMap(update.Headers)
	update.Headers["X-Mcphub-Hub-Token"] = token
	desired := installClientEntryV1(opts.ProjectClientCreate.LogicalEntryName, update, plan.installRelayExePath)
	desired.Name = opts.ProjectClientCreate.LogicalEntryName + "-mcphub"
	prepared, err := p.project.(clients.ProjectHTTPAttachmentClient).PrepareProjectHTTPAttachment(opts.ProjectClientCreate.LogicalEntryName, desired, opts.ProjectClientCreate.PolicySource, p.global)
	if err != nil {
		return err
	}
	hash, err := privateDesiredPlanHash(prepared.DesiredEntry().Raw)
	_, operation := prepared.SchemaOperation()
	if err != nil || hash != p.public.DesiredEntrySHA256 || prepared.BeforeSHA256() != p.public.ConfigBeforeSHA256 || operation != p.public.Operation {
		return errors.Join(clients.ErrProjectClientPlanStale, err)
	}
	p.private.enrollment.Revision = revision
	p.private.enrollment.AttachmentPlan = p.public.PlanSHA256
	p.private.enrollment.Credential = p.private.enrollment.verifier(token)
	p.private.token = token
	p.preparation = prepared
	plan.ClientUpdates[0] = update
	return nil
}

// Borrow one real route row without publishing any definition. beginLocked
// receives an identical before/after scope, hence Ownership is necessarily
// reused. Existing compensation must never stop or remove that route.
func (a *API) receiveProjectPrivateBackend(ctx context.Context, m *config.ServerManifest, plan *Plan, opts InstallOpts, op *managedBackendInstallOperation) error {
	if err := a.retireSessionLocalLegacy(ctx, plan); err != nil {
		return err
	}
	if err := a.verifySessionLocalInstallTarget(ctx, m, plan, opts, true); err != nil {
		return err
	}
	return op.withIntentLockHeld(ctx, func() error {
		current, err := ReadSupervisorIntent(op.path)
		if err != nil {
			return err
		}
		route := findBuiltinRouteDescriptor(current)
		if route == nil || !reflect.DeepEqual(*route, plan.sessionLocal.Route) {
			return ErrSessionLocalInstallUnavailable
		}
		if err := op.beginLocked(ctx, current, current, true, BuiltinRouteServer, BuiltinRouteDaemonName); err != nil {
			return err
		}
		if op.capsule.Ownership != "reused" || op.capsule.Activation.Supervisor != plan.sessionLocal.Supervisor {
			return ErrSessionLocalInstallUnavailable
		}
		return op.receiveLocked(ctx)
	})
}

func stageProjectPrivateAttachment(ctx context.Context, p *projectClientAttachmentPlan, capsule projectClientAttachmentCapsule) error {
	if p.private == nil || capsule.Private == nil {
		return nil
	}
	e := capsule.Private.Enrollment
	staged, _, err := stageHubProjectRebind(ctx, hubProjectRebind{Group: e.Group, Project: e.Project, PriorRevision: e.PriorRevision, AttachmentPlan: e.AttachmentPlan, Epoch: p.private.target.Epoch},
		hubProjectIssuance{Revision: e.Revision, Token: p.private.token, ProjectRoot: e.ProjectRoot, CapsuleLeaf: e.CapsuleLeaf, GroupSHA256: e.GroupSHA256})
	if err != nil {
		return err
	}
	if staged != e {
		return errHubProjectAuthority
	}
	// The original front cancels old group sessions and receives their native
	// receipts before any successor project config can become usable.
	return requestProjectScopeSettlement(ctx, capsule.Private.Endpoint, e)
}

func projectPrivateReceipt(c projectClientAttachmentCapsule) (string, error) {
	c.Phase = "" // phase is a lifecycle marker; the owned binding stays exact.
	return projectClientValueHash(c)
}

func commitProjectPrivateAttachment(ctx context.Context, p *projectClientAttachmentPlan, c projectClientAttachmentCapsule) error {
	if p.private == nil {
		return nil
	}
	if c.Phase != "committed" || c.Private == nil {
		return errHubProjectAuthority
	}
	hash, err := projectPrivateReceipt(c)
	if err != nil {
		return err
	}
	e := c.Private.Enrollment
	if err := commitHubProjectRebind(ctx, e, hubProjectRebindReceiving{Project: e.Project, Revision: e.Revision, AttachmentPlan: e.AttachmentPlan, AttachmentReceipt: hash, Epoch: p.private.target.Epoch}); err != nil {
		return err
	}
	return requestProjectScopeSettlement(ctx, c.Private.Endpoint, e)
}

func revokeProjectPrivateAttachment(ctx context.Context, c projectClientAttachmentCapsule) error {
	if c.Private == nil {
		return nil
	}
	e := c.Private.Enrollment
	epoch := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: e.Generation, Port: e.Port}
	if err := revokeHubProjectEnrollment(ctx, e, epoch); err != nil {
		return err
	}
	return requestProjectScopeSettlement(ctx, c.Private.Endpoint, e)
}

// Read the original attachment owner's private capsule and physical paired
// project binding. HTTP callers supply only a token, never these receiving
// facts or a project path.
func readProjectPrivateReceiving(ctx context.Context, e hubProjectEnrollment) (hubProjectRebindReceiving, error) {
	if e.Revoked || e.AttachmentReceipt == "" || e.ProjectRoot == "" || e.CapsuleLeaf == "" {
		return hubProjectRebindReceiving{}, errHubProjectAuthority
	}
	parent := filepath.Join(e.ProjectRoot, ".codex")
	raw, err := ReadClientConfigBackupBeneathRootNoFollow(ctx, parent, []string{e.CapsuleLeaf}, "")
	if err != nil {
		return hubProjectRebindReceiving{}, err
	}
	capsule, err := decodeProjectClientAttachmentCapsule(raw)
	if err != nil || capsule.Phase != "committed" || capsule.Private == nil {
		return hubProjectRebindReceiving{}, errHubProjectAuthority
	}
	hash, err := projectPrivateReceipt(capsule)
	if err != nil || hash != e.AttachmentReceipt {
		return hubProjectRebindReceiving{}, errHubProjectAuthority
	}
	bound := e
	bound.AttachmentReceipt = ""
	if capsule.Private.Enrollment != bound {
		return hubProjectRebindReceiving{}, errHubProjectAuthority
	}
	project, identity, err := clients.NewCodexCLIForProject(e.ProjectRoot, capsule.OwnedDelta.LogicalKey, NewCodexProjectConfigMutationOwner)
	if err != nil || identity.ProjectIdentitySHA256() != e.Project || filepath.Dir(project.ConfigPath()) != parent {
		return hubProjectRebindReceiving{}, errHubProjectAuthority
	}
	backup, err := (managedRegistrationBackupIO{}).Read(ctx, parent, capsule.BackupID.String(), capsule.BeforeConfigSHA256)
	if err != nil {
		return hubProjectRebindReceiving{}, err
	}
	prepared, err := recoverProjectClientAttachment(project, identity, capsule, backup)
	if err != nil {
		return hubProjectRebindReceiving{}, err
	}
	if err := project.(clients.ProjectHTTPAttachmentClient).VerifyProjectHTTPAttachment(prepared); err != nil {
		return hubProjectRebindReceiving{}, err
	}
	groups, err := LoadGroups()
	if err != nil {
		return hubProjectRebindReceiving{}, err
	}
	groupMatches := false
	for _, group := range groups.Groups {
		if group.Name == e.Group {
			h, err := projectClientValueHash(group)
			groupMatches = err == nil && h == e.GroupSHA256
		}
	}
	if !groupMatches {
		return hubProjectRebindReceiving{}, errHubProjectAuthority
	}
	if err := verifyProjectPrivateBackend(ctx, capsule); err != nil {
		return hubProjectRebindReceiving{}, err
	}
	return hubProjectRebindReceiving{Project: e.Project, Revision: e.Revision, AttachmentPlan: e.AttachmentPlan, AttachmentReceipt: e.AttachmentReceipt,
		Epoch: MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: e.Generation, Port: e.Port}}, nil
}

func verifyProjectPrivateBackend(ctx context.Context, capsule projectClientAttachmentCapsule) error {
	ref := capsule.BackendReceipt.Receipt.BackendInstall
	if capsule.Private == nil || ref == nil || ref.outcome != "sealed" {
		return errHubProjectAuthority
	}
	root, err := DaemonStateDirReadOnly()
	if err != nil {
		return err
	}
	raw, err := (managedRegistrationBackupIO{}).Read(ctx, root, ref.reference, ref.digest)
	if err != nil {
		return err
	}
	backend, err := decodeManagedBackendInstallCapsule(raw)
	if err != nil || backend.Ownership != "reused" || backend.PlanSHA256 != capsule.Plan.PlanSHA256 || backend.TargetGroupSHA256 != capsule.Plan.TargetGroupSHA256 || backend.Activation.State != "ready" {
		return errHubProjectAuthority
	}
	peer, err := ObserveSupervisorProcessIdentityV1(backend.Activation.Supervisor.PID)
	if err != nil {
		return err
	}
	check := func(ctx context.Context) error {
		return VerifySupervisorControlGenerationForPeerFromStateDir(ctx, root, backend.Activation.Supervisor, peer)
	}
	status, err := QuerySupervisorAggregateStatus(ctx, root, backend.Activation.Supervisor, peer, check)
	if err != nil || !status.Valid() || status.Ownership != RouteAggregateOwned || status.Effective != "ready" || uint64(status.Generation) != backend.Activation.PIDGeneration {
		return errors.Join(errHubProjectAuthority, err)
	}
	return nil
}

func (h *HubMcpHandler) projectTokenReceiving(ctx context.Context, scope, token string) (hubProjectRebindReceiving, bool) {
	if h.sessions == nil || h.sessions.opts.StdioSessions == nil || !isLowerHex64(token) {
		return hubProjectRebindReceiving{}, false
	}
	group, ok := strings.CutPrefix(scope, GroupScopeKeyPrefix)
	table := liveTokenTable.Load()
	if !ok || table == nil || !table.verified {
		return hubProjectRebindReceiving{}, false
	}
	e, ok := table.projects.Groups[hubProjectGroupKey(group)]
	if !ok || e.Revoked || !e.matchesCredential(token) {
		return hubProjectRebindReceiving{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, SupervisorAggregateStatusTimeout)
	defer cancel()
	receiving, err := readProjectPrivateReceiving(ctx, e)
	if err != nil {
		return hubProjectRebindReceiving{}, false
	}
	_, ok = currentHubProjectEnrollment(scope, token, receiving)
	return receiving, ok
}

// This request names an existing transaction; it is never a project identity
// grant. The current durable issuance and original front are checked first.
type HubProjectScopeSettlementRequest struct {
	Group          string `json:"group"`
	Project        string `json:"project"`
	Revision       string `json:"revision"`
	AttachmentPlan string `json:"attachment_plan"`
}

func requestProjectScopeSettlement(ctx context.Context, endpoint HubEndpoint, e hubProjectEnrollment) error {
	return requestHubControlReceived(ctx, endpoint, "project-scope-settlement-v1", HubProjectScopeSettlementRequest{e.Group, e.Project, e.Revision, e.AttachmentPlan})
}

func (h *HubMcpHandler) ReceiveProjectScopeSettlement(ctx context.Context, request HubProjectScopeSettlementRequest) error {
	if validateGroupName(request.Group) != nil || !isLowerHex64(request.Project) || !isLowerHex64(request.Revision) || !isLowerHex64(request.AttachmentPlan) || h.sessions == nil {
		return errHubProjectAuthority
	}
	settler, ok := h.sessions.opts.StdioSessions.(interface {
		AwaitRevokedSessions(context.Context, func(context.Context) (bool, error)) error
	})
	if !ok {
		return errHubProjectAuthority
	}
	if _, err := ReloadHubTokens(); err != nil {
		return err
	}
	table := liveTokenTable.Load()
	if table == nil || !table.verified {
		return errHubProjectAuthority
	}
	e, found := table.projects.Groups[hubProjectGroupKey(request.Group)]
	if !found || e.Project != request.Project || e.Revision != request.Revision || e.AttachmentPlan != request.AttachmentPlan {
		return errHubProjectAuthority
	}
	scope := GroupScopeKey(request.Group)
	h.sessions.mu.RLock()
	var sessions []*hubSession
	for _, session := range h.sessions.sessions {
		if session.ScopeKey == scope {
			sessions = append(sessions, session)
		}
	}
	h.sessions.mu.RUnlock()
	for _, session := range sessions {
		session.mu.Lock()
		current := session.stdioCredentialCurrent()
		session.mu.Unlock()
		if current {
			continue
		}
		h.sessions.Delete(session.ClientSessionID)
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	return settler.AwaitRevokedSessions(ctx, func(lifetime context.Context) (bool, error) {
		if lifetime == nil {
			return false, errHubProjectAuthority
		}
		session, _ := lifetime.Value(hubStdioFrontContextKey{}).(*hubSession)
		if session == nil || session.stdioLifetime != lifetime {
			return false, errHubProjectAuthority
		}
		if session.ScopeKey != scope {
			return false, nil
		}
		session.mu.Lock()
		defer session.mu.Unlock()
		return !session.stdioCredentialCurrent(), nil
	})
}
