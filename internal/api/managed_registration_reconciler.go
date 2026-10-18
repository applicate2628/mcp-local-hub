package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"mcp-local-hub/internal/clients"
)

const managedRegistrationCleanupTimeout = 5 * time.Second

// ManagedRegistrationReconciler is the sole Phase C journal and managed-entry
// projection writer. Client capabilities remain the sole syntax writers.
type ManagedRegistrationReconciler struct {
	mu                        sync.Mutex
	options                   ManagedRegistrationReconcilerOptions
	journal                   managedRegistrationJournalStore
	entryNames                map[string]string
	projectAndVerify          func(context.Context, string, string, ManagedRegistrationRow, time.Time) error
	removeAndVerifyProjection func(context.Context, string, ManagedRegistrationRow) error
	durabilityGap             bool
}

func NewManagedRegistrationReconciler(options ManagedRegistrationReconcilerOptions) (*ManagedRegistrationReconciler, error) {
	if options.StateDir == "" || len(options.Clients) == 0 || options.Now == nil || options.BackupIO == nil {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	journal, err := newManagedRegistrationJournalStore(options.StateDir)
	if err != nil {
		return nil, err
	}
	options.Clients = cloneManagedRegistrationClients(options.Clients)
	options.ExistingManagedRelationships = cloneManagedRegistrationRelationships(options.ExistingManagedRelationships)
	options.LegacyManagedRelationships = cloneManagedRegistrationRelationships(options.LegacyManagedRelationships)
	options.CanonicalAliases = cloneManagedRegistrationAliases(options.CanonicalAliases)
	return &ManagedRegistrationReconciler{
		options:                   options,
		journal:                   journal,
		entryNames:                make(map[string]string),
		projectAndVerify:          projectAndVerifyManagedRegistrationEntry,
		removeAndVerifyProjection: removeAndVerifyManagedRegistrationEntry,
	}, nil
}

func cloneManagedRegistrationClients(source map[string]clients.ManagedRegistrationClient) map[string]clients.ManagedRegistrationClient {
	result := make(map[string]clients.ManagedRegistrationClient, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneManagedRegistrationRelationships(source map[string]map[string]string) map[string]map[string]string {
	result := make(map[string]map[string]string, len(source))
	for clientID, relationships := range source {
		copyRelationships := make(map[string]string, len(relationships))
		for entry, server := range relationships {
			copyRelationships[entry] = server
		}
		result[clientID] = copyRelationships
	}
	return result
}

func cloneManagedRegistrationAliases(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for alias, server := range source {
		result[alias] = server
	}
	return result
}

type managedRegistrationPlannedEntry struct {
	intent       ManagedRegistrationEntryIntent
	target       ManagedRegistrationTarget
	client       clients.ManagedRegistrationClient
	before       clients.ManagedRegistrationEntrySnapshot
	desired      clients.MCPEntry
	journalRow   managedRegistrationJournalRow
	journalIndex int
	immediate    *ManagedRegistrationRow
	legacyExact  bool
}

func (r *ManagedRegistrationReconciler) planEntry(
	ctx context.Context,
	generation string,
	intent ManagedRegistrationEntryIntent,
	targets []ManagedRegistrationTarget,
	requireAlreadyDesired bool,
) (managedRegistrationPlannedEntry, error) {
	client, ok := r.options.Clients[intent.ClientID]
	if !ok || client == nil || !managedRegistrationSafeIdentifier(intent.ClientID) || intent.EntryName == "" || intent.Server == "" {
		return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	before, err := client.SnapshotManagedRegistration(ctx, intent.EntryName)
	if err != nil {
		return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureCancelledPrewrite, err)
	}
	if before.ClientID != intent.ClientID {
		return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	if r.entryHasLegacyManagedRelationship(intent) {
		target, err := resolveManagedRegistrationTarget(intent, targets)
		if err != nil {
			return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureLegacyOwnershipUnproven, err)
		}
		targetGroupIdentity, err := managedRegistrationTargetGroupFingerprint(target)
		if err != nil {
			return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureLegacyOwnershipUnproven, err)
		}
		desired, err := managedRegistrationDesiredEntry(intent, target)
		if err != nil {
			return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureLegacyOwnershipUnproven, err)
		}
		matched, err := client.ManagedRegistrationDesiredMatchesSnapshot(ctx, intent.EntryName, desired, before)
		if err != nil || !matched {
			if requireAlreadyDesired {
				return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, err)
			}
			return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureLegacyOwnershipUnproven, err)
		}
		if err := validateManagedRegistrationSnapshot(before); err != nil || !before.Exists || !managedRegistrationFingerprintValid(before.DesiredProjectionSHA256) || !managedRegistrationFingerprintValid(before.ConfigPathSHA256) || !managedRegistrationFingerprintValid(before.EntryKeySHA256) {
			return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureLegacyOwnershipUnproven, err)
		}
		return managedRegistrationPlannedEntry{
			intent: intent, target: target, client: client, before: before, desired: desired, legacyExact: true,
			journalRow: managedRegistrationJournalRow{
				ClientID: intent.ClientID, Owner: "hub-supervisor", Transport: before.Transport, EnabledState: before.EnabledState, IdentityState: before.IdentityState, Exists: before.Exists,
				Settlement: clients.ManagedRegistrationSettlementVerified, Phase: ManagedRegistrationPhaseReadback,
				TargetGroupIdentitySHA256: targetGroupIdentity, Before: before, After: before, SnapshotVerifiedWithoutMutation: true,
			},
		}, nil
	}
	if err := validateManagedRegistrationSnapshot(before); err != nil {
		return managedRegistrationPlannedEntry{}, err
	}
	if !r.entryHasManagedRelationship(intent) {
		if requireAlreadyDesired {
			return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
		}
		row := managedRegistrationRowFromSnapshot(generation, "client-direct", before, clients.ManagedRegistrationSettlementCaptured, ManagedRegistrationPhaseResolve, "")
		return managedRegistrationPlannedEntry{intent: intent, client: client, before: before, immediate: &row}, nil
	}
	target, err := resolveManagedRegistrationTarget(intent, targets)
	if err != nil {
		return managedRegistrationPlannedEntry{}, err
	}
	targetGroupIdentity, err := managedRegistrationTargetGroupFingerprint(target)
	if err != nil {
		return managedRegistrationPlannedEntry{}, err
	}
	desired, err := managedRegistrationDesiredEntry(intent, target)
	if err != nil {
		return managedRegistrationPlannedEntry{}, err
	}
	if intent.Codex != nil && intent.Codex.AlreadyConfigured && !managedRegistrationSnapshotAlreadyDesired(before, intent, target) {
		return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	if managedRegistrationSnapshotAlreadyDesired(before, intent, target) {
		matched, err := client.ManagedRegistrationDesiredMatchesSnapshot(ctx, intent.EntryName, desired, before)
		if err == nil && matched {
			return managedRegistrationPlannedEntry{
				intent: intent, target: target, client: client, before: before, desired: desired, legacyExact: true,
				journalRow: managedRegistrationJournalRow{
					ClientID: intent.ClientID, Owner: "hub-supervisor", Transport: before.Transport, EnabledState: before.EnabledState, IdentityState: before.IdentityState, Exists: before.Exists,
					Settlement: clients.ManagedRegistrationSettlementVerified, Phase: ManagedRegistrationPhaseReadback,
					TargetGroupIdentitySHA256: targetGroupIdentity, Before: before, After: before, SnapshotVerifiedWithoutMutation: true,
				},
			}, nil
		}
	}
	if requireAlreadyDesired {
		return managedRegistrationPlannedEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	return managedRegistrationPlannedEntry{
		intent:  intent,
		target:  target,
		client:  client,
		before:  before,
		desired: desired,
		journalRow: managedRegistrationJournalRow{
			ClientID:                  intent.ClientID,
			Owner:                     "hub-supervisor",
			Transport:                 desiredManagedRegistrationTransport(intent),
			EnabledState:              intent.EnabledState,
			IdentityState:             desiredManagedRegistrationIdentity(intent),
			Exists:                    true,
			Settlement:                clients.ManagedRegistrationSettlementCaptured,
			Phase:                     ManagedRegistrationPhaseCapture,
			TargetGroupIdentitySHA256: targetGroupIdentity,
			Before:                    before,
		},
	}, nil
}

func (r *ManagedRegistrationReconciler) Reconcile(ctx context.Context, request ManagedRegistrationReconcileRequest) (ManagedRegistrationReconcileReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.durabilityGap {
		return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureJournalDurabilityGap, nil)
	}
	if err := ctx.Err(); err != nil {
		return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureCancelledPrewrite, err)
	}
	request.Targets = append([]ManagedRegistrationTarget(nil), request.Targets...)
	request.Entries = append([]ManagedRegistrationEntryIntent(nil), request.Entries...)
	if !managedRegistrationSafeIdentifier(request.GenerationID) || len(request.Entries) == 0 {
		return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	releaseFingerprint, err := managedRegistrationReleaseFingerprint(request.ReleaseIdentity, request.sourceReceipt)
	if err != nil {
		return ManagedRegistrationReconcileReport{}, err
	}
	existing, err := r.journal.read(ctx)
	if err != nil {
		return ManagedRegistrationReconcileReport{}, r.mapContextError(ctx, err, false)
	}
	if existing.GenerationID != "" {
		if existing.GenerationID == request.GenerationID {
			if managedRegistrationJournalSettled(existing) {
				return managedRegistrationReport(existing), nil
			}
			return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureWriteUncertain, nil)
		}
		if managedRegistrationJournalBlocksNewGeneration(existing) {
			return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureWriteUncertain, nil)
		}
	}
	frozenNow := r.options.Now().UTC()
	report := ManagedRegistrationReconcileReport{GenerationID: request.GenerationID}
	journal := managedRegistrationJournalFile{
		Schema:                managedRegistrationJournalSchema,
		GenerationID:          request.GenerationID,
		ReleaseIdentitySHA256: releaseFingerprint,
	}
	plans := make([]managedRegistrationPlannedEntry, 0, len(request.Entries))
	for _, intent := range request.Entries {
		if err := ctx.Err(); err != nil {
			return report, newManagedRegistrationFailure(ManagedRegistrationFailureCancelledPrewrite, err)
		}
		plan, err := r.planEntry(ctx, request.GenerationID, intent, request.Targets, request.RequireAlreadyDesired)
		if err != nil {
			return report, err
		}
		if plan.immediate == nil {
			plan.journalIndex = len(journal.Rows)
			journal.Rows = append(journal.Rows, plan.journalRow)
			r.entryNames[managedRegistrationJournalRowIdentity(plan.journalRow)] = intent.EntryName
		}
		plans = append(plans, plan)
	}
	if len(journal.Rows) != 0 {
		if err := r.journal.write(ctx, journal); err != nil {
			return report, r.mapContextError(ctx, err, false)
		}
	}
	for _, plan := range plans {
		if plan.immediate != nil {
			row := *plan.immediate
			report.Rows = append(report.Rows, row)
			if event, ok := managedRegistrationSettledEvent(row); ok {
				report.Events = append(report.Events, event)
			}
			continue
		}
		if plan.legacyExact {
			row, event, err := r.settleLegacyExactEntry(ctx, frozenNow, plan, &journal)
			report.Rows = append(report.Rows, row)
			if event != nil {
				report.Events = append(report.Events, *event)
			}
			if err != nil {
				return report, err
			}
			continue
		}
		row, event, err := r.reconcilePlannedEntry(ctx, frozenNow, plan, &journal)
		report.Rows = append(report.Rows, row)
		if event != nil {
			report.Events = append(report.Events, *event)
		}
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

func (r *ManagedRegistrationReconciler) settleLegacyExactEntry(ctx context.Context, now time.Time, plan managedRegistrationPlannedEntry, journal *managedRegistrationJournalFile) (ManagedRegistrationRow, *ManagedRegistrationEvent, error) {
	if journal == nil || plan.journalIndex < 0 || plan.journalIndex >= len(journal.Rows) || !plan.legacyExact {
		return ManagedRegistrationRow{}, nil, newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	journalRow := journal.Rows[plan.journalIndex]
	row := managedRegistrationReportRow(journal.GenerationID, journalRow)
	if err := r.projectAndVerify(ctx, r.options.StateDir, plan.intent.Server, row, now); err != nil {
		return row, nil, err
	}
	journalRow.Settlement = managedRegistrationSettlementProjectionDone
	journalRow.Phase = ManagedRegistrationPhaseSettlement
	journal.Rows[plan.journalIndex] = journalRow
	if err := r.journal.write(ctx, *journal); err != nil {
		return managedRegistrationReportRow(journal.GenerationID, journalRow), nil, err
	}
	journalRow.Settlement = clients.ManagedRegistrationSettlementSettled
	journal.Rows[plan.journalIndex] = journalRow
	if err := r.journal.write(ctx, *journal); err != nil {
		return managedRegistrationReportRow(journal.GenerationID, journalRow), nil, err
	}
	row = managedRegistrationReportRow(journal.GenerationID, journalRow)
	event, _ := managedRegistrationSettledEvent(row)
	return row, &event, nil
}

func (r *ManagedRegistrationReconciler) reconcilePlannedEntry(
	ctx context.Context,
	now time.Time,
	plan managedRegistrationPlannedEntry,
	journal *managedRegistrationJournalFile,
) (ManagedRegistrationRow, *ManagedRegistrationEvent, error) {
	if journal == nil || plan.journalIndex < 0 || plan.journalIndex >= len(journal.Rows) {
		return ManagedRegistrationRow{}, nil, newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	intent := plan.intent
	client := plan.client
	before := plan.before
	desired := plan.desired
	generation := journal.GenerationID
	journalRow := journal.Rows[plan.journalIndex]
	request := clients.ManagedRegistrationMutationRequest{
		EntryName:                intent.EntryName,
		ExpectedConfigSHA256:     before.ConfigSHA256,
		ExpectedEntrySHA256:      before.EntrySHA256,
		ExpectedConfigPathSHA256: before.ConfigPathSHA256,
		ExpectedEntryKeySHA256:   before.EntryKeySHA256,
		ExpectedBaseline:         before,
		ExpectedTargetSHA256:     plan.target.TargetIdentitySHA256,
		Desired:                  desired,
		BackupKeepN:              1,
		BackupIO:                 r.options.BackupIO,
		BeforeWrite: func(preparation clients.ManagedRegistrationMutationPreparation) error {
			if err := ctx.Err(); err != nil {
				return newManagedRegistrationFailure(ManagedRegistrationFailureCancelledPrewrite, err)
			}
			if preparation.TargetIdentitySHA256 != plan.target.TargetIdentitySHA256 || !managedRegistrationFingerprintValid(preparation.DesiredProjectionSHA256) {
				return newManagedRegistrationFailure(ManagedRegistrationFailureTargetGroupMismatch, nil)
			}
			if preparation.Before != before || preparation.BackupID.String() == "" {
				return newManagedRegistrationFailure(ManagedRegistrationFailureBackupReferenceInvalid, nil)
			}
			parsed, err := clients.ParseManagedRegistrationBackupID(preparation.BackupID.String())
			if err != nil || parsed != preparation.BackupID {
				return newManagedRegistrationFailure(ManagedRegistrationFailureBackupReferenceInvalid, err)
			}
			journalRow.BackupID = parsed
			journalRow.Settlement = clients.ManagedRegistrationSettlementPrepared
			journalRow.Phase = ManagedRegistrationPhasePrepare
			journal.Rows[plan.journalIndex] = journalRow
			return r.journal.write(ctx, *journal)
		},
	}
	if intent.Codex != nil && !intent.Codex.AlreadyConfigured {
		request.CodexRelocation = &clients.ManagedRegistrationCodexRelocation{
			SourceEntryName: intent.Codex.SourceEntryName,
			TargetEntryName: intent.Codex.TargetEntryName,
			ExpectedSource:  intent.Codex.ExpectedSource,
		}
	}
	var observed clients.ManagedRegistrationMutationObserved
	if before.Exists {
		observed = client.ConditionalManagedRegistrationMutation(ctx, request)
	} else {
		observed = client.ConditionalManagedRegistrationExactCreate(ctx, request)
	}
	if observed.PreconditionConflict {
		journalRow.Before = observed.Before
		journalRow.Settlement = clients.ManagedRegistrationSettlementConflict
		journalRow.Phase = ManagedRegistrationPhaseApply
		journalRow.FailureID = ManagedRegistrationFailurePreconditionConflict
		journal.Rows[plan.journalIndex] = journalRow
		writeCtx, cancel := r.cleanupContext(ctx)
		defer cancel()
		if err := r.journal.write(writeCtx, *journal); err != nil {
			return managedRegistrationReportRow(generation, journalRow), nil, err
		}
		return managedRegistrationReportRow(generation, journalRow), nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	if !observed.Invoked {
		failure := mapManagedRegistrationClientFailure(observed.Failure)
		if failure == "" {
			failure = ManagedRegistrationFailureAdapterFailed
		}
		journalRow.Phase = ManagedRegistrationPhasePrepare
		journalRow.FailureID = failure
		journal.Rows[plan.journalIndex] = journalRow
		writeCtx, cancel := r.cleanupContext(ctx)
		defer cancel()
		if err := r.journal.write(writeCtx, *journal); err != nil {
			return managedRegistrationReportRow(generation, journalRow), nil, err
		}
		return managedRegistrationReportRow(generation, journalRow), nil, newManagedRegistrationFailure(failure, nil)
	}
	journalRow.After = observed.After
	journalRow.Transport = observed.After.Transport
	journalRow.EnabledState = observed.After.EnabledState
	journalRow.IdentityState = observed.After.IdentityState
	journalRow.Exists = observed.After.Exists
	if observed.Settlement != clients.ManagedRegistrationSettlementVerified {
		journalRow.Settlement = clients.ManagedRegistrationSettlementWriteUncertain
		journalRow.Phase = ManagedRegistrationPhaseReadback
		journalRow.FailureID = ManagedRegistrationFailureWriteUncertain
		journal.Rows[plan.journalIndex] = journalRow
		if err := r.persistUncertain(ctx, *journal, ManagedRegistrationFailureJournalDurabilityGap); err != nil {
			return managedRegistrationReportRow(generation, journalRow), nil, err
		}
		if ctx.Err() != nil {
			return managedRegistrationReportRow(generation, journalRow), nil, newManagedRegistrationFailure(ManagedRegistrationFailureTimeoutRollforward, ctx.Err())
		}
		return managedRegistrationReportRow(generation, journalRow), nil, newManagedRegistrationFailure(ManagedRegistrationFailureWriteUncertain, nil)
	}
	snapshotErr := validateManagedRegistrationSnapshot(observed.After)
	if snapshotErr == nil && observed.After.TargetIdentitySHA256 != plan.target.TargetIdentitySHA256 {
		snapshotErr = newManagedRegistrationFailure(ManagedRegistrationFailureTargetGroupMismatch, nil)
	}
	if snapshotErr != nil {
		journalRow.Settlement = clients.ManagedRegistrationSettlementWriteUncertain
		journalRow.Phase = ManagedRegistrationPhaseReadback
		journalRow.FailureID = ManagedRegistrationFailureWriteUncertain
		journal.Rows[plan.journalIndex] = journalRow
		if err := r.persistUncertain(ctx, *journal, ManagedRegistrationFailureJournalDurabilityGap); err != nil {
			return managedRegistrationReportRow(generation, journalRow), nil, err
		}
		return managedRegistrationReportRow(generation, journalRow), nil, newManagedRegistrationFailure(ManagedRegistrationFailureWriteUncertain, snapshotErr)
	}
	journalRow.Settlement = clients.ManagedRegistrationSettlementApplied
	journalRow.Phase = ManagedRegistrationPhaseApply
	journal.Rows[plan.journalIndex] = journalRow
	if err := r.journal.write(ctx, *journal); err != nil {
		return managedRegistrationReportRow(generation, journalRow), nil, err
	}
	journalRow.Settlement = clients.ManagedRegistrationSettlementVerified
	journalRow.Phase = ManagedRegistrationPhaseReadback
	journal.Rows[plan.journalIndex] = journalRow
	if err := r.journal.write(ctx, *journal); err != nil {
		return managedRegistrationReportRow(generation, journalRow), nil, err
	}
	row := managedRegistrationReportRow(generation, journalRow)
	if err := r.projectAndVerify(ctx, r.options.StateDir, intent.Server, row, now); err != nil {
		return row, nil, err
	}
	journalRow.Settlement = managedRegistrationSettlementProjectionDone
	journalRow.Phase = ManagedRegistrationPhaseSettlement
	journal.Rows[plan.journalIndex] = journalRow
	if err := r.journal.write(ctx, *journal); err != nil {
		return managedRegistrationReportRow(generation, journalRow), nil, err
	}
	journalRow.Settlement = clients.ManagedRegistrationSettlementSettled
	journalRow.Phase = ManagedRegistrationPhaseSettlement
	journalRow.FailureID = ""
	journal.Rows[plan.journalIndex] = journalRow
	if err := r.journal.write(ctx, *journal); err != nil {
		return managedRegistrationReportRow(generation, journalRow), nil, err
	}
	row = managedRegistrationReportRow(generation, journalRow)
	event, _ := managedRegistrationSettledEvent(row)
	return row, &event, nil
}

func (r *ManagedRegistrationReconciler) resolveRecoveryEntryName(ctx context.Context, row managedRegistrationJournalRow) (string, error) {
	identity := managedRegistrationJournalRowIdentity(row)
	if identity == "" {
		return "", newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
	}
	if entryName := r.entryNames[identity]; entryName != "" {
		return entryName, nil
	}
	if r.options.ResolveRecoveryEntryName == nil {
		return "", newManagedRegistrationFailure(ManagedRegistrationFailureIdentityUnavailable, nil)
	}
	snapshot := row.After
	if snapshot.ClientID == "" {
		snapshot = row.Before
	}
	entryName, err := r.options.ResolveRecoveryEntryName(ctx, row.ClientID, snapshot)
	if err != nil || entryName == "" {
		return "", newManagedRegistrationFailure(ManagedRegistrationFailureIdentityUnavailable, err)
	}
	r.entryNames[identity] = entryName
	return entryName, nil
}

func (r *ManagedRegistrationReconciler) recoveryServer(row managedRegistrationJournalRow, entryName string) (string, error) {
	if byEntry := r.options.ExistingManagedRelationships[row.ClientID]; byEntry != nil {
		if server := byEntry[entryName]; managedRegistrationSafeIdentifier(server) {
			return server, nil
		}
	}
	return "", newManagedRegistrationFailure(ManagedRegistrationFailureIdentityUnavailable, nil)
}

func (r *ManagedRegistrationReconciler) Recover(ctx context.Context) (ManagedRegistrationReconcileReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureCancelledPrewrite, err)
	}
	file, err := r.journal.read(ctx)
	if err != nil {
		return ManagedRegistrationReconcileReport{}, err
	}
	if file.GenerationID == "" {
		return ManagedRegistrationReconcileReport{}, nil
	}
	if managedRegistrationJournalSettled(file) {
		return managedRegistrationReport(file), nil
	}
	if managedRegistrationFingerprintValid(r.options.ExpectedReleaseIdentitySHA256) && file.ReleaseIdentitySHA256 != r.options.ExpectedReleaseIdentitySHA256 {
		return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureSourceReceiptMismatch, nil)
	}
	for i := range file.Rows {
		row := &file.Rows[i]
		if err := ctx.Err(); err != nil {
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureTimeoutRollforward, err)
		}
		client := r.options.Clients[row.ClientID]
		if client == nil {
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureIdentityUnavailable, nil)
		}
		if r.options.ValidateRecoveryBackup != nil {
			if err := r.options.ValidateRecoveryBackup(ctx, row.ClientID, row.BackupID); err != nil {
				return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureBackupReferenceInvalid, err)
			}
		}
		if row.Settlement == clients.ManagedRegistrationSettlementPrepared && !row.Before.Exists {
			if err := r.recoverPreparedAbsentCreate(ctx, &file, i); err != nil {
				return managedRegistrationReport(file), err
			}
			continue
		}
		entryName, err := r.resolveRecoveryEntryName(ctx, *row)
		if err != nil {
			return managedRegistrationReport(file), err
		}
		current, err := client.SnapshotManagedRegistration(ctx, entryName)
		if err != nil {
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
		}
		if err := validateManagedRegistrationSnapshot(current); err != nil {
			return managedRegistrationReport(file), err
		}
		if row.Phase == ManagedRegistrationPhaseRollback && (row.Settlement == clients.ManagedRegistrationSettlementRollbackPrepared || row.Settlement == clients.ManagedRegistrationSettlementWriteUncertain) {
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
		}
		if row.After.ClientID != "" && sameManagedRegistrationSnapshotIdentity(current, row.After) {
			row.After = current
			row.Transport = current.Transport
			row.EnabledState = current.EnabledState
			row.IdentityState = current.IdentityState
			row.Exists = current.Exists
			if row.Settlement == clients.ManagedRegistrationSettlementApplied || row.Settlement == clients.ManagedRegistrationSettlementVerified || row.Settlement == managedRegistrationSettlementProjectionDone {
				server, err := r.recoveryServer(*row, entryName)
				if err != nil {
					return managedRegistrationReport(file), err
				}
				projected := managedRegistrationReportRow(file.GenerationID, *row)
				if err := r.projectAndVerify(ctx, r.options.StateDir, server, projected, r.options.Now().UTC()); err != nil {
					return managedRegistrationReport(file), err
				}
				row.Settlement = managedRegistrationSettlementProjectionDone
				row.Phase = ManagedRegistrationPhaseSettlement
				row.FailureID = ""
				if err := r.journal.write(ctx, file); err != nil {
					return managedRegistrationReport(file), err
				}
			}
			row.Settlement = clients.ManagedRegistrationSettlementSettled
			row.Phase = ManagedRegistrationPhaseSettlement
			row.FailureID = ""
			if err := r.journal.write(ctx, file); err != nil {
				return managedRegistrationReport(file), err
			}
			continue
		}
		row.Settlement = clients.ManagedRegistrationSettlementConflict
		row.Phase = ManagedRegistrationPhaseRecover
		row.FailureID = ManagedRegistrationFailurePreconditionConflict
		writeCtx, cancel := r.cleanupContext(ctx)
		err = r.journal.write(writeCtx, file)
		cancel()
		if err != nil {
			return managedRegistrationReport(file), err
		}
		return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	return managedRegistrationReport(file), nil
}

