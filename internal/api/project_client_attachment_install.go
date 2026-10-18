package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// A missing materializer-owned component never defaults to backend ownership.
var ErrProjectClientBackendReceiptUnavailable = errors.New("PROJECT_CLIENT_BACKEND_RECEIPT_UNAVAILABLE")

func (a *API) installProjectClientAttachment(ctx context.Context, m *config.ServerManifest, plan *Plan, opts InstallOpts, w io.Writer, receipt func(InstallMutationReceiptV1)) error {
	if err := validateInstallProjectClientCreate(opts); err != nil {
		return err
	}
	if ctx == nil || ctx.Err() != nil || m == nil || plan == nil || !plan.supervisorIntentHashesBound || len(plan.SupervisorIntent) != 1 {
		return clients.ErrProjectClientInvalid
	}
	c := opts.ProjectClientCreate
	project, identity, err := clients.NewCodexCLIForProject(c.ProjectRoot, c.LogicalEntryName, NewCodexProjectConfigMutationOwner)
	if err != nil {
		return err
	}
	globalOwner, err := clients.NewCodexCLI()
	if err != nil {
		return err
	}
	reader, ok := globalOwner.(interface{ ReadProjectAttachmentPolicySnapshot() ([]byte, error) })
	if !ok {
		return clients.ErrProjectClientInvalid
	}
	global, err := reader.ReadProjectAttachmentPolicySnapshot()
	if err != nil {
		return err
	}
	version, commit, _ := buildinfo.Get()
	p, err := prepareProjectClientAttachmentPlan(opts, plan.SupervisorIntent[0].manifestHash, plan, project, identity, global, managedRegistrationReleaseIdentity{Version: version, Commit: commit})
	if err != nil {
		return err
	}
	if opts.DryRun {
		if w != nil {
			if err := json.NewEncoder(w).Encode(p.public); err != nil {
				return err
			}
		}
		if receipt != nil {
			receipt(InstallMutationReceiptV1{DryRun: true})
		}
		return nil
	}
	return a.applyProjectClientAttachment(ctx, m, plan, opts, p, reader, w, receipt)
}

type projectAttachmentReceivingReader struct {
	project     clients.Client
	preparation *clients.ProjectHTTPAttachmentPreparation
}

func (r projectAttachmentReceivingReader) Read(ctx context.Context, expectations []bindingExpectationV1) ([]DaemonStatus, []BindingObservationV1, error) {
	rows, err := readReceivingSupervisorStatusV1(ctx)
	if err != nil {
		return nil, nil, err
	}
	bindings := make([]BindingObservationV1, 0, len(expectations))
	if r.preparation != nil {
		if err := r.project.(clients.ProjectHTTPAttachmentClient).VerifyProjectHTTPAttachment(r.preparation); err != nil {
			return rows, nil, err
		}
	}
	for _, expected := range expectations {
		if r.project == nil || expected.Client != "codex-cli" {
			return rows, nil, clients.ErrProjectClientInvalid
		}
		observed, err := readBindingExpectationV1(expected, r.project)
		if err != nil {
			return rows, bindings, err
		}
		bindings = append(bindings, observed)
	}
	return rows, bindings, nil
}
func projectAttachmentReceiver(project clients.Client, p *clients.ProjectHTTPAttachmentPreparation) (ReceivingReadinessPort, error) {
	return NewReceivingReadinessPort(ReceivingReadinessDepsV1{Reader: projectAttachmentReceivingReader{project, p}, Clock: systemUTCClockV1{}, Waiter: timerWaiterV1{}, Deadline: commandSettlementDeadlineV1{}})
}

