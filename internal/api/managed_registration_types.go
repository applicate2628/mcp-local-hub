package api

import (
	"context"
	"time"

	"mcp-local-hub/internal/clients"
)

const managedRegistrationJournalSchema = "managed-registration-reconcile/v2"

type ManagedRegistrationFailureID string

const (
	ManagedRegistrationFailureTargetAmbiguous            ManagedRegistrationFailureID = "mcp-registration-target-ambiguous"
	ManagedRegistrationFailureDuplicateDaemonOwner       ManagedRegistrationFailureID = "mcp-registration-duplicate-daemon-owner"
	ManagedRegistrationFailurePreconditionConflict       ManagedRegistrationFailureID = "mcp-registration-precondition-conflict"
	ManagedRegistrationFailureTargetUnready              ManagedRegistrationFailureID = "mcp-registration-target-unready"
	ManagedRegistrationFailureWriteUncertain             ManagedRegistrationFailureID = "mcp-registration-write-uncertain"
	ManagedRegistrationFailureCancelledPrewrite          ManagedRegistrationFailureID = "mcp-registration-cancelled-prewrite"
	ManagedRegistrationFailureTimeoutRollforward         ManagedRegistrationFailureID = "mcp-registration-timeout-rollforward"
	ManagedRegistrationFailureClientRestartRequired      ManagedRegistrationFailureID = "mcp-registration-client-restart-required"
	ManagedRegistrationFailureRollbackConflict           ManagedRegistrationFailureID = "mcp-registration-rollback-conflict"
	ManagedRegistrationFailureRelayExecutableUnavailable ManagedRegistrationFailureID = "mcp-registration-relay-executable-unavailable"
	ManagedRegistrationFailureClientSocketRetained       ManagedRegistrationFailureID = "mcp-registration-client-socket-retained"
	ManagedRegistrationFailureReleaseIdentityMismatch    ManagedRegistrationFailureID = "mcp-registration-release-identity-mismatch"
	ManagedRegistrationFailureSafeProjectionFailed       ManagedRegistrationFailureID = "mcp-registration-safe-projection-failed"
	ManagedRegistrationFailureIdentityUnavailable        ManagedRegistrationFailureID = "mcp-registration-identity-unavailable"
	ManagedRegistrationFailureFingerprintInvalid         ManagedRegistrationFailureID = "mcp-registration-fingerprint-invalid"
	ManagedRegistrationFailureDesiredEntryInvalid        ManagedRegistrationFailureID = "mcp-registration-desired-entry-invalid"
	ManagedRegistrationFailureBackupReferenceInvalid     ManagedRegistrationFailureID = "mcp-registration-backup-reference-invalid"
	ManagedRegistrationFailureAdapterFailed              ManagedRegistrationFailureID = "mcp-registration-adapter-failed"
	ManagedRegistrationFailureBackupIDInvalid            ManagedRegistrationFailureID = "managed-registration-backup-id-invalid"
	ManagedRegistrationFailureTargetGroupMismatch        ManagedRegistrationFailureID = "mcp-registration-target-group-mismatch"
	ManagedRegistrationFailureJournalDurabilityGap       ManagedRegistrationFailureID = "mcp-registration-journal-durability-gap"
	ManagedRegistrationFailureRollbackDurabilityGap      ManagedRegistrationFailureID = "mcp-registration-rollback-durability-gap"
	ManagedRegistrationFailureRuntimeInvalid             ManagedRegistrationFailureID = "mcp-registration-runtime-invalid"
	ManagedRegistrationFailureClientPopulationIncomplete ManagedRegistrationFailureID = "mcp-registration-client-population-incomplete"
	ManagedRegistrationFailureRuntimeTargetConflict      ManagedRegistrationFailureID = "mcp-registration-runtime-target-conflict"
	ManagedRegistrationFailureRelationshipConflict       ManagedRegistrationFailureID = "mcp-registration-relationship-conflict"
	ManagedRegistrationFailureRollbackAbsenceUnproven    ManagedRegistrationFailureID = "mcp-registration-rollback-absence-unproven"
	ManagedRegistrationFailureSourceReceiptMismatch      ManagedRegistrationFailureID = "mcp-registration-source-receipt-mismatch"
	ManagedRegistrationFailureLegacyOwnershipUnproven    ManagedRegistrationFailureID = "mcp-registration-legacy-ownership-unproven"
)

const managedRegistrationSettlementProjectionDone clients.ManagedRegistrationSettlement = "projection-done"

