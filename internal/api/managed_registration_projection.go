package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/clients"
)

const managedRegistrationSettledEventID = "managed-registration-reconcile-settled.v2"

type ManagedRegistrationEvent struct {
	EventID              string                                  `json:"event_id"`
	GenerationID         string                                  `json:"generation_id"`
	ClientID             string                                  `json:"client_id"`
	Owner                string                                  `json:"owner"`
	Transport            clients.ManagedRegistrationTransport    `json:"transport"`
	EnabledState         clients.ManagedRegistrationEnabledState `json:"enabled_state"`
	Exists               bool                                    `json:"exists"`
	Settlement           clients.ManagedRegistrationSettlement   `json:"settlement"`
	ConfigSHA256         clients.ManagedRegistrationFingerprint  `json:"config_sha256"`
	EntrySHA256          clients.ManagedRegistrationFingerprint  `json:"entry_sha256"`
	TargetIdentitySHA256 clients.ManagedRegistrationFingerprint  `json:"target_identity_sha256"`
}

func managedRegistrationReportRow(generation string, row managedRegistrationJournalRow) ManagedRegistrationRow {
	snapshot := row.After
	if snapshot.ClientID == "" {
		snapshot = row.Before
	}
	return ManagedRegistrationRow{
		GenerationID:            generation,
		ClientID:                row.ClientID,
		Owner:                   row.Owner,
		Transport:               row.Transport,
		EnabledState:            row.EnabledState,
		IdentityState:           row.IdentityState,
		Exists:                  row.Exists,
		Settlement:              row.Settlement,
		Phase:                   row.Phase,
		FailureID:               row.FailureID,
		ConfigSHA256:            snapshot.ConfigSHA256,
		EntrySHA256:             snapshot.EntrySHA256,
		DesiredProjectionSHA256: snapshot.DesiredProjectionSHA256,
		ConfigPathSHA256:        snapshot.ConfigPathSHA256,
		EntryKeySHA256:          snapshot.EntryKeySHA256,
		TargetIdentitySHA256:    snapshot.TargetIdentitySHA256,
	}
}

func managedRegistrationSettledEvent(row ManagedRegistrationRow) (ManagedRegistrationEvent, bool) {
	if row.Settlement != clients.ManagedRegistrationSettlementSettled {
		return ManagedRegistrationEvent{}, false
	}
	return ManagedRegistrationEvent{
		EventID:              managedRegistrationSettledEventID,
		GenerationID:         row.GenerationID,
		ClientID:             row.ClientID,
		Owner:                row.Owner,
		Transport:            row.Transport,
		EnabledState:         row.EnabledState,
		Exists:               row.Exists,
		Settlement:           row.Settlement,
		ConfigSHA256:         row.ConfigSHA256,
		EntrySHA256:          row.EntrySHA256,
		TargetIdentitySHA256: row.TargetIdentitySHA256,
	}, true
}

func projectManagedRegistrationEntry(ctx context.Context, stateDir, server string, row ManagedRegistrationRow, now time.Time) error {
	return withManagedEntriesLockAt(stateDir, func() error {
		return projectManagedRegistrationEntryUnlocked(ctx, stateDir, server, row, now)
	})
}