func (a *API) applyProjectClientAttachment(ctx context.Context, m *config.ServerManifest, plan *Plan, opts InstallOpts, p *projectClientAttachmentPlan, globalReader interface{ ReadProjectAttachmentPolicySnapshot() ([]byte, error) }, w io.Writer, emit func(InstallMutationReceiptV1)) (resultErr error) {
	policyOwner, ok := globalReader.(clients.ProjectHTTPAttachmentPolicyTransactionOwner)
	if !ok {
		return clients.ErrProjectClientInvalid
	}
	parent := filepath.Dir(p.project.ConfigPath())
	if exists, err := SecureVerifyExistingParentDirForConfigLock(ctx, parent); err != nil {
		return err
	} else if !exists {
		return clients.ErrProjectClientInvalid
	}
	op := &managedBackendInstallOperation{owner: managedRegistrationBackupIO{}, public: p.public, attachment: p}
	backendPlan := *plan
	backendPlan.ClientUpdates = nil
	backendPlan.ownedBackendInstall = op
	backendOpts := opts
	backendOpts.ProjectClientCreate = nil
	backendOpts.ClientsInclude = nil
	backendOpts.IncludeAllClients = false
	backendOpts.SkipClientConfigWrites = true
	backendOpts.requireExistingStdioBridge = true
	// This is the actual backend-only Plan. A copied outer phase/invocation is
	// never authority for its different identity or its separate client policy.
	if backendPlan.serenaPreparedInvocation != nil || backendPlan.serenaPreparedBinding != nil ||
		backendPlan.serenaPreparedRecord != nil || backendPlan.serenaPreparationRequest != nil {
		return errors.New("SERENA_PREPARED_PRECOMMIT_PLAN_EFFECTS_REFUSED")
	}
	selected, err := selectGlobalInstallExecutionPhase(m, &backendPlan, backendOpts.DaemonFilter, false, w, backendOpts.SymlinkConsents)
	if err != nil {
		return err
	}
	backendPlan.selectedExecutionPhase = selected
	if err := a.prepareSerenaInstallPlan(ctx, m, &backendPlan, false); err != nil {
		return err
	}
	if backendPlan.serenaPreparedInvocation != nil {
		defer func() {
			proof, closeErr := backendPlan.serenaPreparedInvocation.CloseObserved()
			resultErr = errors.Join(resultErr, closeErr, proof.Validate(backendPlan.serenaPreparedInvocation))
		}()
		op.preparedInvocation = backendPlan.serenaPreparedInvocation
		if backendPlan.serenaPreparationRequest != nil {
			bounded, cancel := context.WithDeadline(ctx, backendPlan.serenaPreparationRequest.WorkDeadline)
			defer cancel()
			ctx = bounded
		}
		if err := selected.admitPrepared(m, &backendPlan, selected.opts, false); err != nil {
			return err
		}
	}
	root := ""
	if op.preparedInvocation != nil {
		root = op.preparedInvocation.io.Root
	} else {
		root, err = DaemonStateDir()
		if err != nil {
			return err
		}
	}
	op.root, op.path = root, filepath.Join(root, supervisorIntentFileLeaf)
	mutation := frozenInstallMutationReceipt(&backendPlan, backendOpts)
	defer func() {
		mutation.BackendInstall = op.receipt
		if emit != nil {
			emit(mutation)
		}
	}()
	if err := a.admitFrozenStdioBridgeBeforeMutation(ctx, m, &backendPlan, backendOpts); err != nil {
		return err
	}
	if err := a.installPlanCoreWithRoutingTargetLease(ctx, m, &backendPlan, backendOpts, CanonicalAtAdmission(), w); err != nil {
		return err
	}
	mutation.Committed = true
	if op.receipt == nil {
		return ErrProjectClientBackendReceiptUnavailable
	}
	backendReceiver, err := projectAttachmentReceiver(nil, nil)
	if err != nil {
		return err
	}
	ready, err := backendReceiver.Await(ctx, ReceivingReadinessRequestV1{Operation: CommandOperationInstall, ExpectedTasks: mutation.ExpectedTasks, StartupBindDeadline: mutation.StartupBindDeadline})
	if err != nil {
		return errors.Join(err, op.persist(ctx, "backend-readiness", "pending"))
	}
	if err := op.sealReady(ctx, ready.Snapshot); err != nil {
		return err
	}
	if err := a.freezeProjectBackendReceiptBindings(p, op); err != nil {
		return err
	}
	mutation.BackendInstall = op.receipt
	capsule, capsulePrepared, policyExecution, policyErr := commitProjectAttachmentWithPolicy(ctx, policyOwner, p, op, mutation)
	mutation.projectPolicyExecution = &policyExecution
	if policyErr != nil {
		if policyExecution.CleanupError != nil || (policyExecution.ProjectWriteAttempted && policyExecution.ProjectCommitted == nil) {
			return policyErr
		}
		if capsulePrepared {
			return a.compensateProjectClientAttachment(ctx, p, &capsule, op, policyErr)
		}
		return policyErr
	}
	lease, err := NewCodexProjectConfigMutationOwner(p.identity)
	if err != nil {
		return errors.Join(err, &InstallCommittedUnverifiedError{cause: err})
	}
	plan.projectAttachment = p
	deps := productionManagedRegistrationInstallDeps(a)
	input, err := freezeManagedRegistrationInstallInput(m, plan, map[string]clients.Client{"codex-cli": p.project}, nil, deps)
	if err != nil {
		return a.compensateProjectClientAttachment(ctx, p, &capsule, op, err)
	}
	if _, err := deps.Core.ReconcileInstall(ctx, input); err != nil {
		return a.compensateProjectClientAttachment(ctx, p, &capsule, op, err)
	}
	desired := intendedEntryReadbackProjection(p.project, p.preparation.DesiredEntry())
	receiver, err := projectAttachmentReceiver(p.project, p.preparation)
	if err != nil {
		return err
	}
	settled, err := receiver.Await(ctx, ReceivingReadinessRequestV1{Operation: CommandOperationInstall, ExpectedTasks: mutation.ExpectedTasks, BindingExpectations: []bindingExpectationV1{{Client: "codex-cli", Expected: desired}}, RequireBindings: true, StartupBindDeadline: mutation.StartupBindDeadline})
	if err != nil {
		return a.compensateProjectClientAttachment(ctx, p, &capsule, op, err)
	}
	if err := VerifySupervisorControlGenerationFromStateDir(ctx, root, op.capsule.Activation.Supervisor); err != nil {
		return errors.Join(err, &InstallCommittedUnverifiedError{cause: err})
	}
	next := capsule
	next.Phase = "committed"
	if err := lease.RunMutation(func() error { return op.owner.transitionProjectClientCapsule(ctx, parent, capsule, next) }); err != nil {
		return errors.Join(err, &InstallCommittedUnverifiedError{cause: err})
	}
	mutation.projectSettlement = &settled
	return nil
}

