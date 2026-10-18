//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const ownedEntrypointTaskPrefix = "mcp-local-hub-"

// OwnedEntrypointTaskTxn serializes the lossless replacement of the command
// receiver in every current-user mcp-local-hub Task Scheduler task. It owns the
// lock from inventory through forward verification or symmetric restoration.
// Callers must Close it after a successful forward verification or restore.
type OwnedEntrypointTaskTxn struct {
	backend ownedEntrypointTaskBackend
	store   RetainedTaskXMLStore
	ctx     context.Context
	owner   string
	prior   string
	runtime string
	lock    *flock.Flock

	inventory map[string]ownedEntrypointTaskSnapshot
	closed    bool
}

// These transition ports are deliberately unwired. The outer runtime owner
// supplies authentic readiness and retains custody until its own settlement.
type EntrypointTaskApply struct{ Apply bool }
type EntrypointTaskTarget string

const (
	EntrypointTaskCapturedPrior   EntrypointTaskTarget = "captured-prior"
	EntrypointTaskAdmittedRuntime EntrypointTaskTarget = "admitted-runtime"
)

type EntrypointTaskExcludedSnapshot struct {
	Target           EntrypointTaskTarget
	TaskSetSHA256    string
	DefinitionSHA256 map[string]string
	owner            *OwnedEntrypointTaskTxn
}

type EntrypointTaskReadiness struct {
	Target         EntrypointTaskTarget
	ImagePath      string
	ImageSHA256    string
	OwnerPID       int
	OwnerStartedAt string
	EvidenceSHA256 string
}

type EntrypointTaskEnableRequest struct {
	Apply                  bool
	Target                 EntrypointTaskTarget
	ExpectedImagePath      string
	ExpectedImageSHA256    string
	ExpectedOwnerPID       int
	ExpectedOwnerStartedAt string
}

type EntrypointTaskReadinessCheck func(context.Context, EntrypointTaskExcludedSnapshot) (EntrypointTaskReadiness, error)

// EntrypointTaskTransitionError is a stable refusal with its original cause.
type EntrypointTaskTransitionError struct {
	FailureID string
	Cause     error
}

func (e *EntrypointTaskTransitionError) Error() string { return e.FailureID + ": " + e.Cause.Error() }
func (e *EntrypointTaskTransitionError) Unwrap() error { return e.Cause }

func entrypointTransitionError(id string, cause error) error {
	return &EntrypointTaskTransitionError{FailureID: id, Cause: cause}
}

// validateContexts preserves both lifetimes even when the caller's operation
// context is independent of the context retained by the transaction creator.
func (t *OwnedEntrypointTaskTxn) validateContexts(ctx context.Context) error {
	if ctx == nil {
		return entrypointTransitionError("context-required", errors.New("nil context"))
	}
	if err := ctx.Err(); err != nil {
		return entrypointTransitionError("context-expired", err)
	}
	if t == nil {
		return entrypointTransitionError("inventory-required", t.requireInventory())
	}
	if t.ctx == nil {
		return entrypointTransitionError("context-required", errors.New("transaction context is nil"))
	}
	if err := t.ctx.Err(); err != nil {
		return entrypointTransitionError("context-expired", err)
	}
	return nil
}

func (t *OwnedEntrypointTaskTxn) requireTransition(ctx context.Context, apply bool) error {
	if !apply {
		return entrypointTransitionError("apply-required", errors.New("positive Apply is required"))
	}
	if err := t.validateContexts(ctx); err != nil {
		return err
	}
	if err := t.requireInventory(); err != nil {
		return entrypointTransitionError("inventory-required", err)
	}
	if t.backend == nil || t.store == nil || len(t.inventory) == 0 || strings.TrimSpace(t.owner) == "" || strings.TrimSpace(t.prior) == "" || strings.TrimSpace(t.runtime) == "" || sameWindowsEntrypointPath(t.prior, t.runtime) {
		return entrypointTransitionError("custody-required", errors.New("complete durable inventory and distinct admitted images are required"))
	}
	return nil
}

// transitionDefinitions reloads every durable original before any mutation.
// disabled selects D/E; otherwise it selects the original-enabled final F.
func (t *OwnedEntrypointTaskTxn) transitionDefinitions(ctx context.Context, target EntrypointTaskTarget, disabled bool) (map[string][]byte, error) {
	if target != EntrypointTaskCapturedPrior && target != EntrypointTaskAdmittedRuntime {
		return nil, entrypointTransitionError("target-invalid", errors.New("unknown target"))
	}
	out := make(map[string][]byte, len(t.inventory))
	for _, key := range t.sortedInventoryNames() {
		if err := ctx.Err(); err != nil {
			return nil, entrypointTransitionError("context-expired", err)
		}
		s := t.inventory[key]
		if canonicalOwnedEntrypointTaskName(s.name) != key || canonicalOwnedEntrypointTaskName(s.backup.TaskName) != key || strings.TrimSpace(s.backup.Ref) == "" || !validEntrypointDigest(s.backup.SHA256) || sha256Hex(s.xml) != s.backup.SHA256 {
			return nil, entrypointTransitionError("backup-invalid", fmt.Errorf("incomplete backup for %q", key))
		}
		raw, err := t.store.Load(ctx, s.backup.Ref)
		if err != nil {
			return nil, entrypointTransitionError("backup-load", fmt.Errorf("%q: %w", key, err))
		}
		if !bytes.Equal(raw, s.xml) {
			return nil, entrypointTransitionError("backup-drift", fmt.Errorf("retained original changed for %q", key))
		}
		shape, err := parseOwnedEntrypointTaskXML(raw)
		if err != nil {
			return nil, entrypointTransitionError("original-invalid", err)
		}
		if shape.enabledError != nil {
			return nil, entrypointTransitionError("enabled-invalid", fmt.Errorf("%q: %w", key, shape.enabledError))
		}
		if shape.uri != key || !sameWindowsEntrypointPath(shape.command, t.prior) {
			return nil, entrypointTransitionError("original-invalid", fmt.Errorf("captured prior identity mismatch for %q", key))
		}
		if disabled && shape.enabled {
			raw = append(append(append([]byte(nil), raw[:shape.enabledStart]...), []byte("false")...), raw[shape.enabledEnd:]...)
		}
		if target == EntrypointTaskAdmittedRuntime {
			raw, err = rewriteOwnedEntrypointTaskCommand(raw, t.prior, t.runtime)
			if err != nil {
				return nil, entrypointTransitionError("command-invalid", err)
			}
		}
		out[key] = append([]byte(nil), raw...)
	}
	return out, nil
}