func (r *ManagedRegistrationReconciler) recoverPreparedAbsentCreate(ctx context.Context, file *managedRegistrationJournalFile, index int) error {
	if file == nil || index < 0 || index >= len(file.Rows) || r.options.resolvePreparedAbsentCreate == nil || !managedRegistrationFingerprintValid(r.options.ExpectedReleaseIdentitySHA256) {
		return newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
	}
	row := &file.Rows[index]
	resolution, err := r.options.resolvePreparedAbsentCreate(ctx, row.ClientID, row.TargetGroupIdentitySHA256)
	if err != nil || resolution.EntryName == "" {
		return newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, err)
	}
	client := r.options.Clients[row.ClientID]
	if client == nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureIdentityUnavailable, nil)
	}
	current, err := client.SnapshotManagedRegistration(ctx, resolution.EntryName)
	if err != nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	if err := validateManagedRegistrationSnapshot(current); err != nil {
		return err
	}
	if current.Exists {
		wantTransport := desiredManagedRegistrationTransport(ManagedRegistrationEntryIntent{Transport: row.Transport})
		wantIdentity := desiredManagedRegistrationIdentity(ManagedRegistrationEntryIntent{Transport: row.Transport})
		if current.ClientID != row.ClientID || current.EnabledState != row.EnabledState || current.Transport != wantTransport || current.IdentityState != wantIdentity || current.TargetIdentitySHA256 != resolution.Target.TargetIdentitySHA256 || !managedRegistrationFingerprintValid(current.DesiredProjectionSHA256) {
			return newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
		}
		row.After = current
		return r.settleRecoveredPreparedCreate(ctx, file, index, resolution.Target.Server)
	}
	if current != row.Before {
		return newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	observed := client.ConditionalManagedRegistrationExactCreate(ctx, clients.ManagedRegistrationMutationRequest{
		EntryName:                resolution.EntryName,
		ExpectedConfigSHA256:     current.ConfigSHA256,
		ExpectedEntrySHA256:      current.EntrySHA256,
		ExpectedConfigPathSHA256: current.ConfigPathSHA256,
		ExpectedEntryKeySHA256:   current.EntryKeySHA256,
		ExpectedBaseline:         current,
		ExpectedTargetSHA256:     resolution.Target.TargetIdentitySHA256,
		PreparedBackupID:         row.BackupID,
		Desired:                  resolution.Desired,
		BackupKeepN:              1,
		BackupIO:                 r.options.BackupIO,
		BeforeWrite: func(preparation clients.ManagedRegistrationMutationPreparation) error {
			if preparation.Before != current || preparation.BackupID != row.BackupID || preparation.TargetIdentitySHA256 != resolution.Target.TargetIdentitySHA256 || !managedRegistrationFingerprintValid(preparation.DesiredProjectionSHA256) {
				return newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
			}
			return r.journal.write(ctx, *file)
		},
	})
	if observed.PreconditionConflict || !observed.Invoked {
		return newManagedRegistrationFailure(mapManagedRegistrationClientFailure(observed.Failure), nil)
	}
	row.After = observed.After
	if observed.Settlement != clients.ManagedRegistrationSettlementVerified || observed.After.TargetIdentitySHA256 != resolution.Target.TargetIdentitySHA256 {
		row.Settlement = clients.ManagedRegistrationSettlementWriteUncertain
		row.Phase = ManagedRegistrationPhaseReadback
		row.FailureID = ManagedRegistrationFailureWriteUncertain
		if err := r.persistUncertain(ctx, *file, ManagedRegistrationFailureJournalDurabilityGap); err != nil {
			return err
		}
		return newManagedRegistrationFailure(ManagedRegistrationFailureWriteUncertain, nil)
	}
	return r.settleRecoveredPreparedCreate(ctx, file, index, resolution.Target.Server)
}