func projectManagedRegistrationEntryUnlocked(ctx context.Context, stateDir, server string, row ManagedRegistrationRow, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !managedRegistrationSafeIdentifier(server) || row.ClientID == "" {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	path := filepath.Join(stateDir, managedEntriesFileLeaf)
	state := ManagedEntries{Version: managedEntriesSchemaVersion}
	raw, err := ReadStateFileInodeAnchored(path)
	if err == nil {
		if err := json.Unmarshal(raw, &state); err != nil {
			return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, err)
		}
		if state.Version == 0 {
			state.Version = managedEntriesSchemaVersion
		}
		if state.Version != managedEntriesSchemaVersion {
			return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	entry := ManagedEntry{
		Client:                  row.ClientID,
		Server:                  server,
		InstalledAt:             now.UTC(),
		Owner:                   row.Owner,
		Transport:               row.Transport,
		EnabledState:            row.EnabledState,
		IdentityState:           row.IdentityState,
		Exists:                  row.Exists,
		ConfigSHA256:            row.ConfigSHA256,
		EntrySHA256:             row.EntrySHA256,
		DesiredProjectionSHA256: row.DesiredProjectionSHA256,
		ConfigPathSHA256:        row.ConfigPathSHA256,
		EntryKeySHA256:          row.EntryKeySHA256,
		TargetIdentitySHA256:    row.TargetIdentitySHA256,
	}
	replaced := false
	for i := range state.Entries {
		if state.Entries[i].Client == entry.Client && state.Entries[i].Server == entry.Server {
			state.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		state.Entries = append(state.Entries, entry)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := WriteStateFileAtomic(path, &state); err != nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	return ctx.Err()
}

func projectAndVerifyManagedRegistrationEntry(ctx context.Context, stateDir, server string, row ManagedRegistrationRow, now time.Time) error {
	return withManagedEntriesLockAt(stateDir, func() error {
		return projectAndVerifyManagedRegistrationEntryUnlocked(ctx, stateDir, server, row, now)
	})
}

func projectAndVerifyManagedRegistrationEntryUnlocked(ctx context.Context, stateDir, server string, row ManagedRegistrationRow, now time.Time) error {
	if err := projectManagedRegistrationEntryUnlocked(ctx, stateDir, server, row, now); err != nil {
		return err
	}
	raw, err := ReadStateFileInodeAnchored(filepath.Join(stateDir, managedEntriesFileLeaf))
	if err != nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	var state ManagedEntries
	if err := json.Unmarshal(raw, &state); err != nil || state.Version != managedEntriesSchemaVersion {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, err)
	}
	matches := 0
	for _, entry := range state.Entries {
		if entry.Client != row.ClientID || entry.Server != server {
			continue
		}
		matches++
		if entry.Owner != row.Owner || entry.Transport != row.Transport || entry.EnabledState != row.EnabledState || entry.IdentityState != row.IdentityState || entry.Exists != row.Exists || entry.ConfigSHA256 != row.ConfigSHA256 || entry.EntrySHA256 != row.EntrySHA256 || entry.DesiredProjectionSHA256 != row.DesiredProjectionSHA256 || entry.ConfigPathSHA256 != row.ConfigPathSHA256 || entry.EntryKeySHA256 != row.EntryKeySHA256 || entry.TargetIdentitySHA256 != row.TargetIdentitySHA256 {
			return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
		}
	}
	if matches != 1 {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	return ctx.Err()
}

func removeAndVerifyManagedRegistrationEntry(ctx context.Context, stateDir string, row ManagedRegistrationRow) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(stateDir, managedEntriesFileLeaf)
	raw, err := ReadStateFileInodeAnchored(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	var state ManagedEntries
	if err := json.Unmarshal(raw, &state); err != nil || state.Version != managedEntriesSchemaVersion {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, err)
	}
	kept := state.Entries[:0]
	for _, entry := range state.Entries {
		if entry.Client == row.ClientID && entry.TargetIdentitySHA256 == row.TargetIdentitySHA256 && (row.ConfigPathSHA256.String() == "" || entry.ConfigPathSHA256 == row.ConfigPathSHA256 && entry.EntryKeySHA256 == row.EntryKeySHA256) {
			continue
		}
		kept = append(kept, entry)
	}
	state.Entries = kept
	if err := WriteStateFileAtomic(path, &state); err != nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureAdapterFailed, err)
	}
	raw, err = ReadStateFileInodeAnchored(path)
	if err != nil || json.Unmarshal(raw, &state) != nil {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, err)
	}
	for _, entry := range state.Entries {
		if entry.Client == row.ClientID && entry.TargetIdentitySHA256 == row.TargetIdentitySHA256 && (row.ConfigPathSHA256.String() == "" || entry.ConfigPathSHA256 == row.ConfigPathSHA256 && entry.EntryKeySHA256 == row.EntryKeySHA256) {
			return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
		}
	}
	return ctx.Err()
}
