package clients

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EntryRemovalCapability contains no config values or filesystem paths.
type EntryRemovalCapability struct {
	Version     int    `json:"version"`
	SourceToken string `json:"source_token,omitempty"`
	Supported   bool   `json:"supported"`
	Reason      string `json:"reason,omitempty"`
}

type EntryRemovalObserved struct {
	Outcome         string
	Code            string
	EffectiveAbsent bool
	AfterToken      string
}

var ErrEntryRemovalSharedSourceScope = errors.New("removal affects an unselected source owner")
var ErrEntryRemovalUnsupportedSource = errors.New("unsupported removal source")
var ErrEntryRemovalInspection = errors.New("removal source inspection failed")

// These finite bounds belong to this operation, not a deployment/user setting.
const entryRemovalLockWait = 5 * time.Second

func EntryRemovalScopeGuard(selected Client, fleet map[string]Client, name string) error {
	for id, peer := range fleet {
		if id == selected.Name() || peer == nil {
			continue
		}
		shared, err := EntryRemovalAffectsPeer(selected, peer, name)
		if err != nil {
			return errors.Join(ErrEntryRemovalInspection, err)
		}
		if shared {
			return ErrEntryRemovalSharedSourceScope
		}
	}
	return nil
}

// This narrow lexical capability does not grant historical restore/CAS rights.
type physicalEntryTokenOwner interface {
	EntryPhysicalToken([]byte, string) (*entryPhysicalToken, bool, error)
}

func (j *jsonMCPClient) EntryPhysicalToken(data []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(data, j.sectionKey(), name)
}

func (j *jsonMCPClient) removalJSONSource() *jsonMCPClient { return j }

type entryRemovalSource struct {
	token        *entryPhysicalToken
	fingerprint  string
	fileInfo     os.FileInfo
	ancestors    string
	parentHandle uintptr
	release      func()
}

func removalAncestorIdentity(path string) (string, uintptr, func(), error) {
	var components []string
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		components = append(components, parent)
		if filepath.Dir(parent) == parent {
			break
		}
	}
	var ids []string
	var closers []func()
	var parentHandle uintptr
	release := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	for i := len(components) - 1; i >= 0; i-- {
		id, handle, closeHandle, err := openRemovalDirectoryIdentity(components[i])
		if err != nil {
			release()
			return "", 0, nil, err
		}
		ids = append(ids, id)
		closers = append(closers, closeHandle)
		parentHandle = handle
	}
	return strings.Join(ids, "/"), parentHandle, release, nil
}

// EntryRemovalClientForScan binds trusted scan paths through their adapter owner.
// It is an internal composition input, never an HTTP path parameter.
func EntryRemovalClientForScan(client Client, path string) (Client, bool) {
	lc, ok := client.(*lockingClient)
	if !ok || lc == nil || lc.Client == nil {
		return nil, false
	}
	if filepath.Clean(path) == filepath.Clean(client.ConfigPath()) {
		return client, true
	}
	switch c := lc.Client.(type) {
	case interface{ removalJSONSource() *jsonMCPClient }:
		copy := *c.removalJSONSource()
		copy.path = path
		return newLockingClient(&copy), true
	case *claudeCode:
		copy := *c
		copy.path = path
		return newLockingClient(&copy), true
	case *mimoCodeClient:
		return newLockingClient(mimoCodeClientForScanPath(path)), true
	default:
		return nil, false
	}
}

func DescribeEntryRemoval(client Client, name string) EntryRemovalCapability {
	cap, _ := DescribeEntryRemovalContext(context.Background(), client, name)
	return cap
}