// transitionLiveSet checks the exact frozen population, including rows the
// ordinary V3 path intentionally ignores for an operator command.
func (t *OwnedEntrypointTaskTxn) transitionLiveSet() error {
	statuses, err := t.backend.List(ownedEntrypointTaskPrefix)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		name := canonicalOwnedEntrypointTaskName(s.Name)
		if seen[name] || !strings.HasPrefix(name, `\mcp-local-hub-`) || !sameWindowsUser(s.Owner, t.owner) {
			return fmt.Errorf("invalid or duplicate live task %q", name)
		}
		if _, ok := t.inventory[name]; !ok {
			return fmt.Errorf("unknown live task %q", name)
		}
		seen[name] = true
	}
	if len(seen) != len(t.inventory) {
		return errors.New("task set changed")
	}
	return nil
}

func (t *OwnedEntrypointTaskTxn) verifyTransitionDefinitions(ctx context.Context, target EntrypointTaskTarget, expected map[string][]byte) (EntrypointTaskExcludedSnapshot, error) {
	if err := t.transitionLiveSet(); err != nil {
		return EntrypointTaskExcludedSnapshot{}, entrypointTransitionError("task-set-drift", err)
	}
	snapshot := EntrypointTaskExcludedSnapshot{Target: target, DefinitionSHA256: make(map[string]string, len(expected)), owner: t}
	rows := make([]ownedEntrypointTaskDigestRow, 0, len(expected))
	for _, key := range t.sortedInventoryNames() {
		if err := ctx.Err(); err != nil {
			return EntrypointTaskExcludedSnapshot{}, entrypointTransitionError("context-expired", err)
		}
		raw, err := t.backend.ExportXML(t.inventory[key].name)
		if err != nil {
			return EntrypointTaskExcludedSnapshot{}, entrypointTransitionError("readback-failed", err)
		}
		if !bytes.Equal(raw, expected[key]) {
			return EntrypointTaskExcludedSnapshot{}, entrypointTransitionError("definition-drift", fmt.Errorf("task %q is not byte-exact", key))
		}
		hash := sha256Hex(raw)
		snapshot.DefinitionSHA256[key] = hash
		rows = append(rows, ownedEntrypointTaskDigestRow{TaskName: key, XMLSHA256: hash})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return EntrypointTaskExcludedSnapshot{}, entrypointTransitionError("digest-encoding", err)
	}
	snapshot.TaskSetSHA256 = sha256Hex(raw)
	return snapshot, nil
}

func copyEntrypointExcludedSnapshot(s EntrypointTaskExcludedSnapshot) EntrypointTaskExcludedSnapshot {
	copy := s
	copy.DefinitionSHA256 = make(map[string]string, len(s.DefinitionSHA256))
	for k, v := range s.DefinitionSHA256 {
		copy.DefinitionSHA256[k] = v
	}
	return copy
}

func sameEntrypointExcludedSnapshot(a, b EntrypointTaskExcludedSnapshot) bool {
	if a.owner != b.owner || a.Target != b.Target || a.TaskSetSHA256 != b.TaskSetSHA256 || len(a.DefinitionSHA256) != len(b.DefinitionSHA256) {
		return false
	}
	for k, v := range b.DefinitionSHA256 {
		if a.DefinitionSHA256[k] != v {
			return false
		}
	}
	return true
}

func (t *OwnedEntrypointTaskTxn) DisableCapturedVerified(ctx context.Context, request EntrypointTaskApply) (EntrypointTaskExcludedSnapshot, error) {
	if err := t.requireTransition(ctx, request.Apply); err != nil {
		return EntrypointTaskExcludedSnapshot{}, err
	}
	original, err := t.transitionDefinitions(ctx, EntrypointTaskCapturedPrior, false)
	if err != nil {
		return EntrypointTaskExcludedSnapshot{}, err
	}
	disabled, err := t.transitionDefinitions(ctx, EntrypointTaskCapturedPrior, true)
	if err != nil {
		return EntrypointTaskExcludedSnapshot{}, err
	}
	if _, err := t.verifyTransitionDefinitions(ctx, EntrypointTaskCapturedPrior, original); err != nil {
		return EntrypointTaskExcludedSnapshot{}, err
	}
	return t.importExcluded(ctx, disabled)
}

func (t *OwnedEntrypointTaskTxn) RestoreCapturedWhileDisabled(ctx context.Context, request EntrypointTaskApply) (EntrypointTaskExcludedSnapshot, error) {
	if err := t.requireTransition(ctx, request.Apply); err != nil {
		return EntrypointTaskExcludedSnapshot{}, err
	}
	disabled, err := t.transitionDefinitions(ctx, EntrypointTaskCapturedPrior, true)
	if err != nil {
		return EntrypointTaskExcludedSnapshot{}, err
	}
	if err := t.transitionLiveSet(); err != nil {
		return EntrypointTaskExcludedSnapshot{}, entrypointTransitionError("task-set-drift", err)
	}
	return t.importExcluded(ctx, disabled)
}