func (r *ManagedRegistrationReconciler) settleRecoveredPreparedCreate(ctx context.Context, file *managedRegistrationJournalFile, index int, server string) error {
	row := &file.Rows[index]
	row.Owner = "hub-supervisor"
	row.Transport = row.After.Transport
	row.EnabledState = row.After.EnabledState
	row.IdentityState = row.After.IdentityState
	row.Exists = row.After.Exists
	row.Settlement = clients.ManagedRegistrationSettlementApplied
	row.Phase = ManagedRegistrationPhaseApply
	row.FailureID = ""
	if err := r.journal.write(ctx, *file); err != nil {
		return err
	}
	row.Settlement = clients.ManagedRegistrationSettlementVerified
	row.Phase = ManagedRegistrationPhaseReadback
	if err := r.journal.write(ctx, *file); err != nil {
		return err
	}
	projected := managedRegistrationReportRow(file.GenerationID, *row)
	if err := r.projectAndVerify(ctx, r.options.StateDir, server, projected, r.options.Now().UTC()); err != nil {
		return err
	}
	row.Settlement = managedRegistrationSettlementProjectionDone
	row.Phase = ManagedRegistrationPhaseSettlement
	if err := r.journal.write(ctx, *file); err != nil {
		return err
	}
	row.Settlement = clients.ManagedRegistrationSettlementSettled
	if err := r.journal.write(ctx, *file); err != nil {
		return err
	}
	return nil
}

