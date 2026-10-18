package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/clients"
)

const managedRegistrationJournalLeaf = "managed-registration-reconcile.json"

type managedRegistrationJournalFile struct {
	Schema                string                                 `json:"schema"`
	GenerationID          string                                 `json:"generation_id"`
	ReleaseIdentitySHA256 clients.ManagedRegistrationFingerprint `json:"release_identity_sha256"`
	Rows                  []managedRegistrationJournalRow        `json:"rows"`
}

type managedRegistrationJournalRow struct {
	ClientID                        string                                   `json:"client_id"`
	Owner                           string                                   `json:"owner,omitempty"`
	Transport                       clients.ManagedRegistrationTransport     `json:"transport,omitempty"`
	EnabledState                    clients.ManagedRegistrationEnabledState  `json:"enabled_state,omitempty"`
	IdentityState                   clients.ManagedRegistrationIdentityState `json:"identity_state,omitempty"`
	Exists                          bool                                     `json:"exists"`
	Settlement                      clients.ManagedRegistrationSettlement    `json:"settlement"`
	Phase                           ManagedRegistrationPhase                 `json:"phase"`
	FailureID                       ManagedRegistrationFailureID             `json:"failure_id,omitempty"`
	BackupID                        clients.ManagedRegistrationBackupID      `json:"backup_id"`
	TargetGroupIdentitySHA256       clients.ManagedRegistrationFingerprint   `json:"target_group_identity_sha256"`
	Before                          clients.ManagedRegistrationEntrySnapshot `json:"before"`
	After                           clients.ManagedRegistrationEntrySnapshot `json:"after"`
	SnapshotVerifiedWithoutMutation bool                                     `json:"snapshot_verified_without_mutation,omitempty"`
}

// The clients package deliberately rejects an empty fingerprint string. The
// journal wire therefore omits absent opaque values rather than serializing
// them as "". This keeps optional pre-write state restart-readable without
// weakening the fingerprint parser used for every present value.
type managedRegistrationJournalFileWire struct {
	Schema                string                                  `json:"schema"`
	GenerationID          string                                  `json:"generation_id"`
	ReleaseIdentitySHA256 *clients.ManagedRegistrationFingerprint `json:"release_identity_sha256,omitempty"`
	Rows                  []managedRegistrationJournalRow         `json:"rows"`
}

type managedRegistrationJournalRowWire struct {
	ClientID                        string                                   `json:"client_id"`
	Owner                           string                                   `json:"owner,omitempty"`
	Transport                       clients.ManagedRegistrationTransport     `json:"transport,omitempty"`
	EnabledState                    clients.ManagedRegistrationEnabledState  `json:"enabled_state,omitempty"`
	IdentityState                   clients.ManagedRegistrationIdentityState `json:"identity_state,omitempty"`
	Exists                          bool                                     `json:"exists"`
	Settlement                      clients.ManagedRegistrationSettlement    `json:"settlement"`
	Phase                           ManagedRegistrationPhase                 `json:"phase"`
	FailureID                       ManagedRegistrationFailureID             `json:"failure_id,omitempty"`
	BackupID                        clients.ManagedRegistrationBackupID      `json:"backup_id"`
	TargetGroupIdentitySHA256       *clients.ManagedRegistrationFingerprint  `json:"target_group_identity_sha256,omitempty"`
	Before                          *managedRegistrationJournalSnapshotWire  `json:"before,omitempty"`
	After                           *managedRegistrationJournalSnapshotWire  `json:"after,omitempty"`
	SnapshotVerifiedWithoutMutation bool                                     `json:"snapshot_verified_without_mutation,omitempty"`
}

type managedRegistrationJournalSnapshotWire struct {
	ClientID                string                                   `json:"client_id"`
	Exists                  bool                                     `json:"exists"`
	EnabledState            clients.ManagedRegistrationEnabledState  `json:"enabled_state,omitempty"`
	Transport               clients.ManagedRegistrationTransport     `json:"transport,omitempty"`
	IdentityState           clients.ManagedRegistrationIdentityState `json:"identity_state,omitempty"`
	ConfigSHA256            *clients.ManagedRegistrationFingerprint  `json:"config_sha256,omitempty"`
	EntrySHA256             *clients.ManagedRegistrationFingerprint  `json:"entry_sha256,omitempty"`
	DesiredProjectionSHA256 *clients.ManagedRegistrationFingerprint  `json:"desired_projection_sha256,omitempty"`
	ConfigPathSHA256        *clients.ManagedRegistrationFingerprint  `json:"config_path_sha256,omitempty"`
	EntryKeySHA256          *clients.ManagedRegistrationFingerprint  `json:"entry_key_sha256,omitempty"`
	TargetIdentitySHA256    *clients.ManagedRegistrationFingerprint  `json:"target_identity_sha256,omitempty"`
}

