//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
)

type canonicalPrepareFixture struct {
	txn       *WindowsProductTxn
	stages    map[string][]byte
	prior     map[string][]byte
	directory os.FileInfo
}

// The PE bytes are synthetic and never executed. The public admission and
// staging owners still verify their actual metadata, hashes and native objects.
func newCanonicalPrepareFixture(t *testing.T, existingDirectory, guiOwned bool) canonicalPrepareFixture {
	t.Helper()
	ctx := context.Background()
	source, err := binaryadmission.CanonicalEntryTargetsV1(filepath.Join(t.TempDir(), "mcphub.exe"))
	if err != nil {
		t.Fatal(err)
	}
	launcher := mustReadFile(t, writeEditorPEFixture(t, binaryadmission.WindowsCUISubsystem))
	binary.LittleEndian.PutUint16(launcher[0x84:0x86], 0x8664)
	if err := os.WriteFile(source.CanonicalEntryPath, launcher, 0o600); err != nil {
		t.Fatal(err)
	}
	binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(strings.Repeat("a", 32), recoveryTestSHA(launcher), binaryadmission.WindowsCanonicalEntryReverseProjectionV1)
	if err != nil {
		t.Fatal(err)
	}
	comments, err := binaryadmission.EncodeWindowsCanonicalEntryCommentsV1("prepare fixture", binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(source.ProductPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeUpgradeGUIAdmittedProduct(t, source.ProductPath, "PREPARE-SUCCESSOR", comments)
	product, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: source.ProductPath, Role: binaryadmission.WindowsArtifactRoleProduct})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := json.Marshal(binaryadmission.WindowsProductIdentityV1{
		Schema: "windows-product-identity-v1", Role: product.Role, Filename: "mcphub.exe",
		Subsystem: product.Subsystem, Machine: product.Machine, Version: product.Version,
		Commit: product.Commit, BuildDate: product.BuildDate, SHA256: product.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.IdentityPath, identity, 0o600); err != nil {
		t.Fatal(err)
	}
	admitted, err := binaryadmission.AdmitWindowsCanonicalEntrySetV1(ctx, source.CanonicalEntryPath, binding)
	if admitted != nil {
		t.Cleanup(func() {
			if err := admitted.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || admitted == nil {
		t.Fatalf("admit complete source fixture: %v", err)
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV1(filepath.Join(t.TempDir(), "mcphub.exe"))
	if err != nil {
		t.Fatal(err)
	}
	f := canonicalPrepareFixture{stages: make(map[string][]byte), prior: make(map[string][]byte)}
	if existingDirectory {
		if err := os.Mkdir(filepath.Dir(targets.ProductPath), 0o700); err != nil {
			t.Fatal(err)
		}
		f.directory, err = os.Stat(filepath.Dir(targets.ProductPath))
		if err != nil {
			t.Fatal(err)
		}
	}
	if guiOwned {
		writeUpgradeGUIAdmittedProduct(t, targets.CanonicalEntryPath, "PREPARE-LEGACY-PRIOR")
		f.prior[targets.CanonicalEntryPath] = mustReadFile(t, targets.CanonicalEntryPath)
	}
	stage, err := admitted.Stage(ctx, targets.CanonicalEntryPath)
	if stage != nil {
		t.Cleanup(func() {
			if err := stage.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || stage == nil {
		t.Fatalf("native stage fixture: %v", err)
	}
	stages, err := stage.Targets()
	if err != nil {
		t.Fatal(err)
	}
	for member, path := range map[string]string{"launcher": stages.CanonicalEntryPath, "product": stages.ProductPath, "identity": stages.IdentityPath} {
		present, raw, _, err := stage.ReadOriginal(member)
		if err != nil || !present {
			t.Fatalf("read owned %s stage: present=%v error=%v", member, present, err)
		}
		f.stages[path] = raw
	}
	if err := admitted.Close(); err != nil {
		t.Fatal(err)
	}
	var order []string
	f.txn = &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		Context: ctx, StateDir: apitest.HardenedTempDir(t), Mode: WindowsProductPairModeCanonicalize,
		EntrySet:           &stagedWindowsCanonicalEntrySetV1{original: stage},
		CanonicalEntryPath: targets.CanonicalEntryPath, ProductPath: targets.ProductPath, IdentityPath: targets.IdentityPath,
		LegacyWindowlessPath: filepath.Join(filepath.Dir(targets.CanonicalEntryPath), "mcphub-windowless.exe"),
		StagedProduct:        stages.ProductPath, Tasks: &productPairTaskFake{order: &order},
		Deps: WindowsProductTxnDeps{PublishCommitted: func(WindowsProductCommitEvent) (string, error) {
			t.Error("Prepare published a committed event")
			return "", errors.New("unexpected commit publication")
		}},
	}}
	if guiOwned {
		f.txn.Opts.Mode = WindowsProductPairModeUpgrade
		f.txn.Opts.FrozenPorts = []windowsProductIntentPortV1{}
		f.txn.Opts.PreSettlement = &windowsProductQuiescenceObservationV1{
			GUIOwner: windowsProductGUIOwnerConfirmedPresent,
			Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
				Generation: &windowsProductSupervisorGenerationV1{PID: 4123, StartedAt: "2026-09-29T00:00:00Z"}},
			Ports: []windowsProductPortSettlementV1{},
		}
	}
	t.Cleanup(func() {
		if err := f.txn.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func assertCanonicalPrepareColdAdmission(t *testing.T, txn *WindowsProductTxn) {
	t.Helper()
	path := filepath.Join(txn.Opts.StateDir, WindowsProductRecoveryFileLeafV3)
	raw, err := api.ReadStateFileInodeAnchored(path)
	if err != nil {
		t.Fatal(err)
	}
	j, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil || j.EntrySet == nil || j.EntrySet.PayloadDirectory.Original == nil {
		t.Fatalf("first durable checkpoint cannot supply native directory identity: %v", err)
	}
	tasks := &recoveryTaskTxnFake{}
	req := windowsProductRecoveryRequest{
		Context: context.Background(), StateDir: txn.Opts.StateDir,
		CanonicalEntryPath: txn.Opts.CanonicalEntryPath, ProductPath: txn.Opts.ProductPath,
		IdentityPath: txn.Opts.IdentityPath, LegacyWindowlessPath: txn.Opts.LegacyWindowlessPath,
		Deps: windowsProductRecoveryDeps{ReopenEntrySetTasks: func(context.Context, WindowsProductPairTaskRecovery, binaryadmission.WindowsCanonicalEntrySetTargetsV1) (WindowsProductTaskTxn, error) {
			return tasks, nil
		}},
	}
	if err := validateWindowsProductPendingRecovery(req, j); err != nil {
		t.Fatalf("cold native admission of the first durable checkpoint: %v", err)
	}
	if tasks.closeCalls != 1 || tasks.restoreCalls != 0 {
		t.Fatalf("cold admission task effects: close=%d restore=%d", tasks.closeCalls, tasks.restoreCalls)
	}
}

func TestCanonicalPrepareFirstJournalContainsOriginalDirectory(t *testing.T) {
	for _, tc := range []struct {
		name              string
		existingDirectory bool
		guiOwned          bool
	}{
		{name: "created payload directory"},
		{name: "existing payload directory", existingDirectory: true},
		{name: "GUI-owned legacy prior", guiOwned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCanonicalPrepareFixture(t, tc.existingDirectory, tc.guiOwned)
			writes := 0
			f.txn.Opts.Deps.WriteRecoveryJournal = func(path string, raw []byte) error {
				writes++
				j, err := decodeWindowsProductRecoveryJournalV3(raw)
				if err != nil {
					return err
				}
				if j.EntrySet == nil || j.EntrySet.PayloadDirectory.Original == nil {
					return errors.New("first journal has no original payload directory identity")
				}
				d := j.EntrySet.PayloadDirectory
				if d.PriorPresent != tc.existingDirectory || d.Created == tc.existingDirectory || d.Original.Created != d.Created {
					return fmt.Errorf("first journal changed directory preimage: %+v", d)
				}
				wantPhase := WindowsProductRecoveryPrepared
				if tc.guiOwned {
					wantPhase = WindowsProductRecoveryGUIOwnedStopPending
					if j.EntrySet.PriorRuntime == nil || !j.EntrySet.PriorRuntime.Present || j.EntrySet.PriorRuntime.Path != f.txn.Opts.CanonicalEntryPath {
						return errors.New("GUI-owned checkpoint lost its admitted legacy prior runtime")
					}
				}
				if j.Phase != wantPhase {
					return fmt.Errorf("first journal phase=%s, want %s", j.Phase, wantPhase)
				}
				return api.WriteStateFileBytesAtomic(path, raw)
			}
			if err := f.txn.Prepare(); err != nil {
				t.Fatal(err)
			}
			if writes != 1 {
				t.Fatalf("Prepare made %d journal writes; first checkpoint must already be complete", writes)
			}
			if err := f.txn.Close(); err != nil {
				t.Fatal(err)
			}
			assertCanonicalPrepareColdAdmission(t, f.txn)
			for path, want := range f.prior {
				if got := mustReadFile(t, path); !bytes.Equal(got, want) {
					t.Fatalf("Prepare changed prior runtime at %q", path)
				}
			}
		})
	}
}

func TestCanonicalPrepareUnwrittenJournalCleansOnlyOwnedResources(t *testing.T) {
	for _, existingDirectory := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior_directory_%t", existingDirectory), func(t *testing.T) {
			f := newCanonicalPrepareFixture(t, existingDirectory, false)
			fault := errors.New("fixture journal write failed before publication")
			writes := 0
			f.txn.Opts.Deps.WriteRecoveryJournal = func(string, []byte) error { writes++; return fault }
			if err := f.txn.Prepare(); !errors.Is(err, fault) {
				t.Fatalf("Prepare error=%v, want original writer failure", err)
			}
			if err := f.txn.Prepare(); !errors.Is(err, fault) || writes != 1 {
				t.Fatalf("failed preparation retried or reported success: error=%v writes=%d", err, writes)
			}
			for i := 0; i < 2; i++ {
				if err := f.txn.Close(); err != nil {
					t.Fatalf("Close attempt %d: %v", i+1, err)
				}
			}
			for path := range f.stages {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unwritten transaction retained own stage %q: %v", path, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(f.txn.Opts.StateDir, WindowsProductRecoveryFileLeafV3)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unwritten journal unexpectedly present: %v", err)
			}
			current, err := os.Stat(filepath.Dir(f.txn.Opts.ProductPath))
			if existingDirectory {
				if err != nil || !os.SameFile(current, f.directory) {
					t.Fatalf("pre-existing payload directory was removed or replaced: %v", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unwritten transaction retained created payload directory: %v", err)
			}
		})
	}
}

func TestCanonicalPrepareUncertainJournalPreservesColdRecovery(t *testing.T) {
	for _, readbackFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("readback_failure_%t", readbackFailure), func(t *testing.T) {
			f := newCanonicalPrepareFixture(t, false, false)
			fault := errors.New("fixture journal publication acknowledgement unavailable")
			writes := 0
			f.txn.Opts.Deps.WriteRecoveryJournal = func(path string, raw []byte) error {
				writes++
				if err := api.WriteStateFileBytesAtomic(path, raw); err != nil {
					return err
				}
				if !readbackFailure {
					return fault
				}
				return nil
			}
			if readbackFailure {
				f.txn.Opts.Deps.ReadRecoveryJournal = func(string) ([]byte, error) { return nil, fault }
			}
			if err := f.txn.Prepare(); !errors.Is(err, fault) {
				t.Fatalf("Prepare error=%v, want uncertain publication", err)
			}
			if err := f.txn.Prepare(); !errors.Is(err, fault) || writes != 1 {
				t.Fatalf("uncertain preparation retried or reported success: error=%v writes=%d", err, writes)
			}
			for i := 0; i < 2; i++ {
				if err := f.txn.Close(); err != nil {
					t.Fatalf("Close attempt %d: %v", i+1, err)
				}
			}
			for path, want := range f.stages {
				if got := mustReadFile(t, path); !bytes.Equal(got, want) {
					t.Fatalf("uncertain publication changed journal-bound stage %q", path)
				}
			}
			assertCanonicalPrepareColdAdmission(t, f.txn)
		})
	}
}