func (r *ManagedRegistrationReconciler) RollbackTargetGroup(ctx context.Context, request ManagedRegistrationRollbackRequest) (ManagedRegistrationReconcileReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureCancelledPrewrite, err)
	}
	if request.absenceProof == nil || request.absenceProof.server != request.Server || request.absenceProof.daemon != request.Daemon || request.GenerationID == "" || request.Server == "" || request.Daemon == "" {
		return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
	}
	file, err := r.journal.read(ctx)
	if err != nil {
		return ManagedRegistrationReconcileReport{}, err
	}
	if file.GenerationID != request.GenerationID {
		return ManagedRegistrationReconcileReport{}, newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
	}
	targetGroup, err := r.resolveRollbackTargetGroup(ctx, request)
	if err != nil {
		return managedRegistrationReport(file), err
	}
	if len(request.clientIDs) != 0 {
		available := make(map[string]struct{})
		for _, row := range file.Rows {
			if row.TargetGroupIdentitySHA256 == targetGroup {
				available[row.ClientID] = struct{}{}
			}
		}
		for clientID := range request.clientIDs {
			if _, exists := available[clientID]; !exists {
				return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
			}
		}
	}
	matched := false
	for i := range file.Rows {
		row := &file.Rows[i]
		if row.TargetGroupIdentitySHA256 != targetGroup {
			continue
		}
		if len(request.clientIDs) != 0 {
			if _, selected := request.clientIDs[row.ClientID]; !selected {
				continue
			}
		}
		matched = true
		if row.Settlement == clients.ManagedRegistrationSettlementRolledBack {
			continue
		}
		client := r.options.Clients[row.ClientID]
		if client == nil {
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
		}
		entryName, err := r.resolveRecoveryEntryName(ctx, *row)
		if err != nil {
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, err)
		}
		current, err := client.SnapshotManagedRegistration(ctx, entryName)
		if err != nil || !sameManagedRegistrationSnapshotIdentity(current, row.After) {
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, err)
		}
		observed := client.ConditionalManagedRegistrationBackupRestore(ctx, clients.ManagedRegistrationConditionalBackupRestoreRequest{
			EntryName:                 entryName,
			ExpectedApplied:           row.After,
			ExpectedBaseline:          row.Before,
			ExpectedTargetGroupSHA256: targetGroup,
			BackupID:                  row.BackupID,
			BackupIO:                  r.options.BackupIO,
			BeforeWrite: func(preparation clients.ManagedRegistrationBackupRestorePreparation) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if preparation.Applied != row.After || preparation.Baseline != row.Before || preparation.BackupID != row.BackupID {
					return newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
				}
				row.Settlement = clients.ManagedRegistrationSettlementRollbackPrepared
				row.Phase = ManagedRegistrationPhaseRollback
				row.FailureID = ""
				return r.persistJournal(ctx, file, ManagedRegistrationFailureRollbackDurabilityGap)
			},
		})
		if observed.PreconditionConflict {
			row.Settlement = clients.ManagedRegistrationSettlementConflict
			row.Phase = ManagedRegistrationPhaseRollback
			row.FailureID = ManagedRegistrationFailureRollbackConflict
			if err := r.persistJournal(ctx, file, ManagedRegistrationFailureRollbackDurabilityGap); err != nil {
				return managedRegistrationReport(file), err
			}
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
		}
		if !observed.Invoked {
			if r.durabilityGap {
				return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackDurabilityGap, nil)
			}
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
		}
		if observed.Settlement != clients.ManagedRegistrationSettlementVerified {
			row.After = observed.After
			row.Settlement = clients.ManagedRegistrationSettlementWriteUncertain
			row.Phase = ManagedRegistrationPhaseRollback
			row.FailureID = ManagedRegistrationFailureWriteUncertain
			if err := r.persistJournal(ctx, file, ManagedRegistrationFailureRollbackDurabilityGap); err != nil {
				return managedRegistrationReport(file), err
			}
			return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
		}
		projected := managedRegistrationReportRow(file.GenerationID, *row)
		row.After = observed.After
		row.Owner = "client-direct"
		row.Transport = observed.After.Transport
		row.EnabledState = observed.After.EnabledState
		row.IdentityState = observed.After.IdentityState
		row.Exists = observed.After.Exists
		row.Settlement = clients.ManagedRegistrationSettlementRolledBack
		row.Phase = ManagedRegistrationPhaseRollback
		row.FailureID = ""
		if err := r.persistJournal(ctx, file, ManagedRegistrationFailureRollbackDurabilityGap); err != nil {
			return managedRegistrationReport(file), err
		}
		cleanupCtx, cancel := r.cleanupContext(ctx)
		err = r.removeAndVerifyProjection(cleanupCtx, r.options.StateDir, projected)
		cancel()
		if err != nil {
			return managedRegistrationReport(file), err
		}
	}
	if !matched {
		return managedRegistrationReport(file), newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
	}
	return managedRegistrationReport(file), nil
}

