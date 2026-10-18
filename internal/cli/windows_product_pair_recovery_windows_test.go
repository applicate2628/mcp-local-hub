//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/scheduler"
)

type recoveryTaskTxnFake struct {
	order        *[]string
	restoreCalls int
	verifyCalls  int
	closeCalls   int
	digest       string
}

func (*recoveryTaskTxnFake) InventoryExport() error { return nil }
func (*recoveryTaskTxnFake) RecoveryDescriptor() (WindowsProductPairTaskRecovery, error) {
	return WindowsProductPairTaskRecovery{}, nil
}
func (*recoveryTaskTxnFake) RewriteCommand() error  { return nil }
func (f *recoveryTaskTxnFake) VerifyRuntime() error { f.verifyCalls++; return nil }
func (f *recoveryTaskTxnFake) VerifyRuntimeSHA256() (string, error) {
	f.verifyCalls++
	if f.digest == "" {
		return strings.Repeat("e", sha256.Size*2), nil
	}
	return f.digest, nil
}
func (f *recoveryTaskTxnFake) RestoreImport() error {
	f.restoreCalls++
	if f.order != nil {
		*f.order = append(*f.order, "restore-tasks")
	}
	return nil
}
func (*recoveryTaskTxnFake) DiscardRecoveryStore() error { return nil }
func (f *recoveryTaskTxnFake) Close() error              { f.closeCalls++; return nil }

func TestWindowsProductRecoveryRollsBackPredecisionProductTasksLegacyAndReceipts(t *testing.T) {
	fixture := newProductRecoveryV3Fixture(t, WindowsProductRecoverySuccessorStarting)
	tasks := &recoveryTaskTxnFake{digest: fixture.journal.TaskSetSHA256}
	req := fixture.request(tasks)
	settled, err := reconcileWindowsProductRecovery(req)
	if err != nil {
		t.Fatalf("recover predecision: %v", err)
	}
	if settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
		t.Fatalf("settled=%+v", settled)
	}
	assertPairFile(t, fixture.product, fixture.priorProduct)
	assertPairFile(t, fixture.legacy, fixture.priorLegacy)
	assertPairFile(t, fixture.receiptV2, fixture.priorReceiptV2)
	if _, err := os.Stat(fixture.receiptV3); !os.IsNotExist(err) {
		t.Fatalf("V3 receipt preimage was not restored: %v", err)
	}
	if tasks.restoreCalls != 1 {
		t.Fatalf("task restore calls=%d", tasks.restoreCalls)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, WindowsProductRecoveryFileLeafV3)); !os.IsNotExist(err) {
		t.Fatalf("settled rollback journal remains: %v", err)
	}
}

func TestWindowsProductRecoveryGUIOwnedPendingNeverClearsJournalWithoutFleetProof(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoveryPrepared)
	f.journal.Mode = WindowsProductPairModeUpgrade
	f.journal.Phase = WindowsProductRecoveryGUIOwnedStopPending
	f.journal.PreSettlement = &windowsProductPreSettlementV3{
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: 4123, StartedAt: "2026-09-29T00:00:00Z"}},
		Ports: []windowsProductPortSettlementV1{}, GUIOwner: windowsProductGUIOwnerConfirmedPresent,
	}
	path := filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)
	if err := persistWindowsProductRecoveryJournalV3(path, f.journal, api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
		t.Fatal(err)
	}
	tasks := &recoveryTaskTxnFake{}
	settled, err := reconcileWindowsProductRecovery(f.request(tasks))
	if settled != nil || !errors.Is(err, ErrWindowsProductRecoveryPending) {
		t.Fatalf("crash recovery without fleet proof settled=%+v err=%v", settled, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending journal was removed: %v", err)
	}
	req := f.request(tasks)
	req.RecoverPriorSettlement = func(_ context.Context, j windowsProductRecoveryJournalV3) error {
		assertPairFile(t, f.product, f.priorProduct)
		if j.PreSettlement == nil || j.PreSettlement.Supervisor.Generation == nil ||
			j.PreSettlement.Supervisor.Generation.PID != 4123 || j.PriorFleetReaped {
			t.Fatalf("recovery callback lost live preimage: %+v", j)
		}
		return nil
	}
	settled, err = reconcileWindowsProductRecovery(req)
	if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
		t.Fatalf("verified prior fleet did not settle: settled=%+v err=%v", settled, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled journal remains: %v", err)
	}
}

func TestWindowsProductRecoveryCompletesCommitDecisionForward(t *testing.T) {
	fixture := newProductRecoveryV3Fixture(t, WindowsProductRecoveryCommitDecided)
	tasks := &recoveryTaskTxnFake{digest: fixture.journal.TaskSetSHA256}
	req := fixture.request(tasks)
	req.ObserveSuccessorReady = func(context.Context, string, string) (bool, error) { return true, nil }
	req.StartSupervisor = func(string) (SupervisorLaunch, error) { return nil, errors.New("ready successor must not restart") }
	req.WaitSupervisorReady = func(context.Context, string, string) error {
		return errors.New("ready successor must not wait")
	}
	req.Deps.AdmitProduct = func(input binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		digest, err := hashRecoveryFile(input.Path)
		if err != nil {
			return binaryadmission.WindowsArtifact{}, err
		}
		return binaryadmission.WindowsArtifact{
			Path: input.Path, Role: binaryadmission.WindowsArtifactRoleProduct,
			Machine: 34404, Subsystem: binaryadmission.WindowsGUISubsystem,
			Version: "0.4.38", Commit: strings.Repeat("a", 40),
			BuildDate: "2026-09-11T00:00:00Z", SHA256: digest,
		}, nil
	}
	published := 0
	req.Deps.PublishCommitted = func(WindowsProductCommitEvent) (string, error) {
		published++
		return strings.Repeat("f", sha256.Size*2), nil
	}
	req.Deps.Now = func() time.Time { return time.Date(2026, 9, 11, 0, 0, 1, 0, time.UTC) }
	settled, err := reconcileWindowsProductRecovery(req)
	if err != nil {
		t.Fatalf("recover commit_decided: %v", err)
	}
	if settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit || published != 1 {
		t.Fatalf("settled=%+v published=%d", settled, published)
	}
	assertPairFile(t, fixture.product, fixture.newProduct)
	for _, path := range []string{fixture.legacy, fixture.productAside, fixture.legacyAside, fixture.receiptV2, filepath.Join(fixture.stateDir, WindowsProductRecoveryFileLeafV3)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("forward recovery residue %s: %v", path, err)
		}
	}
	raw, err := os.ReadFile(fixture.receiptV3)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil || decoded.V3 == nil || decoded.V3.TransactionID != fixture.journal.TransactionID {
		t.Fatalf("receipt=%+v err=%v", decoded, err)
	}
}