func DescribeEntryRemovalContext(ctx context.Context, client Client, name string) (EntryRemovalCapability, error) {
	cap := EntryRemovalCapability{Version: 1, Reason: "REMOVAL_UNSUPPORTED_SOURCE"}
	lc, ok := client.(*lockingClient)
	if !ok || lc == nil || lc.Client == nil || name == "" {
		return cap, ErrEntryRemovalUnsupportedSource
	}
	if ctx == nil {
		return cap, context.Canceled
	}
	if _, project := lc.projectCodex(); project {
		return cap, ErrEntryRemovalUnsupportedSource
	}
	_, err := withConfigLockExecutionContextMode(lc.Client.ConfigPath(), ctx, func() error {
		data, err := lc.readConfigBytesForLockedOperation()
		if err != nil {
			return err
		}
		source, err := lc.resolveEntryRemovalSource(data, name)
		if err != nil {
			return err
		}
		defer source.release()
		cap.Supported, cap.SourceToken, cap.Reason = true, source.fingerprint, ""
		return nil
	}, false)
	if errors.Is(err, ErrEntryMutationPreconditionConflict) {
		cap.Reason = "REMOVAL_SOURCE_CONFLICT"
	}
	if err != nil && !errors.Is(err, ErrEntryRemovalUnsupportedSource) && !errors.Is(err, ErrEntryMutationPreconditionConflict) {
		cap.Reason = "REMOVAL_INSPECTION_FAILED"
	}
	return cap, err
}

func (l *lockingClient) resolveEntryRemovalSource(data []byte, name string) (*entryRemovalSource, error) {
	owner, ok := l.Client.(physicalEntryTokenOwner)
	if !ok {
		return nil, ErrEntryRemovalUnsupportedSource
	}
	if _, project := l.projectCodex(); project {
		return nil, ErrEntryRemovalUnsupportedSource
	}
	path, err := filepath.Abs(l.Client.ConfigPath())
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("nonregular removal source")
	}
	file, err := openRemovalSourceFile(path)
	if err != nil {
		return nil, err
	}
	retainedFile := false
	defer func() {
		if !retainedFile {
			_ = file.Close()
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, ErrEntryMutationPreconditionConflict
	}
	openedData, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, openedData) {
		return nil, ErrEntryMutationPreconditionConflict
	}
	token, present, err := owner.EntryPhysicalToken(data, name)
	if err != nil {
		return nil, err
	}
	if !present || token == nil {
		return nil, ErrEntryMutationPreconditionConflict
	}
	if m, ok := l.Client.(*mimoCodeClient); ok {
		// The existing merge, import and managed owners decide re-emergence.
		merged, err := m.readMergedLayersExcluding(name)
		if err != nil {
			return nil, err
		}
		entries, _ := merged[mimoCodeMCPKey].(map[string]any)
		if _, survives := entries[name]; survives {
			return nil, ErrEntryRemovalUnsupportedSource
		}
		managed, err := m.mimoCodeManagedLayerReResolves(name)
		if err != nil {
			return nil, err
		}
		if managed {
			return nil, ErrEntryRemovalUnsupportedSource
		}
		linked, err := m.mimoCodeLowerLayerHardLinkedToWriteTargetDefines(name)
		if err != nil {
			return nil, err
		}
		if linked {
			return nil, ErrEntryRemovalUnsupportedSource
		}
	}
	ancestors, parentHandle, release, err := removalAncestorIdentity(path)
	if err != nil {
		return nil, err
	}
	profile := []string{l.Client.Name(), path, token.section, name, ancestors}
	if m, ok := l.Client.(*mimoCodeClient); ok {
		profile = append(profile, m.configFile, m.overlayDir, m.inlineContent, m.claudeHome, os.Getenv(mimoCodeDisableClaudeImportEnv))
		profile = append(profile, m.readLayerFiles()...)
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		release()
		return nil, err
	}
	h := sha256.New()
	_, _ = h.Write(encoded)
	_, _ = h.Write(token.targetBytes)
	retainedFile = true
	return &entryRemovalSource{token: token, fingerprint: hex.EncodeToString(h.Sum(nil)), fileInfo: openedInfo, ancestors: ancestors, parentHandle: parentHandle, release: func() { _ = file.Close(); release() }}, nil
}