func (r *ManagedRegistrationReconciler) resolveRollbackTargetGroup(ctx context.Context, request ManagedRegistrationRollbackRequest) (clients.ManagedRegistrationFingerprint, error) {
	if r.options.ResolveRollbackTargetGroup == nil {
		return clients.ManagedRegistrationFingerprint{}, newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, nil)
	}
	resolution, err := r.options.ResolveRollbackTargetGroup(ctx, request.Server, request.Daemon)
	if err != nil || !managedRegistrationFingerprintValid(resolution.TargetGroupIdentitySHA256) || !managedRegistrationFingerprintValid(resolution.AbsenceProofTargetGroupSHA256) || !managedRegistrationFingerprintValid(resolution.SupervisorSnapshotSHA256) || resolution.TargetGroupIdentitySHA256 != resolution.AbsenceProofTargetGroupSHA256 {
		return clients.ManagedRegistrationFingerprint{}, newManagedRegistrationFailure(ManagedRegistrationFailureRollbackConflict, err)
	}
	return resolution.TargetGroupIdentitySHA256, nil
}

func managedRegistrationSnapshotAlreadyDesired(snapshot clients.ManagedRegistrationEntrySnapshot, intent ManagedRegistrationEntryIntent, target ManagedRegistrationTarget) bool {
	wantTransport := desiredManagedRegistrationTransport(intent)
	wantIdentity := desiredManagedRegistrationIdentity(intent)
	return snapshot.Exists && snapshot.EnabledState == intent.EnabledState && snapshot.Transport == wantTransport && snapshot.IdentityState == wantIdentity && managedRegistrationFingerprintValid(snapshot.TargetIdentitySHA256) && snapshot.TargetIdentitySHA256 == target.TargetIdentitySHA256
}

