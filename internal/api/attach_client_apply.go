package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
)

// AttachClientApplyResult is the redacted internal attach/rollback result. It
// does not expose the selected client's physical path or pinned direct entry.
type AttachClientApplyResult struct {
	State                string `json:"state"`
	ReceiptID            string `json:"receipt_id"`
	Server               string `json:"server"`
	Daemon               string `json:"daemon"`
	Client               string `json:"client"`
	EntryKey             string `json:"entry_key"`
	Endpoint             string `json:"endpoint"`
	BeforeSHA256         string `json:"before_sha256"`
	AfterSHA256          string `json:"after_sha256"`
	IdempotentNoWrite    bool   `json:"idempotent_no_write,omitempty"`
	ManifestRestored     bool   `json:"manifest_restored,omitempty"`
	ManifestSHA256       string `json:"manifest_sha256,omitempty"`
	LeaseNamespaceBefore string `json:"lease_namespace_before,omitempty"`
	LeaseNamespaceAfter  string `json:"lease_namespace_after,omitempty"`
}

// AttachClientApplyError deliberately keeps causes private: filesystem and
// client errors can contain machine paths or raw client configuration.
type AttachClientApplyError struct {
	Code, Stage      string
	Invoked          bool
	RecoveryRequired bool
	cause            error
}

func (e *AttachClientApplyError) Error() string { return e.Code + " at " + e.Stage }
func (e *AttachClientApplyError) Unwrap() error { return e.cause }

func attachApplyFailure(code, stage string, invoked, recovery bool, cause error) error {
	return &AttachClientApplyError{Code: code, Stage: stage, Invoked: invoked, RecoveryRequired: recovery, cause: cause}
}

type attachClientReceiptV1 struct {
	Version           int                 `json:"version"`
	ID                string              `json:"id"`
	State             string              `json:"state"`
	Server            string              `json:"server"`
	Daemon            string              `json:"daemon"`
	Client            string              `json:"client"`
	EntryKey          string              `json:"entry_key"`
	TargetPath        string              `json:"target_path"`
	SnapshotLeaf      string              `json:"snapshot_leaf"`
	BeforeSHA256      string              `json:"before_sha256"`
	BeforeTokenSHA256 string              `json:"before_token_sha256"`
	AfterSHA256       string              `json:"after_sha256,omitempty"`
	AfterTokenSHA256  string              `json:"after_token_sha256,omitempty"`
	ManifestSHA256    string              `json:"manifest_sha256"`
	Owner             SupervisorLockOwner `json:"owner"`
	TaskName          string              `json:"task_name"`
	ChildPID          int                 `json:"child_pid"`
	ChildStartedAt    time.Time           `json:"child_started_at"`
	Port              int                 `json:"port"`
	Endpoint          string              `json:"endpoint"`
}

func attachReceiptLeaves(plan *AttachClientPlan) (receiptLeaf, snapshotLeaf string) {
	sum := sha256.Sum256([]byte(plan.Server + "\x00" + plan.Daemon + "\x00" + plan.Client + "\x00" + plan.frozen.targetPath + "\x00" + plan.EntryKey))
	stem := "attach-client-" + hex.EncodeToString(sum[:])
	return stem + ".receipt.json", stem + ".snapshot"
}

func attachHistoryReceiptLeaf(currentLeaf, id string) string {
	return strings.TrimSuffix(currentLeaf, ".receipt.json") + "-" + id + ".receipt.json"
}

func attachOperationSnapshotLeaf(baseLeaf, id string) string {
	return strings.TrimSuffix(baseLeaf, ".snapshot") + "-" + id + ".snapshot"
}

func attachSnapshotLeafMatches(actual, baseLeaf, id string) bool {
	return actual == baseLeaf || actual == attachOperationSnapshotLeaf(baseLeaf, id)
}

func attachHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func attachReadReceipt(leaf string) (*attachClientReceiptV1, error) {
	return attachReadReceiptUsing(leaf, readHubMcpStateFile)
}

