//go:build windows

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/binaryadmission"
)

func TestRecoveryExecutorIdentityUsesPackageRoot(t *testing.T) {
	_, executor, deps := committedRecoveryFixture(t)
	root := filepath.Join(t.TempDir(), "package")
	nested := filepath.Join(root, "mcphub-product", "mcphub.exe")
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.ReadFile(filepath.Join(filepath.Dir(executor), "windows-product-identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "windows-product-identity.json"), identity, 0o600); err != nil {
		t.Fatal(err)
	}
	admit := func(in binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		in.Path = executor
		admitted, err := deps.AdmitProduct(in)
		admitted.Path = nested
		return admitted, err
	}
	if _, _, err := admitCommittedRecoveryExecutor(nested, admit, deps.BuildInfo); err != nil {
		t.Fatal(err)
	}
	// A malformed adjacent sidecar must not be hidden by a valid package root.
	adjacent := filepath.Join(filepath.Dir(nested), "windows-product-identity.json")
	if err := os.WriteFile(adjacent, []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := admitCommittedRecoveryExecutor(nested, admit, deps.BuildInfo); err == nil {
		t.Fatal("invalid adjacent identity was hidden by package-root fallback")
	}
	// Historical standalone executors remain valid even in a directory whose
	// name happens to match the new Product subdirectory.
	if err := os.Remove(filepath.Join(root, "windows-product-identity.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adjacent, identity, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := admitCommittedRecoveryExecutor(nested, admit, deps.BuildInfo); err != nil {
		t.Fatalf("standalone executor rejected because of its directory name: %v", err)
	}
}

func TestRecoveryCommandsForwardCompleteSet(t *testing.T) {
	for _, policy := range []windowsRecoveryCommandPolicy{windowsRecoveryCommandCommitted, windowsRecoveryCommandPending} {
		t.Run(policy.prefix(), func(t *testing.T) {
			f, executor, deps := committedRecoveryFixture(t)
			canonical := f.product
			targets, err := binaryadmission.CanonicalEntryTargetsV1(canonical)
			if err != nil {
				t.Fatal(err)
			}
			template := canonicalClassifierJournal(t, WindowsProductRecoveryCommitDecided)
			e := template.EntrySet
			e.TransactionID = f.journal.TransactionID
			e.CanonicalEntry.Target = targets.CanonicalEntryPath
			e.Identity.Target = targets.IdentityPath
			e.StagedEntry.Path = filepath.Join(filepath.Dir(canonical), "launcher.stage")
			e.StagedIdentity.Path = filepath.Join(filepath.Dir(canonical), "identity.stage")
			e.Expected.CanonicalEntry.Path = targets.CanonicalEntryPath
			e.Expected.Identity.Path = targets.IdentityPath
			e.Expected.Product.Path = targets.ProductPath
			e.Expected.Product.SHA256 = f.journal.Product.NewSHA256
			e.Expected.ProductArtifact.Path = targets.ProductPath
			e.Expected.ProductArtifact.SHA256 = f.journal.Product.NewSHA256
			e.PayloadDirectory.Path = filepath.Dir(targets.ProductPath)
			e.PayloadDirectory.Original.Root = filepath.Dir(canonical)
			e.PriorRuntime = &windowsCanonicalEntryRuntimePreimageV1{Path: targets.ProductPath, Present: true, SHA256: f.journal.Product.PriorSHA256}
			f.journal.Product.Target = targets.ProductPath
			f.journal.EntrySet = e
			if policy == windowsRecoveryCommandPending {
				f.journal.Phase = WindowsProductRecoveryPrepared
				f.journal.TaskSetSHA256 = ""
				f.journal.PreSettlement = &windowsProductPreSettlementV3{Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent}, GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true, Ports: []windowsProductPortSettlementV1{}}
				f.journal.PriorFleetReaped, f.journal.Quiescence, f.journal.QuiescenceReceiptSHA256 = false, nil, ""
			} else {
				receipt, err := mintWindowsProductQuiescenceReceipt(targets.ProductPath, f.legacy,
					windowsProductPairPreflightV1{PriorCLI: windowsProductPreimageV1{Present: true, SHA256: f.journal.Product.PriorSHA256}, PriorWindowless: windowsProductPreimageV1{Present: true, SHA256: f.journal.LegacyWindowless.PriorSHA256}},
					windowsProductQuiescenceObservationV1{Supervisor: f.journal.PreSettlement.Supervisor, GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true},
					func() time.Time { return time.Date(2026, 9, 29, 0, 0, 1, 0, time.UTC) })
				if err != nil {
					t.Fatal(err)
				}
				projection, digest, err := projectWindowsProductRecoveryQuiescence(receipt)
				if err != nil {
					t.Fatal(err)
				}
				f.journal.Quiescence, f.journal.QuiescenceReceiptSHA256 = &projection, digest
			}
			writeCommittedRecoveryJournal(t, f)
			base := deps.AdmitProduct
			deps.AdmitProduct = func(in binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
				path := in.Path
				if in.Path == targets.ProductPath {
					in.Path = canonical
				}
				admitted, err := base(in)
				admitted.Path = path
				return admitted, err
			}
			check := func(req windowsProductRecoveryRequest) {
				if req.ProductPath != targets.ProductPath || req.CanonicalEntryPath != canonical || req.IdentityPath != targets.IdentityPath || req.LegacyWindowlessPath != f.legacy {
					t.Fatalf("incorrect request: %+v", req)
				}
			}
			deps.ValidatePending = func(req windowsProductRecoveryRequest, _ windowsProductRecoveryJournalV3) error {
				check(req)
				return nil
			}
			ownerCalls := 0
			deps.Recover = func(req windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				ownerCalls++
				check(req)
				if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
					t.Fatal(err)
				}
				outcome := WindowsProductPairRecoverySettledCommit
				if policy == windowsRecoveryCommandPending {
					outcome = WindowsProductPairRecoverySettledRollback
				}
				return &WindowsProductPairRecoverySettled{TransactionID: f.journal.TransactionID, Outcome: outcome}, nil
			}
			cmd, _ := committedRecoveryCommand()
			foreign := filepath.Join(t.TempDir(), "mcphub.exe")
			if err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, foreign, policy, deps); !errors.Is(err, ErrWindowsProductRecoveryIdentity) || ownerCalls != 0 {
				t.Fatalf("foreign canonical root reached recovery: calls=%d, error=%v", ownerCalls, err)
			}
			if err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, canonical, policy, deps); err != nil {
				t.Fatal(err)
			}
			if ownerCalls != 1 {
				t.Fatalf("recovery owner called %d times, want once", ownerCalls)
			}
		})
	}
}