func RegisteredManagedRegistrationFailureIDs() []ManagedRegistrationFailureID {
	return []ManagedRegistrationFailureID{
		ManagedRegistrationFailureTargetAmbiguous,
		ManagedRegistrationFailureDuplicateDaemonOwner,
		ManagedRegistrationFailurePreconditionConflict,
		ManagedRegistrationFailureTargetUnready,
		ManagedRegistrationFailureWriteUncertain,
		ManagedRegistrationFailureCancelledPrewrite,
		ManagedRegistrationFailureTimeoutRollforward,
		ManagedRegistrationFailureClientRestartRequired,
		ManagedRegistrationFailureRollbackConflict,
		ManagedRegistrationFailureRelayExecutableUnavailable,
		ManagedRegistrationFailureClientSocketRetained,
		ManagedRegistrationFailureReleaseIdentityMismatch,
		ManagedRegistrationFailureSafeProjectionFailed,
		ManagedRegistrationFailureIdentityUnavailable,
		ManagedRegistrationFailureFingerprintInvalid,
		ManagedRegistrationFailureDesiredEntryInvalid,
		ManagedRegistrationFailureBackupReferenceInvalid,
		ManagedRegistrationFailureAdapterFailed,
		ManagedRegistrationFailureBackupIDInvalid,
		ManagedRegistrationFailureTargetGroupMismatch,
		ManagedRegistrationFailureJournalDurabilityGap,
		ManagedRegistrationFailureRollbackDurabilityGap,
		ManagedRegistrationFailureRuntimeInvalid,
		ManagedRegistrationFailureClientPopulationIncomplete,
		ManagedRegistrationFailureRuntimeTargetConflict,
		ManagedRegistrationFailureRelationshipConflict,
		ManagedRegistrationFailureRollbackAbsenceUnproven,
		ManagedRegistrationFailureSourceReceiptMismatch,
		ManagedRegistrationFailureLegacyOwnershipUnproven,
	}
}

type managedRegistrationFailureError struct {
	id    ManagedRegistrationFailureID
	cause error
}

func (e *managedRegistrationFailureError) Error() string {
	if e == nil || e.id == "" {
		return string(ManagedRegistrationFailureAdapterFailed)
	}
	return string(e.id)
}

func newManagedRegistrationFailure(id ManagedRegistrationFailureID, cause error) error {
	return &managedRegistrationFailureError{id: id, cause: cause}
}

type ManagedRegistrationPhase string

const (
	ManagedRegistrationPhaseCapture    ManagedRegistrationPhase = "capture"
	ManagedRegistrationPhaseResolve    ManagedRegistrationPhase = "resolve"
	ManagedRegistrationPhasePrepare    ManagedRegistrationPhase = "prepare"
	ManagedRegistrationPhaseApply      ManagedRegistrationPhase = "apply"
	ManagedRegistrationPhaseReadback   ManagedRegistrationPhase = "readback"
	ManagedRegistrationPhaseRecover    ManagedRegistrationPhase = "recover"
	ManagedRegistrationPhaseRollback   ManagedRegistrationPhase = "rollback"
	ManagedRegistrationPhaseSettlement ManagedRegistrationPhase = "settlement"
)

type ManagedRegistrationReleaseIdentity struct {
	Version string
	Commit  string
}

type ManagedRegistrationOperation string

const (
	ManagedRegistrationForward  ManagedRegistrationOperation = "forward"
	ManagedRegistrationRecover  ManagedRegistrationOperation = "recover"
	ManagedRegistrationRollback ManagedRegistrationOperation = "rollback"
)

type ManagedRegistrationTrigger string

const (
	ManagedRegistrationTriggerTargetedMigrate ManagedRegistrationTrigger = "targeted-migrate"
	ManagedRegistrationTriggerInstall         ManagedRegistrationTrigger = "install"
	ManagedRegistrationTriggerLegacyUpgrade   ManagedRegistrationTrigger = "legacy-upgrade"
	ManagedRegistrationTriggerV5Upgrade       ManagedRegistrationTrigger = "v5-upgrade"
	ManagedRegistrationTriggerRecovery        ManagedRegistrationTrigger = "recovery"
	ManagedRegistrationTriggerRollback        ManagedRegistrationTrigger = "rollback"
)

type ManagedRegistrationRuntime struct {
	frozen                     managedRegistrationFrozenInstallInput
	receipt                    string
	stateRoot                  string
	capturedAtUTC              time.Time
	releaseIdentity            ManagedRegistrationReleaseIdentity
	clients                    map[string]clients.ManagedRegistrationClient
	clientConstructionFailures map[string]struct{}
	relationships              map[string]map[string]string
}

// ManagedRegistrationHubOwnerAbsentProof is intentionally opaque and can be
// created only by an immutable runtime snapshot.
type ManagedRegistrationHubOwnerAbsentProof struct {
	server, daemon           string
	targetGroupSHA256        clients.ManagedRegistrationFingerprint
	supervisorSnapshotSHA256 clients.ManagedRegistrationFingerprint
}

type ManagedRegistrationReconcileOptions struct {
	Runtime *ManagedRegistrationRuntime
	Request ManagedRegistrationReconcileRequest
}

// ManagedRegistrationTarget is transient resolver input. It is never embedded
// in a report, journal row, managed-entry projection, or diagnostic.
type ManagedRegistrationTarget struct {
	TaskKey              string
	Server               string
	Daemon               string
	Port                 int
	Path                 string
	TargetIdentitySHA256 clients.ManagedRegistrationFingerprint
	Desired              bool
	Ready                bool
	Live                 bool
}