func attachReadReceiptUsing(leaf string, readFile func(string) ([]byte, error)) (*attachClientReceiptV1, error) {
	raw, err := readFile(leaf)
	if err != nil {
		return nil, err
	}
	var receipt attachClientReceiptV1
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Version != 1 || receipt.ID == "" || receipt.State == "" {
		return nil, errors.New("invalid attachment receipt")
	}
	return &receipt, nil
}

func attachWriteReceipt(leaf string, receipt *attachClientReceiptV1) error {
	return attachWriteReceiptUsing(leaf, receipt, writeHubMcpStateFile, readHubMcpStateFile)
}

func attachWriteReceiptUsing(leaf string, receipt *attachClientReceiptV1, writeFile func(string, []byte) error, readFile func(string) ([]byte, error)) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := writeFile(leaf, raw); err != nil {
		return err
	}
	actual, err := attachReadReceiptUsing(leaf, readFile)
	if err != nil || !reflect.DeepEqual(actual, receipt) {
		return errors.Join(errors.New("attachment receipt readback mismatch"), err)
	}
	return nil
}

func attachOriginMatchesPlan(origin clients.SelectedEntryOriginV1, plan *AttachClientPlan) bool {
	return origin.Version == 1 && origin.Verdict == clients.SelectedEntryOriginTargetOwnedDirect &&
		origin.ClientID == plan.Client && origin.EntryName == plan.EntryKey && origin.Scope == "known-global" &&
		origin.TargetPathInternal == plan.frozen.targetPath && origin.TargetBytesSHA256 == plan.TargetBytesSHA256 &&
		origin.EntryTokenSHA256 == plan.EntryTokenSHA256
}

func sameAttachPlan(a, b *AttachClientPlan) bool {
	return a != nil && b != nil && a.Server == b.Server && a.Daemon == b.Daemon && a.Client == b.Client &&
		a.EntryKey == b.EntryKey && a.Endpoint == b.Endpoint && a.TargetBytesSHA256 == b.TargetBytesSHA256 &&
		a.EntryTokenSHA256 == b.EntryTokenSHA256 && reflect.DeepEqual(a.frozen, b.frozen)
}

func attachReadRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("attachment target is not a regular file")
	}
	return os.ReadFile(path)
}

// ApplyAttachClientPlan is an internal, explicitly called attach action.
// CLI registration remains a separate admitted slice.
func (a *API) ApplyAttachClientPlan(ctx context.Context, plan *AttachClientPlan) (result *AttachClientApplyResult, retErr error) {
	if plan != nil && plan.restore != nil {
		return a.applyRestoreThenAttachClientPlan(ctx, plan)
	}
	return a.applyAttachClientPlan(ctx, plan, nil)
}

// Restoration and client mutation are intentionally two lease transactions.
// The restored source is retained on any later error; the existing selected-
// client action remains the sole client writer and reacquires its own lease.
func (a *API) applyRestoreThenAttachClientPlan(ctx context.Context, plan *AttachClientPlan) (result *AttachClientApplyResult, retErr error) {
	return a.applyRestoreThenAttachClientPlanWithHooks(ctx, plan, nil)
}

// Test-only barriers stay local to the stage coordinator; production passes
// nil and never changes the existing client writer's hook or lease protocol.
type attachRestoreHooks struct{ beforeLink, afterRelease func() }