func (l *lockingClient) snapshotBindingProfile() string {
	parts := []string{l.Client.Name(), l.Client.ConfigPath()}
	if m, ok := l.Client.(*mimoCodeClient); ok {
		parts = append(parts, m.configFile, m.overlayDir, m.inlineContent, m.claudeHome, os.Getenv(mimoCodeDisableClaudeImportEnv))
		parts = append(parts, m.readLayerFiles()...)
	}
	encoded, _ := json.Marshal(parts)
	return string(encoded)
}

func (l *lockingClient) newConfigWriteSnapshot(data []byte, info os.FileInfo, ancestors string, parent uintptr) (*ConfigWriteSnapshot, error) {
	path, err := filepath.Abs(l.Client.ConfigPath())
	if err != nil {
		return nil, err
	}
	profile := l.snapshotBindingProfile()
	return &ConfigWriteSnapshot{path: path, contents: append([]byte(nil), data...), fileInfo: info, parentHandle: parent, ancestors: ancestors,
		checkBinding: func(expectedAncestor string) error {
			if l.snapshotBindingProfile() != profile {
				return ErrConfigWriteSnapshotConflict
			}
			current, _, release, err := removalAncestorIdentity(path)
			if err != nil {
				return err
			}
			defer release()
			if current != expectedAncestor {
				return ErrConfigWriteSnapshotConflict
			}
			return nil
		}}, nil
}

// EntryRemovalAffectsPeer detects shared configured sources, including MiMo imports.
// The selected endpoint grants one client, so any other affected client refuses.
func EntryRemovalAffectsPeer(selected, peer Client, name string) (bool, error) {
	selectedLC, ok := selected.(*lockingClient)
	if !ok {
		return true, errors.New("unsupported selected source")
	}
	peerLC, ok := peer.(*lockingClient)
	if !ok {
		return true, errors.New("unsupported peer source")
	}
	paths := []string{peer.ConfigPath()}
	if m, ok := peerLC.Client.(*mimoCodeClient); ok {
		paths = m.readLayerFiles()
		if m.claudeHome != "" && os.Getenv(mimoCodeDisableClaudeImportEnv) == "" {
			paths = append(paths, m.claudeJSONPath())
		}
	}
	selectedInfo, err := os.Stat(selected.ConfigPath())
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !os.SameFile(selectedInfo, info) {
			continue
		}
		if _, mimo := peerLC.Client.(*mimoCodeClient); mimo {
			return true, nil
		}
		data, err := selectedLC.readConfigBytesForLockedOperation()
		if err != nil {
			return false, err
		}
		owner, ok := peerLC.Client.(physicalEntryTokenOwner)
		if !ok {
			return true, nil
		}
		peerToken, present, err := owner.EntryPhysicalToken(data, name)
		if err != nil {
			return false, err
		}
		selectedOwner, ok := selectedLC.Client.(physicalEntryTokenOwner)
		if !ok {
			return true, nil
		}
		token, _, err := selectedOwner.EntryPhysicalToken(data, name)
		if err != nil {
			return false, err
		}
		return present && token != nil && peerToken.section == token.section, nil
	}
	return false, nil
}