func (t *OwnedEntrypointTaskTxn) importExcluded(ctx context.Context, disabled map[string][]byte) (EntrypointTaskExcludedSnapshot, error) {
	var errs []error
	for _, key := range t.sortedInventoryNames() {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := t.backend.ImportXML(t.inventory[key].name, disabled[key]); err != nil {
			errs = append(errs, fmt.Errorf("disable import %q: %w", key, err))
		}
	}
	snapshot, err := t.verifyTransitionDefinitions(ctx, EntrypointTaskCapturedPrior, disabled)
	if err != nil {
		errs = append(errs, err)
	}
	if cause := errors.Join(errs...); cause != nil {
		return EntrypointTaskExcludedSnapshot{}, &EntrypointTaskPartialProgressError{Cause: entrypointTransitionError("excluded-partial-progress", cause)}
	}
	return copyEntrypointExcludedSnapshot(snapshot), nil
}

func (t *OwnedEntrypointTaskTxn) VerifyExcludedSHA256(target EntrypointTaskTarget) (EntrypointTaskExcludedSnapshot, error) {
	if t == nil {
		return EntrypointTaskExcludedSnapshot{}, entrypointTransitionError("inventory-required", errors.New("nil transaction"))
	}
	if err := t.requireTransition(t.ctx, true); err != nil {
		return EntrypointTaskExcludedSnapshot{}, err
	}
	expected, err := t.transitionDefinitions(t.ctx, target, true)
	if err != nil {
		return EntrypointTaskExcludedSnapshot{}, err
	}
	snapshot, err := t.verifyTransitionDefinitions(t.ctx, target, expected)
	return copyEntrypointExcludedSnapshot(snapshot), err
}

func validEntrypointDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return false
	}
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}

func validEntrypointStartedAt(s string) bool {
	v, err := time.Parse("2006-01-02T15:04:05Z", s)
	return err == nil && v.Year() > 0 && !v.IsZero() && v.Format("2006-01-02T15:04:05Z") == s
}

func (t *OwnedEntrypointTaskTxn) RestoreOriginalEnabledAfterReady(ctx context.Context, request EntrypointTaskEnableRequest, supplied EntrypointTaskExcludedSnapshot, check EntrypointTaskReadinessCheck) (string, error) {
	if err := t.requireTransition(ctx, request.Apply); err != nil {
		return "", err
	}
	excluded, err := t.transitionDefinitions(ctx, request.Target, true)
	if err != nil {
		return "", err
	}
	final, err := t.transitionDefinitions(ctx, request.Target, false)
	if err != nil {
		return "", err
	}
	path := t.prior
	if request.Target == EntrypointTaskAdmittedRuntime {
		path = t.runtime
	}
	if !sameWindowsEntrypointPath(request.ExpectedImagePath, path) || !validEntrypointDigest(request.ExpectedImageSHA256) || request.ExpectedOwnerPID <= 0 || !validEntrypointStartedAt(request.ExpectedOwnerStartedAt) || check == nil {
		return "", entrypointTransitionError("readiness-required", errors.New("complete target-bound readiness request and verifier are required"))
	}
	bound, err := t.verifyTransitionDefinitions(ctx, request.Target, excluded)
	if err != nil {
		return "", err
	}
	if !sameEntrypointExcludedSnapshot(supplied, bound) {
		return "", entrypointTransitionError("snapshot-invalid", errors.New("excluded snapshot is not bound to this transaction"))
	}
	ready, err := check(ctx, copyEntrypointExcludedSnapshot(bound))
	if err != nil {
		return "", entrypointTransitionError("readiness-failed", err)
	}
	if err := t.validateContexts(ctx); err != nil {
		return "", err
	}
	if ready.Target != request.Target || ready.ImagePath != request.ExpectedImagePath || ready.ImageSHA256 != request.ExpectedImageSHA256 || ready.OwnerPID != request.ExpectedOwnerPID || ready.OwnerStartedAt != request.ExpectedOwnerStartedAt || !validEntrypointDigest(ready.EvidenceSHA256) {
		return "", entrypointTransitionError("readiness-mismatch", errors.New("readiness does not match the complete expected generation"))
	}
	// This second readback is the post-callback drift barrier, not a runtime proof.
	if _, err := t.verifyTransitionDefinitions(ctx, request.Target, excluded); err != nil {
		return "", err
	}
	for _, key := range t.sortedInventoryNames() {
		if err := t.validateContexts(ctx); err != nil {
			return "", t.compensateExcluded(excluded, final, err)
		}
		if err := t.backend.ImportXML(t.inventory[key].name, final[key]); err != nil {
			return "", t.compensateExcluded(excluded, final, fmt.Errorf("final import %q: %w", key, err))
		}
	}
	verified, err := t.verifyTransitionDefinitions(ctx, request.Target, final)
	if err != nil {
		return "", t.compensateExcluded(excluded, final, err)
	}
	if err := t.validateContexts(ctx); err != nil {
		return "", t.compensateExcluded(excluded, final, err)
	}
	return verified.TaskSetSHA256, nil
}

func (t *OwnedEntrypointTaskTxn) compensateExcluded(excluded, final map[string][]byte, cause error) error {
	errs := []error{cause}
	for _, key := range t.sortedInventoryNames() {
		raw, err := t.backend.ExportXML(t.inventory[key].name)
		if err != nil {
			errs = append(errs, fmt.Errorf("compensation export %q: %w", key, err))
			continue
		}
		if !bytes.Equal(raw, excluded[key]) && !bytes.Equal(raw, final[key]) {
			errs = append(errs, fmt.Errorf("compensation preserves unknown drift in %q", key))
			continue
		}
		if !bytes.Equal(raw, excluded[key]) {
			if err := t.backend.ImportXML(t.inventory[key].name, excluded[key]); err != nil {
				errs = append(errs, fmt.Errorf("compensation import %q: %w", key, err))
			}
		}
	}
	// Cancellation does not erase the obligation to observe attempted cleanup.
	if _, err := t.verifyTransitionDefinitions(context.WithoutCancel(t.ctx), "", excluded); err != nil {
		errs = append(errs, err)
	}
	return &EntrypointTaskPartialProgressError{Cause: entrypointTransitionError("final-partial-progress", errors.Join(errs...))}
}

