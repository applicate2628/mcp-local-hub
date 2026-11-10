//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/binaryadmission"
)

// Wire input intentionally uses only the historical public decoder API. The
// positive new-discriminator case is a behavioral failure on the old decoder,
// not a test that requires missing new Go declarations merely to compile.
func canonicalEntryReceiptWire(t *testing.T) map[string]any {
	t.Helper()
	root := t.TempDir()
	transaction := strings.Repeat("a", 32)
	return map[string]any{
		"schema": "upgrade-receipt-v3", "transaction_id": transaction, "mode": "setup", "admission": "canonical-entry-product-v1",
		"artifact":        map[string]any{"role": "product", "subsystem": 2, "machine": 0x8664, "version": "0.4.41", "commit": "fixture-commit", "build_date": "fixture-date", "sha256": strings.Repeat("b", 64)},
		"task_set_sha256": strings.Repeat("c", 64), "retired": map[string]any{"windowless_absent": true, "admission_helper_absent_from_payload": true, "rollback_images_absent": true}, "installed_at": "2026-11-10T00:00:00Z",
		"entry_set": map[string]any{"schema": "windows-canonical-entry-set-v1", "transaction_id": transaction, "build_id": strings.Repeat("d", 32), "registry_projection_sha256": strings.Repeat("e", 64),
			"canonical_entry_path": filepath.Join(root, "mcphub.exe"), "canonical_entry_sha256": strings.Repeat("f", 64), "product_path": filepath.Join(root, "mcphub-product", "mcphub.exe"), "identity_path": filepath.Join(root, "windows-product-identity.json"), "identity_sha256": strings.Repeat("1", 64), "machine": 0x8664},
	}
}

func TestCanonicalEntryReceiptNewDiscriminatorAndLegacyCompatibility(t *testing.T) {
	t.Run("complete new set", func(t *testing.T) {
		raw, err := json.Marshal(canonicalEntryReceiptWire(t))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeUpgradeReceipt(raw)
		if err != nil || decoded.V3 == nil {
			t.Fatalf("complete new set rejected: %v", err)
		}
	})
	t.Run("historical single product", func(t *testing.T) {
		wire := canonicalEntryReceiptWire(t)
		delete(wire, "entry_set")
		// Use the actual historical discriminator, not a renamed/new success mode.
		wire["admission"] = UpgradeAdmissionLocalProductV3
		raw, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeUpgradeReceipt(raw)
		if err != nil || decoded.V3 == nil {
			t.Fatalf("historical V3 rejected: %v", err)
		}
	})
}