func TestWindowsProductRecoveryForwardReplaysInterruptedCleanup(t *testing.T) {
	for _, stop := range []string{"staged", "product-prior", "legacy-prior", "product-baseline", "legacy-baseline", "receipt-write", "receipt-readback"} {
		t.Run(stop, func(t *testing.T) {
			f := newProductRecoveryV3Fixture(t, WindowsProductRecoveryCommitDecided)
			productBaseline := f.product + ".old-20260911T000002Z"
			legacyBaseline := f.legacy + ".old-20260911T000003Z"
			for path, body := range map[string][]byte{
				f.staged: f.newProduct, productBaseline: []byte("product-baseline"),
				legacyBaseline: []byte("legacy-baseline"),
			} {
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f.journal.Product.BaselineAsides = []windowsProductRecoveryAsideV3{{
				Basename: filepath.Base(productBaseline), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("product-baseline"))),
			}}
			f.journal.LegacyWindowless.BaselineAsides = []windowsProductRecoveryAsideV3{{
				Basename: filepath.Base(legacyBaseline), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("legacy-baseline"))),
			}}
			journalPath := filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)
			if err := persistWindowsProductRecoveryJournalV3(journalPath, f.journal, api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
				t.Fatal(err)
			}
			tasks := &recoveryTaskTxnFake{digest: f.journal.TaskSetSHA256}
			published := 0
			newRequest := func() windowsProductRecoveryRequest {
				req := f.request(tasks)
				req.ObserveSuccessorReady = func(context.Context, string, string) (bool, error) { return true, nil }
				req.StartSupervisor = func(string) (SupervisorLaunch, error) {
					t.Fatal("ready successor restarted")
					return &fixtureSupervisorLaunch{}, nil
				}
				req.WaitSupervisorReady = func(context.Context, string, string) error { t.Fatal("ready successor waited"); return nil }
				req.Deps.AdmitProduct = func(input binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
					actual, err := hashRecoveryFile(input.Path)
					if err != nil || actual != input.SHA256 {
						return binaryadmission.WindowsArtifact{}, errors.New("canonical successor differs")
					}
					return binaryadmission.WindowsArtifact{Path: input.Path, Role: input.Role, SHA256: actual,
						Machine: 34404, Subsystem: binaryadmission.WindowsGUISubsystem, Version: "0.4.38",
						Commit: strings.Repeat("a", 40), BuildDate: "2026-09-29T00:00:00Z"}, nil
				}
				req.Deps.PublishCommitted = func(event WindowsProductCommitEvent) (string, error) {
					for _, target := range []string{f.product, f.legacy} {
						asides, err := listGeneratedRenameAsideBasenames(target)
						if err != nil || len(asides) != 0 {
							t.Fatalf("published with generated asides target=%s asides=%v err=%v", target, asides, err)
						}
					}
					published++
					return strings.Repeat("f", 64), nil
				}
				return req
			}
			req := newRequest()
			if stop == "receipt-write" {
				req.Deps.WriteReceipt = func(path string, body []byte) error {
					if err := api.WriteStateFileBytesAtomic(path, body); err != nil {
						return err
					}
					return errors.New("injected interruption after receipt write")
				}
			} else if stop == "receipt-readback" {
				req.Deps.ReadReceipt = func(path string) ([]byte, error) {
					body, err := api.ReadStateFileInodeAnchored(path)
					if err != nil {
						return nil, err
					}
					return body, errors.New("injected interruption after receipt readback")
				}
			} else {
				stopPath := map[string]string{"staged": f.staged, "product-prior": f.productAside,
					"legacy-prior": f.legacyAside, "product-baseline": productBaseline, "legacy-baseline": legacyBaseline}[stop]
				req.Deps.RemoveExact = func(path string) error {
					if err := os.Remove(path); err != nil {
						return err
					}
					if path == stopPath {
						return errors.New("injected interruption after aside removal")
					}
					return nil
				}
			}
			if settled, err := reconcileWindowsProductRecovery(req); settled != nil || !errors.Is(err, ErrWindowsProductCleanupPending) {
				t.Fatalf("interruption settled=%+v err=%v", settled, err)
			}
			if published != 0 {
				t.Fatalf("published before cleanup proof: %d", published)
			}
			if _, err := os.Stat(journalPath); err != nil {
				t.Fatalf("commit decision lost: %v", err)
			}
			settled, err := reconcileWindowsProductRecovery(newRequest())
			if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit || published != 1 {
				t.Fatalf("forward replay settled=%+v err=%v published=%d", settled, err, published)
			}
			if tasks.restoreCalls != 0 {
				t.Fatalf("postdecision task rollback: %d", tasks.restoreCalls)
			}
			raw, err := os.ReadFile(f.receiptV3)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := DecodeUpgradeReceipt(raw)
			if err != nil || receipt.V3 == nil || receipt.V3.TransactionID != f.journal.TransactionID ||
				!receipt.V3.Retired.RollbackImagesAbsent {
				t.Fatalf("forward receipt did not bind cleanup: receipt=%+v err=%v", receipt, err)
			}
			if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("settled journal remains: %v", err)
			}
		})
	}
}