func (a *API) applyRestoreThenAttachClientPlanWithHooks(ctx context.Context, plan *AttachClientPlan, hooks *attachRestoreHooks) (result *AttachClientApplyResult, retErr error) {
	if ctx == nil || plan == nil || plan.restore == nil || plan.State != "would-restore-and-attach" {
		return nil, attachApplyFailure("ATTACH_PLAN_INVALID", "restore-admission", false, false, nil)
	}
	req := AttachClientRequest{Server: plan.Server, Daemon: plan.Daemon, Client: plan.Client, ManifestSource: plan.restoreSource}
	report, err := preflightAdoptLeaseNamespaceForAdopt()
	if err != nil {
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "lease-preflight", false, false, err)
	}
	lease, acquired, err := acquireAdoptLeaseForApply(manifestMutationLeaseOwner{}, plan.Server)
	if err != nil || !acquired {
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "lease-acquire", false, false, err)
	}
	leaseHeld := true
	defer func() {
		if leaseHeld {
			if releaseErr := lease.Unlock(); releaseErr != nil {
				result = nil
				retErr = errors.Join(attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "lease-release", false, true, releaseErr), retErr)
			}
		}
	}()
	current, err := a.resolveMissingAttachManifest(req)
	if err != nil || !sameAttachManifestCandidate(current, plan.restore) {
		return nil, attachApplyFailure("ATTACH_MANIFEST_SOURCE_DRIFT", "restore-revalidate", false, false, err)
	}
	fresh, err := a.BuildAttachClientPlan(ctx, req)
	if err != nil || !sameAttachPlan(plan, fresh) || fresh.restore == nil || fresh.State != "would-restore-and-attach" {
		return nil, attachApplyFailure("ATTACH_MANIFEST_SOURCE_DRIFT", "restore-admission", false, false, err)
	}
	dir, err := adoptCommittedManifestDir()
	if err != nil {
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "restore-dir", false, false, err)
	}
	var beforeLink func()
	if hooks != nil {
		beforeLink = hooks.beforeLink
	}
	preLinkAdmission := func() error {
		latest, resolveErr := a.resolveMissingAttachManifest(req)
		if resolveErr != nil || !sameAttachManifestCandidate(latest, current) || ctx.Err() != nil {
			return restoreFailure("ATTACH_MANIFEST_SOURCE_DRIFT", errors.Join(resolveErr, ctx.Err()))
		}
		return nil
	}
	if err := a.manifestRestoreExactInWithAdoptLeaseHeldWithGate(dir, plan.Server, current.raw, beforeLink, preLinkAdmission); err != nil {
		code := "ATTACH_MANIFEST_RESTORE_UNVERIFIED"
		var restoreErr *ManifestRestoreError
		if errors.As(err, &restoreErr) {
			code = restoreErr.Code
		}
		return nil, attachApplyFailure(code, "restore-publish", false, code == "ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	readback, _, readHash, err := a.readAttachManifest(plan.Server)
	if err != nil || readHash != current.hash || readback != current.raw {
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "restore-readback", true, true, err)
	}
	if err := lease.Unlock(); err != nil {
		leaseHeld = false
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "lease-release", true, true, err)
	}
	leaseHeld = false
	postLease, err := preflightAdoptLeaseNamespaceForAdopt()
	if err != nil || postLease.State != AdoptLeaseNamespaceReady {
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "lease-readback", true, true, err)
	}
	if hooks != nil && hooks.afterRelease != nil {
		hooks.afterRelease()
	}
	// A new plan observes the canonical manifest, intent generation, child
	// identity, listener, readiness, and selected source after lease release.
	req.ManifestSource = ""
	fresh, err = a.BuildAttachClientPlan(ctx, req)
	if err != nil || !sameAttachPlan(plan, fresh) || fresh.restore != nil || fresh.State == "already-committed" {
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORED_CLIENT_NOT_ATTACHED", "replan", true, true, err)
	}
	result, err = a.applyAttachClientPlan(ctx, fresh, nil)
	if err != nil {
		var attachErr *AttachClientApplyError
		if errors.As(err, &attachErr) && (attachErr.Invoked || attachErr.RecoveryRequired) {
			return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORED_ATTACHMENT_RECOVERY_REQUIRED", "attach", true, true, err)
		}
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORED_CLIENT_NOT_ATTACHED", "attach", true, true, err)
	}
	result.ManifestRestored, result.ManifestSHA256 = true, current.hash
	result.LeaseNamespaceBefore, result.LeaseNamespaceAfter = string(report.State), string(postLease.State)
	return result, nil
}

// These in-package fault hooks are nil in product calls. The release fault
// runs only after the real locking client returns and can only downgrade its
// confirmed result; it cannot replace the lock or the selected writer.
type attachApplyHooks struct {
	afterPrelock, beforeWrite func()
	writeReceiptFile          func(string, []byte) error
	readReceiptFile           func(string) ([]byte, error)
	readTarget                func(string) ([]byte, error)
	configReleaseFault        func() error
}