func TestCanonicalEntryReceiptCannotIgnorePartialOrForeignArm(t *testing.T) {
	for _, name := range []string{"legacy admission with set", "new admission without set", "present null arm", "foreign transaction", "missing identity", "foreign product path", "machine mismatch"} {
		t.Run(name, func(t *testing.T) {
			wire := canonicalEntryReceiptWire(t)
			arm := wire["entry_set"].(map[string]any)
			switch name {
			case "legacy admission with set":
				wire["admission"] = UpgradeAdmissionLocalProductV3
			case "new admission without set":
				delete(wire, "entry_set")
			case "present null arm":
				wire["admission"] = UpgradeAdmissionLocalProductV3
				wire["entry_set"] = nil
			case "foreign transaction":
				arm["transaction_id"] = strings.Repeat("9", 32)
			case "missing identity":
				delete(arm, "identity_sha256")
			case "foreign product path":
				arm["product_path"] = filepath.Join(t.TempDir(), "mcphub.exe")
			case "machine mismatch":
				arm["machine"] = 0x14c
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeUpgradeReceipt(raw); err == nil {
				t.Fatal("partial/foreign entry set accepted")
			}
		})
	}
}

func TestCanonicalEntryRecoveryRejectsPresentNullArm(t *testing.T) {
	txn, _, _ := newProductTxnFixture(t)
	defer func() {
		if err := txn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := txn.Prepare(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(txn.Opts.StateDir, WindowsProductRecoveryFileLeafV3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeWindowsProductRecoveryJournalV3(raw); err != nil {
		t.Fatal("fixture must be a valid historical journal", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	wire["entry_set"] = nil
	raw, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeWindowsProductRecoveryJournalV3(raw); err == nil {
		t.Fatal("present null entry-set recovery arm accepted")
	}
}

func TestCanonicalEntryReservedMemberCannotBeErasedBeforeDecode(t *testing.T) {
	wire := canonicalEntryReceiptWire(t)
	arm, err := json.Marshal(wire["entry_set"])
	if err != nil {
		t.Fatal(err)
	}
	wire["admission"] = UpgradeAdmissionLocalProductV3
	delete(wire, "entry_set")
	legacyReceipt, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	txn, _, _ := newProductTxnFixture(t)
	defer func() {
		if err := txn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := txn.Prepare(); err != nil {
		t.Fatal(err)
	}
	legacyJournal, err := os.ReadFile(filepath.Join(txn.Opts.StateDir, WindowsProductRecoveryFileLeafV3))
	if err != nil {
		t.Fatal(err)
	}
	appendMembers := func(raw []byte, members string) []byte {
		return []byte(strings.TrimSuffix(strings.TrimSpace(string(raw)), "}") + "," + members + "}")
	}
	for _, reader := range []struct {
		name   string
		raw    []byte
		decode func([]byte) error
	}{
		{"receipt", legacyReceipt, func(raw []byte) error { _, err := DecodeUpgradeReceipt(raw); return err }},
		{"journal", legacyJournal, func(raw []byte) error { _, err := decodeWindowsProductRecoveryJournalV3(raw); return err }},
	} {
		t.Run(reader.name, func(t *testing.T) {
			if err := reader.decode(reader.raw); err != nil {
				t.Fatal("genuinely absent legacy arm rejected", err)
			}
			for _, members := range []string{`"ENTRY_SET":null`, `"entry_set":{},"Entry_Set":null`, `"entry_set":{},"\u0045NTRY_SET":null`} {
				if err := reader.decode(appendMembers(reader.raw, members)); err == nil {
					t.Fatal("case-folded reserved member erased complete-set presence")
				}
			}
		})
	}
	full, err := json.Marshal(canonicalEntryReceiptWire(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{`"entry_set":` + string(arm), `"\u0065ntry_set":` + string(arm), `"ENTRY_SET":` + string(arm)} {
		if _, err := DecodeUpgradeReceipt(appendMembers(full, member)); err == nil {
			t.Fatal("duplicate decoded/escaped reserved member accepted")
		}
	}
}

func TestCanonicalNativeOperationCompletionRetainsOpenOwner(t *testing.T) {
	move := binaryadmission.WindowsCanonicalEntryMoveResultV1{Member: "launcher", SourceRelative: "entry.stage", DestinationRelative: "mcphub.exe", Attempted: true, NativeStatusKnown: true, Applied: true, Readback: true, OriginalOwnerPending: true}
	if err := canonicalEntryMoveResultError(move, nil, "launcher", "mcphub.exe"); err != nil {
		t.Fatal("completed move treated retained custody as failure", err)
	}
	if !move.OriginalOwnerPending {
		t.Fatal("operation receiving changed original owner status")
	}
	discard := binaryadmission.WindowsCanonicalEntryMoveResultV1{Member: "identity", SourceRelative: "identity.stage", Attempted: true, NativeStatusKnown: true, Applied: true, Readback: true, OriginalOwnerPending: true}
	if err := canonicalEntryDiscardResultError(discard, nil, "identity", "identity.stage"); err != nil {
		t.Fatal("completed discard treated retained root as failure", err)
	}
	readbackErr := errors.New("actual move readback failed")
	move.Readback = false
	move.Error = readbackErr
	if err := canonicalEntryMoveResultError(move, nil, "launcher", "mcphub.exe"); !errors.Is(err, readbackErr) {
		t.Fatal("failed move readback became success or lost cause", err)
	}
	closeErr := errors.New("actual discarded member close failed")
	discard.CloseError = closeErr
	if err := canonicalEntryDiscardResultError(discard, nil, "identity", "identity.stage"); !errors.Is(err, closeErr) {
		t.Fatal("discard close failure became success or lost cause", err)
	}
}

func TestGUIOwnedStopUsesActualPriorRuntimeProjection(t *testing.T) {
	root := t.TempDir()
	oldGUI := filepath.Join(root, "mcphub.exe")
	newGUI := filepath.Join(root, "mcphub-product", "mcphub.exe")
	legacy := filepath.Join(root, "mcphub-windowless.exe")
	oldSHA, newSHA := strings.Repeat("1", 64), strings.Repeat("2", 64)
	journal := windowsProductRecoveryJournalV3{Product: windowsProductRecoveryArtifactV3{Target: oldGUI, PriorPresent: true, PriorSHA256: oldSHA}, LegacyWindowless: windowsProductRecoveryLegacyWindowlessV3{Target: legacy}}
	receipt := windowsProductQuiescenceReceiptV1{CanonicalCLIPath: oldGUI, CanonicalWindowlessPath: legacy, PriorCLI: windowsProductPreimageV1{Present: true, SHA256: oldSHA}}
	if err := validateGUIOwnedStopPreimage(journal, receipt); err != nil {
		t.Fatal("legacy single-image comparison changed", err)
	}
	journal.Product = windowsProductRecoveryArtifactV3{Target: newGUI, PriorPresent: true, PriorSHA256: newSHA}
	journal.EntrySet = &windowsCanonicalEntrySetJournalV1{PriorRuntime: &windowsCanonicalEntryRuntimePreimageV1{Path: oldGUI, Present: true, SHA256: oldSHA}}
	if err := validateGUIOwnedStopPreimage(journal, receipt); err != nil {
		t.Fatal("historical GUI2 runtime compared against new payload", err)
	}
	wrong := receipt
	wrong.CanonicalCLIPath = newGUI
	wrong.PriorCLI.SHA256 = newSHA
	if err := validateGUIOwnedStopPreimage(journal, wrong); err == nil {
		t.Fatal("payload-only settlement replaced original runtime projection")
	}
	journal.EntrySet.PriorRuntime = nil
	if err := validateGUIOwnedStopPreimage(journal, receipt); err == nil {
		t.Fatal("missing new-arm runtime silently fell back to Product")
	}
}

func canonicalCleanupJournalData(t *testing.T) windowsProductRecoveryJournalV3 {
	t.Helper()
	root := t.TempDir()
	id := strings.Repeat("a", 32)
	return windowsProductRecoveryJournalV3{TransactionID: id, Phase: WindowsProductRecoveryCommitDecided, Product: windowsProductRecoveryArtifactV3{Target: filepath.Join(root, "mcphub-product", "mcphub.exe"), PriorPresent: true, PriorSHA256: strings.Repeat("1", 64), NewSHA256: strings.Repeat("2", 64)}, Staged: windowsProductRecoveryStagedV3{Path: filepath.Join(root, "product.stage")}, EntrySet: &windowsCanonicalEntrySetJournalV1{CanonicalEntry: windowsProductRecoveryArtifactV3{Target: filepath.Join(root, "mcphub.exe"), PriorPresent: true, PriorSHA256: strings.Repeat("3", 64), NewSHA256: strings.Repeat("4", 64)}, Identity: windowsProductRecoveryArtifactV3{Target: filepath.Join(root, "windows-product-identity.json"), NewSHA256: strings.Repeat("5", 64)}, StagedEntry: windowsProductRecoveryStagedV3{Path: filepath.Join(root, "launcher.stage")}, StagedIdentity: windowsProductRecoveryStagedV3{Path: filepath.Join(root, "identity.stage")}}}
}

func TestCanonicalCleanupRequiresDurableIntentAndNeverAuthorizesRollback(t *testing.T) {
	j := canonicalCleanupJournalData(t)
	row := windowsCanonicalEntryCleanupV1{TransactionID: j.TransactionID, Side: "prior", Member: "product", SourceRelative: "mcphub-product/mcphub.exe.old-20261111T000000Z", ExpectedSHA256: j.Product.PriorSHA256, State: "retained"}
	j.EntrySet.Cleanup = []windowsCanonicalEntryCleanupV1{row}
	if err := validateCanonicalCleanup(j); err != nil {
		t.Fatal("valid retained original rejected", err)
	}
	if cleanupAbsenceAuthorized(&j.EntrySet.Cleanup[0]) {
		t.Fatal("retained record retroactively authorized missing prior")
	}
	j.EntrySet.Cleanup[0].State = "discard_intent"
	if err := validateCanonicalCleanup(j); err != nil || !cleanupAbsenceAuthorized(&j.EntrySet.Cleanup[0]) {
		t.Fatal("durable intent cannot receive its absence", err)
	}
	j.EntrySet.Cleanup[0].State = "absence_received"
	if err := validateCanonicalCleanup(j); err != nil {
		t.Fatal("terminal postcondition rejected", err)
	}
	j.Phase = WindowsProductRecoveryPrepared
	if err := validateCanonicalCleanup(j); err == nil {
		t.Fatal("irreversible cleanup records authorized rollback or predecision absence")
	}
}

func TestCanonicalCleanupRejectsCanonicalForeignOrMismatchedRecords(t *testing.T) {
	for _, name := range []string{"canonical successor", "foreign original hash", "foreign transaction", "duplicate member"} {
		t.Run(name, func(t *testing.T) {
			j := canonicalCleanupJournalData(t)
			row := windowsCanonicalEntryCleanupV1{TransactionID: j.TransactionID, Side: "unused-successor", Member: "launcher", SourceRelative: "launcher.stage", ExpectedSHA256: j.EntrySet.CanonicalEntry.NewSHA256, State: "discard_intent"}
			switch name {
			case "canonical successor":
				row.SourceRelative = "mcphub.exe"
			case "foreign original hash":
				row.ExpectedSHA256 = strings.Repeat("9", 64)
			case "foreign transaction":
				row.TransactionID = strings.Repeat("b", 32)
			}
			j.EntrySet.Cleanup = []windowsCanonicalEntryCleanupV1{row}
			if name == "duplicate member" {
				j.EntrySet.Cleanup = append(j.EntrySet.Cleanup, row)
			}
			if err := validateCanonicalCleanup(j); err == nil {
				t.Fatal("cleanup record escaped exact original transaction/member")
			}
		})
	}
}

func TestPayloadDirectoryJournalRequiresNativeIdentityAndRollbackIntent(t *testing.T) {
	j := canonicalCleanupJournalData(t)
	j.Phase = WindowsProductRecoveryPrepared
	root := filepath.Dir(j.EntrySet.CanonicalEntry.Target)
	j.EntrySet.PayloadDirectory = windowsCanonicalEntryDirectoryV1{Path: filepath.Dir(j.Product.Target), CreationEntered: true, Created: true, State: "created_identity_received"}
	if err := validateOriginalPayloadDirectory(j); err == nil {
		t.Fatal("Created alone minted native directory authority")
	}
	original := binaryadmission.WindowsCanonicalEntryPayloadDirectorySnapshotV1{Schema: binaryadmission.WindowsCanonicalEntryPayloadDirectorySchemaV1, Root: root, RelativePath: "mcphub-product", Created: true, ParentIdentity: binaryadmission.WindowsCanonicalEntryDirectoryIdentityV1{VolumeSerialNumber: 1, FileIndexLow: 2, CreationTime: 3}, DirectoryIdentity: binaryadmission.WindowsCanonicalEntryDirectoryIdentityV1{VolumeSerialNumber: 1, FileIndexLow: 4, CreationTime: 5}}
	j.EntrySet.PayloadDirectory.Original = &original
	if err := validateOriginalPayloadDirectory(j); err != nil {
		t.Fatal("structurally received native snapshot rejected", err)
	}
	j.EntrySet.PayloadDirectory.State = "dispose_intent"
	j.Phase = WindowsProductRecoveryCommitDecided
	if err := validateOriginalPayloadDirectory(j); err == nil {
		t.Fatal("committed required payload directory became disposable")
	}
	j.Phase = WindowsProductRecoveryPrepared
	j.EntrySet.PayloadDirectory.State = "absence_received"
	absence := binaryadmission.WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1{Root: root, RelativePath: "mcphub-product", ParentIdentity: original.ParentIdentity, Absent: true}
	j.EntrySet.PayloadDirectory.Absence = &absence
	if err := validateOriginalPayloadDirectory(j); err != nil {
		t.Fatal("read-only authorized absence grammar rejected", err)
	}
	absence.ParentIdentity.FileIndexLow++
	if err := validateOriginalPayloadDirectory(j); err == nil {
		t.Fatal("foreign parent absence accepted")
	}
}

// Supplies a complete persisted journal to the actual private recovery preparer;
// injected file hashes/admission model classification, not native qualification.
func canonicalClassifierJournal(t *testing.T, phase WindowsProductRecoveryPhase) windowsProductRecoveryJournalV3 {
	t.Helper()
	j := canonicalCleanupJournalData(t)
	root := filepath.Dir(j.EntrySet.CanonicalEntry.Target)
	j.Schema, j.Mode, j.Phase = WindowsProductRecoverySchemaV3, WindowsProductPairModeCanonicalize, phase
	j.LegacyWindowless.Target = filepath.Join(root, "mcphub-windowless.exe")
	j.Receipts.V3.Path = filepath.Join(root, UpgradeReceiptSchemaV3+".json")
	j.Receipts.V2.Path = filepath.Join(root, UpgradeReceiptSchemaV2+".json")
	j.Tasks.StoreRoot = filepath.Join(root, "task-store")
	j.Staged.SHA256 = j.Product.NewSHA256
	if phase == WindowsProductRecoveryCommitDecided {
		j.TaskSetSHA256 = strings.Repeat("6", 64)
	}
	e := j.EntrySet
	e.Schema, e.TransactionID = windowsCanonicalEntrySetSchemaV1, j.TransactionID
	e.CanonicalEntry.PriorPresent, e.CanonicalEntry.PriorSHA256 = false, ""
	e.StagedEntry.SHA256, e.StagedIdentity.SHA256 = e.CanonicalEntry.NewSHA256, e.Identity.NewSHA256
	identity := []byte("classifier identity fixture")
	e.Identity.NewSHA256, e.StagedIdentity.SHA256 = recoveryTestSHA(identity), recoveryTestSHA(identity)
	e.Expected = binaryadmission.WindowsCanonicalEntrySetSnapshotV1{
		Schema: e.Schema, BuildID: strings.Repeat("7", 32), RegistryProjectionSHA256: strings.Repeat("8", 64), Machine: 0x8664,
		CanonicalEntry:  binaryadmission.WindowsCanonicalEntryFileV1{Path: e.CanonicalEntry.Target, SHA256: e.CanonicalEntry.NewSHA256},
		Product:         binaryadmission.WindowsCanonicalEntryFileV1{Path: j.Product.Target, SHA256: j.Product.NewSHA256},
		Identity:        binaryadmission.WindowsCanonicalEntryFileV1{Path: e.Identity.Target, SHA256: e.Identity.NewSHA256},
		ProductArtifact: binaryadmission.WindowsArtifact{Path: j.Product.Target, Role: binaryadmission.WindowsArtifactRoleProduct, Machine: 0x8664, Subsystem: binaryadmission.WindowsGUISubsystem, SHA256: j.Product.NewSHA256},
		IdentityBytes:   identity,
	}
	e.PayloadDirectory = windowsCanonicalEntryDirectoryV1{Path: filepath.Dir(j.Product.Target), PriorPresent: true, CreationEntered: true,
		Original: &binaryadmission.WindowsCanonicalEntryPayloadDirectorySnapshotV1{Schema: binaryadmission.WindowsCanonicalEntryPayloadDirectorySchemaV1, Root: root, RelativePath: "mcphub-product"}}
	return j
}

func canonicalClassifierDeps(t *testing.T, j windowsProductRecoveryJournalV3, hashes map[string]string) windowsProductRecoveryDeps {
	t.Helper()
	raw, err := encodeWindowsProductRecoveryJournalV3(j)
	if err != nil {
		t.Fatal(err)
	}
	return windowsProductRecoveryDeps{
		ReadJournal: func(string) ([]byte, error) { return append([]byte(nil), raw...), nil },
		HashFile: func(path string) (string, error) {
			if hash, ok := hashes[path]; ok {
				return hash, nil
			}
			return "", os.ErrNotExist
		},
		ListAsides: listGeneratedRenameAsideBasenames,
		AdmitEntrySet: func(_ context.Context, targets binaryadmission.WindowsCanonicalEntrySetTargetsV1, expected binaryadmission.WindowsCanonicalEntrySetSnapshotV1) (binaryadmission.WindowsCanonicalEntrySetSnapshotV1, error) {
			if hashes[targets.CanonicalEntryPath] != expected.CanonicalEntry.SHA256 || hashes[targets.ProductPath] != expected.Product.SHA256 || hashes[targets.IdentityPath] != expected.Identity.SHA256 {
				return expected, ErrWindowsProductRecoveryIdentity
			}
			return expected, nil
		},
	}
}

func TestCanonicalCommittedClassifierReceivesDisposedPrior(t *testing.T) {
	for _, state := range []string{"discard_intent", "absence_received", "retained"} {
		t.Run(state, func(t *testing.T) {
			j := canonicalClassifierJournal(t, WindowsProductRecoveryCommitDecided)
			if err := os.Mkdir(filepath.Dir(j.Product.Target), 0o700); err != nil {
				t.Fatal(err)
			}
			j.EntrySet.Cleanup = []windowsCanonicalEntryCleanupV1{{TransactionID: j.TransactionID, Side: "prior", Member: "product", SourceRelative: "mcphub-product/mcphub.exe.old-20261111T000000Z", ExpectedSHA256: j.Product.PriorSHA256, State: state}}
			hashes := map[string]string{j.EntrySet.CanonicalEntry.Target: j.EntrySet.CanonicalEntry.NewSHA256, j.Product.Target: j.Product.NewSHA256, j.EntrySet.Identity.Target: j.EntrySet.Identity.NewSHA256}
			d := canonicalClassifierDeps(t, j, hashes)
			source, err := prepareCanonicalRecoverySource(windowsProductRecoveryRequest{Context: context.Background()}, d, "original-journal", j, canonicalRecoveryCommittedCleanup)
			if state == "retained" {
				if err == nil {
					t.Fatal("missing prior without durable intent was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal("committed cleanup never reached its authorized absence", err)
			}
			defer func() { source.active = false }()
			if source.prior.Product.Present || !source.authorizedAbsence["prior\x00product"] {
				t.Fatal("disposed prior was reconstructed as a live original")
			}
			// The same observed missing prior is still invalid for the generic rollback owner.
			if _, err := inspectWindowsProductPairRecoveryArtifact(canonicalRecoveryArtifact(j.Product), pairRecoveryDepsFromProduct(d)); err == nil {
				t.Fatal("generic rollback no longer requires the prior")
			}
			hashes[j.Product.Target] = strings.Repeat("9", 64)
			if _, err := prepareCanonicalRecoverySource(windowsProductRecoveryRequest{Context: context.Background()}, d, "original-journal", j, canonicalRecoveryCommittedCleanup); err == nil {
				t.Fatal("foreign canonical successor bypassed committed admission")
			}
		})
	}
}

func TestCanonicalRollbackClassifierReceivesDisposedPayloadParent(t *testing.T) {
	j := canonicalClassifierJournal(t, WindowsProductRecoveryPrepared)
	j.Product.PriorPresent, j.Product.PriorSHA256 = false, ""
	dir := &j.EntrySet.PayloadDirectory
	if err := os.Mkdir(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := func(path string) binaryadmission.WindowsCanonicalEntryDirectoryIdentityV1 {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var info windows.ByHandleFileInformation
		err = windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info)
		if err = errors.Join(err, f.Close()); err != nil {
			t.Fatal(err)
		}
		return binaryadmission.WindowsCanonicalEntryDirectoryIdentityV1{VolumeSerialNumber: info.VolumeSerialNumber, FileIndexHigh: info.FileIndexHigh, FileIndexLow: info.FileIndexLow, CreationTime: uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime)}
	}
	dir.PriorPresent, dir.Created, dir.State = false, true, "dispose_intent"
	dir.Original.Created = true
	dir.Original.ParentIdentity = identity(dir.Original.Root)
	dir.Original.DirectoryIdentity = identity(dir.Path)
	if err := os.Remove(dir.Path); err != nil {
		t.Fatal(err)
	}
	d := canonicalClassifierDeps(t, j, map[string]string{})
	originalList := d.ListAsides
	d.ListAsides = func(target string) ([]string, error) {
		if sameWindowsProductPath(target, j.Product.Target) {
			t.Fatal("missing product parent was inventoried before native absence receiving")
		}
		return originalList(target)
	}
	source, err := prepareCanonicalRecoverySource(windowsProductRecoveryRequest{Context: context.Background()}, d, "original-journal", j, canonicalRecoveryRollback)
	if err != nil {
		t.Fatal("durable directory intent could not reach its native absence observer", err)
	}
	defer func() { source.active = false }()
	if source.directory != nil || source.directoryAbsence == nil || !source.directoryAbsence.Absent || source.prior.Product.Present || source.successor.Product.Present {
		t.Fatal("absence invented a native directory or file owner")
	}
	// Removing intent must restore the ordinary missing-parent refusal.
	dir.State = "created_identity_received"
	d = canonicalClassifierDeps(t, j, map[string]string{})
	if _, err := prepareCanonicalRecoverySource(windowsProductRecoveryRequest{Context: context.Background()}, d, "original-journal", j, canonicalRecoveryRollback); err == nil {
		t.Fatal("missing parent without disposal intent was accepted")
	}
}

func TestBuiltWindowsCanonicalEntrySetTransaction(t *testing.T) {
	const inputEnv = "MCPHUB_CANONICAL_ENTRY_FIXTURE"
	const requiredEnv = "MCPHUB_CANONICAL_ENTRY_FIXTURE_REQUIRED"
	required, requireSet := os.LookupEnv(requiredEnv)
	if requireSet && required != "1" {
		t.Fatalf("%s must be 1 when supplied", requiredEnv)
	}
	inputPath, inputSet := os.LookupEnv(inputEnv)
	if !inputSet && !requireSet {
		t.Skip("no built canonical entry fixture supplied")
	}
	if !filepath.IsAbs(inputPath) {
		t.Fatalf("%s requires an absolute fixture input file", inputEnv)
	}
	inputBytes, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		ProductRoot          string `json:"product_root"`
		SourceEpoch          string `json:"source_epoch"`
		ProductVersion       string `json:"product_version"`
		BuildID              string `json:"build_id"`
		ReverseProjection    string `json:"reverse_projection"`
		CanonicalEntrySHA256 string `json:"canonical_entry_sha256"`
		ProductSHA256        string `json:"product_sha256"`
		IdentitySHA256       string `json:"identity_sha256"`
	}
	decoder := json.NewDecoder(bytes.NewReader(inputBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture input must be one JSON object: %v", err)
	}
	if !filepath.IsAbs(input.ProductRoot) || input.ProductVersion == "" ||
		!validLowerHexSHA256(input.SourceEpoch) || !validLowerHexSHA256(input.ProductSHA256) || !validLowerHexSHA256(input.IdentitySHA256) {
		t.Fatal("fixture input requires the absolute built package root, source epoch, version, and three file hashes")
	}
	binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(input.BuildID, input.CanonicalEntrySHA256, input.ReverseProjection)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	entryPath := filepath.Join(input.ProductRoot, binaryadmission.WindowsCanonicalEntryRelativePathV1)
	observed, err := binaryadmission.CheckWindowsCanonicalEntryBuildV1(ctx, entryPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	expected := observed.EntrySet
	if observed.SourceEpoch != input.SourceEpoch || expected.ProductArtifact.Version != input.ProductVersion ||
		expected.CanonicalEntry.SHA256 != input.CanonicalEntrySHA256 || expected.Product.SHA256 != input.ProductSHA256 || expected.Identity.SHA256 != input.IdentitySHA256 {
		t.Fatal("observed build does not match the supplied epoch, version, and file hashes")
	}
	buildTargets, err := binaryadmission.CanonicalEntryTargetsV1(entryPath)
	if err != nil {
		t.Fatal(err)
	}
	original, err := binaryadmission.AcquireWindowsCanonicalEntrySetV1(ctx, buildTargets, expected)
	if original != nil {
		t.Cleanup(func() {
			if err := original.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || original == nil {
		t.Fatalf("acquire original built entry set: %v", err)
	}
	closeStage := func(source *stagedWindowsCanonicalEntrySetV1) error {
		var priorErr error
		if source.prior != nil {
			priorErr = source.prior.Close()
		}
		return errors.Join(priorErr, source.original.Close())
	}
	stageInto := func(t *testing.T, destination string) (*stagedWindowsCanonicalEntrySetV1, binaryadmission.WindowsCanonicalEntrySetTargetsV1) {
		t.Helper()
		stage, err := original.Stage(ctx, destination)
		source := &stagedWindowsCanonicalEntrySetV1{original: stage}
		if stage != nil {
			t.Cleanup(func() {
				if err := closeStage(source); err != nil {
					t.Error(err)
				}
			})
		}
		if err != nil || stage == nil {
			t.Fatalf("stage original built entry set: %v", err)
		}
		paths, err := stage.Targets()
		if err != nil {
			t.Fatal(err)
		}
		return source, paths
	}
	assertAbsent := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, path := range paths {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected fixture path to be absent %q: %v", path, err)
			}
		}
	}
	for _, scenario := range []struct {
		name    string
		inverse bool
	}{
		{name: "commit-and-same-generation-noop"},
		{name: "predecision-inverse", inverse: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Log("Native file transaction and receipt with in-process task state and no Supervisor launch")
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			if err := os.Mkdir(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			targets, err := binaryadmission.CanonicalEntryTargetsV1(filepath.Join(root, binaryadmission.WindowsCanonicalEntryRelativePathV1))
			if err != nil {
				t.Fatal(err)
			}
			payloadDir := filepath.Dir(targets.ProductPath)
			legacy := filepath.Join(root, "mcphub-windowless.exe")
			journalPath := filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3)
			receiptPath := filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json")
			oldReceipt := filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json")
			assertAbsent(t, targets.CanonicalEntryPath, payloadDir, targets.IdentityPath, legacy, journalPath, receiptPath, oldReceipt)
			source, stages := stageInto(t, targets.CanonicalEntryPath)
			var order []string
			tasks := &productPairTaskFake{order: &order}
			publish := windowsProductEventPublisher(stateDir)
			publishCalls := 0
			txn := &WindowsProductTxn{Opts: WindowsProductTxnOpts{
				Context: ctx, EntrySet: source, StateDir: stateDir, Mode: WindowsProductPairModeCanonicalize,
				CanonicalEntryPath: targets.CanonicalEntryPath, ProductPath: targets.ProductPath, IdentityPath: targets.IdentityPath,
				LegacyWindowlessPath: legacy, StagedProduct: stages.ProductPath, Tasks: tasks,
				Deps: WindowsProductTxnDeps{PublishCommitted: func(event WindowsProductCommitEvent) (string, error) {
					publishCalls++
					return publish(event)
				}},
			}}
			fault := errors.New("fixture fault after native three-member promotion and task readback")
			faultReached := false
			if scenario.inverse {
				txn.Opts.Deps.Fault = func(stage WindowsProductStage) error {
					if stage != WindowsProductStageTasksReadBack {
						return nil
					}
					if err := txn.verifyLiveCanonicalEntrySet(); err != nil {
						return err
					}
					raw, err := os.ReadFile(journalPath)
					if err != nil {
						return err
					}
					j, err := decodeWindowsProductRecoveryJournalV3(raw)
					if err != nil {
						return err
					}
					if j.Phase != WindowsProductRecoverySuccessorStarting || j.EntrySet == nil ||
						j.Product.PriorPresent || j.EntrySet.CanonicalEntry.PriorPresent || j.EntrySet.Identity.PriorPresent ||
						!j.EntrySet.PayloadDirectory.Created || j.EntrySet.PayloadDirectory.Original == nil {
						return errors.New("native promotion lacks its original absent preimages and received directory identity")
					}
					faultReached = true
					return fault
				}
			}
			committed, runErr := txn.Run(ctx)
			if err := txn.Close(); err != nil {
				t.Fatal(errors.Join(runErr, err))
			}
			if scenario.inverse {
				if !faultReached || !errors.Is(runErr, fault) ||
					errors.Is(runErr, ErrWindowsProductRecoveryPending) || errors.Is(runErr, ErrWindowsProductCleanupPending) ||
					errors.Is(runErr, ErrWindowsProductPairRecoveryRollback) || errors.Is(runErr, ErrWindowsProductPairRecoveryCleanup) {
					t.Fatalf("fixture did not complete the intended native inverse: %v", runErr)
				}
				if publishCalls != 0 || !tasks.restored || !tasks.closed || !tasks.discarded ||
					!recoveryStepsOrdered(order, []string{"tasks-snapshot", "tasks-rewrite", "tasks-readback", "tasks-restore"}) {
					t.Fatalf("inverse task or publication state differs: %v, publications=%d", order, publishCalls)
				}
				assertAbsent(t, targets.CanonicalEntryPath, targets.ProductPath, targets.IdentityPath, payloadDir, journalPath, receiptPath, oldReceipt, stages.CanonicalEntryPath, stages.ProductPath, stages.IdentityPath)
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(stateDir) {
					t.Fatalf("inverse left fixture entries: %v, %v", entries, err)
				}
				return
			}
			if runErr != nil || committed.Outcome != WindowsProductPairCommittedOutcome || committed.Artifact.SHA256 != input.ProductSHA256 ||
				publishCalls != 1 || tasks.restored || !tasks.closed || !tasks.discarded ||
				!recoveryStepsOrdered(order, []string{"tasks-snapshot", "tasks-rewrite", "tasks-readback"}) {
				t.Fatalf("fresh native commit failed: outcome=%s, tasks=%v, publications=%d, err=%v", committed.Outcome, order, publishCalls, runErr)
			}
			assertAbsent(t, journalPath, oldReceipt, legacy, stages.CanonicalEntryPath, stages.ProductPath, stages.IdentityPath)
			raw, err := os.ReadFile(receiptPath)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeUpgradeReceipt(raw)
			if err != nil || decoded.V3 == nil || decoded.V3.EntrySet == nil {
				t.Fatalf("decode committed complete-set receipt: %v", err)
			}
			receipt := decoded.V3
			if receipt.Admission != UpgradeAdmissionCanonicalEntryProductV1 || receipt.EntrySet.BuildID != input.BuildID ||
				receipt.Artifact.SHA256 != input.ProductSHA256 || receipt.EntrySet.CanonicalEntrySHA256 != input.CanonicalEntrySHA256 ||
				receipt.EntrySet.IdentitySHA256 != input.IdentitySHA256 || receipt.TaskSetSHA256 != committed.TaskSetSHA256 ||
				recoveryTestSHA(raw) != committed.ReceiptSHA256 || !validLowerHexSHA256(committed.EventSHA256) {
				t.Fatal("committed receipt does not bind the original package and task readback")
			}
			paths := []string{targets.CanonicalEntryPath, targets.ProductPath, targets.IdentityPath}
			before := make([]os.FileInfo, len(paths))
			for i, path := range paths {
				before[i], err = os.Lstat(path)
				if err != nil || !before[i].Mode().IsRegular() {
					t.Fatalf("committed member missing or not ordinary %q: %v", path, err)
				}
			}
			next, noopStages := stageInto(t, targets.CanonicalEntryPath)
			received := 0
			noop, err := reconcileStagedCanonicalEntryNoop(ctx, stateDir, WindowsProductPairModeCanonicalize, next, func(current UpgradeReceiptV3) error {
				if current.TransactionID != receipt.TransactionID || current.TaskSetSHA256 != receipt.TaskSetSHA256 {
					return errors.New("no-op received a different committed generation")
				}
				received++
				return nil
			})
			if err != nil || !noop || received != 1 {
				t.Fatalf("same-generation no-op failed: received=%d, err=%v", received, err)
			}
			if err := closeStage(next); err != nil {
				t.Fatal(err)
			}
			assertAbsent(t, journalPath, noopStages.CanonicalEntryPath, noopStages.ProductPath, noopStages.IdentityPath)
			for i, path := range paths {
				after, err := os.Lstat(path)
				if err != nil || !os.SameFile(before[i], after) {
					t.Fatalf("same-generation no-op replaced member %q: %v", path, err)
				}
			}
			afterReceipt, err := os.ReadFile(receiptPath)
			if err != nil || !bytes.Equal(raw, afterReceipt) {
				t.Fatalf("same-generation no-op changed receipt bytes: %v", err)
			}
			if _, err := readCanonicalEntrySet(ctx, targets, expected); err != nil {
				t.Fatalf("same-generation no-op changed the installed package: %v", err)
			}
		})
	}
}
