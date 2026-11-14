//go:build windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

// Setup/canonicalize may fill absent members when every present member already
// matches the candidate. Their cold inverse must restore that exact partial
// file preimage without requiring it to have been an executable runtime.
// These admitted synthetic PE bytes are never executed.
func TestCanonicalNonUpgradePartialPriorColdRecovery(t *testing.T) {
	for _, mode := range []WindowsProductPairMode{WindowsProductPairModeSetup, WindowsProductPairModeCanonicalize} {
		for _, missing := range []string{"launcher", "product", "identity"} {
			for _, operation := range []string{"validate", "rollback"} {
				t.Run(string(mode)+"/"+missing+"/"+operation, func(t *testing.T) {
					f := newCanonicalPrepareFixture(t, true, false)
					txn := f.txn
					txn.Opts.Mode = mode
					stages, err := txn.Opts.EntrySet.original.Targets()
					if err != nil {
						t.Fatal(err)
					}
					prior := make(map[string][]byte)
					var missingPath string
					for _, member := range []struct{ name, stage, target string }{
						{"launcher", stages.CanonicalEntryPath, txn.Opts.CanonicalEntryPath},
						{"product", stages.ProductPath, txn.Opts.ProductPath},
						{"identity", stages.IdentityPath, txn.Opts.IdentityPath},
					} {
						if member.name == missing {
							missingPath = member.target
							continue
						}
						prior[member.target] = f.stages[member.stage]
						if err := os.WriteFile(member.target, prior[member.target], 0o600); err != nil {
							t.Fatal(err)
						}
					}
					// Prove this preimage is accepted by the real producer before
					// simulating its process loss and cold journal acquisition.
					if err := txn.Prepare(); err != nil {
						t.Fatalf("producer rejected valid partial identical prior: %v", err)
					}
					if txn.prepared.disposition.CLI != windowsProductTargetCreate || txn.prepared.journal.EntrySet.PriorRuntime != nil {
						t.Fatal("non-upgrade preparation did not retain its file-only create contract")
					}
					if err := txn.Close(); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(txn.Opts.StateDir, WindowsProductRecoveryFileLeafV3)
					raw, err := api.ReadStateFileInodeAnchored(path)
					if err != nil {
						t.Fatal(err)
					}
					journal, err := decodeWindowsProductRecoveryJournalV3(raw)
					if err != nil {
						t.Fatal(err)
					}
					req, err := windowsCanonicalEntryRecoveryRuntimeRequest(t.Context(), txn.Opts.StateDir, txn.Opts.CanonicalEntryPath, txn.Opts.LegacyWindowlessPath)
					if err != nil {
						t.Fatal(err)
					}
					tasks := &recoveryTaskTxnFake{}
					req.Deps.ReopenEntrySetTasks = func(context.Context, WindowsProductPairTaskRecovery, binaryadmission.WindowsCanonicalEntrySetTargetsV1) (WindowsProductTaskTxn, error) {
						return tasks, nil
					}
					req.Deps.DiscardTaskStore = func(WindowsProductPairTaskRecovery) error { return nil }
					if operation == "validate" {
						if err := validateWindowsProductPendingRecovery(req, journal); err != nil {
							t.Fatalf("cold validation rejected the producer's file preimage: %v", err)
						}
						if tasks.closeCalls != 1 || tasks.restoreCalls != 0 || !bytes.Equal(mustReadFile(t, path), raw) {
							t.Fatal("validation changed the task or journal preimage")
						}
					} else {
						calls := 0
						recoverRuntime := req.RecoverPriorSettlementOriginal
						req.RecoverPriorSettlementOriginal = func(ctx context.Context, j windowsProductRecoveryJournalV3, original *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1) error {
							calls++
							if original != nil {
								return errors.New("non-upgrade recovery acquired a runtime it never stopped")
							}
							return recoverRuntime(ctx, j, original)
						}
						settled, err := rollbackCanonicalEntryRecovery(req, withWindowsProductRecoveryDefaults(req.Deps), path, journal, tasks)
						if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback || calls != 1 || tasks.restoreCalls != 1 {
							t.Fatalf("cold inverse did not restore the file-only preimage: result=%+v callback=%d tasks=%+v err=%v", settled, calls, tasks, err)
						}
						for _, removed := range []string{path, stages.CanonicalEntryPath, stages.ProductPath, stages.IdentityPath} {
							if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
								t.Fatalf("settled inverse retained %q: %v", removed, err)
							}
						}
					}
					for target, expected := range prior {
						if !bytes.Equal(mustReadFile(t, target), expected) {
							t.Fatalf("cold %s changed original member %q", operation, target)
						}
					}
					if _, err := os.Lstat(missingPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("cold %s invented missing prior member %q: %v", operation, missingPath, err)
					}
				})
			}
		}
	}
}