func (file managedRegistrationJournalFile) MarshalJSON() ([]byte, error) {
	return json.Marshal(managedRegistrationJournalFileWire{
		Schema:                file.Schema,
		GenerationID:          file.GenerationID,
		ReleaseIdentitySHA256: managedRegistrationFingerprintPointer(file.ReleaseIdentitySHA256),
		Rows:                  file.Rows,
	})
}

func (file *managedRegistrationJournalFile) UnmarshalJSON(data []byte) error {
	if file == nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	var wire managedRegistrationJournalFileWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*file = managedRegistrationJournalFile{
		Schema:       wire.Schema,
		GenerationID: wire.GenerationID,
		Rows:         wire.Rows,
	}
	if wire.ReleaseIdentitySHA256 != nil {
		file.ReleaseIdentitySHA256 = *wire.ReleaseIdentitySHA256
	}
	return nil
}

func (row managedRegistrationJournalRow) MarshalJSON() ([]byte, error) {
	return json.Marshal(managedRegistrationJournalRowWire{
		ClientID:                        row.ClientID,
		Owner:                           row.Owner,
		Transport:                       row.Transport,
		EnabledState:                    row.EnabledState,
		IdentityState:                   row.IdentityState,
		Exists:                          row.Exists,
		Settlement:                      row.Settlement,
		Phase:                           row.Phase,
		FailureID:                       row.FailureID,
		BackupID:                        row.BackupID,
		TargetGroupIdentitySHA256:       managedRegistrationFingerprintPointer(row.TargetGroupIdentitySHA256),
		Before:                          managedRegistrationJournalSnapshotToWire(row.Before),
		After:                           managedRegistrationJournalSnapshotToWire(row.After),
		SnapshotVerifiedWithoutMutation: row.SnapshotVerifiedWithoutMutation,
	})
}

func (row *managedRegistrationJournalRow) UnmarshalJSON(data []byte) error {
	if row == nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	var wire managedRegistrationJournalRowWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*row = managedRegistrationJournalRow{
		ClientID:                        wire.ClientID,
		Owner:                           wire.Owner,
		Transport:                       wire.Transport,
		EnabledState:                    wire.EnabledState,
		IdentityState:                   wire.IdentityState,
		Exists:                          wire.Exists,
		Settlement:                      wire.Settlement,
		Phase:                           wire.Phase,
		FailureID:                       wire.FailureID,
		BackupID:                        wire.BackupID,
		TargetGroupIdentitySHA256:       managedRegistrationFingerprintValue(wire.TargetGroupIdentitySHA256),
		Before:                          managedRegistrationJournalSnapshotFromWire(wire.Before),
		After:                           managedRegistrationJournalSnapshotFromWire(wire.After),
		SnapshotVerifiedWithoutMutation: wire.SnapshotVerifiedWithoutMutation,
	}
	return nil
}

func managedRegistrationJournalSnapshotToWire(snapshot clients.ManagedRegistrationEntrySnapshot) *managedRegistrationJournalSnapshotWire {
	if snapshot.ClientID == "" && !snapshot.Exists && snapshot.EnabledState == "" && snapshot.Transport == "" && snapshot.IdentityState == "" &&
		snapshot.ConfigSHA256.String() == "" && snapshot.EntrySHA256.String() == "" && snapshot.DesiredProjectionSHA256.String() == "" &&
		snapshot.ConfigPathSHA256.String() == "" && snapshot.EntryKeySHA256.String() == "" && snapshot.TargetIdentitySHA256.String() == "" {
		return nil
	}
	return &managedRegistrationJournalSnapshotWire{
		ClientID:                snapshot.ClientID,
		Exists:                  snapshot.Exists,
		EnabledState:            snapshot.EnabledState,
		Transport:               snapshot.Transport,
		IdentityState:           snapshot.IdentityState,
		ConfigSHA256:            managedRegistrationFingerprintPointer(snapshot.ConfigSHA256),
		EntrySHA256:             managedRegistrationFingerprintPointer(snapshot.EntrySHA256),
		DesiredProjectionSHA256: managedRegistrationFingerprintPointer(snapshot.DesiredProjectionSHA256),
		ConfigPathSHA256:        managedRegistrationFingerprintPointer(snapshot.ConfigPathSHA256),
		EntryKeySHA256:          managedRegistrationFingerprintPointer(snapshot.EntryKeySHA256),
		TargetIdentitySHA256:    managedRegistrationFingerprintPointer(snapshot.TargetIdentitySHA256),
	}
}

