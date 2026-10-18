package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/mcpcompat"
)

type ProjectServiceIntent string

const (
	ProjectServiceDefineStopped ProjectServiceIntent = "define-stopped"
	ProjectServiceAttachTarget  ProjectServiceIntent = "attach-target"
	ProjectServiceDetachTarget  ProjectServiceIntent = "detach-target"
	ProjectServiceRetire        ProjectServiceIntent = "retire"
)

// Selection DATA only; the original API issues every source and preparation.
type ProjectServiceOpts struct {
	Service                                 ProjectServiceKind
	Intent                                  ProjectServiceIntent
	EntryName                               string
	Client                                  string
	ProjectSourceRoot                       string
	Port                                    int
	MCPProtocolCompatibilityProfile         string
	MCPProtocolCompatibilityProfileExplicit bool
}
type ProjectServicePlanInfo struct {
	Service      ProjectServiceKind   `json:"service"`
	Intent       ProjectServiceIntent `json:"intent"`
	State        AdoptOperationState  `json:"state"`
	BindExisting bool                 `json:"bind_existing,omitempty"`
}
type projectServiceConfiguredSourceOwner interface {
	ReadConfiguredProjectProfile(context.Context, string) (*clients.CodexConfiguredProjectProfile, error)
	VerifyConfiguredProjectProfile(context.Context, *clients.CodexConfiguredProjectProfile) error
}
type projectServicePlan struct {
	api                 *API
	ctx                 context.Context
	opts                ProjectServiceOpts
	subject             projectServiceSubject
	sourceOwner         projectServiceConfiguredSourceOwner
	source              *clients.CodexConfiguredProjectProfile
	projectIdentity     clients.CodexProjectIdentity
	projectConfigSHA256 string
	record              *AdoptProvenanceRecord
	storePreimage       []byte
	manifest            []byte
	manifestRead        *ManifestSourceObservation
	request             *ProjectServiceLaunchPreparationRequest
	observation         *ProjectServiceLaunchPreparationObservation
	publicDigest        string
	mu                  sync.Mutex
	consumed            bool
}

func (*projectServicePlan) String() string                  { return "project-service plan (private)" }
func (p *projectServicePlan) GoString() string              { return p.String() }
func (p *projectServicePlan) Format(s fmt.State, verb rune) { fmt.Fprint(s, p.String()) }

func (p *projectServicePlan) retainFailure(err error) error {
	if err == nil {
		return nil
	}
	if failure, ok := err.(*projectServiceLaunchObservationFailure); ok {
		failure.operation = p
		return failure
	}
	return &projectServiceLaunchObservationFailure{operation: p, observation: p.observation, cause: err}
}