type ownedEntrypointTaskBackend interface {
	List(prefix string) ([]TaskStatus, error)
	ExportXML(name string) ([]byte, error)
	ImportXML(name string, xml []byte) error
}

type ownedEntrypointTaskSnapshot struct {
	name   string
	xml    []byte
	backup OwnedEntrypointTaskBackup
}

// RetainedTaskXMLStore is the secure, owner-DACL artifact boundary. Refs are
// opaque to Scheduler and are the only values suitable for JournalV1.Backups.
type RetainedTaskXMLStore interface {
	Retain(ctx context.Context, xml []byte) (opaqueRef string, err error)
	Load(ctx context.Context, opaqueRef string) ([]byte, error)
}

// OwnedEntrypointTaskBackup is journal-safe task recovery metadata. It never
// carries XML bytes, arguments, paths, or other task content.
type OwnedEntrypointTaskBackup struct {
	TaskName string
	Ref      string
	SHA256   string
}

// OwnedEntrypointTaskReference is the read-only inventory record for a task
// whose executable remains under mcphub's managed entrypoint pair.  It carries
// no XML or arguments, so callers cannot use the scan as a task mutation path.
type OwnedEntrypointTaskReference struct {
	TaskName string
	Command  string
}

// ScanOwnedEntrypointTaskReferences returns only current-user hub task
// commands that equal either member of the admitted CLI/runtime pair.  Other
// current-user tasks using the hub name prefix but an operator executable are
// explicitly outside this transaction and are not returned or changed.
func ScanOwnedEntrypointTaskReferences(ctx context.Context, operatorCLI, runtime string) ([]OwnedEntrypointTaskReference, error) {
	backend, err := New()
	if err != nil {
		return nil, fmt.Errorf("entrypoint task scan: scheduler: %w", err)
	}
	owner, err := currentWindowsTaskOwner()
	if err != nil {
		return nil, err
	}
	return scanOwnedEntrypointTaskReferences(ctx, backend, owner, operatorCLI, runtime)
}

func scanOwnedEntrypointTaskReferences(ctx context.Context, backend ownedEntrypointTaskBackend, owner, operatorCLI, runtime string) ([]OwnedEntrypointTaskReference, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if backend == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(operatorCLI) == "" || strings.TrimSpace(runtime) == "" || operatorCLI == runtime {
		return nil, errors.New("entrypoint task scan: invalid admitted pair")
	}
	statuses, err := backend.List(ownedEntrypointTaskPrefix)
	if err != nil {
		return nil, fmt.Errorf("entrypoint task scan: list: %w", err)
	}
	sort.Slice(statuses, func(i, j int) bool {
		return canonicalOwnedEntrypointTaskName(statuses[i].Name) < canonicalOwnedEntrypointTaskName(statuses[j].Name)
	})
	refs := make([]OwnedEntrypointTaskReference, 0, len(statuses))
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := canonicalOwnedEntrypointTaskName(status.Name)
		if !strings.HasPrefix(name, `\mcp-local-hub-`) || !sameWindowsUser(status.Owner, owner) {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("entrypoint task scan: duplicate task %q", name)
		}
		seen[name] = struct{}{}
		raw, err := backend.ExportXML(status.Name)
		if err != nil {
			return nil, fmt.Errorf("entrypoint task scan: export %q: %w", name, err)
		}
		shape, err := parseOwnedEntrypointTaskXML(raw)
		if err != nil || shape.uri != name {
			return nil, fmt.Errorf("entrypoint task scan: invalid owned task %q", name)
		}
		if sameWindowsEntrypointPath(shape.command, operatorCLI) || sameWindowsEntrypointPath(shape.command, runtime) {
			refs = append(refs, OwnedEntrypointTaskReference{TaskName: name, Command: shape.command})
		}
	}
	return refs, nil
}

// BeginOwnedEntrypointTaskTxn acquires the one lock used for the complete
// current-user task migration. A secure retained-artifact store is mandatory:
// no production transaction can retain XML only in memory.
func BeginOwnedEntrypointTaskTxn(ctx context.Context, lockPath, priorCLI, runtime string, store RetainedTaskXMLStore) (*OwnedEntrypointTaskTxn, error) {
	backend, err := New()
	if err != nil {
		return nil, fmt.Errorf("entrypoint task scheduler: %w", err)
	}
	owner, err := currentWindowsTaskOwner()
	if err != nil {
		return nil, err
	}
	return beginOwnedEntrypointTaskTxnWithStore(ctx, lockPath, priorCLI, runtime, backend, owner, store)
}

// BeginOwnedEntrypointTaskTxnWithStore is the durable production entrypoint.
// The caller supplies the secure retained-artifact owner; Scheduler retains and
// reloads only opaque references and SHA-256 digests.
func BeginOwnedEntrypointTaskTxnWithStore(ctx context.Context, lockPath, priorCLI, runtime string, store RetainedTaskXMLStore) (*OwnedEntrypointTaskTxn, error) {
	return BeginOwnedEntrypointTaskTxn(ctx, lockPath, priorCLI, runtime, store)
}