func managedRegistrationJournalSnapshotFromWire(wire *managedRegistrationJournalSnapshotWire) clients.ManagedRegistrationEntrySnapshot {
	if wire == nil {
		return clients.ManagedRegistrationEntrySnapshot{}
	}
	return clients.ManagedRegistrationEntrySnapshot{
		ClientID:                wire.ClientID,
		Exists:                  wire.Exists,
		EnabledState:            wire.EnabledState,
		Transport:               wire.Transport,
		IdentityState:           wire.IdentityState,
		ConfigSHA256:            managedRegistrationFingerprintValue(wire.ConfigSHA256),
		EntrySHA256:             managedRegistrationFingerprintValue(wire.EntrySHA256),
		DesiredProjectionSHA256: managedRegistrationFingerprintValue(wire.DesiredProjectionSHA256),
		ConfigPathSHA256:        managedRegistrationFingerprintValue(wire.ConfigPathSHA256),
		EntryKeySHA256:          managedRegistrationFingerprintValue(wire.EntryKeySHA256),
		TargetIdentitySHA256:    managedRegistrationFingerprintValue(wire.TargetIdentitySHA256),
	}
}

func managedRegistrationFingerprintPointer(value clients.ManagedRegistrationFingerprint) *clients.ManagedRegistrationFingerprint {
	if value.String() == "" {
		return nil
	}
	copy := value
	return &copy
}

func managedRegistrationFingerprintValue(value *clients.ManagedRegistrationFingerprint) clients.ManagedRegistrationFingerprint {
	if value == nil {
		return clients.ManagedRegistrationFingerprint{}
	}
	return *value
}

type managedRegistrationJournalStore struct {
	path      string
	writeHook func(managedRegistrationJournalFile) error
}

func newManagedRegistrationJournalStore(stateDir string) (managedRegistrationJournalStore, error) {
	if stateDir == "" {
		return managedRegistrationJournalStore{}, newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return managedRegistrationJournalStore{}, newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, err)
	}
	return managedRegistrationJournalStore{path: filepath.Join(absolute, managedRegistrationJournalLeaf)}, nil
}

func (s managedRegistrationJournalStore) read(ctx context.Context) (managedRegistrationJournalFile, error) {
	if err := ctx.Err(); err != nil {
		return managedRegistrationJournalFile{}, err
	}
	raw, err := ReadStateFileInodeAnchored(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return managedRegistrationJournalFile{}, nil
		}
		return managedRegistrationJournalFile{}, newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return managedRegistrationJournalFile{}, err
	}
	var file managedRegistrationJournalFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return managedRegistrationJournalFile{}, newManagedRegistrationFailure(ManagedRegistrationFailureBackupReferenceInvalid, err)
	}
	if err := validateManagedRegistrationJournalFile(file); err != nil {
		return managedRegistrationJournalFile{}, err
	}
	return file, nil
}