func RemoveEntrySelected(ctx context.Context, client Client, name, sourceToken string, keepN int, scopeGuard func() error) EntryRemovalObserved {
	result := EntryRemovalObserved{Outcome: "refused", Code: "REMOVAL_SOURCE_CONFLICT"}
	lc, ok := client.(*lockingClient)
	if !ok || ctx == nil || name == "" || sourceToken == "" {
		return result
	}
	observed := lc.ConditionalEntryMutation(ConditionalEntryMutationRequest{
		EntryName: name, ExpectedLive: func(entry *MCPEntry) bool { return entry != nil },
		Operation: EntryMutationRemove, BackupKeepN: &keepN, RemovalSourceToken: sourceToken,
		Context: ctx, LockWaitTimeout: entryRemovalLockWait,
		BeforeMutation: func(EntryMutationPreparation) error {
			if scopeGuard != nil {
				return scopeGuard()
			}
			return nil
		},
	})
	if !observed.Invoked {
		cause := errors.Join(observed.PreparationErr, observed.ObservationErr)
		switch {
		case observed.PreconditionConflict:
		case errors.Is(cause, ErrEntryRemovalUnsupportedSource):
			result.Code = "REMOVAL_UNSUPPORTED_SOURCE"
		case errors.Is(cause, context.Canceled):
			result.Outcome, result.Code = "failed", "REMOVAL_CANCELLED"
		case errors.Is(cause, context.DeadlineExceeded):
			result.Outcome, result.Code = "failed", "REMOVAL_TIMEOUT"
		case observed.ObservationErr != nil || errors.Is(cause, ErrEntryRemovalInspection):
			result.Outcome, result.Code = "failed", "REMOVAL_INSPECTION_FAILED"
		default:
			result.Outcome, result.Code = "failed", "REMOVAL_WRITE_FAILED"
		}
		if errors.Is(observed.PreparationErr, ErrEntryRemovalSharedSourceScope) {
			result.Code = "REMOVAL_SHARED_SOURCE_SCOPE"
		}
		return result
	}
	if errors.Is(observed.MutationErr, ErrConfigWriteSnapshotConflict) {
		result.Code = "REMOVAL_SOURCE_CONFLICT"
		return result
	}
	if errors.Is(observed.MutationErr, ErrConfigWriteSnapshotUnsupported) {
		result.Code = "REMOVAL_UNSUPPORTED_SOURCE"
		return result
	}
	if errors.Is(observed.MutationErr, context.Canceled) {
		result.Outcome, result.Code = "failed", "REMOVAL_CANCELLED"
		return result
	}
	if errors.Is(observed.MutationErr, context.DeadlineExceeded) {
		result.Outcome, result.Code = "failed", "REMOVAL_TIMEOUT"
		return result
	}
	if observed.MutationErr != nil || observed.ObservationErr != nil || observed.After != nil {
		result.Outcome, result.Code = "failed", "REMOVAL_WRITE_FAILED"
		if observed.After != nil {
			result.Code = "REMOVAL_EFFECTIVE_RETAINED"
		}
		if observed.RemovalInverse != nil {
			if err := observed.RemovalInverse.Restore(); err != nil {
				result.Outcome, result.Code = "rollback-conflict", "REMOVAL_ROLLBACK_CONFLICT"
			}
		} else {
			result.Outcome, result.Code = "applied-unverified", "REMOVAL_SETTLEMENT_UNVERIFIED"
		}
		return result
	}
	if !observed.ReleaseConfirmed || observed.ReleaseErr != nil {
		result.Outcome, result.Code = "applied-unverified", "REMOVAL_SETTLEMENT_UNVERIFIED"
		return result
	}
	// Re-read through the adapter after lock release, including its effective merge.
	entry, err := client.GetEntry(name)
	if err != nil || entry != nil {
		result.Outcome, result.Code = "applied-unverified", "REMOVAL_SETTLEMENT_UNVERIFIED"
		return result
	}
	h := sha256.Sum256([]byte(sourceToken + "\x00absent"))
	return EntryRemovalObserved{Outcome: "removed", Code: "REMOVAL_SETTLED", EffectiveAbsent: true, AfterToken: hex.EncodeToString(h[:])}
}

func writePhysicalRemoval(client Client, writer WriteConfigFileFunc, data []byte, token *entryPhysicalToken, expected ...ConfigWriteRequest) error {
	removed, err := removeEntryPhysical(data, token)
	if err != nil {
		return err
	}
	owner, ok := client.(physicalEntryTokenOwner)
	if !ok {
		return errors.New("unsupported physical source")
	}
	after, present, err := owner.EntryPhysicalToken(removed, token.name)
	if err != nil || present || after != nil {
		return errors.New("physical splice retained entry")
	}
	if len(expected) > 0 {
		req := expected[0]
		req.Contents = removed
		return writeConfigRequestWith(req)
	}
	return writeConfigFileWith(writer, client.ConfigPath(), removed)
}

