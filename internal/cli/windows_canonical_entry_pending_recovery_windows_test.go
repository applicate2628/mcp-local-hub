//go:build windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

// This fixture exercises native original-object custody with an absent prior
// runtime. Successor bytes are journal-bound data, not claimed PE qualification.
func canonicalPendingValidationFixture(t *testing.T, phase WindowsProductRecoveryPhase) (windowsProductRecoveryRequest, windowsProductRecoveryJournalV3, map[string][]byte) {
	t.Helper()
	j := canonicalClassifierJournal(t, phase)
	j.Product.PriorPresent, j.Product.PriorSHA256 = false, ""
	j.EntrySet.Identity.PriorPresent, j.EntrySet.Identity.PriorSHA256 = false, ""
	if phase == WindowsProductRecoverySuccessorStarting {
		j.TaskSetSHA256 = strings.Repeat("6", 64)
	}
	root := filepath.Dir(j.EntrySet.CanonicalEntry.Target)
	if err := os.Mkdir(j.EntrySet.PayloadDirectory.Path, 0o700); err != nil {
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
	j.EntrySet.PayloadDirectory.Original.ParentIdentity = identity(root)
	j.EntrySet.PayloadDirectory.Original.DirectoryIdentity = identity(j.EntrySet.PayloadDirectory.Path)
	files := map[string][]byte{
		j.EntrySet.StagedEntry.Path:    []byte("journal-bound successor launcher"),
		j.Staged.Path:                  []byte("journal-bound successor Product"),
		j.EntrySet.StagedIdentity.Path: j.EntrySet.Expected.IdentityBytes,
	}
	j.EntrySet.CanonicalEntry.NewSHA256 = recoveryTestSHA(files[j.EntrySet.StagedEntry.Path])
	j.EntrySet.StagedEntry.SHA256 = j.EntrySet.CanonicalEntry.NewSHA256
	j.EntrySet.Expected.CanonicalEntry.SHA256 = j.EntrySet.CanonicalEntry.NewSHA256
	j.Product.NewSHA256 = recoveryTestSHA(files[j.Staged.Path])
	j.Staged.SHA256 = j.Product.NewSHA256
	j.EntrySet.Expected.Product.SHA256 = j.Product.NewSHA256
	j.EntrySet.Expected.ProductArtifact.SHA256 = j.Product.NewSHA256
	for path, body := range files {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw := writeCanonicalPendingValidationJournal(t, root, j)
	journalPath := filepath.Join(root, WindowsProductRecoveryFileLeafV3)
	files[journalPath] = raw
	return windowsProductRecoveryRequest{Context: context.Background(), StateDir: root, ProductPath: j.Product.Target, CanonicalEntryPath: j.EntrySet.CanonicalEntry.Target, IdentityPath: j.EntrySet.Identity.Target, LegacyWindowlessPath: j.LegacyWindowless.Target}, j, files
}

func writeCanonicalPendingValidationJournal(t *testing.T, stateDir string, j windowsProductRecoveryJournalV3) []byte {
	t.Helper()
	raw, err := encodeWindowsProductRecoveryJournalV3(j)
	if err != nil {
		t.Fatal(err)
	}
	// Use production publication and admission, including the Windows file DACL.
	// Validation keeps its default inode-anchored reader without a test bypass.
	if err := persistEncodedWindowsProductRecoveryJournalV3(filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3), j, raw, api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertCanonicalPendingValidationUnchanged(t *testing.T, j windowsProductRecoveryJournalV3, files map[string][]byte) {
	t.Helper()
	for path, expected := range files {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("validation changed original bytes at %q: %v", path, err)
		}
		// Also exercise owner release after both successful and failed validation.
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("validation retained original file custody: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{j.Product.Target, j.EntrySet.CanonicalEntry.Target, j.EntrySet.Identity.Target, j.LegacyWindowless.Target, j.Receipts.V3.Path, j.Receipts.V2.Path} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("validation created or restored %q: %v", path, err)
		}
	}
}

func TestCanonicalPendingValidationUsesOriginalSetAndEntrySetTasksWithoutEffects(t *testing.T) {
	for _, phase := range []WindowsProductRecoveryPhase{WindowsProductRecoveryPrepared, WindowsProductRecoveryGUIOwnedStopPending, WindowsProductRecoverySuccessorStarting, WindowsProductRecoverySettledRollback} {
		t.Run(string(phase), func(t *testing.T) {
			req, j, files := canonicalPendingValidationFixture(t, phase)
			tasks := &recoveryTaskTxnFake{}
			opened := 0
			req.Deps.ReopenTasks = func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error) {
				t.Fatal("complete set entered legacy Product-only task admission")
				return nil, nil
			}
			req.Deps.ReopenEntrySetTasks = func(_ context.Context, _ WindowsProductPairTaskRecovery, targets binaryadmission.WindowsCanonicalEntrySetTargetsV1) (WindowsProductTaskTxn, error) {
				opened++
				if targets.CanonicalEntryPath != req.CanonicalEntryPath || targets.ProductPath != req.ProductPath || targets.IdentityPath != req.IdentityPath {
					t.Fatal("task inverse received wrong complete-set targets")
				}
				return tasks, nil
			}
			if err := validateWindowsProductPendingRecovery(req, j); err != nil {
				t.Fatal(err)
			}
			want := 1
			if phase == WindowsProductRecoverySettledRollback {
				want = 0
			}
			if opened != want || tasks.closeCalls != want || tasks.restoreCalls != 0 || tasks.verifyCalls != 0 {
				t.Fatalf("unexpected task effects: opened=%d tasks=%+v", opened, tasks)
			}
			assertCanonicalPendingValidationUnchanged(t, j, files)
		})
	}
}

func TestCanonicalPendingValidationRejectsUnknownCompanionBeforeTaskAdmission(t *testing.T) {
	for _, member := range []string{"launcher", "identity"} {
		t.Run(member, func(t *testing.T) {
			req, j, files := canonicalPendingValidationFixture(t, WindowsProductRecoveryPrepared)
			path := j.EntrySet.CanonicalEntry.Target
			if member == "identity" {
				path = j.EntrySet.Identity.Target
			}
			body := []byte("unrecognized canonical preimage")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			req.Deps.ReopenEntrySetTasks = func(context.Context, WindowsProductPairTaskRecovery, binaryadmission.WindowsCanonicalEntrySetTargetsV1) (WindowsProductTaskTxn, error) {
				t.Fatal("unrecognized companion reached task owner")
				return nil, nil
			}
			if err := validateWindowsProductPendingRecovery(req, j); err == nil {
				t.Fatal("unrecognized complete-set companion admitted")
			}
			if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, body) {
				t.Fatal("validation changed foreign companion", err)
			}
			for originalPath, expected := range files {
				if actual, err := os.ReadFile(originalPath); err != nil || !bytes.Equal(actual, expected) {
					t.Fatal("failed validation changed original bytes", err)
				}
			}
		})
	}
}