func (s managedRegistrationJournalStore) write(ctx context.Context, file managedRegistrationJournalFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateManagedRegistrationJournalFile(file); err != nil {
		return err
	}
	if s.writeHook != nil {
		if err := s.writeHook(file); err != nil {
			return newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
		}
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := WriteStateFileBytesAtomic(s.path, raw); err != nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	return ctx.Err()
}

func validateManagedRegistrationJournalFile(file managedRegistrationJournalFile) error {
	if file.Schema != managedRegistrationJournalSchema || !managedRegistrationSafeIdentifier(file.GenerationID) || len(file.Rows) == 0 {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	if file.ReleaseIdentitySHA256.String() != "" && !managedRegistrationFingerprintValid(file.ReleaseIdentitySHA256) {
		return newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
	}
	seen := make(map[string]struct{}, len(file.Rows))
	for _, row := range file.Rows {
		if err := validateManagedRegistrationJournalRow(row); err != nil {
			return err
		}
		identity := managedRegistrationJournalRowIdentity(row)
		if identity == "" {
			return newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
		}
		if _, duplicate := seen[identity]; duplicate {
			return newManagedRegistrationFailure(ManagedRegistrationFailureDuplicateDaemonOwner, nil)
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func managedRegistrationJournalRowIdentity(row managedRegistrationJournalRow) string {
	snapshot := row.Before
	if snapshot.ClientID == "" {
		snapshot = row.After
	}
	if row.ClientID == "" || snapshot.ConfigPathSHA256.String() == "" || snapshot.EntryKeySHA256.String() == "" {
		return ""
	}
	return strings.Join([]string{row.ClientID, snapshot.ConfigPathSHA256.String(), snapshot.EntryKeySHA256.String()}, "\x00")
}

func validateManagedRegistrationJournalRow(row managedRegistrationJournalRow) error {
	if !managedRegistrationSafeIdentifier(row.ClientID) || !managedRegistrationSettlementValid(row.Settlement) {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	if !managedRegistrationFingerprintValid(row.TargetGroupIdentitySHA256) {
		return newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
	}
	if row.SnapshotVerifiedWithoutMutation {
		if row.BackupID.String() != "" || row.Before.ClientID == "" || row.Before != row.After || validateManagedRegistrationSnapshot(row.Before) != nil {
			return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
		}
	}
	backupRequired := !row.SnapshotVerifiedWithoutMutation && row.Settlement != clients.ManagedRegistrationSettlementCaptured && row.Settlement != clients.ManagedRegistrationSettlementConflict
	if backupRequired && row.BackupID.String() == "" {
		return newManagedRegistrationFailure(ManagedRegistrationFailureBackupReferenceInvalid, nil)
	}
	if row.BackupID.String() != "" {
		parsed, err := clients.ParseManagedRegistrationBackupID(row.BackupID.String())
		if err != nil || parsed != row.BackupID {
			return newManagedRegistrationFailure(ManagedRegistrationFailureBackupReferenceInvalid, err)
		}
	}
	if row.Before.ClientID != "" && row.Before.ClientID != row.ClientID {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	if row.After.ClientID != "" && row.After.ClientID != row.ClientID {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	return nil
}

func managedRegistrationSettlementValid(settlement clients.ManagedRegistrationSettlement) bool {
	switch settlement {
	case clients.ManagedRegistrationSettlementCaptured,
		clients.ManagedRegistrationSettlementPrepared,
		clients.ManagedRegistrationSettlementRollbackPrepared,
		clients.ManagedRegistrationSettlementWriteUncertain,
		clients.ManagedRegistrationSettlementApplied,
		clients.ManagedRegistrationSettlementVerified,
		clients.ManagedRegistrationSettlementRestartRequired,
		clients.ManagedRegistrationSettlementSettled,
		clients.ManagedRegistrationSettlementConflict,
		managedRegistrationSettlementProjectionDone,
		clients.ManagedRegistrationSettlementRolledBack:
		return true
	default:
		return false
	}
}

func managedRegistrationSafeIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("-_.", char) {
			continue
		}
		return false
	}
	return true
}

func managedRegistrationReleaseFingerprint(identity ManagedRegistrationReleaseIdentity, sourceReceipt string) (clients.ManagedRegistrationFingerprint, error) {
	if identity.Version == "" || identity.Commit == "" || sourceReceipt == "" {
		return clients.ManagedRegistrationFingerprint{}, newManagedRegistrationFailure(ManagedRegistrationFailureReleaseIdentityMismatch, nil)
	}
	hash := sha256.New()
	for _, part := range []string{"mcp-local-hub/managed-registration/release-identity/v1", identity.Version, identity.Commit, sourceReceipt} {
		_, _ = fmt.Fprintf(hash, "%d:%s", len(part), part)
	}
	hexDigest := hex.EncodeToString(hash.Sum(nil))
	var fingerprint clients.ManagedRegistrationFingerprint
	if err := json.Unmarshal([]byte(`"`+hexDigest+`"`), &fingerprint); err != nil {
		return clients.ManagedRegistrationFingerprint{}, newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, err)
	}
	return fingerprint, nil
}