func TestWindowsProductRecoveryForwardRejectsUnboundAsides(t *testing.T) {
	for _, mutation := range []string{"foreign-product", "foreign-legacy", "wrong-prior-hash", "duplicate-prior", "successor-aside", "wrong-baseline-hash"} {
		t.Run(mutation, func(t *testing.T) {
			f := newProductRecoveryV3Fixture(t, WindowsProductRecoveryCommitDecided)
			if err := os.Remove(f.productAside); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(f.legacyAside); err != nil {
				t.Fatal(err)
			}
			target, body := f.productAside, []byte("foreign-product")
			switch mutation {
			case "foreign-product":
				target = f.product + ".old-20260911T000004Z"
			case "foreign-legacy":
				target, body = f.legacy+".old-20260911T000004Z", []byte("foreign-legacy")
			case "wrong-prior-hash":
			case "duplicate-prior":
				if err := os.WriteFile(f.productAside, f.priorProduct, 0o600); err != nil {
					t.Fatal(err)
				}
				target, body = f.product+".old-20260911T000004Z", f.priorProduct
			case "successor-aside":
				target, body = f.product+".old-20260911T000004Z", f.newProduct
			case "wrong-baseline-hash":
				f.journal.Product.BaselineAsides = []windowsProductRecoveryAsideV3{{
					Basename: filepath.Base(f.productAside), SHA256: f.journal.Product.PriorSHA256,
				}}
			}
			if err := os.WriteFile(target, body, 0o600); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)
			if err := persistWindowsProductRecoveryJournalV3(journalPath, f.journal, api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
				t.Fatal(err)
			}
			tasks := &recoveryTaskTxnFake{digest: f.journal.TaskSetSHA256}
			req := f.request(tasks)
			req.ObserveSuccessorReady = func(context.Context, string, string) (bool, error) { return true, nil }
			req.StartSupervisor = func(string) (SupervisorLaunch, error) {
				t.Fatal("ready successor restarted")
				return &fixtureSupervisorLaunch{}, nil
			}
			req.WaitSupervisorReady = func(context.Context, string, string) error { t.Fatal("ready successor waited"); return nil }
			req.Deps.AdmitProduct = func(input binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
				actual, err := hashRecoveryFile(input.Path)
				if err != nil || actual != input.SHA256 {
					return binaryadmission.WindowsArtifact{}, errors.New("canonical successor differs")
				}
				return binaryadmission.WindowsArtifact{Path: input.Path, Role: input.Role, SHA256: actual}, nil
			}
			req.Deps.PublishCommitted = func(WindowsProductCommitEvent) (string, error) {
				t.Fatal("published with unbound aside")
				return "", nil
			}
			if settled, err := reconcileWindowsProductRecovery(req); settled != nil || !errors.Is(err, ErrWindowsProductCleanupPending) {
				t.Fatalf("unbound aside settled=%+v err=%v", settled, err)
			}
			if _, err := os.Stat(journalPath); err != nil {
				t.Fatalf("commit decision lost: %v", err)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("unbound aside was removed: %v", err)
			}
			if tasks.restoreCalls != 0 {
				t.Fatalf("postdecision task rollback: %d", tasks.restoreCalls)
			}
		})
	}
}

func TestWindowsProductRecoveryPredecisionStillRequiresPriorAside(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySuccessorStarting)
	if err := os.Remove(f.productAside); err != nil {
		t.Fatal(err)
	}
	tasks := &recoveryTaskTxnFake{digest: f.journal.TaskSetSHA256}
	settled, err := reconcileWindowsProductRecovery(f.request(tasks))
	if settled != nil || !errors.Is(err, ErrWindowsProductRecoveryIdentity) {
		t.Fatalf("predecision missing prior aside settled=%+v err=%v", settled, err)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		t.Fatalf("predecision journal lost: %v", err)
	}
}

type productRecoveryV3Fixture struct {
	stateDir, product, legacy, staged, receiptV3, receiptV2 string
	productAside, legacyAside                               string
	priorProduct, newProduct, priorLegacy, priorReceiptV2   []byte
	journal                                                 windowsProductRecoveryJournalV3
}

func newProductRecoveryV3Fixture(t *testing.T, phase WindowsProductRecoveryPhase) productRecoveryV3Fixture {
	t.Helper()
	dir := t.TempDir()
	f := productRecoveryV3Fixture{
		stateDir: filepath.Join(dir, "state"), product: filepath.Join(dir, "mcphub.exe"),
		legacy: filepath.Join(dir, "mcphub-windowless.exe"), staged: filepath.Join(dir, "mcphub.exe.stage"),
		priorProduct: []byte("prior-product"), newProduct: []byte("new-product"),
		priorLegacy: []byte("prior-legacy"), priorReceiptV2: []byte("prior-v2-receipt"),
	}
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.receiptV3 = filepath.Join(f.stateDir, UpgradeReceiptSchemaV3+".json")
	f.receiptV2 = filepath.Join(f.stateDir, UpgradeReceiptSchemaV2+".json")
	f.productAside = f.product + ".old-20260911T000000Z"
	f.legacyAside = f.legacy + ".old-20260911T000001Z"
	for path, body := range map[string][]byte{
		f.product: f.newProduct, f.productAside: f.priorProduct,
		f.legacyAside: f.priorLegacy, f.receiptV3: []byte("partial-v3"),
		f.receiptV2: []byte("partial-v2"),
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hash := func(body []byte) string { return fmt.Sprintf("%x", sha256.Sum256(body)) }
	f.journal = windowsProductRecoveryJournalV3{
		Schema: WindowsProductRecoverySchemaV3, TransactionID: strings.Repeat("a", 32),
		Phase: phase, Mode: WindowsProductPairModeCanonicalize,
		Product: windowsProductRecoveryArtifactV3{
			Target: f.product, PriorPresent: true, PriorSHA256: hash(f.priorProduct),
			NewSHA256: hash(f.newProduct), BaselineAsides: []windowsProductRecoveryAsideV3{},
		},
		LegacyWindowless: windowsProductRecoveryLegacyWindowlessV3{
			Target: f.legacy, PriorPresent: true, PriorSHA256: hash(f.priorLegacy),
			BaselineAsides: []windowsProductRecoveryAsideV3{},
		},
		Receipts: windowsProductRecoveryReceiptsV3{
			V3: windowsProductPairRecoveryReceipt{Path: f.receiptV3},
			V2: windowsProductPairRecoveryReceipt{Path: f.receiptV2, PriorPresent: true, PriorBytes: append([]byte(nil), f.priorReceiptV2...)},
		},
		Tasks:       WindowsProductPairTaskRecovery{StoreRoot: filepath.Join(f.stateDir, "task-store"), Backups: []WindowsProductPairTaskBackup{}},
		Staged:      windowsProductRecoveryStagedV3{Path: f.staged, SHA256: hash(f.newProduct)},
		FrozenPorts: []api.SupervisorFrozenPort{}, TaskSetSHA256: strings.Repeat("e", sha256.Size*2),
	}
	if phase == WindowsProductRecoveryPrepared || phase == WindowsProductRecoverySettledRollback {
		f.journal.TaskSetSHA256 = ""
	}
	if err := persistWindowsProductRecoveryJournalV3(
		filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3), f.journal,
		api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored,
	); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f productRecoveryV3Fixture) request(tasks WindowsProductTaskTxn) windowsProductRecoveryRequest {
	return windowsProductRecoveryRequest{
		Context: context.Background(), StateDir: f.stateDir,
		ProductPath: f.product, LegacyWindowlessPath: f.legacy,
		Deps: windowsProductRecoveryDeps{
			ReopenTasks: func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error) {
				return tasks, nil
			},
			DiscardTaskStore: func(WindowsProductPairTaskRecovery) error { return nil },
		},
	}
}