func (a *API) applyAttachClientPlan(ctx context.Context, plan *AttachClientPlan, hooks *attachApplyHooks) (result *AttachClientApplyResult, retErr error) {
	if ctx == nil || plan == nil || plan.Client != "mimocode" || plan.EntryKey == "" || plan.frozen.targetPath == "" {
		return nil, attachApplyFailure("ATTACH_PLAN_INVALID", "admission", false, false, nil)
	}
	readReceipt, writeReceipt, readTarget := attachReadReceipt, attachWriteReceipt, attachReadRegular
	if hooks != nil {
		if hooks.readReceiptFile != nil || hooks.writeReceiptFile != nil {
			readFile, writeFile := readHubMcpStateFile, writeHubMcpStateFile
			if hooks.readReceiptFile != nil {
				readFile = hooks.readReceiptFile
			}
			if hooks.writeReceiptFile != nil {
				writeFile = hooks.writeReceiptFile
			}
			readReceipt = func(leaf string) (*attachClientReceiptV1, error) {
				return attachReadReceiptUsing(leaf, readFile)
			}
			writeReceipt = func(leaf string, receipt *attachClientReceiptV1) error {
				return attachWriteReceiptUsing(leaf, receipt, writeFile, readFile)
			}
		}
		if hooks.readTarget != nil {
			readTarget = hooks.readTarget
		}
	}
	req := AttachClientRequest{Server: plan.Server, Daemon: plan.Daemon, Client: plan.Client}
	lease, acquired, err := NewAdoptLeaseOwner().AcquireAdoptLease(plan.Server)
	if err != nil || !acquired {
		return nil, attachApplyFailure("ATTACH_LEASE_UNAVAILABLE", "lease-acquire", false, false, err)
	}
	leaseHeld := true
	var receipt *attachClientReceiptV1
	var receiptLeaf, snapshotLeaf string
	var receiptAttempted, snapshotAttempted, writeInvoked, cleanupPrepared bool
	defer func() {
		if !leaseHeld {
			return
		}
		if releaseErr := lease.ReleaseAndRemove(); releaseErr != nil {
			if receiptAttempted && receipt != nil && receipt.State == "prepared" {
				receipt.State = "recovery-required"
				releaseErr = errors.Join(releaseErr, writeReceipt(receiptLeaf, receipt))
			}
			result = nil
			retErr = errors.Join(attachApplyFailure("ATTACH_LEASE_RELEASE_UNVERIFIED", "lease-release", writeInvoked, writeInvoked || snapshotAttempted || receiptAttempted, releaseErr), retErr)
			return
		}
		if !cleanupPrepared {
			return
		}
		// A prewrite veto may discard its evidence only after both lock owners
		// have settled and the selected physical bytes still match the pin.
		current, cleanupErr := readTarget(plan.frozen.targetPath)
		if cleanupErr == nil && attachHash(current) != plan.TargetBytesSHA256 {
			cleanupErr = errors.New("attachment preimage changed after veto")
		}
		if cleanupErr == nil {
			var stateDir string
			stateDir, cleanupErr = DaemonStateDir()
			if cleanupErr == nil && receiptAttempted {
				cleanupErr = os.Remove(filepath.Join(stateDir, receiptLeaf))
				if errors.Is(cleanupErr, os.ErrNotExist) {
					cleanupErr = nil
				}
			}
			if cleanupErr == nil && snapshotAttempted {
				cleanupErr = os.Remove(filepath.Join(stateDir, snapshotLeaf))
				if errors.Is(cleanupErr, os.ErrNotExist) {
					cleanupErr = nil
				}
			}
		}
		if cleanupErr != nil {
			if receiptAttempted {
				receipt.State = "recovery-required"
				cleanupErr = errors.Join(cleanupErr, writeReceipt(receiptLeaf, receipt))
			}
			result = nil
			retErr = errors.Join(attachApplyFailure("ATTACH_RECOVERY_REQUIRED", "prewrite-cleanup", false, true, cleanupErr), retErr)
		}
	}()
	manifestDir, dirErr := adoptCommittedManifestDir()
	if dirErr != nil {
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "restore-reentry", false, false, dirErr)
	}
	if settleErr := a.settlePublishedRestoreUnderLease(manifestDir, plan.Server, plan.frozen.manifestHash); settleErr != nil {
		return nil, attachApplyFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", "restore-reentry", false, true, settleErr)
	}

	receiptLeaf, snapshotLeaf = attachReceiptLeaves(plan)
	if plan.State == "already-committed" {
		prior, readErr := readReceipt(receiptLeaf)
		if readErr != nil || prior == nil || prior.State != "committed" || prior.ID != plan.ReceiptID ||
			plan.BeforeTransport != "http" || plan.AfterTransport != "http" ||
			prior.AfterSHA256 != plan.TargetBytesSHA256 || prior.AfterTokenSHA256 != plan.EntryTokenSHA256 {
			return nil, attachApplyFailure("ATTACH_COMMITTED_DRIFT", "receipt-readback", false, true, readErr)
		}
		post, postErr := a.BuildAttachClientPlan(ctx, req)
		current, currentErr := readReceipt(receiptLeaf)
		if postErr != nil || currentErr != nil || !reflect.DeepEqual(current, prior) || post == nil ||
			post.State != "already-committed" || post.ReceiptID != prior.ID ||
			post.BeforeTransport != "http" || post.AfterTransport != "http" || !sameAttachPlan(plan, post) {
			return nil, attachApplyFailure("ATTACH_COMMITTED_DRIFT", "receipt-readback", false, true, errors.Join(postErr, currentErr))
		}
		return &AttachClientApplyResult{State: "already-committed", IdempotentNoWrite: true, ReceiptID: prior.ID, Server: plan.Server,
			Daemon: plan.Daemon, Client: plan.Client, EntryKey: plan.EntryKey, Endpoint: plan.Endpoint,
			BeforeSHA256: prior.BeforeSHA256, AfterSHA256: prior.AfterSHA256}, nil
	}
	var priorRolledBack *attachClientReceiptV1
	if prior, readErr := readReceipt(receiptLeaf); readErr == nil && prior != nil {
		if prior.State == "rolled-back" {
			priorRolledBack = prior
		} else {
			if prior.State != "committed" || prior.Server != plan.Server || prior.Daemon != plan.Daemon ||
				prior.Client != plan.Client || prior.EntryKey != plan.EntryKey || prior.TargetPath != plan.frozen.targetPath ||
				!attachSnapshotLeafMatches(prior.SnapshotLeaf, snapshotLeaf, prior.ID) || prior.BeforeSHA256 != plan.TargetBytesSHA256 ||
				prior.BeforeTokenSHA256 != plan.EntryTokenSHA256 || prior.ManifestSHA256 != plan.frozen.manifestHash ||
				prior.Owner != plan.frozen.owner || prior.TaskName != plan.frozen.taskName ||
				prior.ChildPID != plan.frozen.childPID || !prior.ChildStartedAt.Equal(plan.frozen.childStartedAt) ||
				prior.Port != plan.frozen.port || prior.Endpoint != plan.Endpoint ||
				prior.AfterSHA256 == "" || prior.AfterTokenSHA256 == "" {
				return nil, attachApplyFailure("ATTACH_COMPETING_OWNER", "receipt", false, true, nil)
			}
			post, postErr := a.BuildAttachClientPlan(ctx, req)
			if postErr == nil && post != nil && post.State == "already-committed" && post.ReceiptID == prior.ID &&
				post.Server == plan.Server && post.Daemon == plan.Daemon && post.Client == plan.Client &&
				post.EntryKey == plan.EntryKey && post.Endpoint == plan.Endpoint &&
				post.TargetBytesSHA256 == prior.AfterSHA256 && post.EntryTokenSHA256 == prior.AfterTokenSHA256 &&
				reflect.DeepEqual(post.frozen, plan.frozen) {
				return &AttachClientApplyResult{State: prior.State, IdempotentNoWrite: true, ReceiptID: prior.ID, Server: plan.Server,
					Daemon: plan.Daemon, Client: plan.Client, EntryKey: plan.EntryKey, Endpoint: plan.Endpoint,
					BeforeSHA256: prior.BeforeSHA256, AfterSHA256: prior.AfterSHA256}, nil
			}
			return nil, attachApplyFailure("ATTACH_COMMITTED_DRIFT", "receipt-readback", false, true, postErr)
		}
	} else if !isHubMcpStateMissingErr(readErr) {
		return nil, attachApplyFailure("ATTACH_RECOVERY_REQUIRED", "receipt-read", false, true, readErr)
	}

	prelock, err := a.BuildAttachClientPlan(ctx, req)
	if err != nil || !sameAttachPlan(plan, prelock) {
		return nil, attachApplyFailure("ATTACH_PRELOCK_DRIFT", "prelock", false, false, err)
	}
	if hooks != nil && hooks.afterPrelock != nil {
		hooks.afterPrelock()
	}
	client, err := clients.NewMimoCode()
	if err != nil || client.ConfigPath() != plan.frozen.targetPath {
		return nil, attachApplyFailure("ATTACH_SOURCE_DRIFT", "client", false, false, err)
	}
	reader, ok := client.(clients.SelectedEntryOriginReader)
	if !ok {
		return nil, attachApplyFailure("ATTACH_WRITER_UNSUPPORTED", "client", false, false, nil)
	}
	mutator, ok := client.(clients.ConditionalEntryMutator)
	if !ok {
		return nil, attachApplyFailure("ATTACH_WRITER_UNSUPPORTED", "client", false, false, nil)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, attachApplyFailure("ATTACH_PREPARE_FAILED", "nonce", false, false, err)
	}
	if priorRolledBack != nil {
		oldID, idErr := hex.DecodeString(priorRolledBack.ID)
		if idErr != nil || len(oldID) != 16 || priorRolledBack.Server != plan.Server || priorRolledBack.Daemon != plan.Daemon ||
			priorRolledBack.Client != plan.Client || priorRolledBack.EntryKey != plan.EntryKey || priorRolledBack.TargetPath != plan.frozen.targetPath ||
			!attachSnapshotLeafMatches(priorRolledBack.SnapshotLeaf, snapshotLeaf, priorRolledBack.ID) {
			return nil, attachApplyFailure("ATTACH_RECOVERY_REQUIRED", "prior-receipt", false, true, idErr)
		}
		oldSnapshot, snapshotErr := readHubMcpStateFile(priorRolledBack.SnapshotLeaf)
		if snapshotErr != nil || attachHash(oldSnapshot) != priorRolledBack.BeforeSHA256 {
			return nil, attachApplyFailure("ATTACH_RECOVERY_REQUIRED", "prior-snapshot", false, true, snapshotErr)
		}
		current, currentErr := readTarget(plan.frozen.targetPath)
		if currentErr != nil || !attachRestoredEntryMatches(client, oldSnapshot, current, plan.EntryKey) {
			return nil, attachApplyFailure("ATTACH_RECOVERY_REQUIRED", "prior-direct", false, true, currentErr)
		}
		historyLeaf := attachHistoryReceiptLeaf(receiptLeaf, priorRolledBack.ID)
		history, historyErr := readReceipt(historyLeaf)
		if isHubMcpStateMissingErr(historyErr) {
			if err := writeReceipt(historyLeaf, priorRolledBack); err != nil {
				return nil, attachApplyFailure("ATTACH_HISTORY_UNVERIFIED", "archive", false, true, err)
			}
		} else if historyErr != nil || !reflect.DeepEqual(history, priorRolledBack) {
			return nil, attachApplyFailure("ATTACH_HISTORY_UNVERIFIED", "archive", false, true, historyErr)
		}
		// The current receipt leaf advances only after its old operation has an
		// independently read-back historical receipt. Its snapshot stays pinned;
		// this new operation gets a distinct private snapshot leaf.
		snapshotLeaf = attachOperationSnapshotLeaf(snapshotLeaf, hex.EncodeToString(nonce[:]))
	}
	receipt = &attachClientReceiptV1{Version: 1, ID: hex.EncodeToString(nonce[:]), State: "prepared",
		Server: plan.Server, Daemon: plan.Daemon, Client: plan.Client, EntryKey: plan.EntryKey,
		TargetPath: plan.frozen.targetPath, SnapshotLeaf: snapshotLeaf,
		BeforeSHA256: plan.TargetBytesSHA256, BeforeTokenSHA256: plan.EntryTokenSHA256,
		ManifestSHA256: plan.frozen.manifestHash, Owner: plan.frozen.owner,
		TaskName: plan.frozen.taskName, ChildPID: plan.frozen.childPID,
		ChildStartedAt: plan.frozen.childStartedAt, Port: plan.frozen.port, Endpoint: plan.Endpoint}
	writer := func(path string, contents []byte) error {
		if hooks != nil && hooks.beforeWrite != nil {
			hooks.beforeWrite()
		}
		if filepath.Clean(path) != filepath.Clean(plan.frozen.targetPath) {
			return errors.New("attachment writer target drift")
		}
		return secureWritePathBased(path, contents)
	}
	observed := mutator.ConditionalEntryMutation(clients.ConditionalEntryMutationRequest{
		EntryName: plan.EntryKey, Operation: clients.EntryMutationAdd,
		Entry:        clients.MCPEntry{Name: plan.EntryKey, URL: plan.Endpoint},
		ExpectedLive: func(entry *clients.MCPEntry) bool { return entry != nil && entry.URL == "" && !entry.Disabled },
		Context:      ctx, LockWaitTimeout: 10 * time.Second, ConfigWriter: writer,
		BeforeMutation: func(_ clients.EntryMutationPreparation) error {
			origin := reader.ReadSelectedEntryOrigin(ctx, plan.EntryKey)
			if !attachOriginMatchesPlan(origin, plan) {
				return attachApplyFailure("ATTACH_SOURCE_DRIFT", "pin", false, false, nil)
			}
			before, err := readTarget(origin.TargetPathInternal)
			if err != nil || attachHash(before) != plan.TargetBytesSHA256 {
				return attachApplyFailure("ATTACH_PREIMAGE_CHANGED", "pin", false, false, err)
			}
			snapshotAttempted = true
			if err := writeHubMcpStateFile(snapshotLeaf, before); err != nil {
				return attachApplyFailure("ATTACH_PREPARE_FAILED", "snapshot-write", false, false, err)
			}
			pinned, err := readHubMcpStateFile(snapshotLeaf)
			if err != nil || !bytes.Equal(pinned, before) {
				return attachApplyFailure("ATTACH_PREPARE_FAILED", "snapshot-readback", false, false, err)
			}
			receiptAttempted = true
			if err := writeReceipt(receiptLeaf, receipt); err != nil {
				return attachApplyFailure("ATTACH_PREPARE_FAILED", "receipt-prepare", false, true, err)
			}
			finalPlan, err := a.buildAttachClientPlan(ctx, req, &origin)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return attachApplyFailure("ATTACH_PREWRITE_CANCELLED", "final-admission", false, false, errors.Join(ctxErr, err))
			}
			if err != nil || !sameAttachPlan(plan, finalPlan) {
				return attachApplyFailure("ATTACH_FINAL_ADMISSION_DRIFT", "final-admission", false, false, err)
			}
			// Source proof is deliberately the final admission observation.
			lastOrigin := reader.ReadSelectedEntryOrigin(ctx, plan.EntryKey)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return attachApplyFailure("ATTACH_PREWRITE_CANCELLED", "final-origin", false, false, ctxErr)
			}
			if !attachOriginMatchesPlan(lastOrigin, plan) {
				return attachApplyFailure("ATTACH_SOURCE_DRIFT", "final-origin", false, false, nil)
			}
			return nil
		},
	})
	if hooks != nil && hooks.configReleaseFault != nil && observed.ReleaseConfirmed {
		if releaseErr := hooks.configReleaseFault(); releaseErr != nil {
			observed.ReleaseConfirmed = false
			observed.ReleaseErr = attachApplyFailure("ATTACH_LOCK_RELEASE_UNVERIFIED", "config-lock-release", observed.Invoked, observed.Invoked || receiptAttempted, releaseErr)
		}
	}
	writeInvoked = observed.Invoked
	if !observed.Invoked {
		var cleanupErr error
		cleanupPrepared = (snapshotAttempted || receiptAttempted) && observed.ReleaseConfirmed
		recovery := (snapshotAttempted || receiptAttempted) && !observed.ReleaseConfirmed
		if recovery && receiptAttempted {
			receipt.State = "recovery-required"
			cleanupErr = errors.Join(cleanupErr, writeReceipt(receiptLeaf, receipt))
		}
		code := "ATTACH_PREWRITE_VETO"
		var preparation *AttachClientApplyError
		if errors.As(observed.PreparationErr, &preparation) {
			code = preparation.Code
		} else if errors.Is(observed.PreparationErr, context.Canceled) {
			code = "ATTACH_PREWRITE_CANCELLED"
		}
		if !observed.ReleaseConfirmed && (snapshotAttempted || receiptAttempted) {
			code = "ATTACH_LOCK_RELEASE_UNVERIFIED"
		}
		if !snapshotAttempted && !receiptAttempted && (errors.Is(observed.PreparationErr, context.DeadlineExceeded) || errors.Is(observed.PreparationErr, context.Canceled)) {
			code = "ATTACH_LOCK_WAIT_TIMEOUT"
		}
		return nil, attachApplyFailure(code, "prewrite", false, recovery,
			errors.Join(observed.PreparationErr, observed.ObservationErr, observed.ReleaseErr, cleanupErr))
	}
	// An invoked write is never translated into a prewrite/no-op outcome.
	var after []byte
	var afterToken string
	if observed.MutationErr == nil && observed.ObservationErr == nil && observed.ReleaseConfirmed {
		after, err = readTarget(plan.frozen.targetPath)
		if err == nil {
			afterToken, err = clients.SelectedEntryPhysicalTokenSHA256(client, after, plan.EntryKey)
		}
		if err == nil {
			entry, entryErr := client.GetEntry(plan.EntryKey)
			if entryErr != nil || entry == nil || entry.URL != plan.Endpoint || entry.Disabled || attachHash(after) == plan.TargetBytesSHA256 {
				err = errors.New("attachment physical/effective readback mismatch")
			}
		}
		if err == nil {
			frozenOrigin := clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginTargetOwnedDirect,
				ClientID: plan.Client, EntryName: plan.EntryKey, Scope: "known-global", TargetPathInternal: plan.frozen.targetPath,
				TargetBytesSHA256: plan.TargetBytesSHA256, EntryTokenSHA256: plan.EntryTokenSHA256}
			var post *AttachClientPlan
			post, err = a.buildAttachClientPlan(ctx, req, &frozenOrigin)
			if err == nil && !sameAttachPlan(plan, post) {
				err = errors.New("attachment daemon route drift")
			}
		}
	}
	if observed.MutationErr != nil || observed.ObservationErr != nil || !observed.ReleaseConfirmed || err != nil {
		receipt.State = "recovery-required"
		_ = writeReceipt(receiptLeaf, receipt)
		code := "ATTACH_COMMIT_UNVERIFIED"
		if info, statErr := os.Lstat(plan.frozen.targetPath); observed.MutationErr != nil && statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			code = "ATTACH_TARGET_SYMLINK_REFUSED"
		}
		return nil, attachApplyFailure(code, "postwrite", true, true,
			errors.Join(observed.MutationErr, observed.ObservationErr, observed.ReleaseErr, err))
	}
	receipt.AfterSHA256 = attachHash(after)
	receipt.AfterTokenSHA256 = afterToken
	if releaseErr := lease.ReleaseAndRemove(); releaseErr != nil {
		leaseHeld = false
		receipt.State = "recovery-required"
		_ = writeReceipt(receiptLeaf, receipt)
		return nil, attachApplyFailure("ATTACH_COMMIT_UNVERIFIED", "lease-release", true, true, releaseErr)
	}
	leaseHeld = false
	receipt.State = "committed"
	if err := writeReceipt(receiptLeaf, receipt); err != nil {
		receipt.State = "recovery-required"
		_ = writeReceipt(receiptLeaf, receipt)
		return nil, attachApplyFailure("ATTACH_COMMIT_UNVERIFIED", "receipt-commit", true, true, err)
	}
	return &AttachClientApplyResult{State: receipt.State, ReceiptID: receipt.ID,
		Server: plan.Server, Daemon: plan.Daemon, Client: plan.Client, EntryKey: plan.EntryKey,
		Endpoint: plan.Endpoint, BeforeSHA256: receipt.BeforeSHA256, AfterSHA256: receipt.AfterSHA256}, nil
}