func projectServiceSlot(subject projectServiceSubject) string {
	return "project-service-" + string(subject.service) + "-" + WorkspaceKey(subject.workspace)
}
func projectServiceRecordBytes(record *AdoptProvenanceRecord) ([]byte, error) {
	if record == nil {
		return nil, nil
	}
	return json.Marshal(record)
}
func sameProjectServiceRecord(a, b *AdoptProvenanceRecord) (bool, error) {
	left, err := projectServiceRecordBytes(a)
	if err != nil {
		return false, err
	}
	right, err := projectServiceRecordBytes(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}
func validateProjectServiceOptions(ctx context.Context, opts ProjectServiceOpts) (projectServiceSubject, error) {
	if ctx == nil {
		return projectServiceSubject{}, ErrCodexSharedStdioActionBudgetRequired
	}
	if !sameProjectServiceContext(ctx, ctx) {
		return projectServiceSubject{}, errors.New("PROJECT_SERVICE_ACTION_CONTEXT_IDENTITY_REQUIRED")
	}
	if _, finite := ctx.Deadline(); !finite {
		return projectServiceSubject{}, ErrCodexSharedStdioActionBudgetRequired
	}
	if err := ctx.Err(); err != nil {
		return projectServiceSubject{}, err
	}
	if opts.Service != ProjectServiceGraphify && opts.Service != ProjectServiceGraft {
		return projectServiceSubject{}, errors.New("PROJECT_SERVICE_KIND_INVALID")
	}
	switch opts.Intent {
	case ProjectServiceDefineStopped, ProjectServiceAttachTarget, ProjectServiceDetachTarget, ProjectServiceRetire:
	default:
		return projectServiceSubject{}, errors.New("PROJECT_SERVICE_INTENT_INVALID")
	}
	if opts.EntryName == "" || opts.Client != "codex-cli" || !filepath.IsAbs(opts.ProjectSourceRoot) {
		return projectServiceSubject{}, errors.New("PROJECT_SERVICE_ORIGINAL_PROJECT_SOURCE_REQUIRED")
	}
	if opts.MCPProtocolCompatibilityProfileExplicit && opts.MCPProtocolCompatibilityProfile == "" {
		return projectServiceSubject{}, errors.New("PROJECT_SERVICE_PROTOCOL_PROFILE_EMPTY")
	}
	if _, err := mcpcompat.ResolveProtocolCompatibilityProfile(opts.MCPProtocolCompatibilityProfile); err != nil {
		return projectServiceSubject{}, err
	}
	canonical, err := CanonicalWorkspacePath(opts.ProjectSourceRoot)
	if err != nil {
		return projectServiceSubject{}, err
	}
	return projectServiceSubject{workspace: canonical, service: opts.Service}, nil
}
func acquireProjectServiceLease(owner AdoptLeaseOwner, name string) (AdoptLease, error) {
	lease, acquired, err := acquireAdoptLeaseForApply(owner, name)
	if err != nil {
		return nil, err
	}
	if !acquired || lease == nil {
		return nil, errors.New("PROJECT_SERVICE_OPERATION_IN_PROGRESS")
	}
	return lease, nil
}
func (a *API) withProjectServiceLeases(subject projectServiceSubject, owner AdoptLeaseOwner, commitState *string, work func(*AdoptProvenanceRecord, []byte, string) error) (resultErr error) {
	if owner == nil {
		owner = NewAdoptLeaseOwner()
	}
	slot := projectServiceSlot(subject)
	if err := CheckManifestName(slot); err != nil {
		return err
	}
	slotLease, err := acquireProjectServiceLease(owner, slot)
	if err != nil {
		return newAdoptStageError("lease-acquire", "uncommitted", err)
	}
	defer func() {
		if err := slotLease.Unlock(); err != nil {
			resultErr = errors.Join(resultErr, newAdoptStageError("lease-release", *commitState, err))
		}
	}()
	record, preimage, err := readProjectServiceAssociation(subject)
	if err != nil {
		return err
	}
	manifestName := slot
	if record != nil {
		manifestName = record.ManifestName
	}
	if manifestName != slot {
		manifestLease, err := acquireProjectServiceLease(owner, manifestName)
		if err != nil {
			return newAdoptStageError("lease-acquire", "uncommitted", err)
		}
		defer func() {
			if err := manifestLease.Unlock(); err != nil {
				resultErr = errors.Join(resultErr, newAdoptStageError("lease-release", *commitState, err))
			}
		}()
	}
	return work(record, preimage, manifestName)
}
func findProjectServiceBindExisting(subject projectServiceSubject, identity clients.CodexProjectIdentity, entry string) (selected *AdoptProvenanceRecord, resultErr error) {
	resultErr = withAdoptedEntriesLock(func() error {
		store, err := readCanonicalGraphifyAdoptedEntries()
		if err != nil {
			return err
		}
		for _, record := range store.Records {
			if record.ProjectService != nil || record.ProviderSource != nil || record.OperationState != AdoptOperationStateAdopted {
				continue
			}
			matched := false
			for _, target := range record.Clients {
				if target.Client == "codex-cli" && target.ConfigTarget != nil && target.ConfigTarget.Kind == adoptClientConfigTargetKindCodexProject &&
					target.ConfigTarget.ProjectIdentitySHA256 == identity.ProjectIdentitySHA256() && sameProjectServicePath(target.ConfigTarget.ProjectRoot, identity.CanonicalRoot()) && adoptClientTargetEntryName(record, target) == entry {
					matched = true
				}
			}
			if !matched {
				continue
			}
			if selected != nil {
				return errors.New("PROJECT_SERVICE_BIND_EXISTING_AMBIGUOUS")
			}
			value := record
			selected = &value
		}
		return nil
	})
	return selected, resultErr
}
func chooseProjectServiceDefinitionPort(requested int, inventory *adoptPortInventoryObservation) (port int, resultErr error) {
	if inventory == nil || inventory.cause != nil || inventory.used == nil {
		return 0, errors.New("PROJECT_SERVICE_PORT_INVENTORY_UNQUALIFIED")
	}
	resultErr = withAdoptedEntriesLock(func() error {
		store, err := readCanonicalGraphifyAdoptedEntries()
		if err != nil {
			return err
		}
		reserved := make(map[int]bool, len(store.Records))
		for _, record := range store.Records {
			if record.OperationState != AdoptOperationStateClosed {
				reserved[record.Port] = true
			}
		}
		if requested != 0 {
			if requested < adoptPortStart || requested > adoptPortEnd || inventory.used[requested] || reserved[requested] {
				return errors.New("PROJECT_SERVICE_PORT_CONFLICT")
			}
			port = requested
			return nil
		}
		for candidate := adoptPortStart; candidate <= adoptPortEnd; candidate++ {
			if !inventory.used[candidate] && !reserved[candidate] {
				port = candidate
				return nil
			}
		}
		return errors.New("PROJECT_SERVICE_PORT_CAPACITY_EXHAUSTED")
	})
	if resultErr != nil {
		return 0, resultErr
	}
	_, resultErr = adoptPortBindableObserved(port)
	return port, resultErr
}
func (a *API) BuildProjectServicePlan(ctx context.Context, opts ProjectServiceOpts) (*AdoptPlan, error) {
	private, err := a.acquireProjectServicePlan(ctx, opts)
	if err != nil {
		return nil, err
	}
	return private.continueBuild()
}

func (a *API) acquireProjectServicePlan(ctx context.Context, opts ProjectServiceOpts) (operation *projectServicePlan, resultErr error) {
	subject, err := validateProjectServiceOptions(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts.Intent != ProjectServiceDefineStopped {
		return nil, errors.New("PROJECT_SERVICE_TARGET_OPERATION_REQUIRES_BOUND_RECEIVER")
	}
	if a == nil || a.projectServicePreparationFactory == nil || a.projectServicePreparationFactory.api != a || a.projectServicePreparationFactory.reader == nil {
		return nil, errors.New("PROJECT_SERVICE_PREPARATION_OWNER_UNAVAILABLE")
	}
	project, identity, err := clients.NewCodexCLIForProject(opts.ProjectSourceRoot, opts.EntryName, NewCodexProjectConfigMutationOwner)
	if err != nil {
		return nil, err
	}
	owner, ok := project.(projectServiceConfiguredSourceOwner)
	if !ok {
		return nil, errors.New("PROJECT_SERVICE_CONFIGURED_SOURCE_OWNER_UNAVAILABLE")
	}
	private := &projectServicePlan{api: a, ctx: ctx, opts: opts, subject: subject, sourceOwner: owner, projectIdentity: identity}
	source, err := owner.ReadConfiguredProjectProfile(ctx, opts.EntryName)
	if source != nil {
		private.source = source
		defer func() { resultErr = private.retainFailure(resultErr) }()
	}
	if err != nil {
		return nil, err
	}
	return private, nil
}

func (private *projectServicePlan) continueBuild() (plan *AdoptPlan, resultErr error) {
	defer func() { resultErr = private.retainFailure(resultErr) }()
	a, ctx, opts := private.api, private.ctx, private.opts
	subject, source, identity := private.subject, private.source, private.projectIdentity
	snapshot, err := clients.ReadCodexProjectConfigSnapshot(identity.CanonicalRoot())
	if err != nil {
		return nil, err
	}
	private.projectConfigSHA256 = snapshot.ConfigSHA256()
	commitState := "uncommitted"
	err = a.withProjectServiceLeases(subject, nil, &commitState, func(record *AdoptProvenanceRecord, preimage []byte, manifestName string) error {
		private.record, private.storePreimage = record, bytes.Clone(preimage)
		bindExisting := false
		if record == nil {
			candidate, err := findProjectServiceBindExisting(subject, identity, opts.EntryName)
			if err != nil {
				return err
			}
			if candidate != nil {
				// The slot was acquired before selection. An original legacy name
				// additionally needs its actual manifest lease, never name/port matching.
				lease, err := acquireProjectServiceLease(NewAdoptLeaseOwner(), candidate.ManifestName)
				if err != nil {
					return err
				}
				defer func() {
					if err := lease.Unlock(); err != nil {
						resultErr = errors.Join(resultErr, newAdoptStageError("lease-release", "uncommitted", err))
					}
				}()
				record, manifestName, bindExisting = candidate, candidate.ManifestName, true
				private.record = candidate
			}
		}
		request := &ProjectServiceLaunchPreparationRequest{api: a, factory: a.projectServicePreparationFactory, service: opts.Service, canonicalWorkspace: subject.workspace, source: source, manifestName: manifestName, ctx: ctx}
		private.request = request
		port := 0
		protocol := opts.MCPProtocolCompatibilityProfile
		if record != nil {
			if record.OperationState != AdoptOperationStateDefined && record.OperationState != AdoptOperationStateAdopted {
				return errors.New("PROJECT_SERVICE_GENERATION_REQUIRES_EXPLICIT_RECOVERY")
			}
			if opts.Port != 0 && opts.Port != record.Port {
				return errors.New("PROJECT_SERVICE_PREVIEW_PORT_CHANGED")
			}
			if opts.MCPProtocolCompatibilityProfileExplicit && protocol != record.MCPProtocolCompatibilityProfile {
				return errors.New("PROJECT_SERVICE_PROFILE_CONFLICT")
			}
			port, protocol = record.Port, record.MCPProtocolCompatibilityProfile
			observation, err := a.ManifestGetObserved(ctx, record.ManifestName)
			if err != nil {
				return err
			}
			private.manifestRead = observation
			private.manifest = observation.Bytes()
			if ManifestHashContent(private.manifest) != record.ExpectedManifestHash {
				return errors.New("PROJECT_SERVICE_MANIFEST_GENERATION_CHANGED")
			}
			request.sourceKind, request.selectedExisting = ProjectServiceManagedDefinition, true
			if bindExisting {
				if err := verifyProjectServiceLegacyTargets(record, private.manifest); err != nil {
					return err
				}
				request.bindExisting = record
			} else {
				request.managed = record
			}
		} else {
			if opts.Intent != ProjectServiceDefineStopped {
				return errors.New("PROJECT_SERVICE_STOPPED_DEFINITION_REQUIRED")
			}
			dir, err := adoptCommittedManifestDir()
			if err != nil {
				return err
			}
			exists, err := manifestExistsIn(dir, manifestName)
			if err != nil {
				return err
			}
			if exists || embeddedManifestNamesContains(manifestName) {
				return errors.New("PROJECT_SERVICE_SUBJECT_LOCATOR_CONFLICT")
			}
			launch, err := source.ConfiguredStdioLaunch()
			if err != nil {
				return err
			}
			env, _ := launch.Environment()
			if len(env) != 0 {
				return errors.New("PROJECT_SERVICE_SECRET_MATERIALIZATION_REQUIRED")
			}
			inventory, err := collectUsedAdoptPortsObserved()
			if err != nil {
				return err
			}
			port, err = chooseProjectServiceDefinitionPort(opts.Port, inventory)
			if err != nil {
				return err
			}
			forward := make([]string, 0, len(launch.EnvironmentReferences()))
			for _, reference := range launch.EnvironmentReferences() {
				if reference.Source() != "local" {
					return errors.New("PROJECT_SERVICE_REMOTE_ENV_MATERIALIZATION_REQUIRED")
				}
				forward = append(forward, reference.Name())
			}
			cwd := ""
			if value := launch.ConfiguredCWD(); value != nil {
				cwd = *value
			}
			private.manifest = []byte(renderProviderStdioBridgeManifestYAML(manifestName, launch.Command(), launch.Args(), env, forward, cwd, port, []config.ClientBinding{}, protocol))
			if _, err := a.ManifestValidateMode(string(private.manifest), ValidateModeStrict); err != nil {
				return err
			}
			request.sourceKind, request.launch = ProjectServiceConfiguredStdio, launch
		}
		request.manifest, request.manifestHash = bytes.Clone(private.manifest), ManifestHashContent(private.manifest)
		observation, err := a.projectServicePreparationFactory.reader.ReadPreparedProjectServiceLaunch(ctx, request)
		private.observation = observation
		if err != nil {
			return err
		}
		if observation == nil || observation.request != request || !observation.qualified {
			return errors.New("PROJECT_SERVICE_ORIGINAL_RECEIVER_REQUIRED")
		}
		if err := observation.original.validate(ctx, request); err != nil {
			return err
		}
		if record != nil && !bindExisting && record.ProjectService.ProfileSHA256 != observation.original.recipe.profileSHA256 {
			return errors.New("PROJECT_SERVICE_COLD_PROFILE_QUALIFICATION_REQUIRED")
		}
		state := AdoptOperationStateDefined
		if record != nil {
			state = record.OperationState
		}
		plan = &AdoptPlan{EntryName: opts.EntryName, SourceClient: opts.Client, ManifestName: manifestName, Port: port, AdoptClients: []string{}, MCPProtocolCompatibilityProfile: protocol,
			ProjectService: &ProjectServicePlanInfo{Service: opts.Service, Intent: opts.Intent, State: state, BindExisting: bindExisting}, projectService: private}
		fingerprint, err := source.Fingerprint()
		if err != nil {
			return err
		}
		recordBytes, err := projectServiceRecordBytes(record)
		if err != nil {
			return err
		}
		token, err := json.Marshal(struct {
			Domain, Workspace, Service, Intent, Source, Config, Manifest, Profile string
			Record                                                                []byte
			Port                                                                  int
		}{
			"project-service-preview-v1", subject.workspace, string(opts.Service), string(opts.Intent), fingerprint, private.projectConfigSHA256, request.manifestHash, observation.original.recipe.profileSHA256, recordBytes, port})
		if err != nil {
			return err
		}
		plan.projectSourcePreimageToken = ManifestHashContent(token)
		public, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		private.publicDigest = ManifestHashContent(public)
		return ctx.Err()
	})
	resultErr = errors.Join(err, resultErr)
	if resultErr != nil {
		return nil, resultErr
	}
	return plan, nil
}
func projectServiceCurrentSource(ctx context.Context, p *projectServicePlan) error {
	if err := p.sourceOwner.VerifyConfiguredProjectProfile(ctx, p.source); err != nil {
		return err
	}
	snapshot, err := clients.ReadCodexProjectConfigSnapshot(p.projectIdentity.CanonicalRoot())
	if err != nil {
		return err
	}
	if snapshot.Identity().ProjectIdentitySHA256() != p.projectIdentity.ProjectIdentitySHA256() || snapshot.ConfigSHA256() != p.projectConfigSHA256 {
		return clients.ErrCodexProjectSourceChanged
	}
	return nil
}
func verifyProjectServiceLegacyTargets(record *AdoptProvenanceRecord, manifest []byte) error {
	bindings, err := expectedAdoptBindingsFromManifestBytes(record, manifest)
	if err != nil {
		return err
	}
	refs := make(map[string]adoptClientTargetRef, len(record.Clients))
	names := make(map[string]string, len(record.Clients))
	for _, target := range record.Clients {
		if target.ConfigTarget == nil {
			return errors.New("PROJECT_SERVICE_LEGACY_FULL_TARGET_QUALIFICATION_REQUIRED")
		}
		if _, duplicate := refs[target.Client]; duplicate {
			return errors.New("PROJECT_SERVICE_LEGACY_TARGET_AMBIGUOUS")
		}
		name := adoptClientTargetEntryName(*record, target)
		refs[target.Client] = adoptClientTargetRef{clientName: target.Client, targetEntryName: name, configTarget: target.ConfigTarget}
		names[target.Client] = name
	}
	// This is DATA for the existing physical-target receiving owner, not a
	// plan capability or a replacement transport/policy comparator.
	legacy := &AdoptPlan{EntryName: record.SourceEntryName, SourceClient: record.SourceClient, ManifestName: record.ManifestName, Port: record.Port,
		AdoptClients: slices.Clone(record.AdoptClients), TargetEntryNames: names, clientTargets: refs, toolTimeoutByClient: adoptToolTimeoutByClient(bindings)}
	return verifyAdoptClientReadbacksWithResolver(legacy, fmt.Sprintf("http://127.0.0.1:%d%s", record.Port, adoptDefaultURLPath), newAdoptClientTargetResolver())
}
func projectServicePrepareBoundRecord(p *projectServicePlan, plan *AdoptPlan) (AdoptProvenanceRecord, error) {
	profile := p.observation.original.recipe.profileSHA256
	if p.record != nil {
		record := *p.record
		if record.ProjectService != nil {
			return record, nil
		}
		record.ProjectService = &ProjectServiceProvenanceV1{Schema: "project-service-v1", Service: p.subject.service, WorkspacePath: p.subject.workspace, WorkspaceKey: WorkspaceKey(p.subject.workspace), ProfileSHA256: profile}
		record.UpdatedAt = time.Now().UTC()
		record.Clients = slices.Clone(record.Clients)
		for index := range record.Clients {
			id, err := graphifyProjectTargetID(record, record.Clients[index])
			if err != nil {
				return record, errors.New("PROJECT_SERVICE_LEGACY_TARGET_QUALIFICATION_REQUIRED")
			}
			record.Clients[index].TargetID = id
			record.Clients[index].AttachmentState = GraphifyTargetAttached
		}
		return record, validateProjectServiceRecord(record)
	}
	now := time.Now().UTC()
	return AdoptProvenanceRecord{ManifestName: plan.ManifestName, SourceClient: plan.SourceClient, SourceEntryName: plan.EntryName, Port: plan.Port, MCPProtocolCompatibilityProfile: plan.MCPProtocolCompatibilityProfile,
		AdoptClients: []string{}, AdoptManifestHash: p.request.manifestHash, ExpectedManifestHash: p.request.manifestHash, OperationState: AdoptOperationStateAdopting, CreatedAt: now, UpdatedAt: now, Clients: []AdoptClientProvenance{},
		ProjectService: &ProjectServiceProvenanceV1{Schema: "project-service-v1", Service: p.subject.service, WorkspacePath: p.subject.workspace, WorkspaceKey: WorkspaceKey(p.subject.workspace), ProfileSHA256: profile}}, nil
}
func projectServicePublishRecord(expected *AdoptProvenanceRecord, next AdoptProvenanceRecord, subject projectServiceSubject, inventory *adoptPortInventoryObservation) error {
	if expected == nil && (inventory == nil || inventory.cause != nil || inventory.used == nil) {
		return errors.New("PROJECT_SERVICE_PORT_RESERVATION_REQUIRED")
	}
	return withAdoptedEntriesLock(func() error {
		store, err := readCanonicalGraphifyAdoptedEntries()
		if err != nil {
			return err
		}
		selected := -1
		for index, record := range store.Records {
			if record.ProjectService != nil && record.ProjectService.WorkspacePath == subject.workspace && record.ProjectService.Service == subject.service && record.ManifestName != next.ManifestName {
				return fmt.Errorf("%w: PROJECT_SERVICE_SUBJECT_CHANGED", clients.ErrCodexProjectSourceChanged)
			}
			if record.ManifestName == next.ManifestName {
				selected = index
			}
			if inventory != nil && record.OperationState != AdoptOperationStateClosed && record.Port == next.Port && record.ManifestName != next.ManifestName {
				return fmt.Errorf("%w: PROJECT_SERVICE_PORT_CONFLICT", clients.ErrCodexProjectSourceChanged)
			}
		}
		var current *AdoptProvenanceRecord
		if selected >= 0 {
			current = &store.Records[selected]
		}
		same, err := sameProjectServiceRecord(current, expected)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("%w: PROJECT_SERVICE_PROVENANCE_CHANGED", clients.ErrCodexProjectSourceChanged)
		}
		if inventory != nil && inventory.used[next.Port] {
			return fmt.Errorf("%w: PROJECT_SERVICE_PORT_CONFLICT", clients.ErrCodexProjectSourceChanged)
		}
		store.selectedProjectService = next.ManifestName
		if selected >= 0 {
			store.Records[selected] = next
		} else {
			store.Records = append(store.Records, next)
		}
		return writeAdoptedEntries(store)
	})
}
func (a *API) executeProjectServicePlan(plan *AdoptPlan, w io.Writer, opts ExecuteAdoptOpts) (resultErr error) {
	p := plan.projectService
	if p == nil || p.api != a || p.request == nil || p.observation == nil || !sameProjectServiceContext(opts.Context, p.ctx) {
		return errors.New("PROJECT_SERVICE_PLAN_UNISSUED_OR_FOREIGN")
	}
	defer func() { resultErr = p.retainFailure(resultErr) }()
	p.mu.Lock()
	if p.consumed {
		p.mu.Unlock()
		return errors.New("PROJECT_SERVICE_PLAN_ALREADY_CONSUMED")
	}
	p.consumed = true
	p.mu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return err
	}
	public, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if ManifestHashContent(public) != p.publicDigest {
		return errors.New("PROJECT_SERVICE_PUBLIC_PLAN_CHANGED")
	}
	if opts.result != nil {
		opts.result.MCPProtocolCompatibilityProfile = plan.MCPProtocolCompatibilityProfile
	}
	// Other explicit intents retain their distinct physical/backend owners. This
	// metadata operation never converts detach/retire into stop or definition.
	if p.opts.Intent != ProjectServiceDefineStopped {
		return errors.New("PROJECT_SERVICE_TARGET_OPERATION_REQUIRES_BOUND_RECEIVER")
	}
	if w == nil {
		w = io.Discard
	}
	commitState := "uncommitted"
	operationErr := a.withProjectServiceLeases(p.subject, opts.LeaseOwner, &commitState, func(current *AdoptProvenanceRecord, _ []byte, _ string) error {
		if p.request.bindExisting != nil {
			candidate, err := findProjectServiceBindExisting(p.subject, p.projectIdentity, p.opts.EntryName)
			if err != nil {
				return err
			}
			same, err := sameProjectServiceRecord(candidate, p.record)
			if err != nil {
				return err
			}
			if current != nil || !same {
				return fmt.Errorf("%w: PROJECT_SERVICE_BIND_EXISTING_PREIMAGE_CHANGED", clients.ErrCodexProjectSourceChanged)
			}
			lease, err := acquireProjectServiceLease(opts.projectServiceLeaseOwner(), p.record.ManifestName)
			if err != nil {
				return err
			}
			defer func() {
				if err := lease.Unlock(); err != nil {
					resultErr = errors.Join(resultErr, newAdoptStageError("lease-release", commitState, err))
				}
			}()
		} else {
			same, err := sameProjectServiceRecord(current, p.record)
			if err != nil {
				return err
			}
			if !same {
				return fmt.Errorf("%w: PROJECT_SERVICE_SUBJECT_CHANGED", clients.ErrCodexProjectSourceChanged)
			}
		}
		if err := projectServiceCurrentSource(p.ctx, p); err != nil {
			return err
		}
		if err := p.observation.recheck(p.ctx); err != nil {
			return err
		}
		if p.record != nil {
			manifest, err := a.ManifestGetObserved(p.ctx, p.record.ManifestName)
			if err != nil {
				return err
			}
			if !bytes.Equal(manifest.Bytes(), p.manifest) {
				return fmt.Errorf("%w: PROJECT_SERVICE_MANIFEST_GENERATION_CHANGED", clients.ErrCodexProjectSourceChanged)
			}
			if p.request.bindExisting == nil {
				if current.ProjectService.ProfileSHA256 != p.observation.original.recipe.profileSHA256 {
					return errors.New("PROJECT_SERVICE_COLD_PROFILE_QUALIFICATION_REQUIRED")
				}
				return nil
			}
			bound, err := projectServicePrepareBoundRecord(p, plan)
			if err != nil {
				return err
			}
			if err := verifyProjectServiceLegacyTargets(p.record, p.manifest); err != nil {
				return err
			}
			commitState = "pending"
			if err := projectServicePublishRecord(p.record, bound, p.subject, nil); err != nil {
				return newAdoptStageError("provenance-bind-existing", commitState, err)
			}
			commitState = "committed_unverified"
			return nil
		}
		inventory, err := collectUsedAdoptPortsObserved()
		if err != nil {
			return err
		}
		if inventory.used[plan.Port] {
			return fmt.Errorf("%w: PROJECT_SERVICE_PREVIEW_PORT_CHANGED", clients.ErrCodexProjectSourceChanged)
		}
		if _, err := adoptPortBindableObserved(plan.Port); err != nil {
			return err
		}
		record, err := projectServicePrepareBoundRecord(p, plan)
		if err != nil {
			return err
		}
		commitState = "pending"
		if err := projectServicePublishRecord(nil, record, p.subject, inventory); err != nil {
			return newAdoptStageError("provenance-capture", commitState, err)
		}
		if err := a.manifestCreateWithAdoptLeaseHeld(plan.ManifestName, string(p.manifest)); err != nil {
			return newAdoptStageError("manifest-create", "pending", err)
		}
		readback, err := a.ManifestGetObserved(p.ctx, plan.ManifestName)
		if err != nil {
			return newAdoptStageError("manifest-verify", "committed_unverified", err)
		}
		if !bytes.Equal(readback.Bytes(), p.manifest) {
			return newAdoptStageError("manifest-verify", "committed_unverified", errors.New("PROJECT_SERVICE_MANIFEST_READBACK_CHANGED"))
		}
		defined := record
		defined.OperationState = AdoptOperationStateDefined
		defined.UpdatedAt = time.Now().UTC()
		if err := projectServicePublishRecord(&record, defined, p.subject, nil); err != nil {
			return newAdoptStageError("provenance-defined", "committed_unverified", err)
		}
		commitState = "committed_unverified"
		return nil
	})
	if settledErr := errors.Join(operationErr, resultErr); settledErr != nil {
		return settledErr
	}
	if _, err := fmt.Fprintln(w, "Project-service definition received; no backend or client effect was requested."); err != nil {
		return newAdoptStageError("result-publication", "committed_unverified", err)
	}
	return nil
}
func (opts ExecuteAdoptOpts) projectServiceLeaseOwner() AdoptLeaseOwner {
	if opts.LeaseOwner != nil {
		return opts.LeaseOwner
	}
	return NewAdoptLeaseOwner()
}