type recoveryFixture struct {
	stateDir, cli, windowless, receipt, taskStore string
	priorCLI, priorWindowless, priorReceipt       []byte
	newCLI, newWindowless                         []byte
	journal                                       windowsProductPairRecoveryJournal
}

func recoveryAppendUTF16Z(dst []byte, value string) []byte {
	for _, r := range value {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(r))
	}
	return binary.LittleEndian.AppendUint16(dst, 0)
}

func recoveryAlign4(dst []byte) []byte {
	for len(dst)%4 != 0 {
		dst = append(dst, 0)
	}
	return dst
}

func recoveryVersionBlock(key string, value []byte, valueWords uint16, blockType uint16, children ...[]byte) []byte {
	block := make([]byte, 6)
	binary.LittleEndian.PutUint16(block[2:4], valueWords)
	binary.LittleEndian.PutUint16(block[4:6], blockType)
	block = recoveryAppendUTF16Z(block, key)
	block = recoveryAlign4(block)
	block = append(block, value...)
	block = recoveryAlign4(block)
	for _, child := range children {
		block = append(block, child...)
		block = recoveryAlign4(block)
	}
	binary.LittleEndian.PutUint16(block[0:2], uint16(len(block)))
	return block
}

func recoveryVersionString(key, value string) []byte {
	raw := recoveryAppendUTF16Z(nil, value)
	return recoveryVersionBlock(key, raw, uint16(len(raw)/2), 1)
}

func recoveryVersionResource(role binaryadmission.WindowsArtifactRole) []byte {
	filename, internalName, roleName := "mcphub.exe", "mcphub-cli", "cli"
	if role == binaryadmission.WindowsArtifactRoleWindowless {
		filename, internalName, roleName = "mcphub-windowless.exe", "mcphub-windowless", "windowless"
	}
	children := [][]byte{
		recoveryVersionString("ProductName", "mcp-local-hub"),
		recoveryVersionString("ProductVersion", "0.4.36"),
		recoveryVersionString("PrivateBuild", "mcphub-build-v1;commit=abcdef1;build_date=2026-09-10T00:00:00Z"),
		recoveryVersionString("SpecialBuild", "mcphub-role-v1;role="+roleName),
		recoveryVersionString("InternalName", internalName),
		recoveryVersionString("OriginalFilename", filename),
	}
	stringTable := recoveryVersionBlock("040904B0", nil, 0, 1, children...)
	stringInfo := recoveryVersionBlock("StringFileInfo", nil, 0, 1, stringTable)
	translation := make([]byte, 4)
	binary.LittleEndian.PutUint16(translation[0:2], 0x0409)
	binary.LittleEndian.PutUint16(translation[2:4], 0x04b0)
	varInfo := recoveryVersionBlock("VarFileInfo", nil, 0, 1, recoveryVersionBlock("Translation", translation, uint16(len(translation)), 0))
	fixed := make([]byte, 52)
	binary.LittleEndian.PutUint32(fixed[0:4], 0xFEEF04BD)
	binary.LittleEndian.PutUint32(fixed[4:8], 0x00010000)
	return recoveryVersionBlock("VS_VERSION_INFO", fixed, uint16(len(fixed)), 0, stringInfo, varInfo)
}

