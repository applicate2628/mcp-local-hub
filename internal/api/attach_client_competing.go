package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/clients"
)

// attachmentBindingGuard holds the existing manifest operation lease across an
// ordinary selected MiMoCode migration body. Other adapters do not acquire it.
type attachmentBindingGuard struct {
	lease           AdoptLease
	alreadyRestored bool
}

func (g attachmentBindingGuard) release() error {
	if g.lease == nil {
		return nil
	}
	return g.lease.ReleaseAndRemove()
}

func acquireAttachmentBindingGuard(server, clientID string, adapter clients.Client, demigrate bool) (attachmentBindingGuard, error) {
	var guard attachmentBindingGuard
	if adapter == nil || clientID != "mimocode" {
		return guard, nil
	}
	lease, acquired, err := NewAdoptLeaseOwner().AcquireAdoptLease(server)
	if err != nil || !acquired {
		return guard, attachApplyFailure("ATTACH_LEASE_UNAVAILABLE", "competing-path", false, false, err)
	}
	guard.lease = lease
	fail := func(cause error) (attachmentBindingGuard, error) {
		return attachmentBindingGuard{}, errors.Join(cause, guard.release())
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return fail(attachApplyFailure("ATTACH_RECEIPT_UNVERIFIED", "competing-path", false, true, err))
	}
	entries, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return guard, nil
	}
	if err != nil || len(entries) > 4096 {
		return fail(attachApplyFailure("ATTACH_RECEIPT_UNVERIFIED", "competing-path", false, true, err))
	}
	for _, item := range entries {
		name := item.Name()
		if !strings.HasPrefix(name, "attach-client-") || !strings.HasSuffix(name, ".receipt.json") {
			continue
		}
		receipt, readErr := attachReadReceipt(name)
		if readErr != nil {
			return fail(attachApplyFailure("ATTACH_RECEIPT_UNVERIFIED", "competing-path", false, true, readErr))
		}
		if receipt.Server != server || receipt.Client != clientID || receipt.EntryKey != server || filepath.Clean(receipt.TargetPath) != filepath.Clean(adapter.ConfigPath()) {
			continue
		}
		switch receipt.State {
		case "prepared", "committed", "rollback-pending", "recovery-required":
			return fail(attachApplyFailure("ATTACH_COMPETING_OWNER", "competing-path", false, true, nil))
		case "rolled-back":
			if demigrate && attachmentRestoredKeyMatches(adapter, receipt) {
				guard.alreadyRestored = true
			}
		default:
			return fail(attachApplyFailure("ATTACH_RECEIPT_UNVERIFIED", "competing-path", false, true, nil))
		}
	}
	return guard, nil
}

func attachmentRestoredKeyMatches(adapter clients.Client, receipt *attachClientReceiptV1) bool {
	if receipt.BeforeSHA256 == "" || receipt.BeforeTokenSHA256 == "" || receipt.SnapshotLeaf == "" {
		return false
	}
	snapshot, err := readHubMcpStateFile(receipt.SnapshotLeaf)
	if err != nil || attachHash(snapshot) != receipt.BeforeSHA256 {
		return false
	}
	current, err := attachReadRegular(adapter.ConfigPath())
	if err != nil {
		return false
	}
	if !attachRestoredEntryMatches(adapter, snapshot, current, receipt.EntryKey) {
		return false
	}
	entry, err := adapter.GetEntry(receipt.EntryKey)
	return err == nil && entry != nil && entry.URL == "" && !entry.Disabled
}