func desiredManagedRegistrationTransport(intent ManagedRegistrationEntryIntent) clients.ManagedRegistrationTransport {
	if intent.Transport == clients.ManagedRegistrationTransportStdioRelay || intent.Transport == "stdio-relay" {
		return clients.ManagedRegistrationTransportStdioRelay
	}
	return clients.ManagedRegistrationTransportHTTP
}

func desiredManagedRegistrationIdentity(intent ManagedRegistrationEntryIntent) clients.ManagedRegistrationIdentityState {
	if desiredManagedRegistrationTransport(intent) == clients.ManagedRegistrationTransportStdioRelay {
		return clients.ManagedRegistrationIdentityRelay
	}
	return clients.ManagedRegistrationIdentityManagedHTTP
}

func managedRegistrationRowFromSnapshot(generation, owner string, snapshot clients.ManagedRegistrationEntrySnapshot, settlement clients.ManagedRegistrationSettlement, phase ManagedRegistrationPhase, failure ManagedRegistrationFailureID) ManagedRegistrationRow {
	return ManagedRegistrationRow{
		GenerationID:            generation,
		ClientID:                snapshot.ClientID,
		Owner:                   owner,
		Transport:               snapshot.Transport,
		EnabledState:            snapshot.EnabledState,
		IdentityState:           snapshot.IdentityState,
		Exists:                  snapshot.Exists,
		Settlement:              settlement,
		Phase:                   phase,
		FailureID:               failure,
		ConfigSHA256:            snapshot.ConfigSHA256,
		EntrySHA256:             snapshot.EntrySHA256,
		DesiredProjectionSHA256: snapshot.DesiredProjectionSHA256,
		ConfigPathSHA256:        snapshot.ConfigPathSHA256,
		EntryKeySHA256:          snapshot.EntryKeySHA256,
		TargetIdentitySHA256:    snapshot.TargetIdentitySHA256,
	}
}