func beginOwnedEntrypointTaskTxnWith(ctx context.Context, lockPath, priorCLI, runtime string, backend ownedEntrypointTaskBackend, owner string) (*OwnedEntrypointTaskTxn, error) {
	if backend == nil {
		return nil, errors.New("entrypoint task transaction: scheduler backend is required")
	}
	if strings.TrimSpace(lockPath) == "" {
		return nil, errors.New("entrypoint task transaction: lock path is required")
	}
	if priorCLI == "" || runtime == "" || priorCLI == runtime {
		return nil, errors.New("entrypoint task transaction: distinct prior and runtime paths are required")
	}
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("entrypoint task transaction: current owner is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("entrypoint task transaction lock: %w", err)
	}
	if !locked {
		_ = lock.Close()
		return nil, fmt.Errorf("entrypoint task transaction lock is held: %s", lockPath)
	}
	return &OwnedEntrypointTaskTxn{
		backend: backend,
		ctx:     ctx,
		owner:   owner,
		prior:   priorCLI,
		runtime: runtime,
		lock:    lock,
	}, nil
}

func beginOwnedEntrypointTaskTxnWithStore(ctx context.Context, lockPath, priorCLI, runtime string, backend ownedEntrypointTaskBackend, owner string, store RetainedTaskXMLStore) (*OwnedEntrypointTaskTxn, error) {
	if store == nil {
		return nil, errors.New("entrypoint task transaction: retained XML store is required")
	}
	txn, err := beginOwnedEntrypointTaskTxnWith(ctx, lockPath, priorCLI, runtime, backend, owner)
	if err != nil {
		return nil, err
	}
	txn.store = store
	return txn, nil
}

// ReopenOwnedEntrypointTaskTxnWithStore reacquires the same lock after a crash,
// validates retained XML bodies against their journal metadata, and returns a
// transaction that may only restore through the ordinary Import/readback path.
func ReopenOwnedEntrypointTaskTxnWithStore(ctx context.Context, lockPath, priorCLI, runtime string, store RetainedTaskXMLStore, backups []OwnedEntrypointTaskBackup) (*OwnedEntrypointTaskTxn, error) {
	backend, err := New()
	if err != nil {
		return nil, fmt.Errorf("entrypoint task scheduler: %w", err)
	}
	owner, err := currentWindowsTaskOwner()
	if err != nil {
		return nil, err
	}
	return reopenOwnedEntrypointTaskTxnWithStore(ctx, lockPath, priorCLI, runtime, backend, owner, store, backups)
}

func reopenOwnedEntrypointTaskTxnWithStore(ctx context.Context, lockPath, priorCLI, runtime string, backend ownedEntrypointTaskBackend, owner string, store RetainedTaskXMLStore, backups []OwnedEntrypointTaskBackup) (*OwnedEntrypointTaskTxn, error) {
	txn, err := beginOwnedEntrypointTaskTxnWithStore(ctx, lockPath, priorCLI, runtime, backend, owner, store)
	if err != nil {
		return nil, err
	}
	inventory := make(map[string]ownedEntrypointTaskSnapshot, len(backups))
	for _, backup := range backups {
		name := canonicalOwnedEntrypointTaskName(backup.TaskName)
		if !strings.HasPrefix(name, `\mcp-local-hub-`) || backup.Ref == "" || backup.SHA256 == "" {
			_ = txn.Close()
			return nil, errors.New("entrypoint task reopen: invalid backup metadata")
		}
		if _, exists := inventory[name]; exists {
			_ = txn.Close()
			return nil, fmt.Errorf("entrypoint task reopen: duplicate backup %q", name)
		}
		raw, loadErr := store.Load(txn.ctx, backup.Ref)
		if loadErr != nil {
			return nil, errors.Join(fmt.Errorf("entrypoint task reopen: retained XML load failed for %q: %w", name, loadErr), txn.Close())
		}
		if sha256Hex(raw) != backup.SHA256 {
			_ = txn.Close()
			return nil, fmt.Errorf("entrypoint task reopen: retained XML verification failed for %q", name)
		}
		shape, parseErr := parseOwnedEntrypointTaskXML(raw)
		if parseErr != nil || shape.uri != name || (!sameWindowsEntrypointPath(shape.command, priorCLI) && !sameWindowsEntrypointPath(shape.command, runtime)) {
			_ = txn.Close()
			return nil, fmt.Errorf("entrypoint task reopen: retained XML is invalid for %q", name)
		}
		inventory[name] = ownedEntrypointTaskSnapshot{name: backup.TaskName, xml: append([]byte(nil), raw...), backup: backup}
	}
	txn.inventory = inventory
	return txn, nil
}

func currentWindowsTaskOwner() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("entrypoint task current user: %w", err)
	}
	name := strings.TrimSpace(u.Username)
	if i := strings.LastIndex(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "", errors.New("entrypoint task current user is empty")
	}
	return name, nil
}

// sameWindowsEntrypointPath compares only Windows path casing. It deliberately
// does not clean, resolve, or otherwise normalize different path targets: the
// migration may rewrite only the two exact installed executable identities.
func sameWindowsEntrypointPath(left, right string) bool {
	return strings.EqualFold(left, right)
}