func writeRecoveryAdmittedArtifact(t *testing.T, path string, role binaryadmission.WindowsArtifactRole, tag string) {
	t.Helper()
	subsystem := binaryadmission.WindowsCUISubsystem
	if role == binaryadmission.WindowsArtifactRoleWindowless {
		subsystem = binaryadmission.WindowsGUISubsystem
	}
	versionResource := recoveryVersionResource(role)
	const peOffset, optionalSize, rawOffset, resourceRVA, resourceDataOffset = 0x80, 0xf0, 0x200, 0x1000, 0x80
	data := make([]byte, rawOffset+resourceDataOffset+len(versionResource))
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], peOffset)
	copy(data[peOffset:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[peOffset+4:peOffset+6], 0x8664)
	binary.LittleEndian.PutUint16(data[peOffset+6:peOffset+8], 1)
	binary.LittleEndian.PutUint16(data[peOffset+20:peOffset+22], optionalSize)
	optional := data[peOffset+24 : peOffset+24+optionalSize]
	binary.LittleEndian.PutUint16(optional[0:2], 0x20b)
	binary.LittleEndian.PutUint16(optional[68:70], subsystem)
	binary.LittleEndian.PutUint32(optional[108:112], 16)
	binary.LittleEndian.PutUint32(optional[128:132], resourceRVA)
	binary.LittleEndian.PutUint32(optional[132:136], uint32(resourceDataOffset+len(versionResource)))
	section := data[peOffset+24+optionalSize : peOffset+24+optionalSize+40]
	copy(section[0:8], ".rsrc")
	binary.LittleEndian.PutUint32(section[8:12], uint32(resourceDataOffset+len(versionResource)))
	binary.LittleEndian.PutUint32(section[12:16], resourceRVA)
	binary.LittleEndian.PutUint32(section[16:20], uint32(resourceDataOffset+len(versionResource)))
	binary.LittleEndian.PutUint32(section[20:24], rawOffset)
	resource := data[rawOffset:]
	binary.LittleEndian.PutUint16(resource[14:16], 1)
	binary.LittleEndian.PutUint32(resource[16:20], 16)
	binary.LittleEndian.PutUint32(resource[20:24], 0x80000020)
	binary.LittleEndian.PutUint16(resource[0x20+14:0x20+16], 1)
	binary.LittleEndian.PutUint32(resource[0x30:0x34], 1)
	binary.LittleEndian.PutUint32(resource[0x34:0x38], 0x80000040)
	binary.LittleEndian.PutUint16(resource[0x40+14:0x40+16], 1)
	binary.LittleEndian.PutUint32(resource[0x50:0x54], 0x0409)
	binary.LittleEndian.PutUint32(resource[0x54:0x58], 0x60)
	binary.LittleEndian.PutUint32(resource[0x60:0x64], resourceRVA+resourceDataOffset)
	binary.LittleEndian.PutUint32(resource[0x64:0x68], uint32(len(versionResource)))
	copy(resource[resourceDataOffset:], versionResource)
	data = append(data, tag...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRecoveryAdmittedPair(t *testing.T, dir, tag string) string {
	t.Helper()
	cli := filepath.Join(dir, "mcphub.exe")
	writeRecoveryAdmittedArtifact(t, cli, binaryadmission.WindowsArtifactRoleCLI, tag+"-cli")
	writeRecoveryAdmittedArtifact(t, filepath.Join(dir, "mcphub-windowless.exe"), binaryadmission.WindowsArtifactRoleWindowless, tag+"-windowless")
	return cli
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	installDir := filepath.Join(root, "install")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := recoveryFixture{
		stateDir:   stateDir,
		cli:        filepath.Join(installDir, "mcphub.exe"),
		windowless: filepath.Join(installDir, "mcphub-windowless.exe"),
		receipt:    filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json"),
		taskStore:  filepath.Join(stateDir, "windows-product-pair-task-rollback-fixture"),
		priorCLI:   []byte("prior-cli"), priorWindowless: []byte("prior-windowless"), priorReceipt: []byte("prior-receipt\n"),
		newCLI: []byte("successor-cli"), newWindowless: []byte("successor-windowless"),
	}
	for path, body := range map[string][]byte{f.cli: f.priorCLI, f.windowless: f.priorWindowless, f.receipt: f.priorReceipt} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(f.taskStore, 0o700); err != nil {
		t.Fatal(err)
	}
	f.journal = windowsProductPairRecoveryJournal{
		Schema:        WindowsProductPairRecoverySchemaV1,
		TransactionID: strings.Repeat("a", 32),
		Phase:         WindowsProductPairRecoveryPrepared,
		Mode:          WindowsProductPairModeUpgrade,
		CLI:           windowsProductPairRecoveryArtifact{Target: f.cli, PriorPresent: true, PriorSHA256: recoveryTestSHA(f.priorCLI), NewSHA256: recoveryTestSHA(f.newCLI)},
		Windowless:    windowsProductPairRecoveryArtifact{Target: f.windowless, PriorPresent: true, PriorSHA256: recoveryTestSHA(f.priorWindowless), NewSHA256: recoveryTestSHA(f.newWindowless)},
		Receipt:       windowsProductPairRecoveryReceipt{Path: f.receipt, PriorPresent: true, PriorBytes: append([]byte(nil), f.priorReceipt...)},
		Tasks:         WindowsProductPairTaskRecovery{StoreRoot: f.taskStore},
		Staged: windowsProductPairRecoveryStaged{
			CLIPath: filepath.Join(installDir, "mcphub.exe.stage"), CLISHA256: recoveryTestSHA(f.newCLI),
			WindowlessPath: filepath.Join(installDir, "mcphub-windowless.exe.stage"), WindowlessSHA256: recoveryTestSHA(f.newWindowless),
		},
	}
	return f
}

func recoveryTestSHA(body []byte) string { return fmt.Sprintf("%x", sha256.Sum256(body)) }

func (f recoveryFixture) persistJournal(t *testing.T) {
	t.Helper()
	raw, err := json.MarshalIndent(f.journal, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.WriteStateFileBytesAtomic(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf), append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

func recoveryAsidePath(target string, second int) string {
	return target + fmt.Sprintf(".old-20260908T1735%02dZ", second)
}

func moveRecoveryPriorAside(t *testing.T, target string, second int) string {
	t.Helper()
	aside := recoveryAsidePath(target, second)
	if err := os.Rename(target, aside); err != nil {
		t.Fatal(err)
	}
	return aside
}

func promoteRecoveryFixture(t *testing.T, target string, successor []byte, second int) {
	t.Helper()
	moveRecoveryPriorAside(t, target, second)
	if err := os.WriteFile(target, successor, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f recoveryFixture) request(tasks *recoveryTaskTxnFake, order *[]string, eventErr error) windowsProductPairRecoveryRequest {
	tasks.order = order
	defaults := withWindowsProductPairDefaults(WindowsProductPairTxnDeps{})
	return windowsProductPairRecoveryRequest{
		Context: context.Background(), StateDir: f.stateDir, CLIPath: f.cli, WindowlessPath: f.windowless,
		SettleSuccessor: func(string) error { *order = append(*order, "settle-successor"); return nil },
		ObservePriorReady: func(context.Context, string, string) (bool, error) {
			*order = append(*order, "observe-prior")
			return false, nil
		},
		RestartPrior: func(string) (SupervisorLaunch, error) {
			*order = append(*order, "restart-prior")
			return &fixtureSupervisorLaunch{}, nil
		},
		WaitPriorReady: func(context.Context, string, string) error { *order = append(*order, "wait-prior"); return nil },
		ReconcileCommitted: func(windowsProductPairRecoveryJournal) error {
			*order = append(*order, "reconcile-committed")
			return eventErr
		},
		Deps: windowsProductPairRecoveryDeps{
			ReopenTasks: func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductPairTaskTxn, error) {
				*order = append(*order, "reopen-tasks")
				return tasks, nil
			},
			DiscardTaskStore: func(WindowsProductPairTaskRecovery) error {
				*order = append(*order, "discard-store")
				if err := os.Remove(f.taskStore); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				return nil
			},
			AdmitPair: func(cli, windowless binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error) {
				return binaryadmission.WindowsProductPair{
					CLI:        binaryadmission.WindowsArtifact{Path: cli.Path, Role: binaryadmission.WindowsArtifactRoleCLI, Version: "0.4.36", Commit: "fixture", BuildDate: "2026-09-08T00:00:00Z", SHA256: f.journal.CLI.NewSHA256},
					Windowless: binaryadmission.WindowsArtifact{Path: windowless.Path, Role: binaryadmission.WindowsArtifactRoleWindowless, Version: "0.4.36", Commit: "fixture", BuildDate: "2026-09-08T00:00:00Z", SHA256: f.journal.Windowless.NewSHA256},
				}, nil
			},
			RestorePromotion: func(p WindowsProductPairPromotion) (string, error) {
				if sameWindowsRecoveryPath(p.Target, f.cli) {
					*order = append(*order, "restore-cli")
				} else {
					*order = append(*order, "restore-windowless")
				}
				return defaults.RestorePromotion(p)
			},
			RestoreNoReplace: func(source, target string) error {
				if sameWindowsRecoveryPath(target, f.cli) {
					*order = append(*order, "restore-cli")
				} else {
					*order = append(*order, "restore-windowless")
				}
				return moveWindowsFileNoReplace(source, target)
			},
			WriteReceipt: func(path string, raw []byte) error {
				*order = append(*order, "restore-receipt")
				return api.WriteStateFileBytesAtomic(path, raw)
			},
			RemoveReceipt: func(path string) error {
				*order = append(*order, "restore-receipt")
				return removeRecoveryLeaf(path)
			},
			RemoveExact: func(path string) error {
				if sameWindowsRecoveryPath(path, f.cli) {
					*order = append(*order, "restore-cli")
				} else if sameWindowsRecoveryPath(path, f.windowless) {
					*order = append(*order, "restore-windowless")
				}
				return os.Remove(path)
			},
		},
	}
}

func TestWindowsProductPairRecoveryPriorAbsentCrashMatrix(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, recoveryFixture)
	}{
		{name: "prepared before first promotion"},
		{name: "windowless promoted", mutate: func(t *testing.T, f recoveryFixture) {
			if err := os.WriteFile(f.windowless, f.newWindowless, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "both promoted", mutate: func(t *testing.T, f recoveryFixture) {
			if err := os.WriteFile(f.windowless, f.newWindowless, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.cli, f.newCLI, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			for _, path := range []string{f.cli, f.windowless, f.receipt} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			f.journal.CLI.PriorPresent, f.journal.CLI.PriorSHA256 = false, ""
			f.journal.Windowless.PriorPresent, f.journal.Windowless.PriorSHA256 = false, ""
			f.journal.Receipt.PriorPresent, f.journal.Receipt.PriorBytes = false, nil
			f.persistJournal(t)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			var order []string
			tasks := &recoveryTaskTxnFake{}
			settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
			if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
				t.Fatalf("settled=%+v err=%v order=%v", settled, err, order)
			}
			for _, path := range []string{f.cli, f.windowless, f.receipt} {
				if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("prior-absent path %s exists: %v", path, statErr)
				}
			}
			if tc.name == "both promoted" && !recoveryStepsOrdered(order, []string{"restore-cli", "restore-tasks", "restore-windowless", "restore-receipt"}) {
				t.Fatalf("prior-absent reverse order=%v", order)
			}
		})
	}
}

func TestWindowsProductPairRecoveryIncompleteReceiptRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*testing.T, recoveryFixture)
	}{
		{name: "missing", write: func(t *testing.T, f recoveryFixture) {
			if err := os.Remove(f.receipt); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "old"},
		{name: "partial atomic payload", write: func(t *testing.T, f recoveryFixture) {
			if err := api.WriteStateFileBytesAtomic(f.receipt, []byte(`{"schema":"upgrade-receipt-v2"`)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			promoteRecoveryFixture(t, f.windowless, f.newWindowless, 16)
			promoteRecoveryFixture(t, f.cli, f.newCLI, 17)
			if tc.write != nil {
				tc.write(t, f)
			}
			f.persistJournal(t)
			var order []string
			tasks := &recoveryTaskTxnFake{}
			settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
			if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
				t.Fatalf("settled=%+v err=%v order=%v", settled, err, order)
			}
			if containsRecoveryStep(order, "reconcile-committed") || tasks.restoreCalls != 1 {
				t.Fatalf("incomplete receipt committed: tasks=%+v order=%v", tasks, order)
			}
			if got, readErr := os.ReadFile(f.receipt); readErr != nil || !bytes.Equal(got, f.priorReceipt) {
				t.Fatalf("receipt=%q err=%v", got, readErr)
			}
		})
	}
}

func TestWindowsProductPairRecoveryFreshOwnerRollbackCrashMatrix(t *testing.T) {
	tests := []struct {
		name             string
		phase            WindowsProductPairRecoveryPhase
		priorFleetReaped bool
		mutate           func(*testing.T, recoveryFixture)
	}{
		{name: "after prepared before first rename"},
		{name: "during windowless target to old", mutate: func(t *testing.T, f recoveryFixture) { moveRecoveryPriorAside(t, f.windowless, 1) }},
		{name: "after windowless promotion", mutate: func(t *testing.T, f recoveryFixture) { promoteRecoveryFixture(t, f.windowless, f.newWindowless, 2) }},
		{name: "during task rewrite", mutate: func(t *testing.T, f recoveryFixture) { promoteRecoveryFixture(t, f.windowless, f.newWindowless, 3) }},
		{name: "during CLI target to old", mutate: func(t *testing.T, f recoveryFixture) {
			promoteRecoveryFixture(t, f.windowless, f.newWindowless, 4)
			moveRecoveryPriorAside(t, f.cli, 5)
		}},
		{name: "after both promotions before successor", mutate: func(t *testing.T, f recoveryFixture) {
			promoteRecoveryFixture(t, f.windowless, f.newWindowless, 6)
			promoteRecoveryFixture(t, f.cli, f.newCLI, 7)
		}},
		{name: "after durable successor starting", phase: WindowsProductPairRecoverySuccessorStarting, priorFleetReaped: true, mutate: func(t *testing.T, f recoveryFixture) {
			promoteRecoveryFixture(t, f.windowless, f.newWindowless, 8)
			promoteRecoveryFixture(t, f.cli, f.newCLI, 9)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			if tc.phase != "" {
				f.journal.Phase = tc.phase
			}
			f.journal.PriorFleetReaped = tc.priorFleetReaped
			f.persistJournal(t)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			var order []string
			tasks := &recoveryTaskTxnFake{}
			settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
			if err != nil {
				t.Fatalf("fresh-owner recovery: %v; order=%v", err, order)
			}
			if settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
				t.Fatalf("settled=%+v", settled)
			}
			for path, want := range map[string][]byte{f.cli: f.priorCLI, f.windowless: f.priorWindowless, f.receipt: f.priorReceipt} {
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, want) {
					t.Fatalf("restored %s=%q err=%v want=%q", filepath.Base(path), got, readErr, want)
				}
			}
			if tasks.restoreCalls != 1 || tasks.closeCalls != 1 {
				t.Fatalf("task restore=%d close=%d order=%v", tasks.restoreCalls, tasks.closeCalls, order)
			}
			if _, statErr := os.Stat(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("journal survived settlement: %v", statErr)
			}
			if tc.priorFleetReaped {
				want := []string{"settle-successor", "reopen-tasks", "observe-prior", "restart-prior", "wait-prior", "discard-store"}
				for _, step := range want {
					if !containsRecoveryStep(order, step) {
						t.Fatalf("missing %s in %v", step, order)
					}
				}
			} else if containsRecoveryStep(order, "restart-prior") {
				t.Fatalf("non-upgrade recovery restarted prior: %v", order)
			}
			if tc.name == "after both promotions before successor" || tc.name == "after durable successor starting" {
				wantOrder := []string{"restore-cli", "restore-tasks", "restore-windowless", "restore-receipt"}
				if !recoveryStepsOrdered(order, wantOrder) {
					t.Fatalf("reverse order=%v want subsequence=%v", order, wantOrder)
				}
			}
		})
	}
}