func managedRegistrationReport(file managedRegistrationJournalFile) ManagedRegistrationReconcileReport {
	report := ManagedRegistrationReconcileReport{GenerationID: file.GenerationID, Rows: make([]ManagedRegistrationRow, 0, len(file.Rows))}
	for _, journalRow := range file.Rows {
		row := managedRegistrationReportRow(file.GenerationID, journalRow)
		report.Rows = append(report.Rows, row)
		if event, ok := managedRegistrationSettledEvent(row); ok {
			report.Events = append(report.Events, event)
		}
	}
	return report
}

func managedRegistrationJournalSettled(file managedRegistrationJournalFile) bool {
	if len(file.Rows) == 0 {
		return false
	}
	for _, row := range file.Rows {
		if row.Settlement != clients.ManagedRegistrationSettlementSettled && row.Settlement != clients.ManagedRegistrationSettlementRolledBack {
			return false
		}
	}
	return true
}

func managedRegistrationJournalBlocksNewGeneration(file managedRegistrationJournalFile) bool {
	for _, row := range file.Rows {
		if row.Settlement == clients.ManagedRegistrationSettlementWriteUncertain || row.Settlement == clients.ManagedRegistrationSettlementPrepared || row.Settlement == clients.ManagedRegistrationSettlementApplied || row.Settlement == clients.ManagedRegistrationSettlementVerified {
			return true
		}
	}
	return false
}