// InventoryExport snapshots every owned task before mutation. It refuses a
// duplicate, foreign, unreadable, malformed, or non-prior command receiver.
func (t *OwnedEntrypointTaskTxn) InventoryExport() error {
	if err := t.requireOpen(); err != nil {
		return err
	}
	if t.inventory != nil {
		return errors.New("entrypoint task transaction: inventory already captured")
	}
	statuses, err := t.backend.List(ownedEntrypointTaskPrefix)
	if err != nil {
		return fmt.Errorf("entrypoint task inventory: %w", err)
	}
	sort.Slice(statuses, func(i, j int) bool {
		return canonicalOwnedEntrypointTaskName(statuses[i].Name) < canonicalOwnedEntrypointTaskName(statuses[j].Name)
	})

	inventory := make(map[string]ownedEntrypointTaskSnapshot, len(statuses))
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		name := canonicalOwnedEntrypointTaskName(status.Name)
		if !strings.HasPrefix(name, `\mcp-local-hub-`) {
			return fmt.Errorf("entrypoint task inventory: non-hub task %q", name)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("entrypoint task inventory: duplicate task %q", name)
		}
		seen[name] = struct{}{}
		if !sameWindowsUser(status.Owner, t.owner) {
			return fmt.Errorf("entrypoint task inventory: foreign owner for %q", name)
		}
		raw, err := t.backend.ExportXML(status.Name)
		if err != nil {
			return fmt.Errorf("entrypoint task inventory export %q: %w", name, err)
		}
		shape, err := parseOwnedEntrypointTaskXML(raw)
		if err != nil {
			return fmt.Errorf("entrypoint task inventory parse %q: %w", name, err)
		}
		if shape.uri != name {
			return fmt.Errorf("entrypoint task inventory: XML URI mismatch for %q", name)
		}
		if !sameWindowsEntrypointPath(shape.command, t.prior) && !sameWindowsEntrypointPath(shape.command, t.runtime) {
			continue
		}
		snapshot := ownedEntrypointTaskSnapshot{name: status.Name, xml: append([]byte(nil), raw...)}
		if t.store != nil {
			ref, retainErr := t.store.Retain(t.ctx, snapshot.xml)
			if retainErr != nil {
				return fmt.Errorf("entrypoint task inventory retain %q: %w", name, retainErr)
			}
			if strings.TrimSpace(ref) == "" {
				return fmt.Errorf("entrypoint task inventory retain %q: retained XML unavailable", name)
			}
			snapshot.backup = OwnedEntrypointTaskBackup{TaskName: status.Name, Ref: ref, SHA256: sha256Hex(snapshot.xml)}
		}
		inventory[name] = snapshot
	}
	t.inventory = inventory
	return nil
}

// Backups returns the deterministic opaque refs/hashes that the caller may
// store in JournalV1. Nil is returned for the non-durable test seam.
func (t *OwnedEntrypointTaskTxn) Backups() []OwnedEntrypointTaskBackup {
	if t == nil || t.inventory == nil {
		return nil
	}
	backups := make([]OwnedEntrypointTaskBackup, 0, len(t.inventory))
	for _, name := range t.sortedInventoryNames() {
		backup := t.inventory[name].backup
		if backup.Ref != "" {
			backups = append(backups, backup)
		}
	}
	return backups
}

// RewriteCommand updates only the text inside the one Exec Command XML node.
// A failed write can have partially applied, so it returns a typed error and
// leaves the captured state untouched for the bundle coordinator to restore.
func (t *OwnedEntrypointTaskTxn) RewriteCommand() error {
	if err := t.requireInventory(); err != nil {
		return err
	}
	for _, key := range t.sortedInventoryNames() {
		snapshot := t.inventory[key]
		shape, err := parseOwnedEntrypointTaskXML(snapshot.xml)
		if err != nil {
			return &EntrypointTaskPartialProgressError{Cause: fmt.Errorf("entrypoint task rewrite %q: %w", key, err)}
		}
		if sameWindowsEntrypointPath(shape.command, t.runtime) {
			continue
		}
		rewritten, err := rewriteOwnedEntrypointTaskCommand(snapshot.xml, t.prior, t.runtime)
		if err != nil {
			return &EntrypointTaskPartialProgressError{Cause: fmt.Errorf("entrypoint task rewrite %q: %w", key, err)}
		}
		if err := t.backend.ImportXML(snapshot.name, rewritten); err != nil {
			return &EntrypointTaskPartialProgressError{Cause: fmt.Errorf("entrypoint task import %q: %w", key, err)}
		}
	}
	return nil
}

// VerifyRuntime requires the exact task set, the runtime command in each task,
// and byte-equivalence outside that Command node. A failed verification leaves
// restoration to the bundle coordinator that owns the transaction settlement.
func (t *OwnedEntrypointTaskTxn) VerifyRuntime() error {
	_, err := t.VerifyRuntimeSHA256()
	return err
}

type ownedEntrypointTaskDigestRow struct {
	TaskName  string `json:"task_name"`
	XMLSHA256 string `json:"xml_sha256"`
}