// ManagedRegistrationTargetGroupResolution is a path-free, raw-name-free
// current supervisor proof used only to authorize one rollback target group.
type ManagedRegistrationTargetGroupResolution struct {
	TargetGroupIdentitySHA256     clients.ManagedRegistrationFingerprint
	AbsenceProofTargetGroupSHA256 clients.ManagedRegistrationFingerprint
	SupervisorSnapshotSHA256      clients.ManagedRegistrationFingerprint
}

// ManagedRegistrationEntryIntent is transient planning input. EntryName and
// RelayExecutable remain inside the reconciler/client call boundary.
type ManagedRegistrationEntryIntent struct {
	ClientID        string
	EntryName       string
	Server          string
	EnabledState    clients.ManagedRegistrationEnabledState
	Transport       clients.ManagedRegistrationTransport
	RelayExecutable string
	Desired         clients.MCPEntry
	Codex           *managedRegistrationFrozenCodexRelocation
}

type ManagedRegistrationReconcileRequest struct {
	Operation       ManagedRegistrationOperation
	Trigger         ManagedRegistrationTrigger
	Servers         []string
	Clients         []string
	NoClientConfig  bool
	GenerationID    string
	ReleaseIdentity ManagedRegistrationReleaseIdentity
	Targets         []ManagedRegistrationTarget
	Entries         []ManagedRegistrationEntryIntent
	// RequireAlreadyDesired is an install-only transient guard. It admits only
	// the reconciler's verified no-write path after the PR install core commits.
	RequireAlreadyDesired bool
	sourceReceipt         string
}

type ManagedRegistrationReconcilerOptions struct {
	StateDir string
	Clients  map[string]clients.ManagedRegistrationClient
	Now      func() time.Time
	BackupIO clients.ManagedRegistrationBackupIO

	// ExistingManagedRelationships and CanonicalAliases are resolver-owned
	// positive proofs. Their keys are registry-owned safe identifiers.
	ExistingManagedRelationships  map[string]map[string]string
	LegacyManagedRelationships    map[string]map[string]string
	CanonicalAliases              map[string]string
	ExpectedReleaseIdentitySHA256 clients.ManagedRegistrationFingerprint

	// ValidateRecoveryBackup is an optional client-owned, path-free recovery
	// hook. Phase C passes only the parsed opaque value back to that owner.
	ValidateRecoveryBackup func(context.Context, string, clients.ManagedRegistrationBackupID) error

	// ResolveRecoveryEntryName re-resolves one transient raw entry name from
	// the durable opaque row identity. The returned name is used only inside
	// the client call and is never persisted or projected.
	ResolveRecoveryEntryName func(context.Context, string, clients.ManagedRegistrationEntrySnapshot) (string, error)

	// ResolveRollbackTargetGroup returns a current supervisor-owned absence
	// proof. Server and daemon are transient inputs and are never persisted.
	ResolveRollbackTargetGroup  func(context.Context, string, string) (ManagedRegistrationTargetGroupResolution, error)
	resolvePreparedAbsentCreate func(context.Context, string, clients.ManagedRegistrationFingerprint) (managedRegistrationPreparedCreateResolution, error)
}

type managedRegistrationPreparedCreateResolution struct {
	EntryName string
	Desired   clients.MCPEntry
	Target    ManagedRegistrationTarget
}

type ManagedRegistrationRow struct {
	GenerationID              string                                   `json:"generation_id"`
	ClientID                  string                                   `json:"client_id"`
	Owner                     string                                   `json:"owner"`
	Transport                 clients.ManagedRegistrationTransport     `json:"transport"`
	EnabledState              clients.ManagedRegistrationEnabledState  `json:"enabled_state"`
	IdentityState             clients.ManagedRegistrationIdentityState `json:"identity_state"`
	Exists                    bool                                     `json:"exists"`
	Settlement                clients.ManagedRegistrationSettlement    `json:"settlement"`
	Phase                     ManagedRegistrationPhase                 `json:"phase"`
	FailureID                 ManagedRegistrationFailureID             `json:"failure_id,omitempty"`
	ConfigSHA256              clients.ManagedRegistrationFingerprint   `json:"config_sha256"`
	EntrySHA256               clients.ManagedRegistrationFingerprint   `json:"entry_sha256"`
	DesiredProjectionSHA256   clients.ManagedRegistrationFingerprint   `json:"desired_projection_sha256"`
	ConfigPathSHA256          clients.ManagedRegistrationFingerprint   `json:"config_path_sha256"`
	EntryKeySHA256            clients.ManagedRegistrationFingerprint   `json:"entry_key_sha256"`
	TargetIdentitySHA256      clients.ManagedRegistrationFingerprint   `json:"target_identity_sha256"`
	TargetGroupIdentitySHA256 clients.ManagedRegistrationFingerprint   `json:"target_group_identity_sha256,omitempty"`
}

type ManagedRegistrationReconcileReport struct {
	GenerationID string                     `json:"generation_id"`
	Rows         []ManagedRegistrationRow   `json:"rows"`
	Events       []ManagedRegistrationEvent `json:"events,omitempty"`
}

type ManagedRegistrationRollbackRequest struct {
	GenerationID string
	Server       string
	Daemon       string
	absenceProof *ManagedRegistrationHubOwnerAbsentProof
	clientIDs    map[string]struct{}
}