type canonicalPendingTaskCloseFailure struct {
	recoveryTaskTxnFake
	failure error
}

func (t *canonicalPendingTaskCloseFailure) Close() error {
	t.closeCalls++
	return t.failure
}

func TestCanonicalPendingValidationClosesTaskOwnerReturnedWithError(t *testing.T) {
	req, j, files := canonicalPendingValidationFixture(t, WindowsProductRecoveryPrepared)
	openFailure, closeFailure := errors.New("task inverse unavailable"), errors.New("task owner release unconfirmed")
	tasks := &canonicalPendingTaskCloseFailure{failure: closeFailure}
	req.Deps.ReopenEntrySetTasks = func(context.Context, WindowsProductPairTaskRecovery, binaryadmission.WindowsCanonicalEntrySetTargetsV1) (WindowsProductTaskTxn, error) {
		return tasks, openFailure
	}
	err := validateWindowsProductPendingRecovery(req, j)
	if !errors.Is(err, openFailure) || !errors.Is(err, closeFailure) || tasks.closeCalls != 1 {
		t.Fatalf("task admission/release evidence lost: error=%v closes=%d", err, tasks.closeCalls)
	}
	assertCanonicalPendingValidationUnchanged(t, j, files)
}

func TestCanonicalPendingValidationAdmitsOriginalLegacyRootRuntime(t *testing.T) {
	req, j, files := canonicalPendingValidationFixture(t, WindowsProductRecoveryPrepared)
	priorPath := j.EntrySet.CanonicalEntry.Target
	writeUpgradeGUIAdmittedProduct(t, priorPath, "ORIGINAL-LEGACY-RUNTIME")
	priorBytes := mustReadFile(t, priorPath)
	j.EntrySet.CanonicalEntry.PriorPresent = true
	j.EntrySet.CanonicalEntry.PriorSHA256 = recoveryTestSHA(priorBytes)
	j.Mode = WindowsProductPairModeUpgrade
	j.FrozenPorts = []api.SupervisorFrozenPort{}
	j.PreSettlement = &windowsProductPreSettlementV3{Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent}, GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true, Ports: []windowsProductPortSettlementV1{}}
	j.EntrySet.PriorRuntime = &windowsCanonicalEntryRuntimePreimageV1{Path: priorPath, Present: true, SHA256: recoveryTestSHA(priorBytes)}
	files[priorPath] = priorBytes
	files[filepath.Join(req.StateDir, WindowsProductRecoveryFileLeafV3)] = writeCanonicalPendingValidationJournal(t, req.StateDir, j)
	tasks := &recoveryTaskTxnFake{}
	req.Deps.AdmitProduct = func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		t.Fatal("original root runtime replaced with nested Product path admission")
		return binaryadmission.WindowsArtifact{}, nil
	}
	req.Deps.ReopenEntrySetTasks = func(context.Context, WindowsProductPairTaskRecovery, binaryadmission.WindowsCanonicalEntrySetTargetsV1) (WindowsProductTaskTxn, error) {
		return tasks, nil
	}
	if err := validateWindowsProductPendingRecovery(req, j); err != nil {
		t.Fatal(err)
	}
	if tasks.closeCalls != 1 || tasks.restoreCalls != 0 {
		t.Fatalf("unexpected original runtime validation task effects: %+v", tasks)
	}
	for path, expected := range files {
		if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("original runtime validation changed %q: %v", path, err)
		}
	}
}
