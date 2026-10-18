package api

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
)

// attachRollbackHooks are test-only, nil in product calls. beforeCAS runs after
// pending receipt persistence; beforeWrite runs under the existing CAS lock.
type attachRollbackHooks struct {
	beforeCAS, beforeWrite func()
}

// RollbackAttachClient restores only the selected MiMoCode key from a committed
// attachment receipt. The receipt ID is mandatory: an unowned HTTP entry never
// grants restoration authority.
func (a *API) RollbackAttachClient(ctx context.Context, req AttachClientRequest, receiptID string) (*AttachClientApplyResult, error) {
	return a.rollbackAttachClient(ctx, req, receiptID, nil)
}

// BuildAttachClientRollbackPlan previews one current receipt without acquiring
// a mutation lease or changing the receipt, snapshot, client, or daemon.
func (a *API) BuildAttachClientRollbackPlan(ctx context.Context, req AttachClientRequest, receiptID string) (*AttachClientPlan, error) {
	deny := func(code string) (*AttachClientPlan, error) { return nil, &AttachPlanError{Code: code} }
	if ctx == nil || ctx.Err() != nil || req.Server == "" || req.Daemon == "" || req.Client != "mimocode" || receiptID == "" {
		return deny("rollback-invalid")
	}
	foreground, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client, err := clients.NewMimoCode()
	if err != nil || client.ConfigPath() == "" {
		return deny("rollback-source-unavailable")
	}
	reader, ok := client.(clients.SelectedEntryOriginReader)
	if !ok {
		return deny("rollback-source-unavailable")
	}
	receipt, snapshot, _, err := attachReadRollbackEvidence(req, receiptID, client)
	if err != nil {
		var failure *AttachClientApplyError
		if errors.As(err, &failure) {
			return deny(strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(failure.Code, "ATTACH_"), "_", "-")))
		}
		return deny("rollback-evidence-unverified")
	}
	plan := &AttachClientPlan{ReceiptID: receipt.ID, Server: req.Server, Daemon: req.Daemon,
		Client: req.Client, EntryKey: req.Server, Endpoint: receipt.Endpoint,
		BeforeTransport: "http", AfterTransport: "direct"}
	switch receipt.State {
	case "committed":
		if _, err := attachCommittedHTTPProof(foreground, client, reader, req, receipt); err != nil {
			return deny("rollback-conflict")
		}
		plan.State = "would-rollback"
		plan.TargetBytesSHA256, plan.EntryTokenSHA256 = receipt.AfterSHA256, receipt.AfterTokenSHA256
	case "rolled-back":
		current, err := attachReadRegular(receipt.TargetPath)
		if err != nil || !attachRestoredEntryMatches(client, snapshot, current, req.Server) {
			return deny("rollback-conflict")
		}
		currentToken, err := clients.SelectedEntryPhysicalTokenSHA256(client, current, req.Server)
		if err != nil {
			return deny("rollback-conflict")
		}
		origin := reader.ReadSelectedEntryOrigin(foreground, req.Server)
		if origin.Version != 1 || origin.Verdict != clients.SelectedEntryOriginTargetOwnedDirect ||
			origin.ClientID != req.Client || origin.EntryName != req.Server || origin.Scope != "known-global" ||
			origin.SourceKind != "write-target" || origin.TargetPathInternal != receipt.TargetPath ||
			origin.TargetBytesSHA256 != attachHash(current) || origin.EntryTokenSHA256 != currentToken {
			return deny("rollback-conflict")
		}
		plan.State = "already-rolled-back"
		plan.BeforeTransport = "direct"
		plan.TargetBytesSHA256, plan.EntryTokenSHA256 = origin.TargetBytesSHA256, origin.EntryTokenSHA256
	default:
		return deny("rollback-recovery-required")
	}
	if foreground.Err() != nil {
		return deny("rollback-admission-timeout")
	}
	return plan, nil
}