func mapManagedRegistrationClientFailure(failure clients.ManagedRegistrationFailureClass) ManagedRegistrationFailureID {
	switch failure {
	case clients.ManagedRegistrationFailurePreconditionConflict:
		return ManagedRegistrationFailurePreconditionConflict
	case clients.ManagedRegistrationFailureSafeProjection:
		return ManagedRegistrationFailureSafeProjectionFailed
	case clients.ManagedRegistrationFailureIdentityUnavailable:
		return ManagedRegistrationFailureIdentityUnavailable
	case clients.ManagedRegistrationFailureFingerprintInvalid:
		return ManagedRegistrationFailureFingerprintInvalid
	case clients.ManagedRegistrationFailureDesiredEntryInvalid,
		clients.ManagedRegistrationFailureEnabledStateInvalid:
		return ManagedRegistrationFailureDesiredEntryInvalid
	case clients.ManagedRegistrationFailureBackupIDInvalid:
		return ManagedRegistrationFailureBackupReferenceInvalid
	case clients.ManagedRegistrationFailureBackup:
		return ManagedRegistrationFailureBackupReferenceInvalid
	case clients.ManagedRegistrationFailureTargetGroupMismatch:
		return ManagedRegistrationFailureTargetGroupMismatch
	case clients.ManagedRegistrationFailureNone:
		return ""
	default:
		return ManagedRegistrationFailureAdapterFailed
	}
}

func (r *ManagedRegistrationReconciler) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(context.Background(), managedRegistrationCleanupTimeout)
}

func (r *ManagedRegistrationReconciler) persistUncertain(ctx context.Context, journal managedRegistrationJournalFile, failure ManagedRegistrationFailureID) error {
	return r.persistJournal(ctx, journal, failure)
}

func (r *ManagedRegistrationReconciler) persistJournal(ctx context.Context, journal managedRegistrationJournalFile, failure ManagedRegistrationFailureID) error {
	if err := r.journal.write(ctx, journal); err == nil {
		return nil
	}
	writeCtx, cancel := r.cleanupContext(ctx)
	defer cancel()
	if err := r.journal.write(writeCtx, journal); err == nil {
		return nil
	}
	r.durabilityGap = true
	return newManagedRegistrationFailure(failure, nil)
}

func (r *ManagedRegistrationReconciler) mapContextError(ctx context.Context, err error, invoked bool) error {
	if ctx.Err() == nil {
		return err
	}
	if invoked {
		return newManagedRegistrationFailure(ManagedRegistrationFailureTimeoutRollforward, ctx.Err())
	}
	return newManagedRegistrationFailure(ManagedRegistrationFailureCancelledPrewrite, ctx.Err())
}

var _ = errors.Is