func TestWindowsProductPairRecoveryMatchingReceiptCommitsWithoutRollback(t *testing.T) {
	f := newRecoveryFixture(t)
	promoteRecoveryFixture(t, f.windowless, f.newWindowless, 10)
	promoteRecoveryFixture(t, f.cli, f.newCLI, 11)
	pair, err := f.request(&recoveryTaskTxnFake{}, new([]string), nil).Deps.AdmitPair(binaryadmission.WindowsArtifact{Path: f.cli}, binaryadmission.WindowsArtifact{Path: f.windowless})
	if err != nil {
		t.Fatal(err)
	}
	receipt := UpgradeReceiptV2{Schema: UpgradeReceiptSchemaV2, Mode: WindowsProductPairModeUpgrade, Admission: UpgradeAdmissionLocalProduct, Version: pair.CLI.Version, Commit: pair.CLI.Commit, BuildDate: pair.CLI.BuildDate, Artifacts: pair, InstalledAt: "2026-09-08T00:00:00Z"}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.WriteStateFileBytesAtomic(f.receipt, append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	f.persistJournal(t)
	var order []string
	tasks := &recoveryTaskTxnFake{}
	settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
	if err != nil {
		t.Fatalf("commit recovery: %v order=%v", err, order)
	}
	if settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit {
		t.Fatalf("settled=%+v", settled)
	}
	if tasks.restoreCalls != 0 || tasks.verifyCalls != 1 || containsRecoveryStep(order, "restart-prior") || !containsRecoveryStep(order, "reconcile-committed") {
		t.Fatalf("commit polarity violated: tasks=%+v order=%v", tasks, order)
	}
	if got, _ := os.ReadFile(f.cli); !bytes.Equal(got, f.newCLI) {
		t.Fatalf("committed CLI rolled back: %q", got)
	}
}

func TestWindowsProductPairRecoveryCommittedEventFailureNeverRollsBack(t *testing.T) {
	f := newRecoveryFixture(t)
	promoteRecoveryFixture(t, f.windowless, f.newWindowless, 12)
	promoteRecoveryFixture(t, f.cli, f.newCLI, 13)
	pair, _ := f.request(&recoveryTaskTxnFake{}, new([]string), nil).Deps.AdmitPair(binaryadmission.WindowsArtifact{Path: f.cli}, binaryadmission.WindowsArtifact{Path: f.windowless})
	receipt := UpgradeReceiptV2{Schema: UpgradeReceiptSchemaV2, Mode: WindowsProductPairModeUpgrade, Admission: UpgradeAdmissionLocalProduct, Version: pair.CLI.Version, Commit: pair.CLI.Commit, BuildDate: pair.CLI.BuildDate, Artifacts: pair, InstalledAt: "2026-09-08T00:00:00Z"}
	raw, _ := json.MarshalIndent(receipt, "", "  ")
	if err := api.WriteStateFileBytesAtomic(f.receipt, append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	f.persistJournal(t)
	var order []string
	tasks := &recoveryTaskTxnFake{}
	eventErr := fmt.Errorf("%w: fixture", ErrWindowsProductPairCommittedObservabilityFailed)
	settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, eventErr))
	if !errors.Is(err, ErrWindowsProductPairCommittedObservabilityFailed) || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if tasks.restoreCalls != 0 || containsRecoveryStep(order, "restart-prior") {
		t.Fatalf("event failure rolled back: tasks=%+v order=%v", tasks, order)
	}
	if _, statErr := os.Stat(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("settled journal survived event failure: %v", statErr)
	}
}