// physicalEntryInverse is armed before writing and restores only its exact member.
type physicalEntryInverse struct {
	client    *lockingClient
	writer    WriteConfigFileFunc
	token     *entryPhysicalToken
	ancestors string
	guarded   bool
}

func (r *physicalEntryInverse) Restore() error {
	if r == nil || r.client == nil || r.token == nil {
		return ErrCleanupRestoreConflict
	}
	if !r.guarded {
		return r.client.withMutationLock(r.restoreUnderLock)
	}
	ctx, cancel := context.WithTimeout(context.Background(), entryRemovalLockWait)
	defer cancel()
	execution, err := withConfigLockExecutionContext(r.client.Client.ConfigPath(), ctx, func() error { return r.restoreUnderLockContext(ctx) })
	if !execution.releaseConfirmed || execution.releaseErr != nil {
		return errors.Join(err, execution.releaseErr, ErrCleanupRestoreConflict)
	}
	return err
}

func (r *physicalEntryInverse) restoreUnderLock() error {
	return r.restoreUnderLockContext(nil)
}

func (r *physicalEntryInverse) restoreUnderLockContext(ctx context.Context) error {
	if r.ancestors != "" {
		identity, _, release, err := removalAncestorIdentity(r.client.Client.ConfigPath())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCleanupRestoreConflict, err)
		}
		defer release()
		if identity != r.ancestors {
			return ErrCleanupRestoreConflict
		}
	}
	data, err := r.client.readConfigBytesForLockedOperation()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupRestoreConflict, err)
	}
	owner, ok := r.client.Client.(physicalEntryTokenOwner)
	if !ok {
		return ErrCleanupRestoreConflict
	}
	current, present, err := owner.EntryPhysicalToken(data, r.token.name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCleanupRestoreConflict, err)
	}
	if present {
		if entryPhysicalTokensEqual(current, r.token) {
			return nil
		}
		return ErrCleanupRestoreConflict
	}
	restored, err := restoreEntryPhysical(data, r.token)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCleanupRestoreConflict, err)
	}
	var writeErr error
	if r.guarded {
		snapshot, release, err := r.client.captureConfigWriteSnapshot(ctx, data, r.ancestors)
		if err != nil {
			return err
		}
		defer release()
		writeErr = writeConfigRequestWith(ConfigWriteRequest{Context: ctx, Path: r.client.Client.ConfigPath(), Contents: restored, ExpectedSnapshot: snapshot})
	} else {
		writeErr = writeConfigFileWith(r.writer, r.client.Client.ConfigPath(), restored)
	}
	if writeErr != nil {
		return writeErr
	}
	data, err = r.client.readConfigBytesForLockedOperation()
	if err != nil {
		return err
	}
	current, present, err = owner.EntryPhysicalToken(data, r.token.name)
	if err != nil || !present || !entryPhysicalTokensEqual(current, r.token) {
		return ErrCleanupRestoreConflict
	}
	return nil
}

func (l *lockingClient) captureConfigWriteSnapshot(ctx context.Context, data []byte, expectedAncestors string) (*ConfigWriteSnapshot, func(), error) {
	if ctx == nil {
		return nil, nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	path, err := filepath.Abs(l.Client.ConfigPath())
	if err != nil {
		return nil, nil, err
	}
	file, err := openRemovalSourceFile(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	current, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(current, data) {
		file.Close()
		return nil, nil, ErrConfigWriteSnapshotConflict
	}
	identity, parent, release, err := removalAncestorIdentity(path)
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	closeAll := func() { _ = file.Close(); release() }
	if expectedAncestors == "" || identity != expectedAncestors {
		closeAll()
		return nil, nil, ErrConfigWriteSnapshotConflict
	}
	snapshot, err := l.newConfigWriteSnapshot(data, info, identity, parent)
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	return snapshot, closeAll, nil
}