// One owner validates the exact current receipt and its pinned direct
// snapshot for both the preview and the F3 mutator.
func attachReadRollbackEvidence(req AttachClientRequest, receiptID string, client clients.Client) (*attachClientReceiptV1, []byte, string, error) {
	lookup := &AttachClientPlan{Server: req.Server, Daemon: req.Daemon, Client: req.Client, EntryKey: req.Server}
	lookup.frozen.targetPath = client.ConfigPath()
	receiptLeaf, snapshotLeaf := attachReceiptLeaves(lookup)
	receipt, err := attachReadReceipt(receiptLeaf)
	if err != nil {
		code := "ATTACH_ROLLBACK_RECEIPT_UNVERIFIED"
		if isHubMcpStateMissingErr(err) {
			code = "ATTACH_ROLLBACK_UNOWNED"
		}
		return nil, nil, receiptLeaf, attachApplyFailure(code, "receipt", false, false, err)
	}
	if receipt.ID != receiptID || receipt.Server != req.Server || receipt.Daemon != req.Daemon || receipt.Client != req.Client || receipt.EntryKey != req.Server || receipt.TargetPath != client.ConfigPath() || !attachSnapshotLeafMatches(receipt.SnapshotLeaf, snapshotLeaf, receipt.ID) || receipt.BeforeSHA256 == "" || receipt.BeforeTokenSHA256 == "" || receipt.AfterSHA256 == "" || receipt.AfterTokenSHA256 == "" || receipt.Endpoint == "" {
		return nil, nil, receiptLeaf, attachApplyFailure("ATTACH_ROLLBACK_RECEIPT_UNVERIFIED", "receipt-identity", false, true, nil)
	}
	snapshot, err := readHubMcpStateFile(receipt.SnapshotLeaf)
	if err != nil || attachHash(snapshot) != receipt.BeforeSHA256 {
		return nil, nil, receiptLeaf, attachApplyFailure("ATTACH_ROLLBACK_SNAPSHOT_UNVERIFIED", "snapshot", false, true, err)
	}
	snapshotToken, err := clients.SelectedEntryPhysicalTokenSHA256(client, snapshot, req.Server)
	if err != nil || snapshotToken != receipt.BeforeTokenSHA256 {
		return nil, nil, receiptLeaf, attachApplyFailure("ATTACH_ROLLBACK_SNAPSHOT_UNVERIFIED", "snapshot-token", false, true, err)
	}
	return receipt, snapshot, receiptLeaf, nil
}