func TestWindowsProductPairRecoveryRefusesAmbiguousPostJournalAside(t *testing.T) {
	f := newRecoveryFixture(t)
	baseline := recoveryAsidePath(f.cli, 14)
	if err := os.WriteFile(baseline, f.priorCLI, 0o600); err != nil {
		t.Fatal(err)
	}
	f.journal.CLI.BaselineAsideBasenames = []string{filepath.Base(baseline)}
	f.persistJournal(t)
	extra := recoveryAsidePath(f.cli, 15)
	if err := os.WriteFile(extra, f.priorCLI, 0o600); err != nil {
		t.Fatal(err)
	}
	var order []string
	tasks := &recoveryTaskTxnFake{}
	settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
	if settled != nil || !errors.Is(err, ErrWindowsProductPairRecoveryIdentity) {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if tasks.restoreCalls != 0 || len(order) != 0 {
		t.Fatalf("identity refusal mutated state: tasks=%+v order=%v", tasks, order)
	}
	if got, _ := os.ReadFile(f.cli); !bytes.Equal(got, f.priorCLI) {
		t.Fatalf("canonical CLI changed: %q", got)
	}
}

func TestWindowsProductPairRecoverySettledCleanupRetriesWithoutMutation(t *testing.T) {
	f := newRecoveryFixture(t)
	f.journal.Phase = WindowsProductPairRecoverySettledRollback
	f.persistJournal(t)
	var order []string
	removeCalls := 0
	req := f.request(&recoveryTaskTxnFake{}, &order, nil)
	req.Deps.RemoveJournal = func(path string) error {
		removeCalls++
		if removeCalls == 1 {
			return errors.New("injected remove failure")
		}
		return removeRecoveryLeaf(path)
	}
	req.Deps.ReopenTasks = func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductPairTaskTxn, error) {
		return nil, errors.New("settled cleanup must not reopen tasks")
	}
	if _, err := reconcileWindowsProductPairRecovery(req); !errors.Is(err, ErrWindowsProductPairRecoveryCleanup) {
		t.Fatalf("first cleanup err=%v", err)
	}
	settled, err := reconcileWindowsProductPairRecovery(req)
	if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
		t.Fatalf("retry settled=%+v err=%v", settled, err)
	}
	if containsRecoveryStep(order, "reopen-tasks") || containsRecoveryStep(order, "restart-prior") {
		t.Fatalf("cleanup replayed mutation: %v", order)
	}
}