// Backup, capsule, paired write and readback are one policy/project lock body.
// It returns observed close outcomes only after both owners have released.
func commitProjectAttachmentWithPolicy(ctx context.Context, policyOwner clients.ProjectHTTPAttachmentPolicyTransactionOwner, p *projectClientAttachmentPlan, op *managedBackendInstallOperation, mutation InstallMutationReceiptV1) (capsule projectClientAttachmentCapsule, prepared bool, observed clients.ProjectHTTPAttachmentPolicyExecution, err error) {
	parent := filepath.Dir(p.project.ConfigPath())
	observed, err = policyOwner.WithProjectHTTPAttachmentPolicyTransaction(ctx, p.project, p.preparation, func(heldCtx context.Context, txn *clients.ProjectHTTPAttachmentPolicyTxn) error {
		if exists, err := SecureVerifyExistingParentDirForConfigLock(heldCtx, parent); err != nil {
			return err
		} else if !exists {
			return clients.ErrProjectClientInvalid
		}
		id, err := txn.CaptureBackup(heldCtx, op.owner)
		if err != nil {
			return err
		}
		capsule, err = newProjectClientAttachmentCapsule(p, id, projectClientBackendReceipt{Ownership: op.capsule.Ownership, Receipt: mutation})
		if err != nil {
			return err
		}
		if err := op.owner.createProjectClientCapsule(heldCtx, parent, capsule); err != nil {
			return err
		}
		prepared = true
		if _, err := txn.Create(heldCtx); err != nil {
			return err
		}
		next := capsule
		next.Phase = "project-written"
		if err := op.owner.transitionProjectClientCapsule(heldCtx, parent, capsule, next); err != nil {
			return err
		}
		capsule = next
		return txn.Verify(heldCtx)
	})
	if err != nil && (observed.CleanupError != nil || (observed.ProjectWriteAttempted && observed.ProjectCommitted == nil)) {
		err = errors.Join(err, &InstallCommittedUnverifiedError{cause: err})
	}
	return
}