func (a *API) rollbackAttachClient(ctx context.Context, req AttachClientRequest, receiptID string, hooks *attachRollbackHooks) (result *AttachClientApplyResult, retErr error) {
	if ctx == nil || ctx.Err() != nil || req.Server == "" || req.Daemon == "" || req.Client != "mimocode" || receiptID == "" {
		return nil, attachApplyFailure("ATTACH_ROLLBACK_INVALID", "admission", false, false, nil)
	}
	lease, acquired, err := NewAdoptLeaseOwner().AcquireAdoptLease(req.Server)
	if err != nil || !acquired {
		return nil, attachApplyFailure("ATTACH_LEASE_UNAVAILABLE", "rollback-lease", false, false, err)
	}
	leaseHeld := true
	casInvoked := false
	var receipt *attachClientReceiptV1
	var receiptLeaf string
	defer func() {
		if !leaseHeld {
			return
		}
		if releaseErr := lease.ReleaseAndRemove(); releaseErr != nil {
			if receipt != nil && receipt.State == "rollback-pending" {
				receipt.State = "recovery-required"
				releaseErr = errors.Join(releaseErr, attachWriteReceipt(receiptLeaf, receipt))
			}
			result = nil
			retErr = errors.Join(retErr, attachApplyFailure("ATTACH_LEASE_RELEASE_UNVERIFIED", "rollback-lease-release", casInvoked, true, releaseErr))
		}
	}()
	client, err := clients.NewMimoCode()
	if err != nil || client.ConfigPath() == "" {
		return nil, attachApplyFailure("ATTACH_ROLLBACK_SOURCE_UNAVAILABLE", "client", false, false, err)
	}
	cas, ok := client.(clients.CASRollbackConfigWriter)
	if !ok {
		return nil, attachApplyFailure("ATTACH_ROLLBACK_WRITER_UNSUPPORTED", "client", false, false, nil)
	}
	var snapshot []byte
	receipt, snapshot, receiptLeaf, err = attachReadRollbackEvidence(req, receiptID, client)
	if err != nil {
		return nil, err
	}
	projection := func(state, afterHash string) *AttachClientApplyResult {
		return &AttachClientApplyResult{State: state, ReceiptID: receipt.ID, Server: req.Server,
			Daemon: req.Daemon, Client: req.Client, EntryKey: req.Server, Endpoint: receipt.Endpoint,
			BeforeSHA256: receipt.AfterSHA256, AfterSHA256: afterHash}
	}
	if receipt.State == "rolled-back" {
		current, readErr := attachReadRegular(receipt.TargetPath)
		if readErr == nil && attachRestoredEntryMatches(client, snapshot, current, req.Server) {
			var entry *clients.MCPEntry
			entry, readErr = client.GetEntry(req.Server)
			if readErr == nil && entry != nil && entry.URL == "" && !entry.Disabled {
				settled := projection("rolled-back", attachHash(current))
				settled.IdempotentNoWrite = true
				return settled, nil
			}
		}
		return nil, attachApplyFailure("ATTACH_ROLLBACK_CONFLICT", "already-rolled-back", false, true, readErr)
	}
	if receipt.State != "committed" {
		return nil, attachApplyFailure("ATTACH_ROLLBACK_RECOVERY_REQUIRED", "receipt-state", false, true, nil)
	}
	markRecovery := func(code, stage string, invoked bool, cause error) (*AttachClientApplyResult, error) {
		receipt.State = "recovery-required"
		return nil, attachApplyFailure(code, stage, invoked, true, errors.Join(cause, attachWriteReceipt(receiptLeaf, receipt)))
	}
	current, err := attachReadRegular(receipt.TargetPath)
	if err != nil || attachHash(current) != receipt.AfterSHA256 {
		return markRecovery("ATTACH_ROLLBACK_CONFLICT", "physical-postimage", false, err)
	}
	currentToken, err := clients.SelectedEntryPhysicalTokenSHA256(client, current, req.Server)
	if err != nil || currentToken != receipt.AfterTokenSHA256 {
		return markRecovery("ATTACH_ROLLBACK_CONFLICT", "entry-postimage", false, err)
	}
	entry, err := client.GetEntry(req.Server)
	if err != nil || entry == nil || entry.URL != receipt.Endpoint || entry.Disabled {
		return markRecovery("ATTACH_ROLLBACK_CONFLICT", "effective-postimage", false, err)
	}
	receipt.State = "rollback-pending"
	if err := attachWriteReceipt(receiptLeaf, receipt); err != nil {
		return markRecovery("ATTACH_ROLLBACK_UNVERIFIED", "pending-receipt", false, err)
	}
	match := func(live *clients.MCPEntry) bool {
		if ctx.Err() != nil || live == nil || live.URL != receipt.Endpoint || live.Disabled {
			return false
		}
		physical, err := attachReadRegular(receipt.TargetPath)
		if err != nil || attachHash(physical) != receipt.AfterSHA256 {
			return false
		}
		token, err := clients.SelectedEntryPhysicalTokenSHA256(client, physical, req.Server)
		return err == nil && token == receipt.AfterTokenSHA256
	}
	writer := func(path string, contents []byte) error {
		if hooks != nil && hooks.beforeWrite != nil {
			hooks.beforeWrite()
		}
		if filepath.Clean(path) != filepath.Clean(receipt.TargetPath) {
			return errors.New("attachment rollback writer target drift")
		}
		return secureWritePathBased(path, contents)
	}
	if hooks != nil && hooks.beforeCAS != nil {
		hooks.beforeCAS()
	}
	casInvoked = true
	if err := cas.CASRestoreEntryFromBytesForRollbackWithConfigWriter(req.Server, match, snapshot, writer); err != nil {
		code := "ATTACH_ROLLBACK_UNVERIFIED"
		if errors.Is(err, clients.ErrCASConflict) {
			code = "ATTACH_ROLLBACK_CONFLICT"
		}
		return markRecovery(code, "cas-restore", true, err)
	}
	restored, err := attachReadRegular(receipt.TargetPath)
	if err == nil && !attachRestoredEntryMatches(client, snapshot, restored, req.Server) {
		err = errors.New("restored selected entry differs from pinned snapshot")
	}
	if err == nil {
		var direct *clients.MCPEntry
		direct, err = client.GetEntry(req.Server)
		if err == nil && (direct == nil || direct.URL != "" || direct.Disabled) {
			err = errors.New("restored selected entry not effective direct")
		}
	}
	if err != nil {
		return markRecovery("ATTACH_ROLLBACK_UNVERIFIED", "readback", true, err)
	}
	if err := lease.ReleaseAndRemove(); err != nil {
		leaseHeld = false
		return markRecovery("ATTACH_LEASE_RELEASE_UNVERIFIED", "rollback-lease-release", true, err)
	}
	leaseHeld = false
	receipt.State = "rolled-back"
	if err := attachWriteReceipt(receiptLeaf, receipt); err != nil {
		return markRecovery("ATTACH_ROLLBACK_UNVERIFIED", "receipt-finalize", true, err)
	}
	return projection("rolled-back", attachHash(restored)), nil
}

// The existing MiMoCode CAS core preserves the selected entry's parsed value,
// not its source whitespace/key order. Compare that one value to the pinned
// snapshot; the exact lexical hashes remain the pre-restore CAS admission.
func attachRestoredEntryMatches(client clients.Client, snapshot, current []byte, name string) bool {
	cas, ok := clients.AsCASEntryMutator(client)
	if !ok {
		return false
	}
	want, wantPresent, err := cas.EntryRawSubtree(snapshot, name)
	if err != nil || !wantPresent {
		return false
	}
	got, gotPresent, err := cas.EntryRawSubtree(current, name)
	return err == nil && gotPresent && reflect.DeepEqual(got, want)
}