func TestWindowsProductPairRecoveryWiringReopensExistingSchedulerStore(t *testing.T) {
	withNoopSchedulerEnv(t)
	f := newRecoveryFixture(t)
	if err := os.Remove(f.taskStore); err != nil {
		t.Fatal(err)
	}
	store, err := newWindowsProductPairTaskStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	taskTxn, err := scheduler.BeginOwnedEntrypointTaskTxnWithStore(
		context.Background(), filepath.Join(f.stateDir, "windows-product-pair-entrypoint.lock"),
		f.cli, f.windowless, store,
	)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	owner := &windowsProductPairTaskOwner{txn: taskTxn, store: store}
	if err := owner.InventoryExport(); err != nil {
		t.Fatal(err)
	}
	recovery, err := owner.RecoveryDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	f.journal.Tasks = recovery
	f.persistJournal(t)
	settled, err := reconcileWiredWindowsProductPairRecovery(windowsProductPairRecoveryRequest{
		Context: context.Background(), StateDir: f.stateDir, CLIPath: f.cli, WindowlessPath: f.windowless,
		ReconcileCommitted: func(windowsProductPairRecoveryJournal) error { return nil },
	})
	if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if _, statErr := os.Stat(recovery.StoreRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("retained store survived recovery: %v", statErr)
	}
}

func TestWindowsProductPairSetupAndCanonicalizeFenceBeforeRecoveryOrMutation(t *testing.T) {
	stateDir := withTempStateDir(t)
	holder, err := api.AcquireUpgradeFence(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })
	originalFactory := newWindowsProductTxnFn
	t.Cleanup(func() { newWindowsProductTxnFn = originalFactory })
	called := false
	newWindowsProductTxnFn = func(windowsProductTxnRequest) (*WindowsProductTxn, error) {
		called = true
		return nil, errors.New("unreachable")
	}
	for _, run := range []struct {
		name string
		fn   func() error
	}{
		{name: "setup", fn: func() error {
			return bootstrapProductToTarget(new(bytes.Buffer), filepath.Join(t.TempDir(), "source.exe"), filepath.Join(t.TempDir(), "mcphub.exe"))
		}},
		{name: "canonicalize", fn: func() error {
			return canonicalizeProductToTarget(new(bytes.Buffer), filepath.Join(t.TempDir(), "source.exe"), filepath.Join(t.TempDir(), "mcphub.exe"))
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			if err := run.fn(); err == nil || !strings.Contains(err.Error(), "another product-pair transaction is active") {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if called {
		t.Fatal("pair transaction factory ran behind held fence")
	}
}

const windowsProductPairCrashChildEnv = "MCPHUB_WINDOWS_PAIR_CRASH_CHILD"

type windowsProductPairCrashChildTasks struct {
	storeRoot string
	crashAt   string
}

func (*windowsProductPairCrashChildTasks) InventoryExport() error { return nil }
func (t *windowsProductPairCrashChildTasks) RecoveryDescriptor() (WindowsProductPairTaskRecovery, error) {
	return WindowsProductPairTaskRecovery{StoreRoot: t.storeRoot}, nil
}
func (t *windowsProductPairCrashChildTasks) RewriteCommand() error {
	if t.crashAt == "during-tasks-rewrite" {
		os.Exit(86)
	}
	return nil
}
func (*windowsProductPairCrashChildTasks) VerifyRuntime() error { return nil }
func (*windowsProductPairCrashChildTasks) RestoreImport() error { return nil }
func (t *windowsProductPairCrashChildTasks) DiscardRecoveryStore() error {
	if err := os.Remove(t.storeRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (*windowsProductPairCrashChildTasks) Close() error { return nil }

func containsRecoveryStep(order []string, want string) bool {
	for _, step := range order {
		if step == want {
			return true
		}
	}
	return false
}

func recoveryStepsOrdered(order, want []string) bool {
	next := 0
	for _, step := range order {
		if next < len(want) && step == want[next] {
			next++
		}
	}
	return next == len(want)
}