func (a *API) freezeProjectBackendReceiptBindings(p *projectClientAttachmentPlan, op *managedBackendInstallOperation) error {
	if p == nil || op == nil || op.receipt == nil || op.receipt.outcome != "sealed" || op.capsule.PlanSHA256 != p.public.PlanSHA256 || op.capsule.ManifestSHA256 != p.public.ManifestSHA256 || op.capsule.TargetGroupSHA256 != p.public.TargetGroupSHA256 {
		return ErrProjectClientBackendReceiptUnavailable
	}
	return nil
}

func (a *API) compensateProjectClientAttachment(ctx context.Context, p *projectClientAttachmentPlan, c *projectClientAttachmentCapsule, op *managedBackendInstallOperation, cause error) error {
	parent := filepath.Dir(p.project.ConfigPath())
	lease, err := NewCodexProjectConfigMutationOwner(p.identity)
	if err != nil {
		return errors.Join(cause, err)
	}
	pending := *c
	pending.Phase = "rollback-pending"
	if err := lease.RunMutation(func() error { return op.owner.transitionProjectClientCapsule(ctx, parent, *c, pending) }); err != nil {
		return errors.Join(cause, err)
	}
	*c = pending
	backup, err := op.owner.Read(ctx, parent, c.BackupID.String(), c.BeforeConfigSHA256)
	if err != nil {
		return errors.Join(cause, err)
	}
	owner := p.project.(clients.ProjectHTTPAttachmentClient)
	if err := owner.VerifyRestoredProjectHTTPAttachment(p.preparation); err != nil {
		if _, err := owner.RestoreCreatedProjectHTTPAttachment(p.preparation, backup); err != nil {
			return errors.Join(cause, err)
		}
	}
	if err := retireProjectAttachmentProjection(ctx, op.root, p, c); err != nil {
		return errors.Join(cause, err)
	}
	if _, err := a.restoreOwnedBackendInstallWithOperation(ctx, op.receipt, op); err != nil {
		return errors.Join(cause, err)
	}
	rolled := *c
	rolled.Phase = "rolled-back"
	if err := lease.RunMutation(func() error { return op.owner.transitionProjectClientCapsule(ctx, parent, *c, rolled) }); err != nil {
		return errors.Join(cause, err)
	}
	*c = rolled
	return cause
}

func retireProjectAttachmentProjection(ctx context.Context, root string, p *projectClientAttachmentPlan, c *projectClientAttachmentCapsule) error {
	journal, err := newManagedRegistrationJournalStore(root)
	if err != nil {
		return err
	}
	file, err := journal.read(ctx)
	if err != nil {
		return err
	}
	_, alias, _, _, _ := p.preparation.OwnedDelta()
	managed, ok := clients.AsManagedRegistrationClient(p.project)
	if !ok {
		return clients.ErrProjectClientInvalid
	}
	identity, err := managed.SnapshotManagedRegistration(ctx, alias)
	if err != nil {
		return err
	}
	for i := range file.Rows {
		row := file.Rows[i]
		if row.ClientID != "codex-cli" || row.After.ConfigPathSHA256 != identity.ConfigPathSHA256 || row.After.EntryKeySHA256 != identity.EntryKeySHA256 {
			continue
		}
		if row.After.EntrySHA256.String() != p.public.DesiredEntrySHA256 {
			return clients.ErrProjectClientRollbackIncomplete
		}
		safe := managedRegistrationReportRow(file.GenerationID, row)
		if err := removeAndVerifyManagedRegistrationEntry(ctx, root, safe); err != nil {
			return err
		}
		file.Rows[i].Settlement = clients.ManagedRegistrationSettlementRolledBack
		file.Rows[i].Phase = ManagedRegistrationPhaseRollback
	}
	if file.Schema != "" {
		return journal.write(ctx, file)
	}
	return nil
}