// VerifyRuntimeSHA256 proves the command-only rewrite and exact live task set,
// then digests the verified rows from that same export pass.
func (t *OwnedEntrypointTaskTxn) VerifyRuntimeSHA256() (string, error) {
	if err := t.requireInventory(); err != nil {
		return "", err
	}
	rows, err := t.verify(func(shape ownedEntrypointTaskShape, snapshot ownedEntrypointTaskSnapshot, raw []byte) error {
		if !sameWindowsEntrypointPath(shape.command, t.runtime) {
			return errors.New("runtime command mismatch")
		}
		priorShape, err := parseOwnedEntrypointTaskXML(snapshot.xml)
		if err != nil {
			return err
		}
		expected := snapshot.xml
		if sameWindowsEntrypointPath(priorShape.command, t.prior) {
			expected, err = rewriteOwnedEntrypointTaskCommand(snapshot.xml, t.prior, t.runtime)
			if err != nil {
				return err
			}
		}
		if !bytes.Equal(raw, expected) {
			return errors.New("XML differs outside the Command node")
		}
		return nil
	})
	if err != nil {
		return "", &EntrypointTaskPartialProgressError{Cause: fmt.Errorf("entrypoint task runtime verification: %w", err)}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return "", fmt.Errorf("entrypoint task digest encoding: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// RestoreImport imports every captured raw XML body, then performs the
// symmetric set/readback check. It deliberately continues after an import
// failure so a later task is never abandoned without a restore attempt.
func (t *OwnedEntrypointTaskTxn) RestoreImport() error {
	if err := t.requireInventory(); err != nil {
		return err
	}
	var errs []error
	for _, key := range t.sortedInventoryNames() {
		snapshot := t.inventory[key]
		if err := t.backend.ImportXML(snapshot.name, snapshot.xml); err != nil {
			errs = append(errs, fmt.Errorf("entrypoint task restore import %q: %w", key, err))
		}
	}
	if _, err := t.verify(func(_ ownedEntrypointTaskShape, snapshot ownedEntrypointTaskSnapshot, raw []byte) error {
		if !bytes.Equal(raw, snapshot.xml) {
			return errors.New("restored XML is not byte-exact")
		}
		return nil
	}); err != nil {
		errs = append(errs, fmt.Errorf("entrypoint task restore verification: %w", err))
	}
	return errors.Join(errs...)
}

// Close releases the cross-process lock. It is idempotent and must be called
// only after the caller has reached a verified forward or restore terminal path.
func (t *OwnedEntrypointTaskTxn) Close() error {
	if t == nil || t.closed {
		return nil
	}
	t.closed = true
	if err := t.lock.Close(); err != nil {
		return fmt.Errorf("entrypoint task transaction unlock: %w", err)
	}
	return nil
}

// EntrypointTaskPartialProgressError tells the transaction coordinator that a
// receiver write may already have changed one or more owned task rows. The
// owner deliberately performs no compensation itself: exactly one coordinator
// issues the guarded restore directive and records its receipt.
type EntrypointTaskPartialProgressError struct{ Cause error }

func (e *EntrypointTaskPartialProgressError) Error() string {
	if e == nil || e.Cause == nil {
		return "entrypoint task transaction partially progressed"
	}
	return e.Cause.Error()
}

func (e *EntrypointTaskPartialProgressError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (t *OwnedEntrypointTaskTxn) verify(check func(ownedEntrypointTaskShape, ownedEntrypointTaskSnapshot, []byte) error) ([]ownedEntrypointTaskDigestRow, error) {
	statuses, err := t.backend.List(ownedEntrypointTaskPrefix)
	if err != nil {
		return nil, fmt.Errorf("list owned tasks: %w", err)
	}
	type liveTask struct {
		shape ownedEntrypointTaskShape
		raw   []byte
	}
	live := make(map[string]liveTask, len(statuses))
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		name := canonicalOwnedEntrypointTaskName(status.Name)
		if !strings.HasPrefix(name, `\mcp-local-hub-`) {
			return nil, fmt.Errorf("non-hub task %q", name)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate task %q", name)
		}
		seen[name] = struct{}{}
		if !sameWindowsUser(status.Owner, t.owner) {
			return nil, fmt.Errorf("foreign owner for %q", name)
		}
		raw, err := t.backend.ExportXML(status.Name)
		if err != nil {
			return nil, fmt.Errorf("export %q: %w", name, err)
		}
		shape, err := parseOwnedEntrypointTaskXML(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", name, err)
		}
		if shape.uri != name {
			return nil, fmt.Errorf("XML URI mismatch for %q", name)
		}
		if !sameWindowsEntrypointPath(shape.command, t.prior) && !sameWindowsEntrypointPath(shape.command, t.runtime) {
			continue
		}
		live[name] = liveTask{shape: shape, raw: raw}
	}
	if len(live) != len(t.inventory) {
		return nil, errors.New("task set changed")
	}
	rows := make([]ownedEntrypointTaskDigestRow, 0, len(t.inventory))
	for _, key := range t.sortedInventoryNames() {
		snapshot := t.inventory[key]
		item, exists := live[key]
		if !exists {
			return nil, fmt.Errorf("task %q is missing", key)
		}
		if err := check(item.shape, snapshot, item.raw); err != nil {
			return nil, fmt.Errorf("task %q: %w", key, err)
		}
		rows = append(rows, ownedEntrypointTaskDigestRow{TaskName: key, XMLSHA256: sha256Hex(item.raw)})
	}
	return rows, nil
}

func (t *OwnedEntrypointTaskTxn) sortedInventoryNames() []string {
	names := make([]string, 0, len(t.inventory))
	for name := range t.inventory {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (t *OwnedEntrypointTaskTxn) requireOpen() error {
	if t == nil || t.closed {
		return errors.New("entrypoint task transaction is closed")
	}
	return nil
}

func (t *OwnedEntrypointTaskTxn) requireInventory() error {
	if err := t.requireOpen(); err != nil {
		return err
	}
	if t.inventory == nil {
		return errors.New("entrypoint task transaction inventory is required")
	}
	return nil
}

func canonicalOwnedEntrypointTaskName(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, `\`) {
		name = `\` + name
	}
	return name
}

type ownedEntrypointTaskShape struct {
	uri          string
	command      string
	commandStart int
	commandEnd   int
	enabled      bool
	enabledStart int
	enabledEnd   int
	enabledError error
}

func parseOwnedEntrypointTaskXML(raw []byte) (ownedEntrypointTaskShape, error) {
	body, base, err := taskXMLBody(raw)
	if err != nil {
		return ownedEntrypointTaskShape{}, err
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = true
	dec.Entity = nil
	dec.CharsetReader = nil

	var (
		stack                                                 []xml.Name
		actionsElements                                       int
		actionCount                                           int
		execCount                                             int
		commandCount                                          int
		uriCount                                              int
		uriActive, commandActive                              bool
		commandNested                                         bool
		uri, command                                          strings.Builder
		shape                                                 ownedEntrypointTaskShape
		taskCount, settingsCount, enabledCount                int
		enabledActive, enabledNested, enabledNamespaceInvalid bool
		enabled                                               strings.Builder
	)
	for {
		token, tokenErr := dec.Token()
		if tokenErr == nil {
			switch value := token.(type) {
			case xml.StartElement:
				if enabledActive {
					enabledNested = true
				}
				if len(stack) == 0 && value.Name.Local == "Task" {
					taskCount++
					if value.Name.Space != "http://schemas.microsoft.com/windows/2004/02/mit/task" {
						enabledNamespaceInvalid = true
					}
				}
				if len(stack) == 1 && stack[0].Local == "Task" && value.Name.Local == "Settings" {
					settingsCount++
					if value.Name.Space != stack[0].Space {
						enabledNamespaceInvalid = true
					}
				}
				if len(stack) == 2 && stack[0].Local == "Task" && stack[1].Local == "Settings" && value.Name.Local == "Enabled" {
					enabledCount++
					enabledActive = true
					shape.enabledStart = base + int(dec.InputOffset())
					if value.Name.Space != stack[0].Space || stack[1].Space != stack[0].Space {
						enabledNamespaceInvalid = true
					}
				}
				parent := xml.Name{}
				if len(stack) > 0 {
					parent = stack[len(stack)-1]
				}
				if parent.Local == "Task" && value.Name.Local == "Actions" {
					actionsElements++
				}
				if parent.Local == "Actions" {
					actionCount++
					if value.Name.Local == "Exec" {
						execCount++
					}
				}
				if parent.Local == "RegistrationInfo" && value.Name.Local == "URI" {
					uriCount++
					uriActive = true
				}
				if parent.Local == "Exec" && value.Name.Local == "Command" {
					commandCount++
					commandActive = true
					shape.commandStart = base + int(dec.InputOffset())
				} else if commandActive {
					commandNested = true
				}
				stack = append(stack, value.Name)
			case xml.CharData:
				if enabledActive {
					enabled.Write([]byte(value))
					shape.enabledEnd = base + int(dec.InputOffset())
				}
				if uriActive {
					uri.Write([]byte(value))
				}
				if commandActive {
					command.Write([]byte(value))
					shape.commandEnd = base + int(dec.InputOffset())
				}
			case xml.EndElement:
				if len(stack) == 3 && value.Name.Local == "Enabled" {
					enabledActive = false
				}
				if value.Name.Local == "URI" && uriActive {
					uriActive = false
				}
				if value.Name.Local == "Command" && commandActive {
					commandActive = false
				}
				if len(stack) == 0 || stack[len(stack)-1] != value.Name {
					return ownedEntrypointTaskShape{}, errors.New("malformed XML nesting")
				}
				stack = stack[:len(stack)-1]
			}
		} else if errors.Is(tokenErr, io.EOF) {
			break
		} else {
			return ownedEntrypointTaskShape{}, fmt.Errorf("invalid XML: %w", tokenErr)
		}
	}
	if len(stack) != 0 {
		return ownedEntrypointTaskShape{}, errors.New("malformed XML nesting")
	}
	if actionsElements != 1 || actionCount != 1 || execCount != 1 {
		return ownedEntrypointTaskShape{}, errors.New("unknown action count")
	}
	if uriCount != 1 || strings.TrimSpace(uri.String()) == "" {
		return ownedEntrypointTaskShape{}, errors.New("missing task URI")
	}
	if commandCount != 1 || commandNested || shape.commandStart >= shape.commandEnd {
		return ownedEntrypointTaskShape{}, errors.New("missing or ambiguous Exec Command")
	}
	shape.uri = canonicalOwnedEntrypointTaskName(uri.String())
	shape.command = command.String()
	// Optional metadata keeps every ordinary V3 parser caller unchanged.
	if taskCount != 1 || settingsCount != 1 || enabledCount != 1 || enabledNested || enabledNamespaceInvalid || shape.enabledStart >= shape.enabledEnd {
		shape.enabledError = errors.New("missing or ambiguous namespace-qualified Settings/Enabled")
	} else {
		span := raw[shape.enabledStart:shape.enabledEnd]
		trimmed := bytes.TrimSpace(span)
		if !bytes.Equal(trimmed, []byte("true")) && !bytes.Equal(trimmed, []byte("false")) || strings.TrimSpace(enabled.String()) != string(trimmed) {
			shape.enabledError = errors.New("Settings/Enabled requires explicit true or false text")
		} else {
			shape.enabled = string(trimmed) == "true"
			shape.enabledStart += bytes.Index(span, trimmed)
			shape.enabledEnd = shape.enabledStart + len(trimmed)
		}
	}
	return shape, nil
}

func rewriteOwnedEntrypointTaskCommand(raw []byte, prior, runtime string) ([]byte, error) {
	shape, err := parseOwnedEntrypointTaskXML(raw)
	if err != nil {
		return nil, err
	}
	if !sameWindowsEntrypointPath(shape.command, prior) {
		return nil, errors.New("command is not exact prior CLI path")
	}
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(runtime)); err != nil {
		return nil, fmt.Errorf("escape runtime command: %w", err)
	}
	out := make([]byte, 0, len(raw)-shape.commandEnd+shape.commandStart+escaped.Len())
	out = append(out, raw[:shape.commandStart]...)
	out = append(out, escaped.Bytes()...)
	out = append(out, raw[shape.commandEnd:]...)
	return out, nil
}

func taskXMLBody(raw []byte) ([]byte, int, error) {
	if len(raw) >= 2 && ((raw[0] == 0xff && raw[1] == 0xfe) || (raw[0] == 0xfe && raw[1] == 0xff)) {
		return nil, 0, errors.New("UTF-16 task XML bytes are unsupported")
	}
	start := 0
	if len(raw) >= 3 && bytes.Equal(raw[:3], []byte{0xef, 0xbb, 0xbf}) {
		start = 3
	}
	for start < len(raw) && (raw[start] == ' ' || raw[start] == '\t' || raw[start] == '\r' || raw[start] == '\n') {
		start++
	}
	if bytes.HasPrefix(raw[start:], []byte("<?xml")) {
		end := bytes.Index(raw[start:], []byte("?>"))
		if end < 0 {
			return nil, 0, errors.New("unterminated XML declaration")
		}
		start += end + len("?>")
	}
	if len(bytes.TrimSpace(raw[start:])) == 0 {
		return nil, 0, errors.New("empty task XML")
	}
	return raw[start:], start, nil
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("%x", digest[:])
}
