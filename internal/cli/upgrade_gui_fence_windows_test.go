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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

type upgradeGUIOutputFailure struct {
	err   error
	short bool
}

type upgradeGUICommandWriter func([]byte) (int, error)

func (w upgradeGUICommandWriter) Write(p []byte) (int, error) { return w(p) }

type upgradeGUICommandC3Fixture struct {
	stateDir, candidate, target string
	order                       *[]string
	tasks                       *productPairTaskFake
	txn                         *WindowsProductTxn
	fault                       error
	factoryCalls                int
	priorBytes                  []byte
}

func writeUpgradeGUIAdmittedProduct(t *testing.T, path, tag string) {
	t.Helper()
	children := [][]byte{
		recoveryVersionString("ProductName", "mcp-local-hub"),
		recoveryVersionString("ProductVersion", "0.4.38"),
		recoveryVersionString("PrivateBuild", "mcphub-build-v1;commit=abcdef1;build_date=2026-09-28T00:00:00Z"),
		recoveryVersionString("SpecialBuild", "mcphub-role-v1;role=product"),
		recoveryVersionString("InternalName", "mcphub"),
		recoveryVersionString("OriginalFilename", "mcphub.exe"),
	}
	stringInfo := recoveryVersionBlock("StringFileInfo", nil, 0, 1, recoveryVersionBlock("040904B0", nil, 0, 1, children...))
	translation := []byte{0x09, 0x04, 0xb0, 0x04}
	varInfo := recoveryVersionBlock("VarFileInfo", nil, 0, 1, recoveryVersionBlock("Translation", translation, uint16(len(translation)), 0))
	fixed := make([]byte, 52)
	binary.LittleEndian.PutUint32(fixed[0:4], 0xFEEF04BD)
	binary.LittleEndian.PutUint32(fixed[4:8], 0x00010000)
	versionResource := recoveryVersionBlock("VS_VERSION_INFO", fixed, uint16(len(fixed)), 0, stringInfo, varInfo)
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
	binary.LittleEndian.PutUint16(optional[68:70], binaryadmission.WindowsGUISubsystem)
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
	if err := os.WriteFile(path, append(data, tag...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newUpgradeGUICommandC3Fixture(t *testing.T) *upgradeGUICommandC3Fixture {
	t.Helper()
	withNoopSchedulerEnv(t)
	f := &upgradeGUICommandC3Fixture{stateDir: withTempStateDir(t), candidate: filepath.Join(t.TempDir(), "mcphub.exe"), order: new([]string)}
	writeUpgradeGUIAdmittedProduct(t, f.candidate, "C3-CANDIDATE")
	f.target = filepath.Join(t.TempDir(), "mcphub.exe")
	writeUpgradeGUIAdmittedProduct(t, f.target, "C3-PRIOR")
	t.Logf("C3 test-owned GUI PE candidate=%s target=%s", f.candidate, f.target)
	var readErr error
	f.priorBytes, readErr = os.ReadFile(f.target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	writeStateSidecarBytes(t, filepath.Join(f.stateDir, "supervisor-intent.json"), []byte(`{"version":1,"updated_at":"2026-09-28T00:00:00Z","daemons":[],"strict_mode":false}`))
	priorFactory := newWindowsProductTxnFn
	t.Cleanup(func() { newWindowsProductTxnFn = priorFactory })
	newWindowsProductTxnFn = func(req windowsProductTxnRequest) (*WindowsProductTxn, error) {
		f.factoryCalls++
		if req.StateDir != f.stateDir || req.ProductPath != f.target || req.Mode != WindowsProductPairModeUpgrade {
			t.Fatalf("unexpected Product request: %+v", req)
		}
		f.tasks = &productPairTaskFake{order: f.order}
		deps := productTxnTestDeps(f.order)
		if f.fault != nil {
			deps.Fault = func(stage WindowsProductStage) error {
				if stage == WindowsProductStageProductPromoted {
					return f.fault
				}
				return nil
			}
		}
		f.txn = &WindowsProductTxn{Opts: WindowsProductTxnOpts{StateDir: req.StateDir, ProductPath: req.ProductPath, LegacyWindowlessPath: req.LegacyWindowlessPath, StagedProduct: req.StagedProduct, Mode: req.Mode, Tasks: f.tasks, Deps: deps}}
		return f.txn, nil
	}
	return f
}

func TestUpgradeDefaultCompletionOutputCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		writeErr error
		short    bool
	}{
		{name: "short write", short: true},
		{name: "writer error", writeErr: errors.New("stdout unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpgradeGUICommandC3Fixture(t)
			cmd := newUpgradeCmdReal()
			cmd.SetErr(io.Discard)
			var attempted string
			cmd.SetOut(upgradeGUICommandWriter(func(p []byte) (int, error) {
				attempted = string(p)
				if tc.short {
					return len(p) - 1, nil
				}
				return 0, tc.writeErr
			}))
			err := runV5UpgradeWindowsWithPaths(cmd, f.candidate, f.target)
			if err != nil {
				t.Fatalf("default settled Product result changed on stdout failure: %v", err)
			}
			if attempted != "v0.5 upgrade complete.\n" {
				t.Fatalf("default completion attempt=%q", attempted)
			}
			if f.factoryCalls != 1 || f.txn == nil || f.tasks == nil || !f.tasks.closed {
				t.Fatalf("Product did not settle once: calls=%d txn=%v tasks=%v", f.factoryCalls, f.txn, f.tasks)
			}
			if _, err := os.Stat(filepath.Join(f.stateDir, UpgradeReceiptSchemaV3+".json")); err != nil {
				t.Fatalf("committed receipt unavailable: %v", err)
			}
		})
	}
}

type upgradeGUICommandLease struct {
	stateDir string
	released int
	order    *[]string
}

func (l *upgradeGUICommandLease) Release() { _ = l.ReleaseErr() }
func (l *upgradeGUICommandLease) ReleaseErr() error {
	lease, acquired, err := api.TryAcquireUpgradeFence(context.Background(), l.stateDir)
	if acquired {
		_ = lease.Release()
		return errors.New("upgrade fence released before GUI reservation")
	}
	if err != nil {
		return err
	}
	l.released++
	*l.order = append(*l.order, "gui-release")
	return nil
}

func upgradeGUICommandC3Deps(t *testing.T, f *upgradeGUICommandC3Fixture, cmd *cobra.Command, order *[]string) (upgradeGUIFencedDeps, *upgradeGUICommandLease, *int, *int) {
	t.Helper()
	deps := upgradeGUIFencedDepsForCommand(cmd)
	classifications, exits := new(int), new(int)
	lease := &upgradeGUICommandLease{stateDir: f.stateDir, order: order}
	checkFence := func() {
		t.Helper()
		probe, acquired, err := api.TryAcquireUpgradeFence(context.Background(), f.stateDir)
		if acquired {
			_ = probe.Release()
			t.Fatal("upgrade fence not held")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	deps.Classify = func(stateDir, candidate, target string) (upgradeGUIProductOutcome, error) {
		if stateDir != f.stateDir || candidate != f.candidate || target != f.target {
			return "", errors.New("receipt classification arguments changed")
		}
		checkFence()
		*classifications++
		if _, err := os.Stat(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json")); !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("test-owned receipt changed: %v", err)
		}
		return upgradeGUINewTransactionRequired, nil
	}
	deps.ExitAndReserve = func(_ context.Context, stateDir, target string) (gui.OwnedSingleInstanceLease, error) {
		if stateDir != f.stateDir || target != f.target {
			return nil, errors.New("GUI reservation arguments changed")
		}
		checkFence()
		*exits++
		return lease, nil
	}
	return deps, lease, classifications, exits
}

func TestUpgradeGUIOutputSettlement(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		preparedFailure, rollback, finalShort bool
		finalErr                              error
	}{
		{name: "prepared output after commit", preparedFailure: true},
		{name: "prepared output and Product rollback", preparedFailure: true, rollback: true},
		{name: "short final output after commit", finalShort: true},
		{name: "failed final output after commit", finalErr: errors.New("stdout unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpgradeGUICommandC3Fixture(t)
			if tc.rollback {
				f.fault = errors.New("synthetic Product promotion failure")
			}
			cmd := newUpgradeCmdReal()
			if err := cmd.Flags().Set("terminate-gui-for-upgrade", "true"); err != nil {
				t.Fatal(err)
			}
			var order []string
			preparedObserved := false
			cmd.SetErr(upgradeGUICommandWriter(func(p []byte) (int, error) {
				if strings.HasPrefix(string(p), "UPGRADE_GUI_PREPARED:") {
					raw, err := os.ReadFile(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3))
					if err != nil {
						return 0, fmt.Errorf("prepared journal absent: %w", err)
					}
					journal, err := decodeWindowsProductRecoveryJournalV3(raw)
					if err != nil || journal.Phase != WindowsProductRecoveryPrepared {
						return 0, fmt.Errorf("prepared journal unreadable: %v", err)
					}
					preparedObserved = true
					if tc.preparedFailure {
						return 0, errors.New("prepared stderr unavailable")
					}
				}
				return len(p), nil
			}))
			deps, lease, classifications, exits := upgradeGUICommandC3Deps(t, f, cmd, &order)
			var completionAttempt string
			cmd.SetOut(upgradeGUICommandWriter(func(p []byte) (int, error) {
				completionAttempt = string(p)
				order = append(order, "completion-write")
				if lease.released != 1 {
					return 0, errors.New("GUI lease still held during completion")
				}
				probe, acquired, err := api.TryAcquireUpgradeFence(context.Background(), f.stateDir)
				if err != nil || !acquired {
					return 0, fmt.Errorf("fence still held during completion: %v", err)
				}
				if err := probe.Release(); err != nil {
					return 0, err
				}
				if tc.finalShort {
					return len(p) - 1, nil
				}
				if tc.finalErr != nil {
					return 0, tc.finalErr
				}
				return len(p), nil
			}))
			err := runV5UpgradeWindowsWithPathsAndDeps(cmd, f.candidate, f.target, deps)
			if !preparedObserved || *classifications != 2 || *exits != 1 || lease.released != 1 || f.factoryCalls != 1 {
				t.Fatalf("path observations: prepared=%v classify=%d exit=%d release=%d Product=%d err=%v", preparedObserved, *classifications, *exits, lease.released, f.factoryCalls, err)
			}
			if tc.preparedFailure {
				if err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_STAGE_OUTPUT_UNCONFIRMED") || !strings.Contains(err.Error(), "prepared stderr unavailable") {
					t.Fatalf("stage result=%v", err)
				}
				if completionAttempt != "" {
					t.Fatalf("generic completion after stage failure: %q", completionAttempt)
				}
			} else if err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_COMPLETION_OUTPUT_UNCONFIRMED") || completionAttempt != "UPGRADE_GUI_TXN_SETTLED: requested Product transaction complete\n" {
				t.Fatalf("completion result=(%v,%q)", err, completionAttempt)
			}
			if tc.rollback {
				if !errors.Is(err, f.fault) || !f.tasks.restored {
					t.Fatalf("Product rollback cause/restore lost: err=%v tasks=%+v", err, f.tasks)
				}
				if _, statErr := os.Stat(filepath.Join(f.stateDir, UpgradeReceiptSchemaV3+".json")); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("rollback left committed receipt: %v", statErr)
				}
				priorNow, readErr := os.ReadFile(f.target)
				if readErr != nil || !bytes.Equal(priorNow, f.priorBytes) {
					t.Fatalf("rollback did not restore prior Product: %v", readErr)
				}
			} else {
				raw, readErr := os.ReadFile(filepath.Join(f.stateDir, UpgradeReceiptSchemaV3+".json"))
				var receipt UpgradeReceiptV3
				if readErr != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Mode != WindowsProductPairModeUpgrade {
					t.Fatalf("committed Product receipt unreadable: %v", readErr)
				}
				candidateSHA, hashErr := hashRecoveryFile(f.candidate)
				if hashErr != nil || receipt.Artifact.SHA256 != candidateSHA {
					t.Fatalf("receipt candidate SHA mismatch: %v", hashErr)
				}
				if f.tasks.restored {
					t.Fatal("post-commit reporting failure rolled Product back")
				}
			}
			if _, statErr := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("settled journal remains: %v", statErr)
			}
			if !tc.preparedFailure && (len(order) != 2 || order[0] != "gui-release" || order[1] != "completion-write") {
				t.Fatalf("release/output order=%v", order)
			}
			probe, acquired, probeErr := api.TryAcquireUpgradeFence(context.Background(), f.stateDir)
			if probeErr != nil || !acquired {
				t.Fatalf("fence remained held after return: %v", probeErr)
			}
			if releaseErr := probe.Release(); releaseErr != nil {
				t.Fatal(releaseErr)
			}
		})
	}
}

func (w upgradeGUIOutputFailure) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, w.err
}

func TestUpgradeGUIOutputFailureAfterProductSettlement(t *testing.T) {
	stageWriteErr := errors.New("prepared output unavailable")
	var recorded error
	prepared := upgradeGUIPreparedStageReporter(upgradeGUIOutputFailure{err: stageWriteErr}, &recorded)
	if err := prepared(); err != nil {
		t.Fatalf("prepared reporting aborts Product: %v", err)
	}
	if !errors.Is(recorded, stageWriteErr) || !strings.Contains(recorded.Error(), "UPGRADE_GUI_STAGE_OUTPUT_UNCONFIRMED") {
		t.Fatalf("prepared output error was not retained: %v", recorded)
	}
	if err := writeUpgradeGUICompletion(upgradeGUIOutputFailure{short: true}, "UPGRADE_GUI_TXN_SETTLED: requested Product transaction complete"); err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_COMPLETION_OUTPUT_UNCONFIRMED") {
		t.Fatalf("short final completion write was accepted: %v", err)
	}
	if err := writeUpgradeGUICompletion(io.Discard, "UPGRADE_GUI_TXN_SETTLED: requested Product transaction complete"); err != nil {
		t.Fatal(err)
	}
}

type upgradeGUITestLease struct {
	released   int
	releaseErr error
	onRelease  func()
}

func (l *upgradeGUITestLease) Release() { l.released++ }
func (l *upgradeGUITestLease) ReleaseErr() error {
	l.released++
	if l.onRelease != nil {
		l.onRelease()
	}
	return l.releaseErr
}

type upgradeGUITestHeldGeneration struct {
	pid          int
	role         string
	order        *[]string
	terminateErr error
}

func (h *upgradeGUITestHeldGeneration) PID() int                                      { return h.pid }
func (h *upgradeGUITestHeldGeneration) VerifyIdentity(process.PIDIdentityProof) error { return nil }
func (h *upgradeGUITestHeldGeneration) Terminate() (bool, error) {
	*h.order = append(*h.order, "terminate-"+h.role)
	return h.terminateErr == nil, h.terminateErr
}
func (h *upgradeGUITestHeldGeneration) Close() error {
	*h.order = append(*h.order, "close-"+h.role)
	return nil
}

type upgradeGUITestHeldJob struct{ order *[]string }

func (h *upgradeGUITestHeldJob) VerifyIdentity(process.PIDIdentityProof) error { return nil }
func (h *upgradeGUITestHeldJob) InAnyJob() (bool, error)                       { return false, nil }
func (h *upgradeGUITestHeldJob) Close() error {
	*h.order = append(*h.order, "close-supervisor")
	return nil
}

type upgradeGUITestIdentityReader struct{ identity process.PIDHandleIdentity }

func (h upgradeGUITestIdentityReader) Identity() (process.PIDHandleIdentity, error) {
	return h.identity, nil
}

func TestUpgradeGUIHeldIdentityRejectsSubSecondSwap(t *testing.T) {
	started := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	target := `C:\product\mcphub.exe`
	base := process.PIDHandleIdentity{PID: 101, ExecutablePath: target, StartedAt: started}
	for _, tc := range []struct {
		name          string
		held, current process.PIDHandleIdentity
		wantErr       bool
	}{
		{name: "stable exact generation", held: base, current: base},
		{name: "nomination changed before hold", held: process.PIDHandleIdentity{PID: 101, ExecutablePath: target, StartedAt: started.Add(500 * time.Millisecond)}, current: base, wantErr: true},
		{name: "PID changed while metadata read", held: base, current: process.PIDHandleIdentity{PID: 101, ExecutablePath: target, StartedAt: started.Add(500 * time.Millisecond)}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readCurrent := 0
			_, err := verifyUpgradeGUIHeldIdentity(upgradeGUITestIdentityReader{tc.held}, 101, target, started, func(int) (process.PIDHandleIdentity, error) { readCurrent++; return tc.current, nil })
			if (err != nil) != tc.wantErr {
				t.Fatalf("exact held check=%v, want error=%v", err, tc.wantErr)
			}
			if !tc.wantErr && readCurrent != 1 {
				t.Fatalf("current generation checks=%d", readCurrent)
			}
		})
	}
}

type upgradeGUIAuthorityHold struct {
	identity           process.PIDHandleIdentity
	closed, terminated int
}

func (h *upgradeGUIAuthorityHold) PID() int { return h.identity.PID }
func (h *upgradeGUIAuthorityHold) Identity() (process.PIDHandleIdentity, error) {
	return h.identity, nil
}
func (h *upgradeGUIAuthorityHold) VerifyIdentity(process.PIDIdentityProof) error { return nil }
func (h *upgradeGUIAuthorityHold) Terminate() (bool, error)                      { h.terminated++; return true, nil }
func (h *upgradeGUIAuthorityHold) Close() error                                  { h.closed++; return nil }

type upgradeGUIAuthorityJobHold struct {
	identity        process.PIDHandleIdentity
	closed, queries int
	member          bool
	queryErr        error
}

func (h *upgradeGUIAuthorityJobHold) Identity() (process.PIDHandleIdentity, error) {
	return h.identity, nil
}
func (h *upgradeGUIAuthorityJobHold) VerifyIdentity(proof process.PIDIdentityProof) error {
	if proof.PID != h.identity.PID || proof.StartedAt != h.identity.StartedAt.Format(time.RFC3339Nano) {
		return process.ErrProcessIdentityMismatch
	}
	return nil
}
func (h *upgradeGUIAuthorityJobHold) InAnyJob() (bool, error) {
	h.queries++
	return h.member, h.queryErr
}
func (h *upgradeGUIAuthorityJobHold) Close() error { h.closed++; return nil }

func TestUpgradeGUIHeldGenerationAuthority(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		mutate                     func(*upgradeGUIObservationInputs, *upgradeGUIAuthorityHold, *upgradeGUIAuthorityHold, *upgradeGUIAuthorityJobHold, string, time.Time)
		wantGUI, wantTray, wantJob int
		wantID                     string
		pair                       bool
	}{
		{name: "stable v2 owner and pre-exit recheck", wantGUI: 1, wantTray: 1, wantJob: 1},
		{name: "held younger GUI-child Job pair", pair: true, wantGUI: 1, wantTray: 1, wantJob: 1},
		{name: "missing v2 owner record", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, target string, _ time.Time) {
			in.OwnerPath = func() (string, error) { return filepath.Join(filepath.Dir(target), "absent-owner"), nil }
		}, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "legacy owner record", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, target string, _ time.Time) {
			in.OwnerPath = func() (string, error) {
				path := filepath.Join(filepath.Dir(target), "legacy-owner")
				return path, os.WriteFile(path, []byte("101 8721"), 0o600)
			}
		}, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "missing GUI hold result", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			in.HoldTermination = func(int) (process.HeldPIDGeneration, error) { return nil, nil }
		}, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "GUI nomination swapped 500ms before hold", mutate: func(_ *upgradeGUIObservationInputs, guiHold, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			guiHold.identity.StartedAt = guiHold.identity.StartedAt.Add(500 * time.Millisecond)
		}, wantGUI: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "tray nomination swapped 500ms before hold", mutate: func(_ *upgradeGUIObservationInputs, _, trayHold *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			trayHold.identity.StartedAt = trayHold.identity.StartedAt.Add(500 * time.Millisecond)
		}, wantGUI: 1, wantTray: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "current GUI generation drifts 500ms during metadata", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			old := in.CurrentIdentity
			calls := 0
			in.CurrentIdentity = func(pid int) (process.PIDHandleIdentity, error) {
				id, err := old(pid)
				if pid == 101 {
					calls++
					if calls == 2 {
						id.StartedAt = id.StartedAt.Add(500 * time.Millisecond)
					}
				}
				return id, err
			}
		}, wantGUI: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "GUI role contradicts held metadata", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, target string, _ time.Time) {
			old := in.CommandIdentity
			in.CommandIdentity = func(ctx context.Context, pid int) (process.ProcessIdentity, error) {
				if pid == 101 {
					return process.ProcessIdentity{PID: 101, ExecutablePath: target, CommandLine: target + " daemon", CreationDateUnix: time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC).Unix()}, nil
				}
				return old(ctx, pid)
			}
		}, wantGUI: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "GUI SID contradicts held metadata", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			in.SIDMatches = func(pid int) (bool, error) { return pid != 101, nil }
		}, wantGUI: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "cancelled metadata after GUI hold", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			in.CommandIdentity = func(context.Context, int) (process.ProcessIdentity, error) {
				return process.ProcessIdentity{}, context.Canceled
			}
		}, wantGUI: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "tray parent contradicts held metadata", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			in.ParentPID = func(int) (int, error) { return 999, nil }
		}, wantGUI: 1, wantTray: 1, wantJob: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "supervisor sidecar wrong generation", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, supervisorStart time.Time) {
			old := in.SupervisorOwner
			in.SupervisorOwner = func(path string) (api.SupervisorLockOwner, error) {
				owner, err := old(path)
				owner.StartedAt = supervisorStart.Add(500 * time.Millisecond).Format(time.RFC3339Nano)
				return owner, err
			}
		}, wantGUI: 1, wantTray: 1, wantJob: 1, wantID: "UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN"},
		{name: "missing supervisor query hold result", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			in.HoldJob = func(int) (process.HeldPIDJobObservation, error) { return nil, nil }
		}, wantGUI: 1, wantTray: 1, wantID: "UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN"},
		{name: "supervisor Job owned", mutate: func(_ *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, job *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			job.member = true
		}, wantGUI: 1, wantTray: 1, wantJob: 1, wantID: "UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN"},
		{name: "supervisor Job unknown", mutate: func(_ *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, job *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			job.queryErr = os.ErrPermission
		}, wantGUI: 1, wantTray: 1, wantJob: 1, wantID: "UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN"},
		{name: "failure after GUI hold", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			in.ListProcesses = func([]string) ([]api.ProcessInfo, error) { return nil, os.ErrPermission }
		}, wantGUI: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{name: "failure after tray hold", mutate: func(in *upgradeGUIObservationInputs, _, _ *upgradeGUIAuthorityHold, _ *upgradeGUIAuthorityJobHold, _ string, _ time.Time) {
			in.ParentPID = func(int) (int, error) { return 0, os.ErrPermission }
		}, wantGUI: 1, wantTray: 1, wantID: "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := withTempStateDir(t)
			target := filepath.Join(t.TempDir(), "mcphub.exe")
			ownerPath := filepath.Join(stateDir, "test-gui-pidport")
			t.Logf("A_TEST_STATE_DIR=%s OWNER_RECORD=%s TARGET=%s", stateDir, ownerPath, target)
			guiStart := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
			trayStart := guiStart.Add(time.Second)
			supervisorStart := guiStart.Add(-time.Hour)
			if tc.pair {
				supervisorStart = guiStart.Add(2 * time.Second)
			}
			record := gui.GUIOwnerRecord{Version: 2, State: "active", PID: 101, StartTime: guiStart, Port: 8721, Generation: "test-owned-v2"}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			writeStateSidecarBytes(t, ownerPath, raw)
			guiHold := &upgradeGUIAuthorityHold{identity: process.PIDHandleIdentity{PID: 101, ExecutablePath: target, StartedAt: guiStart}}
			trayHold := &upgradeGUIAuthorityHold{identity: process.PIDHandleIdentity{PID: 102, ExecutablePath: target, StartedAt: trayStart}}
			jobHold := &upgradeGUIAuthorityJobHold{identity: process.PIDHandleIdentity{PID: 100, ExecutablePath: target, StartedAt: supervisorStart}}
			base := map[int]process.PIDHandleIdentity{101: guiHold.identity, 102: trayHold.identity, 100: jobHold.identity}
			inputs := upgradeGUIObservationInputs{
				OwnerPath:        func() (string, error) { return ownerPath, nil },
				VerifyListener:   func(context.Context, string, int, int) error { return nil },
				ProbeLeaseUnheld: func(string) (bool, error) { return false, nil },
				FindGUIs:         func(string) ([]api.ProcessInfo, error) { return []api.ProcessInfo{{PID: 101}}, nil },
				ListProcesses: func([]string) ([]api.ProcessInfo, error) {
					return []api.ProcessInfo{{PID: 102, Cmdline: target + " tray"}}, nil
				},
				HoldTermination: func(pid int) (process.HeldPIDGeneration, error) {
					if pid == 101 {
						return guiHold, nil
					}
					if pid == 102 {
						return trayHold, nil
					}
					return nil, os.ErrNotExist
				},
				HoldJob: func(pid int) (process.HeldPIDJobObservation, error) {
					if pid != 100 {
						return nil, os.ErrNotExist
					}
					return jobHold, nil
				},
				CurrentIdentity: func(pid int) (process.PIDHandleIdentity, error) {
					id, ok := base[pid]
					if !ok {
						return id, os.ErrNotExist
					}
					return id, nil
				},
				CommandIdentity: func(_ context.Context, pid int) (process.ProcessIdentity, error) {
					id, ok := base[pid]
					if !ok {
						return process.ProcessIdentity{}, os.ErrNotExist
					}
					role := map[int]string{100: "supervise", 101: "gui", 102: "tray"}[pid]
					return process.ProcessIdentity{PID: pid, ExecutablePath: target, CommandLine: target + " " + role, CreationDateUnix: id.StartedAt.Unix()}, nil
				},
				SIDMatches: func(int) (bool, error) { return true, nil },
				ParentPID:  func(int) (int, error) { return 101, nil },
				SupervisorOwner: func(string) (api.SupervisorLockOwner, error) {
					return api.SupervisorLockOwner{PID: 100, StartedAt: supervisorStart.Format(time.RFC3339Nano)}, nil
				},
				TaskActions: func(context.Context, string) ([]upgradeGUITaskAction, error) {
					return []upgradeGUITaskAction{{Name: "\\mcphub-test", Command: target, XMLSHA256: [32]byte{1}}}, nil
				},
				PortOwners: func(context.Context, string, string, int) ([32]byte, []windowsProductIntentPortV1, []upgradeGUIPortOwner, error) {
					return [32]byte{1}, []windowsProductIntentPortV1{{TaskName: "\\mcphub-test", Port: 9137}}, []upgradeGUIPortOwner{{TaskName: "\\mcphub-test", Port: 9137, PID: 104, ParentPID: 100, StartedAt: supervisorStart}}, nil
				},
			}
			if tc.mutate != nil {
				tc.mutate(&inputs, guiHold, trayHold, jobHold, target, supervisorStart)
			}
			if tc.pair {
				jobHold.member = true
			}
			if tc.wantID == "" {
				var held *upgradeGUIHeldTopology
				var err error
				if tc.pair {
					held, err = observeUpgradeGUIOwnedPairHeldTopologyWithInputs(context.Background(), stateDir, target, inputs)
				} else {
					held, err = observeUpgradeGUIHeldTopologyWithInputs(context.Background(), stateDir, target, inputs)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := recheckUpgradeGUIBeforeExit(context.Background(), held); err != nil {
					t.Fatalf("stable v2 pre-exit recheck: %v", err)
				}
				if err := held.Close(); err != nil {
					t.Fatal(err)
				}
				if jobHold.queries != 2 {
					t.Fatalf("stable same-handle Job query count=%d, want admission and recheck", jobHold.queries)
				}
			} else {
				terminates, reserves := 0, 0
				_, err := exitAndReserveUpgradeGUIWithOps(context.Background(), stateDir, target, upgradeGUIExitOps{
					Observe: func(ctx context.Context, sd, target string) (*upgradeGUIHeldTopology, error) {
						return observeUpgradeGUIHeldTopologyWithInputs(ctx, sd, target, inputs)
					},
					PreExitCheck:  recheckUpgradeGUIBeforeExit,
					Reserve:       func(string) (gui.OwnedSingleInstanceLease, error) { reserves++; return &upgradeGUITestLease{}, nil },
					PostExitCheck: func(context.Context, *upgradeGUIHeldTopology) error { return nil },
				})
				terminates = guiHold.terminated + trayHold.terminated
				if err == nil || !strings.Contains(err.Error(), tc.wantID) || terminates != 0 || reserves != 0 {
					t.Fatalf("refusal=%v terminations=%d reservations=%d", err, terminates, reserves)
				}
			}
			if guiHold.closed != tc.wantGUI || trayHold.closed != tc.wantTray || jobHold.closed != tc.wantJob {
				t.Fatalf("held cleanup gui=%d tray=%d supervisor=%d, want %d/%d/%d", guiHold.closed, trayHold.closed, jobHold.closed, tc.wantGUI, tc.wantTray, tc.wantJob)
			}
			if jobHold.queries > 0 && tc.wantJob != 1 {
				t.Fatalf("Job query without held supervisor: %d", jobHold.queries)
			}
		})
	}
}

func TestUpgradeGUIHeldGenerationAuthorityIncompleteInputsRefuseBeforeReads(t *testing.T) {
	reads := 0
	in := upgradeGUIObservationInputs{OwnerPath: func() (string, error) { reads++; return "", nil }}
	_, err := observeUpgradeGUIHeldTopologyWithInputs(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "mcphub.exe"), in)
	if err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_IDENTITY_AMBIGUOUS") || reads != 0 {
		t.Fatalf("partial observer inputs=(%v), production read count=%d", err, reads)
	}
}

func TestUpgradeGUIHeldExitTargetsOnlyGUIAndTray(t *testing.T) {
	for _, tc := range []struct {
		name          string
		preErr        error
		jobOwned      bool
		younger       bool
		stageErr      error
		guiErr        error
		wantTerminate int
	}{
		{name: "exact target", wantTerminate: 2},
		{name: "pre-exit drift", preErr: os.ErrPermission},
		{name: "job-owned supervisor", jobOwned: true},
		{name: "younger job-owned supervisor", jobOwned: true, younger: true},
		{name: "stage write failure", stageErr: os.ErrPermission},
		{name: "GUI terminate failure", guiErr: os.ErrPermission, wantTerminate: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			identity := upgradeGUIIdentitySnapshot{
				Target:             `C:\\product\\mcphub.exe`,
				GUI:                upgradeGUIProcessIdentity{PID: 101, Role: "gui", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC), SameUser: true},
				Tray:               upgradeGUIProcessIdentity{PID: 102, Role: "tray", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 1, 0, time.UTC), ParentPID: 101, SameUser: true},
				Supervisor:         upgradeGUIProcessIdentity{PID: 100, Role: "supervise", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC), SameUser: true},
				SupervisorInAnyJob: tc.jobOwned,
			}
			if tc.younger {
				identity.Supervisor.StartedAt = identity.GUI.StartedAt.Add(time.Second)
				identity.Supervisor.ParentPID = identity.GUI.PID
			}
			lease := &upgradeGUITestLease{}
			gotLease, err := exitAndReserveUpgradeGUIWithOps(context.Background(), "state", identity.Target, upgradeGUIExitOps{
				Observe: func(context.Context, string, string) (*upgradeGUIHeldTopology, error) {
					return &upgradeGUIHeldTopology{
						Identity: identity, PidportPath: "pidport",
						GUI:        &upgradeGUITestHeldGeneration{pid: 101, role: "GUI", order: &order, terminateErr: tc.guiErr},
						Tray:       &upgradeGUITestHeldGeneration{pid: 102, role: "tray", order: &order},
						Supervisor: &upgradeGUITestHeldJob{order: &order},
					}, nil
				},
				PreExitCheck:    func(context.Context, *upgradeGUIHeldTopology) error { order = append(order, "pre"); return tc.preErr },
				OnExitRequested: func() error { order = append(order, "stage"); return tc.stageErr },
				Reserve: func(string) (gui.OwnedSingleInstanceLease, error) {
					order = append(order, "reserve")
					return lease, nil
				},
				PostExitCheck: func(context.Context, *upgradeGUIHeldTopology) error { order = append(order, "post"); return nil },
			})
			terminations := 0
			for _, step := range order {
				if strings.HasPrefix(step, "terminate-") {
					terminations++
				}
			}
			if terminations != tc.wantTerminate {
				t.Fatalf("order=%v, want %d terminations", order, tc.wantTerminate)
			}
			if tc.wantTerminate == 2 {
				if err != nil || gotLease != lease || strings.Join(order[:5], ",") != "pre,stage,terminate-GUI,terminate-tray,reserve" {
					t.Fatalf("held exit=(%v,%v), order=%v", gotLease, err, order)
				}
				if releaseErr := gotLease.ReleaseErr(); releaseErr != nil {
					t.Fatal(releaseErr)
				}
			} else if err == nil || gotLease != nil {
				t.Fatalf("refused exit=(%v,%v)", gotLease, err)
			}
			for _, required := range []string{"close-GUI", "close-tray", "close-supervisor"} {
				found := false
				for _, step := range order {
					if step == required {
						found = true
					}
				}
				if !found {
					t.Fatalf("handle %s not closed: %v", required, order)
				}
			}
		})
	}
}

func TestUpgradeGUIOwnedPairIdentityRequiresExactChildJobTopology(t *testing.T) {
	base := upgradeGUIIdentitySnapshot{
		Target:             `C:\product\mcphub.exe`,
		GUI:                upgradeGUIProcessIdentity{PID: 101, Role: "gui", Image: `C:\product\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC), SameUser: true},
		Tray:               upgradeGUIProcessIdentity{PID: 102, Role: "tray", Image: `C:\product\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 1, 0, time.UTC), ParentPID: 101, SameUser: true},
		Supervisor:         upgradeGUIProcessIdentity{PID: 103, Role: "supervise", Image: `C:\product\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 2, 0, time.UTC), ParentPID: 101, SameUser: true},
		SupervisorInAnyJob: true,
	}
	for _, tc := range []struct {
		name      string
		change    func(*upgradeGUIIdentitySnapshot)
		wantError bool
	}{
		{name: "exact GUI child Job pair"},
		{name: "independent supervisor", change: func(s *upgradeGUIIdentitySnapshot) { s.SupervisorInAnyJob = false }, wantError: true},
		{name: "wrong supervisor parent", change: func(s *upgradeGUIIdentitySnapshot) { s.Supervisor.ParentPID = 999 }, wantError: true},
		{name: "older supervisor", change: func(s *upgradeGUIIdentitySnapshot) { s.Supervisor.StartedAt = s.GUI.StartedAt.Add(-time.Second) }, wantError: true},
		{name: "foreign supervisor image", change: func(s *upgradeGUIIdentitySnapshot) { s.Supervisor.Image = `C:\foreign\mcphub.exe` }, wantError: true},
		{name: "different user", change: func(s *upgradeGUIIdentitySnapshot) { s.Supervisor.SameUser = false }, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := base
			if tc.change != nil {
				tc.change(&identity)
			}
			if err := validateUpgradeGUIOwnedPairIdentitySnapshot(identity); (err != nil) != tc.wantError {
				t.Fatalf("pair identity error=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestUpgradeGUIOwnedPairPreparePersistsLivePreimage(t *testing.T) {
	f := newUpgradeGUICommandC3Fixture(t)
	txn, err := newWindowsProductTxnFn(windowsProductTxnRequest{
		Context: context.Background(), StateDir: f.stateDir, ProductPath: f.target,
		LegacyWindowlessPath: filepath.Join(filepath.Dir(f.target), "mcphub-windowless.exe"),
		StagedProduct:        f.candidate, Mode: WindowsProductPairModeUpgrade,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Close()
	guiStart := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	held := &upgradeGUIHeldTopology{
		SupervisorOwner: api.SupervisorLockOwner{PID: 103, StartedAt: guiStart.Add(2 * time.Second).Format(time.RFC3339Nano)},
		Identity: upgradeGUIIdentitySnapshot{
			Target:             f.target,
			GUI:                upgradeGUIProcessIdentity{PID: 101, Role: "gui", Image: f.target, StartedAt: guiStart, SameUser: true},
			Tray:               upgradeGUIProcessIdentity{PID: 102, Role: "tray", Image: f.target, StartedAt: guiStart.Add(time.Second), ParentPID: 101, SameUser: true},
			Supervisor:         upgradeGUIProcessIdentity{PID: 103, Role: "supervise", Image: f.target, StartedAt: guiStart.Add(2 * time.Second), ParentPID: 101, SameUser: true},
			SupervisorInAnyJob: true,
		},
		FrozenPorts: []windowsProductIntentPortV1{{TaskName: `\mcphub-running`, Port: 9137, ArgvSHA256: strings.Repeat("a", 64)}, {TaskName: `\mcphub-stopped`, Port: 9138, ArgvSHA256: strings.Repeat("b", 64)}},
		PortOwners:  []upgradeGUIPortOwner{{TaskName: `\mcphub-running`, Port: 9137, PID: 104, ParentPID: 103, StartedAt: guiStart.Add(3 * time.Second), ArgvSHA256: strings.Repeat("a", 64)}, {TaskName: `\mcphub-stopped`, Port: 9138}},
	}
	if err := prepareUpgradeGUIOwnedPairTransaction(txn, held); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Phase != WindowsProductRecoveryGUIOwnedStopPending || journal.PriorFleetReaped ||
		journal.PreSettlement == nil || journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent ||
		journal.PreSettlement.Supervisor.Generation == nil || journal.PreSettlement.Supervisor.Generation.PID != 103 ||
		len(journal.PreSettlement.Ports) != 2 || journal.PreSettlement.Ports[0].PrePID != 104 || journal.PreSettlement.Ports[1].PreState != windowsProductPortUnbound ||
		len(journal.FrozenPorts) != 2 {
		t.Fatalf("incomplete read-back GUI-owned V3 preimage: %+v", journal)
	}
}

func TestUpgradeGUIOwnedPriorHealthRejectsLivenessWithoutFrozenFleet(t *testing.T) {
	journal := windowsProductRecoveryJournalV3{
		FrozenPorts: []windowsProductFrozenPortV3{{TaskName: `\mcphub-running`, Port: 9137, ArgvSHA256: strings.Repeat("a", 64)}, {TaskName: `\mcphub-stopped`, Port: 9138, ArgvSHA256: strings.Repeat("b", 64)}},
		PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent,
			Ports: []windowsProductPortSettlementV1{{TaskName: `\mcphub-running`, Port: 9137, PreState: windowsProductPortCanonicalOwned, PrePID: 104}, {TaskName: `\mcphub-stopped`, Port: 9138, PreState: windowsProductPortUnbound}}},
		Tasks: WindowsProductPairTaskRecovery{Backups: []WindowsProductPairTaskBackup{{TaskName: `\mcphub-gui`, SHA256: strings.Repeat("c", 64)}}},
	}
	held := &upgradeGUIHeldTopology{
		Identity:    upgradeGUIIdentitySnapshot{Target: `C:\product\mcphub.exe`, GUI: upgradeGUIProcessIdentity{PID: 101}, Tray: upgradeGUIProcessIdentity{PID: 102}, Supervisor: upgradeGUIProcessIdentity{PID: 103}},
		TaskActions: []upgradeGUITaskAction{{Name: `\mcphub-gui`, XMLSHA256: [32]byte{0xcc}}},
		FrozenPorts: []windowsProductIntentPortV1{{TaskName: `\mcphub-running`, Port: 9137, ArgvSHA256: strings.Repeat("a", 64)}, {TaskName: `\mcphub-stopped`, Port: 9138, ArgvSHA256: strings.Repeat("b", 64)}},
		PortOwners:  []upgradeGUIPortOwner{{TaskName: `\mcphub-running`, Port: 9137, PID: 204, ParentPID: 103, ArgvSHA256: strings.Repeat("a", 64)}, {TaskName: `\mcphub-stopped`, Port: 9138}},
	}
	journal.PreSettlement.Ports[0], journal.PreSettlement.Ports[1] = journal.PreSettlement.Ports[1], journal.PreSettlement.Ports[0]
	journal.Tasks.Backups[0].SHA256 = strings.Repeat("c", 64)
	for i := range held.TaskActions[0].XMLSHA256 {
		held.TaskActions[0].XMLSHA256[i] = 0xcc
	}
	if err := validateUpgradeGUIOwnedPriorHealth(journal, held); err != nil {
		t.Fatalf("healthy frozen rows: %v", err)
	}
	held.PortOwners[0].PID = 0
	if err := validateUpgradeGUIOwnedPriorHealth(journal, held); err == nil {
		t.Fatal("mere GUI/supervisor liveness accepted with running daemon absent")
	}
	held.PortOwners[0].PID = 204
	held.PortOwners[1].PID = 205
	if err := validateUpgradeGUIOwnedPriorHealth(journal, held); err == nil {
		t.Fatal("intentionally stopped row became bound")
	}
	held.PortOwners[1].PID = 0
	held.TaskActions[0].XMLSHA256[0] = 0
	if err := validateUpgradeGUIOwnedPriorHealth(journal, held); err == nil {
		t.Fatal("task XML drift accepted")
	}
}

func TestUpgradeGUIOwnedPriorRecoveryRequiresReadbackAndHealth(t *testing.T) {
	journal := windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}}
	for _, tc := range []struct {
		name        string
		readbackErr error
		healthErr   error
		guiAbsent   bool
		state       guiOwnedPriorSupervisorState
		wantStarts  int
		wantPending bool
	}{
		{name: "product readback fails", readbackErr: errors.New("prior Product differs"), guiAbsent: true, state: guiOwnedPriorSupervisorAbsent, wantPending: true},
		{name: "live GUI but running fleet missing", healthErr: errors.New("running daemon port unbound"), state: guiOwnedPriorSupervisorSame, wantPending: true},
		{name: "absent GUI and fleet restored", healthErr: errors.New("GUI absent"), guiAbsent: true, state: guiOwnedPriorSupervisorAbsent, wantStarts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starts := 0
			healthCalls := 0
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := recoverGUIOwnedPriorSettlementWithOps(ctx, journal, guiOwnedPriorRecoveryOps{
				Readback: func(windowsProductRecoveryJournalV3) error { return tc.readbackErr },
				ObserveHealthy: func(context.Context, windowsProductRecoveryJournalV3) error {
					healthCalls++
					if starts == tc.wantStarts && tc.wantStarts > 0 {
						return nil
					}
					return tc.healthErr
				},
				Reservation: func(context.Context) (gui.OwnedSingleInstanceLease, error) {
					if !tc.guiAbsent {
						return nil, errors.New("GUI remains live")
					}
					return &upgradeGUITestLease{}, nil
				},
				SupervisorState: func(context.Context, windowsProductRecoveryJournalV3) (guiOwnedPriorSupervisorState, error) {
					return tc.state, nil
				},
				SettleSame:          func(context.Context, windowsProductRecoveryJournalV3) error { return nil },
				ProveAbsent:         func(context.Context, windowsProductRecoveryJournalV3) error { return nil },
				StartSupervisor:     func() error { starts++; return nil },
				WaitSupervisorReady: func(context.Context) error { return nil },
				ProveIndependent:    func(context.Context, windowsProductRecoveryJournalV3) error { return nil },
				StartGUI:            func() error { starts++; return nil },
				PollInterval:        time.Millisecond,
			})
			if tc.wantPending && (!errors.Is(err, ErrWindowsProductRecoveryPending) || starts != 0) {
				t.Fatalf("pending recovery=(%v, starts=%d), want no start and typed pending", err, starts)
			}
			if !tc.wantPending && (err != nil || starts != tc.wantStarts || healthCalls < 2) {
				t.Fatalf("recovery=(%v, starts=%d, healthCalls=%d)", err, starts, healthCalls)
			}
		})
	}
}

func TestUpgradeGUIOwnedPriorSurvivingJobMustSettleBeforeIndependentRestart(t *testing.T) {
	journal := windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent,
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled, Generation: &windowsProductSupervisorGenerationV1{PID: 103, StartedAt: "2026-09-29T00:00:00Z"}}}}
	steps := []string{}
	lease := &upgradeGUITestLease{onRelease: func() { steps = append(steps, "release-lease") }}
	health := false
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := recoverGUIOwnedPriorSettlementWithOps(ctx, journal, guiOwnedPriorRecoveryOps{
		Readback: func(windowsProductRecoveryJournalV3) error { return nil },
		ObserveHealthy: func(context.Context, windowsProductRecoveryJournalV3) error {
			if health {
				return nil
			}
			return errors.New("old GUI-child Job topology cannot be adopted")
		},
		Reservation: func(context.Context) (gui.OwnedSingleInstanceLease, error) { return lease, nil },
		SupervisorState: func(context.Context, windowsProductRecoveryJournalV3) (guiOwnedPriorSupervisorState, error) {
			steps = append(steps, "original-same")
			return guiOwnedPriorSupervisorSame, nil
		},
		SettleSame: func(context.Context, windowsProductRecoveryJournalV3) error {
			steps = append(steps, "settle-original")
			return nil
		},
		ProveAbsent: func(context.Context, windowsProductRecoveryJournalV3) error {
			steps = append(steps, "lock-ports-free")
			return nil
		},
		StartSupervisor:     func() error { steps = append(steps, "start-independent"); return nil },
		WaitSupervisorReady: func(context.Context) error { steps = append(steps, "ready-independent"); return nil },
		ProveIndependent: func(context.Context, windowsProductRecoveryJournalV3) error {
			steps = append(steps, "independent-fleet-healthy")
			return nil
		},
		StartGUI: func() error {
			steps = append(steps, "start-gui-task")
			if strings.Join(steps, ",") != "original-same,settle-original,lock-ports-free,start-independent,ready-independent,independent-fleet-healthy,release-lease,start-gui-task" {
				return fmt.Errorf("surviving Job was not settled before GUI restart: %v", steps)
			}
			health = true
			return nil
		},
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("exact survivor settlement: %v", err)
	}
}

func TestUpgradeGUIOwnedPriorReservationAcceptsOwnedLeaseButRejectsForeignHolder(t *testing.T) {
	owned := &upgradeGUITestLease{}
	acquires := 0
	ops := guiOwnedPriorReservationOps{
		ProveNoGUIOrTray: func(context.Context) error { return nil },
		AcquireExact: func() (gui.OwnedSingleInstanceLease, error) {
			acquires++
			return nil, gui.ErrSingleInstanceBusy
		},
	}
	lease, err := acquireGUIOwnedPriorReservation(context.Background(), owned, ops)
	if err != nil || lease != owned || acquires != 0 || owned.released != 0 {
		t.Fatalf("caller-held absence reservation=(%v,%v), acquire=%d release=%d", lease, err, acquires, owned.released)
	}
	lease, err = acquireGUIOwnedPriorReservation(context.Background(), nil, ops)
	if lease != nil || !errors.Is(err, ErrWindowsProductRecoveryPending) || acquires != 1 {
		t.Fatalf("foreign holder was accepted: lease=%v err=%v acquires=%d", lease, err, acquires)
	}
	ops.ProveNoGUIOrTray = func(context.Context) error { return errors.New("stale tray generation") }
	lease, err = acquireGUIOwnedPriorReservation(context.Background(), owned, ops)
	if lease != nil || !errors.Is(err, ErrWindowsProductRecoveryPending) || acquires != 1 {
		t.Fatalf("stale GUI/tray was accepted: lease=%v err=%v acquires=%d", lease, err, acquires)
	}
}

func TestUpgradeGUIOwnedPriorRollbackUsesHeldReservationBeforeGUIReturn(t *testing.T) {
	journal := windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}}
	steps := []string{}
	owned := &upgradeGUITestLease{onRelease: func() { steps = append(steps, "release-owned") }}
	health := false
	err := recoverGUIOwnedPriorSettlementWithOps(context.Background(), journal, guiOwnedPriorRecoveryOps{
		Readback: func(windowsProductRecoveryJournalV3) error { return nil },
		ObserveHealthy: func(context.Context, windowsProductRecoveryJournalV3) error {
			if health {
				return nil
			}
			return errors.New("GUI is absent")
		},
		Reservation: func(ctx context.Context) (gui.OwnedSingleInstanceLease, error) {
			steps = append(steps, "prove-absence-under-owned-lease")
			return acquireGUIOwnedPriorReservation(ctx, owned, guiOwnedPriorReservationOps{
				ProveNoGUIOrTray: func(context.Context) error { return nil },
				AcquireExact: func() (gui.OwnedSingleInstanceLease, error) {
					t.Fatal("foreign lease probe while caller owns reservation")
					return nil, nil
				},
			})
		},
		SupervisorState: func(context.Context, windowsProductRecoveryJournalV3) (guiOwnedPriorSupervisorState, error) {
			steps = append(steps, "supervisor-absent")
			return guiOwnedPriorSupervisorAbsent, nil
		},
		SettleSame: func(context.Context, windowsProductRecoveryJournalV3) error {
			t.Fatal("absent branch reaped a process")
			return nil
		},
		ProveAbsent:         func(context.Context, windowsProductRecoveryJournalV3) error { return nil },
		StartSupervisor:     func() error { steps = append(steps, "restart-prior"); return nil },
		WaitSupervisorReady: func(context.Context) error { steps = append(steps, "ready-prior"); return nil },
		ProveIndependent:    func(context.Context, windowsProductRecoveryJournalV3) error { return nil },
		StartGUI:            func() error { steps = append(steps, "start-canonical-gui"); health = true; return nil },
		PollInterval:        time.Millisecond,
	})
	if err != nil || strings.Join(steps, ",") != "prove-absence-under-owned-lease,supervisor-absent,restart-prior,ready-prior,release-owned,start-canonical-gui" || owned.released != 1 {
		t.Fatalf("held reservation rollback err=%v steps=%v releases=%d", err, steps, owned.released)
	}
}

func TestUpgradeGUIOwnedExactPriorReaperBindsFallbackToRecordedGeneration(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "mcphub.exe")
	lockPath := filepath.Join(root, "supervisor.lock")
	started := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	expected := api.SupervisorLockOwner{PID: 103, StartedAt: started.Format(time.RFC3339Nano)}
	if err := api.WriteStateFileAtomic(lockPath+".owner.json", expected); err != nil {
		t.Fatal(err)
	}
	swapProcessLookupForTest(t, process.ProcessIdentity{Basename: "mcphub.exe", CommandLine: `"` + target + `" supervise`, ExecutablePath: target, CreationDateUnix: started.Unix()}, nil)
	swapReapOwnerSIDForTest(t, true, nil)
	killed := swapKillPIDViaTaskkillForTest(t)
	deps := &v5UpgradeDeps{exePath: target, supervisorLockDir: lockPath, pipePath: "test-owned-pipe",
		readSupervisorOwner: func(string) (api.SupervisorLockOwner, error) { return api.ReadSupervisorLockOwner(lockPath) },
		sendIPC: func(_ context.Context, _ string, owner api.SupervisorLockOwner, _ string, _ map[string]any, _ time.Duration) (api.IPCResponse, error) {
			if owner != expected {
				t.Fatalf("IPC target changed: %+v", owner)
			}
			return api.IPCResponse{}, errors.New("injected IPC failure")
		},
	}
	reaper := guiOwnedExactPriorReap{deps: deps, expected: expected}
	if err := api.ReapSupervisor(context.Background(), api.SupervisorReapOpts{PipePath: deps.pipePath, Deps: reaper}); err != nil || len(*killed) != 1 || (*killed)[0] != 103 {
		t.Fatalf("exact fallback err=%v targets=%v", err, *killed)
	}
	if err := api.WriteStateFileAtomic(lockPath+".owner.json", api.SupervisorLockOwner{PID: 999, StartedAt: expected.StartedAt}); err != nil {
		t.Fatal(err)
	}
	if err := api.ReapSupervisor(context.Background(), api.SupervisorReapOpts{PipePath: deps.pipePath, Deps: reaper}); err == nil || len(*killed) != 1 {
		t.Fatalf("drifted generation reaped: err=%v targets=%v", err, *killed)
	}
}

func TestUpgradeGUIOwnedIndependentFleetProvesFortyOneRunningOneStopped(t *testing.T) {
	const supervisorPID = 500
	journal := windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}}
	var observed []windowsProductIntentPortV1
	var owners []upgradeGUIPortOwner
	for i := 0; i < 42; i++ {
		name := fmt.Sprintf(`\mcp-%02d`, i)
		port := 9100 + i
		argv := fmt.Sprintf("%064x", i+1)
		journal.FrozenPorts = append(journal.FrozenPorts, windowsProductFrozenPortV3{TaskName: name, Port: port, ArgvSHA256: argv})
		observed = append(observed, windowsProductIntentPortV1{TaskName: name, Port: port, ArgvSHA256: argv})
		pre := windowsProductPortSettlementV1{TaskName: name, Port: port, PreState: windowsProductPortCanonicalOwned, PrePID: 100 + i}
		owner := upgradeGUIPortOwner{TaskName: name, Port: port, PID: 200 + i, ParentPID: supervisorPID, ArgvSHA256: argv}
		if i == 41 {
			pre.PreState, pre.PrePID = windowsProductPortUnbound, 0
			owner.PID, owner.ParentPID, owner.ArgvSHA256 = 0, 0, ""
		}
		journal.PreSettlement.Ports = append(journal.PreSettlement.Ports, pre)
		owners = append(owners, owner)
	}
	for i, j := 0, len(journal.PreSettlement.Ports)-1; i < j; i, j = i+1, j-1 {
		journal.PreSettlement.Ports[i], journal.PreSettlement.Ports[j] = journal.PreSettlement.Ports[j], journal.PreSettlement.Ports[i]
	}
	if err := validateGUIOwnedIndependentFleet(journal, observed, owners, supervisorPID); err != nil {
		t.Fatalf("41 running/1 stopped: %v", err)
	}
	owners[41].PID = 999
	if err := validateGUIOwnedIndependentFleet(journal, observed, owners, supervisorPID); err == nil {
		t.Fatal("stopped intent port became bound")
	}
	owners[41].PID = 0
	owners[0].PID = 0
	if err := validateGUIOwnedIndependentFleet(journal, observed, owners, supervisorPID); err == nil {
		t.Fatal("running intent port remained unbound")
	}
	owners[0].PID = 200
	owners[0].ParentPID = 999
	if err := validateGUIOwnedIndependentFleet(journal, observed, owners, supervisorPID); err == nil {
		t.Fatal("running port owned by another supervisor")
	}
}

func TestUpgradeGUIOwnedPriorProductReadbackRejectsDrift(t *testing.T) {
	root := t.TempDir()
	product := filepath.Join(root, "mcphub.exe")
	prior := []byte("test-owned prior Product")
	if err := os.WriteFile(product, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	journal := windowsProductRecoveryJournalV3{
		Product:          windowsProductRecoveryArtifactV3{Target: product, PriorPresent: true, PriorSHA256: fmt.Sprintf("%x", sha256.Sum256(prior))},
		LegacyWindowless: windowsProductRecoveryLegacyWindowlessV3{Target: filepath.Join(root, "mcphub-windowless.exe")},
		Receipts: windowsProductRecoveryReceiptsV3{
			V3: windowsProductPairRecoveryReceipt{Path: filepath.Join(root, "upgrade-receipt-v3.json")},
			V2: windowsProductPairRecoveryReceipt{Path: filepath.Join(root, "upgrade-receipt-v2.json")},
		},
	}
	if err := readbackGUIOwnedPriorProduct(journal); err != nil {
		t.Fatalf("unchanged prior readback: %v", err)
	}
	if err := os.WriteFile(product, []byte("changed Product"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readbackGUIOwnedPriorProduct(journal); err == nil {
		t.Fatal("changed prior Product accepted")
	}
}

func TestUpgradeGUIOwnedPairCallerPersistsBeforeExitAndRetainsPendingOnFailedRecovery(t *testing.T) {
	f := newUpgradeGUICommandC3Fixture(t)
	cmd := newUpgradeCmdReal()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	for _, flag := range []string{"terminate-gui-for-upgrade", "stop-gui-owned-fleet-for-upgrade"} {
		if err := cmd.Flags().Set(flag, "true"); err != nil {
			t.Fatal(err)
		}
	}
	guiStart := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	held := &upgradeGUIHeldTopology{StateDir: f.stateDir,
		SupervisorOwner: api.SupervisorLockOwner{PID: 103, StartedAt: guiStart.Add(2 * time.Second).Format(time.RFC3339Nano)},
		Identity: upgradeGUIIdentitySnapshot{Target: f.target,
			GUI:                upgradeGUIProcessIdentity{PID: 101, Role: "gui", Image: f.target, StartedAt: guiStart, SameUser: true},
			Tray:               upgradeGUIProcessIdentity{PID: 102, Role: "tray", Image: f.target, StartedAt: guiStart.Add(time.Second), ParentPID: 101, SameUser: true},
			Supervisor:         upgradeGUIProcessIdentity{PID: 103, Role: "supervise", Image: f.target, StartedAt: guiStart.Add(2 * time.Second), ParentPID: 101, SameUser: true},
			SupervisorInAnyJob: true},
		FrozenPorts: []windowsProductIntentPortV1{{TaskName: `\mcphub-running`, Port: 9137, ArgvSHA256: strings.Repeat("a", 64)}, {TaskName: `\mcphub-stopped`, Port: 9138, ArgvSHA256: strings.Repeat("b", 64)}},
		PortOwners:  []upgradeGUIPortOwner{{TaskName: `\mcphub-running`, Port: 9137, PID: 104, ParentPID: 103, StartedAt: guiStart.Add(3 * time.Second), ArgvSHA256: strings.Repeat("a", 64)}, {TaskName: `\mcphub-stopped`, Port: 9138}},
	}
	stops, callbacks := 0, 0
	priorRunner := runInstallUpgradeWindowsFn
	t.Cleanup(func() { runInstallUpgradeWindowsFn = priorRunner })
	runInstallUpgradeWindowsFn = func(_ context.Context, opts UpgradeOpts) error {
		if !opts.GUIOwnedPending || opts.WindowsProduct != f.txn || f.txn.prepared == nil || f.txn.prepared.journal.Phase != WindowsProductRecoveryGUIOwnedStopPending || stops != 1 {
			t.Fatalf("Product run did not receive held pending preimage after exact exit")
		}
		return f.txn.rollback(errors.New("injected predecision failure"))
	}
	deps := upgradeGUIFencedDeps{
		Classify: func(string, string, string) (upgradeGUIProductOutcome, error) {
			return upgradeGUINewTransactionRequired, nil
		},
		PublishCurrent: func(string, string, string, WindowsProductPairMode) (bool, error) {
			t.Fatal("unexpected current publication")
			return false, nil
		},
		OnNewTransaction: func() error { return nil },
		ExitAndReserve: func(context.Context, string, string) (gui.OwnedSingleInstanceLease, error) {
			t.Fatal("old independent exit path used")
			return nil, nil
		},
		ObservePair: func(context.Context, string, string) (*upgradeGUIHeldTopology, error) { return held, nil },
		ExitPair: func(_ context.Context, got *upgradeGUIHeldTopology, txn *WindowsProductTxn) (gui.OwnedSingleInstanceLease, error) {
			if got != held || txn != f.txn {
				t.Fatal("exact pair/transaction changed")
			}
			raw, err := os.ReadFile(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3))
			if err != nil {
				t.Fatal(err)
			}
			journal, err := decodeWindowsProductRecoveryJournalV3(raw)
			if err != nil || journal.Phase != WindowsProductRecoveryGUIOwnedStopPending || journal.PreSettlement == nil || journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent {
				t.Fatalf("no readback pending journal before stop: %v %+v", err, journal)
			}
			stops++
			return &upgradeGUITestLease{}, nil
		},
		ObserveAfterPair: func(context.Context, string, string, []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
			t.Fatal("runner stub should not observe")
			return windowsProductQuiescenceObservationV1{}, nil
		},
		RecoverPriorPair: func(windowsProductRecoveryJournalV3) error {
			callbacks++
			return errors.New("prior fleet remains unproven")
		},
		AfterCommitPair: func(windowsProductRecoveryJournalV3, func() error) error {
			t.Fatal("predecision failure reached committed GUI return")
			return nil
		},
	}
	err := runV5UpgradeWindowsWithPathsAndDeps(cmd, f.candidate, f.target, deps)
	if !errors.Is(err, ErrWindowsProductRecoveryPending) || stops != 1 || callbacks != 1 {
		t.Fatalf("pair predecision result=(%v, stops=%d, callbacks=%d), want typed pending", err, stops, callbacks)
	}
	if _, statErr := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); statErr != nil {
		t.Fatalf("pending V3 journal was removed: %v", statErr)
	}
	got, err := os.ReadFile(f.target)
	if err != nil || !bytes.Equal(got, f.priorBytes) {
		t.Fatalf("prior Product changed before failed rollback: %v", err)
	}
}

func TestUpgradeGUIOwnedPairSettlesBeforePromotionAndRestoresGUIBeforeJournalRemoval(t *testing.T) {
	f := newUpgradeGUICommandC3Fixture(t)
	intent := `{"version":1,"updated_at":"2026-09-28T00:00:00Z","daemons":[{"task_name":"\\mcp-running","command":"mcphub","args":["daemon","running"],"port":9137},{"task_name":"\\mcp-stopped","command":"mcphub","args":["daemon","stopped"],"port":9138}],"strict_mode":false}`
	writeStateSidecarBytes(t, filepath.Join(f.stateDir, "supervisor-intent.json"), []byte(intent))
	cmd := newUpgradeCmdReal()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	for _, flag := range []string{"terminate-gui-for-upgrade", "stop-gui-owned-fleet-for-upgrade"} {
		if err := cmd.Flags().Set(flag, "true"); err != nil {
			t.Fatal(err)
		}
	}
	guiStart := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	held := &upgradeGUIHeldTopology{StateDir: f.stateDir,
		SupervisorOwner: api.SupervisorLockOwner{PID: 103, StartedAt: guiStart.Add(2 * time.Second).Format(time.RFC3339Nano)},
		Identity: upgradeGUIIdentitySnapshot{Target: f.target,
			GUI:        upgradeGUIProcessIdentity{PID: 101, Role: "gui", Image: f.target, StartedAt: guiStart, SameUser: true},
			Tray:       upgradeGUIProcessIdentity{PID: 102, Role: "tray", Image: f.target, StartedAt: guiStart.Add(time.Second), ParentPID: 101, SameUser: true},
			Supervisor: upgradeGUIProcessIdentity{PID: 103, Role: "supervise", Image: f.target, StartedAt: guiStart.Add(2 * time.Second), ParentPID: 101, SameUser: true}, SupervisorInAnyJob: true},
		FrozenPorts: []windowsProductIntentPortV1{{TaskName: `\mcp-running`, Port: 9137, ArgvSHA256: windowsProductArgvSHA256([]string{"mcphub", "daemon", "running"})}, {TaskName: `\mcp-stopped`, Port: 9138, ArgvSHA256: windowsProductArgvSHA256([]string{"mcphub", "daemon", "stopped"})}},
		PortOwners:  []upgradeGUIPortOwner{{TaskName: `\mcp-running`, Port: 9137, PID: 104, ParentPID: 103, StartedAt: guiStart.Add(3 * time.Second), ArgvSHA256: windowsProductArgvSHA256([]string{"mcphub", "daemon", "running"})}, {TaskName: `\mcp-stopped`, Port: 9138}},
	}
	lease := &upgradeGUITestLease{}
	stops, forward := 0, 0
	deps := upgradeGUIFencedDeps{
		Classify: func(string, string, string) (upgradeGUIProductOutcome, error) {
			return upgradeGUINewTransactionRequired, nil
		},
		PublishCurrent: func(string, string, string, WindowsProductPairMode) (bool, error) {
			t.Fatal("unexpected current publication")
			return false, nil
		},
		OnNewTransaction: func() error { return nil },
		ExitAndReserve: func(context.Context, string, string) (gui.OwnedSingleInstanceLease, error) {
			t.Fatal("old exit path")
			return nil, nil
		},
		ObservePair: func(context.Context, string, string) (*upgradeGUIHeldTopology, error) { return held, nil },
		ExitPair: func(_ context.Context, _ *upgradeGUIHeldTopology, txn *WindowsProductTxn) (gui.OwnedSingleInstanceLease, error) {
			if txn.prepared == nil || txn.prepared.journal.Phase != WindowsProductRecoveryGUIOwnedStopPending {
				t.Fatal("stop before V3 pending readback")
			}
			stops++
			return lease, nil
		},
		ObserveAfterPair: func(_ context.Context, _, _ string, ports []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
			if stops != 1 || len(ports) != 2 {
				t.Fatalf("post-stop observation order/ports: stops=%d ports=%v", stops, ports)
			}
			return windowsProductQuiescenceObservationV1{Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent}, GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true,
				Ports: []windowsProductPortSettlementV1{{TaskName: `\mcp-running`, Port: 9137, PreState: windowsProductPortUnbound, PostUnbound: true}, {TaskName: `\mcp-stopped`, Port: 9138, PreState: windowsProductPortUnbound, PostUnbound: true}}}, nil
		},
		AfterCommitPair: func(journal windowsProductRecoveryJournalV3, release func() error) error {
			forward++
			if journal.Phase != WindowsProductRecoveryCommitDecided || journal.Quiescence == nil || !journal.PriorFleetReaped || journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent {
				t.Fatalf("forward before settled V3: %+v", journal)
			}
			if _, err := os.Stat(filepath.Join(f.stateDir, UpgradeReceiptSchemaV3+".json")); err != nil {
				t.Fatalf("receipt not read back before GUI return: %v", err)
			}
			return release()
		},
	}
	if err := runV5UpgradeWindowsWithPathsAndDeps(cmd, f.candidate, f.target, deps); err != nil {
		t.Fatal(err)
	}
	if stops != 1 || forward != 1 || lease.released != 1 || f.txn.prepared.journal.Phase != WindowsProductRecoverySettledCommit {
		t.Fatalf("settlement stops=%d forward=%d release=%d phase=%s", stops, forward, lease.released, f.txn.prepared.journal.Phase)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled journal remains: %v", err)
	}
}

func TestUpgradeGUIOwnedCommittedForwardRetainsJournalUntilGUIProof(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoveryCommitDecided)
	f.journal.Mode = WindowsProductPairModeUpgrade
	f.journal.PreSettlement = &windowsProductPreSettlementV3{Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
		Generation: &windowsProductSupervisorGenerationV1{PID: 103, StartedAt: "2026-09-29T00:00:00Z"}}, GUIOwner: windowsProductGUIOwnerConfirmedPresent}
	receipt, err := mintWindowsProductQuiescenceReceipt(f.product, f.legacy,
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
	f.journal.Quiescence, f.journal.QuiescenceReceiptSHA256, f.journal.PriorFleetReaped = &projection, digest, true
	journalPath := filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)
	if err := persistWindowsProductRecoveryJournalV3(journalPath, f.journal, api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
		t.Fatal(err)
	}
	tasks := &recoveryTaskTxnFake{digest: f.journal.TaskSetSHA256}
	forwardCalls := 0
	req := f.request(tasks)
	req.ObserveSuccessorReady = func(context.Context, string, string) (bool, error) { return true, nil }
	req.StartSupervisor = func(string) error { t.Fatal("committed successor must not restart"); return nil }
	req.WaitSupervisorReady = func(context.Context, string, string) error { t.Fatal("committed successor must not wait"); return nil }
	req.Deps.AdmitProduct = func(input binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		hash, err := hashRecoveryFile(input.Path)
		if err != nil {
			return binaryadmission.WindowsArtifact{}, err
		}
		return binaryadmission.WindowsArtifact{Path: input.Path, Role: binaryadmission.WindowsArtifactRoleProduct, Machine: 34404,
			Subsystem: binaryadmission.WindowsGUISubsystem, Version: "0.4.38", Commit: strings.Repeat("a", 40), BuildDate: "2026-09-29T00:00:00Z", SHA256: hash}, nil
	}
	req.Deps.PublishCommitted = func(WindowsProductCommitEvent) (string, error) {
		forwardCalls++
		if forwardCalls == 1 {
			return "", fmt.Errorf("%w: GUI/tray not healthy", ErrWindowsProductRecoveryPending)
		}
		return strings.Repeat("f", 64), nil
	}
	if _, err := reconcileWindowsProductRecovery(req); err == nil {
		t.Fatal("committed forward falsely settled without GUI proof")
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("commit_decided journal lost after GUI failure: %v", err)
	}
	assertPairFile(t, f.product, f.newProduct)
	if tasks.restoreCalls != 0 {
		t.Fatalf("old Product rollback after commit decision: %d", tasks.restoreCalls)
	}
	settled, err := reconcileWindowsProductRecovery(req)
	if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit || forwardCalls != 2 {
		t.Fatalf("forward retry settled=%+v err=%v calls=%d", settled, err, forwardCalls)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled forward journal remains: %v", err)
	}
}

func TestUpgradeGUIReservationErrorReleasesTentativeLease(t *testing.T) {
	var order []string
	lease := &upgradeGUITestLease{releaseErr: os.ErrPermission}
	identity := upgradeGUIIdentitySnapshot{
		Target:     `C:\\product\\mcphub.exe`,
		GUI:        upgradeGUIProcessIdentity{PID: 101, Role: "gui", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC), SameUser: true},
		Tray:       upgradeGUIProcessIdentity{PID: 102, Role: "tray", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 1, 0, time.UTC), ParentPID: 101, SameUser: true},
		Supervisor: upgradeGUIProcessIdentity{PID: 100, Role: "supervise", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC), SameUser: true},
	}
	got, err := exitAndReserveUpgradeGUIWithOps(context.Background(), "state", identity.Target, upgradeGUIExitOps{
		Observe: func(context.Context, string, string) (*upgradeGUIHeldTopology, error) {
			return &upgradeGUIHeldTopology{Identity: identity, PidportPath: "pidport", GUI: &upgradeGUITestHeldGeneration{pid: 101, role: "GUI", order: &order}, Tray: &upgradeGUITestHeldGeneration{pid: 102, role: "tray", order: &order}, Supervisor: &upgradeGUITestHeldJob{order: &order}}, nil
		},
		PreExitCheck:  func(context.Context, *upgradeGUIHeldTopology) error { return nil },
		Reserve:       func(string) (gui.OwnedSingleInstanceLease, error) { return lease, os.ErrPermission },
		PostExitCheck: func(context.Context, *upgradeGUIHeldTopology) error { return nil },
	})
	if got != nil || err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_LEASE_RELEASE_UNCONFIRMED") || lease.released != 1 {
		t.Fatalf("reservation=(%v,%v), release count=%d; want refused and released", got, err, lease.released)
	}
}

func TestUpgradeGUIStageCleanupPreservesJournalBoundSibling(t *testing.T) {
	for _, tc := range []struct {
		name      string
		journal   string
		directory bool
		wantStage bool
		wantErr   bool
	}{
		{name: "unjournaled stage removed"},
		{name: "V3 journal preserves stage", journal: WindowsProductRecoveryFileLeafV3, wantStage: true},
		{name: "uncertain journal preserves stage and reports", journal: WindowsProductPairRecoveryFileLeaf, directory: true, wantStage: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			stage := filepath.Join(t.TempDir(), "mcphub.exe.new")
			body := []byte("test-owned candidate")
			if err := os.WriteFile(stage, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.journal != "" {
				path := filepath.Join(stateDir, tc.journal)
				if tc.directory {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte("journal"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := cleanupUpgradeStagedProduct(stateDir, stage, fmt.Sprintf("%x", sha256.Sum256(body)))
			if (err != nil) != tc.wantErr {
				t.Fatalf("cleanup error=%v, want error=%v", err, tc.wantErr)
			}
			_, statErr := os.Stat(stage)
			if tc.wantStage && statErr != nil {
				t.Fatalf("journal-bound stage was removed: %v", statErr)
			}
			if !tc.wantStage && !os.IsNotExist(statErr) {
				t.Fatalf("unjournaled stage remains: %v", statErr)
			}
		})
	}
}

func TestUpgradeGUIOwnerPathBinding(t *testing.T) {
	guiRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", guiRoot)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", "")
	stateDir := t.TempDir()
	want, err := gui.PidportPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("test-owned owner record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveUpgradeGUIOwnerPath()
	if err != nil || got != want || samePath(got, filepath.Join(stateDir, gui.PidportFileLeaf)) {
		t.Fatalf("GUI path=(%q,%v), want actual GUI resolver path distinct from fence root", got, err)
	}
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", t.TempDir())
	if _, err := resolveUpgradeGUIOwnerPath(); err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_LOCK_PATH_MISMATCH") {
		t.Fatalf("GUI test override error=%v, want path mismatch refusal", err)
	}
}

func TestUpgradeGUIFenceCoversExitToTxn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		journal     string
		outcome     upgradeGUIProductOutcome
		lateJournal bool
		wantError   string
		wantExits   int
	}{
		{name: "v3 pending refuses", journal: WindowsProductRecoveryFileLeafV3, outcome: upgradeGUINewTransactionRequired, wantError: "UPGRADE_GUI_PENDING_RECOVERY"},
		{name: "legacy pending refuses", journal: WindowsProductPairRecoveryFileLeaf, outcome: upgradeGUINewTransactionRequired, wantError: "UPGRADE_GUI_PENDING_RECOVERY"},
		{name: "already current keeps GUI", outcome: upgradeGUIAlreadyCurrent},
		{name: "new candidate exits under fence", outcome: upgradeGUINewTransactionRequired, wantExits: 1},
		{name: "journal appears before exit", outcome: upgradeGUINewTransactionRequired, lateJournal: true, wantError: "UPGRADE_GUI_PENDING_RECOVERY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			if tc.journal != "" {
				if err := os.WriteFile(filepath.Join(stateDir, tc.journal), []byte("test-owned"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fence, err := api.AcquireUpgradeFence(context.Background(), stateDir)
			if err != nil {
				t.Fatal(err)
			}
			defer fence.Release()
			exits, publications, newStages := 0, 0, 0
			var lease *upgradeGUITestLease
			gotLease, gotOutcome, err := runUpgradeGUIFencedPrelude(context.Background(), fence, stateDir, "candidate", "target", upgradeGUIFencedDeps{
				Classify: func(string, string, string) (upgradeGUIProductOutcome, error) {
					if tc.lateJournal {
						if writeErr := os.WriteFile(filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3), []byte("late"), 0o600); writeErr != nil {
							t.Fatal(writeErr)
						}
					}
					return tc.outcome, nil
				},
				PublishCurrent:   func(string, string, string, WindowsProductPairMode) (bool, error) { publications++; return true, nil },
				OnNewTransaction: func() error { newStages++; return nil },
				ExitAndReserve: func(context.Context, string, string) (gui.OwnedSingleInstanceLease, error) {
					exits++
					_, acquired, probeErr := api.TryAcquireUpgradeFence(context.Background(), stateDir)
					if probeErr != nil || acquired {
						t.Fatalf("fence was not held at GUI exit: acquired=%v err=%v", acquired, probeErr)
					}
					lease = &upgradeGUITestLease{}
					return lease, nil
				},
			})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("prelude error=%v, want %s", err, tc.wantError)
				}
			} else if err != nil || gotOutcome != tc.outcome {
				t.Fatalf("prelude=(%q,%v), want outcome %q", gotOutcome, err, tc.outcome)
			}
			if exits != tc.wantExits {
				t.Fatalf("GUI exits=%d, want %d", exits, tc.wantExits)
			}
			if tc.outcome == upgradeGUIAlreadyCurrent && tc.wantError == "" && publications != 1 {
				t.Fatalf("current event publications=%d, want 1", publications)
			}
			if tc.wantError != "" && publications != 0 {
				t.Fatalf("refusal published current event %d times", publications)
			}
			if newStages != tc.wantExits {
				t.Fatalf("new-transaction stages=%d, want %d before exit", newStages, tc.wantExits)
			}
			if gotLease != nil {
				if releaseErr := gotLease.ReleaseErr(); releaseErr != nil {
					t.Fatal(releaseErr)
				}
			}
			if lease != nil && lease.released != 1 {
				t.Fatalf("GUI lease releases=%d, want 1", lease.released)
			}
		})
	}
}

func TestUpgradeGUIExactGenerationOnly(t *testing.T) {
	base := upgradeGUIIdentitySnapshot{
		Target:     `C:\\product\\mcphub.exe`,
		GUI:        upgradeGUIProcessIdentity{PID: 101, Role: "gui", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC), SameUser: true},
		Tray:       upgradeGUIProcessIdentity{PID: 102, Role: "tray", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 8, 0, 1, 0, time.UTC), ParentPID: 101, SameUser: true},
		Supervisor: upgradeGUIProcessIdentity{PID: 100, Role: "supervise", Image: `C:\\product\\mcphub.exe`, StartedAt: time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC), SameUser: true},
	}
	for _, tc := range []struct {
		name      string
		change    func(*upgradeGUIIdentitySnapshot)
		wantError bool
	}{
		{name: "independent exact generations"},
		{name: "job owned supervisor", change: func(s *upgradeGUIIdentitySnapshot) { s.SupervisorInAnyJob = true }, wantError: true},
		{name: "supervisor newer than GUI", change: func(s *upgradeGUIIdentitySnapshot) { s.Supervisor.StartedAt = s.GUI.StartedAt.Add(time.Second) }, wantError: true},
		{name: "tray wrong parent", change: func(s *upgradeGUIIdentitySnapshot) { s.Tray.ParentPID = 999 }, wantError: true},
		{name: "tray foreign image", change: func(s *upgradeGUIIdentitySnapshot) { s.Tray.Image = `C:\\foreign\\mcphub.exe` }, wantError: true},
		{name: "tray foreign owner", change: func(s *upgradeGUIIdentitySnapshot) { s.Tray.SameUser = false }, wantError: true},
		{name: "GUI wrong role", change: func(s *upgradeGUIIdentitySnapshot) { s.GUI.Role = "daemon" }, wantError: true},
		{name: "supervisor targeted as tray", change: func(s *upgradeGUIIdentitySnapshot) { s.Tray.PID = s.Supervisor.PID }, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := base
			if tc.change != nil {
				tc.change(&snapshot)
			}
			err := validateUpgradeGUIIdentitySnapshot(snapshot)
			if (err != nil) != tc.wantError {
				t.Fatalf("identity error=%v, want error=%v", err, tc.wantError)
			}
		})
	}
}

func TestUpgradeGUIPriorOutcomeBeforeExitExactJournalLeaves(t *testing.T) {
	for _, tc := range []struct {
		name        string
		make        func(*testing.T, string)
		wantPresent bool
		wantErr     bool
	}{
		{name: "both absent"},
		{name: "v3 present", make: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, WindowsProductRecoveryFileLeafV3), []byte("pending"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantPresent: true},
		{name: "legacy present", make: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, WindowsProductPairRecoveryFileLeaf), []byte("pending"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantPresent: true},
		{name: "v3 regular and legacy directory is uncertain", make: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, WindowsProductRecoveryFileLeafV3), []byte("pending"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, WindowsProductPairRecoveryFileLeaf), 0o700); err != nil {
				t.Fatal(err)
			}
		}, wantErr: true},
		{name: "v3 directory is uncertain", make: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(dir, WindowsProductRecoveryFileLeafV3), 0o700); err != nil {
				t.Fatal(err)
			}
		}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.make != nil {
				tc.make(t, dir)
			}
			before, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			present, err := inspectWindowsProductRecoveryLeaves(dir)
			if (err != nil) != tc.wantErr || (!tc.wantErr && present != tc.wantPresent) {
				t.Fatalf("observation=(%v,%v), want present=%v error=%v", present, err, tc.wantPresent, tc.wantErr)
			}
			after, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("read-only observation changed leaf count %d -> %d", len(before), len(after))
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing-root")
	if _, err := inspectWindowsProductRecoveryLeaves(missing); err == nil || !strings.Contains(err.Error(), "state root") {
		t.Fatalf("missing state root error=%v, want uncertainty", err)
	}
}

func TestUpgradeGUIFlagIsPositiveAndUpgradeOnly(t *testing.T) {
	prior := upgradeDispatcher
	t.Cleanup(func() { upgradeDispatcher = prior })
	var observed []bool
	upgradeDispatcher = func(cmd *cobra.Command) error {
		value, err := cmd.Flags().GetBool("terminate-gui-for-upgrade")
		if err != nil {
			return err
		}
		observed = append(observed, value)
		return nil
	}
	for _, tc := range []struct {
		name    string
		command *cobra.Command
		args    []string
	}{
		{name: "alias default", command: newUpgradeCmdReal()},
		{name: "alias opt in", command: newUpgradeCmdReal(), args: []string{"--terminate-gui-for-upgrade"}},
		{name: "install opt in", command: newInstallCmdReal(), args: []string{"--upgrade", "--terminate-gui-for-upgrade"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.command.SetOut(io.Discard)
			tc.command.SetErr(io.Discard)
			tc.command.SetArgs(tc.args)
			if err := tc.command.Execute(); err != nil {
				t.Fatalf("dispatch: %v", err)
			}
		})
	}
	if len(observed) != 3 || observed[0] || !observed[1] || !observed[2] {
		t.Fatalf("dispatcher opt-in values=%v, want [false true true]", observed)
	}
	install := newInstallCmdReal()
	install.SetOut(io.Discard)
	install.SetErr(io.Discard)
	install.SetArgs([]string{"--terminate-gui-for-upgrade"})
	if err := install.Execute(); err == nil || !strings.Contains(err.Error(), "requires --upgrade") {
		t.Fatalf("flag without upgrade error=%v, want refusal", err)
	}
}

func TestUpgradeGUIOwnedFleetStopRequiresBothPositiveUpgradeFlags(t *testing.T) {
	prior := upgradeDispatcher
	t.Cleanup(func() { upgradeDispatcher = prior })
	dispatches := 0
	upgradeDispatcher = func(cmd *cobra.Command) error {
		dispatches++
		gui, guiErr := cmd.Flags().GetBool("terminate-gui-for-upgrade")
		fleet, fleetErr := cmd.Flags().GetBool("stop-gui-owned-fleet-for-upgrade")
		if guiErr != nil || fleetErr != nil || !gui || !fleet {
			return fmt.Errorf("both positive upgrade grants required: gui=%v fleet=%v guiErr=%v fleetErr=%v", gui, fleet, guiErr, fleetErr)
		}
		return nil
	}
	for _, makeCommand := range []func() *cobra.Command{newUpgradeCmdReal, newInstallCmdReal} {
		cmd := makeCommand()
		args := []string{"--terminate-gui-for-upgrade", "--stop-gui-owned-fleet-for-upgrade"}
		if cmd.Name() == "install" {
			args = append([]string{"--upgrade"}, args...)
		}
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%s two-flag dispatch: %v", cmd.Name(), err)
		}
	}
	if dispatches != 2 {
		t.Fatalf("two-flag dispatches=%d, want 2", dispatches)
	}
	for _, args := range [][]string{{"--stop-gui-owned-fleet-for-upgrade"}, {"--upgrade", "--stop-gui-owned-fleet-for-upgrade"}} {
		cmd := newInstallCmdReal()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("install args %v accepted without both required grants", args)
		}
	}
	if dispatches != 2 {
		t.Fatalf("invalid flags reached upgrade dispatcher: %d", dispatches)
	}
}

func TestUpgradeGUIOptInBypassesOnlyDefaultLiveGUIRefusal(t *testing.T) {
	resetUpgradeSeams(t)
	upgradeExecutableFn = func() (string, error) { return filepath.Join(t.TempDir(), "candidate.exe"), nil }
	upgradeTargetPathFn = func() (string, error) { return filepath.Join(t.TempDir(), "mcphub.exe"), nil }
	findRunningGUIsOnTargetFn = func(string) ([]api.ProcessInfo, error) { return []api.ProcessInfo{{PID: 4321}}, nil }
	prior := v5UpgradeFn
	t.Cleanup(func() { v5UpgradeFn = prior })
	calls := 0
	v5UpgradeFn = func(*cobra.Command) error { calls++; return nil }
	defaultCmd := newUpgradeCmdReal()
	defaultCmd.SetErr(io.Discard)
	if err := runV5UpgradeReal(defaultCmd); err == nil || !strings.Contains(err.Error(), "running mcphub GUI") {
		t.Fatalf("default GUI refusal=%v", err)
	}
	if calls != 0 {
		t.Fatalf("default route entered transaction %d times", calls)
	}
	optInCmd := newUpgradeCmdReal()
	if err := optInCmd.Flags().Set("terminate-gui-for-upgrade", "true"); err != nil {
		t.Fatal(err)
	}
	if err := runV5UpgradeReal(optInCmd); err != nil || calls != 1 {
		t.Fatalf("opt-in dispatch=(%v,%d), want one transaction call", err, calls)
	}
}

func TestUpgradeGUIOptInCandidateAdmissionPrecedesFenceWait(t *testing.T) {
	withTempStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := newUpgradeCmdReal()
	cmd.SetContext(ctx)
	if err := cmd.Flags().Set("terminate-gui-for-upgrade", "true"); err != nil {
		t.Fatal(err)
	}
	err := runV5UpgradeWindowsWithPaths(cmd, filepath.Join(t.TempDir(), "invalid-candidate.exe"), filepath.Join(t.TempDir(), "mcphub.exe"))
	if err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE") {
		t.Fatalf("invalid candidate before fence error=%v, want typed admission refusal", err)
	}
}

func TestUpgradeGUIPriorOutcomeBeforeExitReceipt(t *testing.T) {
	stateDir := t.TempDir()
	candidate := filepath.Join(t.TempDir(), "candidate.exe")
	target := filepath.Join(t.TempDir(), "mcphub.exe")
	for _, path := range []string{candidate, target} {
		if err := os.WriteFile(path, []byte("test-owned requested Product"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(input binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		body, err := os.ReadFile(input.Path)
		if err != nil {
			return binaryadmission.WindowsArtifact{}, err
		}
		return binaryadmission.WindowsArtifact{
			Path: input.Path, Role: binaryadmission.WindowsArtifactRoleProduct,
			Machine: 34404, Subsystem: binaryadmission.WindowsGUISubsystem,
			Version: "0.4.38", Commit: strings.Repeat("c", 40), BuildDate: "2026-09-28T00:00:00Z",
			SHA256: fmt.Sprintf("%x", sha256.Sum256(body)),
		}, nil
	}
	product, err := admit(binaryadmission.WindowsArtifact{Path: candidate, Role: binaryadmission.WindowsArtifactRoleProduct})
	if err != nil {
		t.Fatal(err)
	}
	receipt := UpgradeReceiptV3{
		Schema: UpgradeReceiptSchemaV3, TransactionID: strings.Repeat("a", 32),
		Mode: WindowsProductPairModeUpgrade, Admission: UpgradeAdmissionLocalProductV3,
		Artifact: UpgradeReceiptArtifactV3{
			Role: binaryadmission.WindowsArtifactRoleProduct, Machine: product.Machine,
			Subsystem: product.Subsystem, Version: product.Version, Commit: product.Commit,
			BuildDate: product.BuildDate, SHA256: product.SHA256,
		},
		TaskSetSHA256: strings.Repeat("b", 64),
		Retired:       UpgradeReceiptRetiredV3{WindowlessAbsent: true, AdmissionHelperAbsentFromPayload: true, RollbackImagesAbsent: true},
		InstalledAt:   "2026-09-28T00:00:01Z",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json")
	if err := os.WriteFile(receiptPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, err := inspectWindowsProductUpgradeReceiptWithAdmit(stateDir, candidate, target, admit)
	if err != nil || outcome != upgradeGUIAlreadyCurrent {
		t.Fatalf("matching receipt outcome=(%q,%v), want already current", outcome, err)
	}
	after, err := os.ReadFile(receiptPath)
	if err != nil || string(after) != string(raw) {
		t.Fatalf("classification rewrote receipt: %v", err)
	}
	if err := os.WriteFile(receiptPath, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectWindowsProductUpgradeReceiptWithAdmit(stateDir, candidate, target, admit); err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE") {
		t.Fatalf("corrupt receipt error=%v, want typed refusal", err)
	}
	if err := os.WriteFile(receiptPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("drifted canonical Product"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectWindowsProductUpgradeReceiptWithAdmit(stateDir, candidate, target, admit); err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE") {
		t.Fatalf("canonical/receipt mismatch error=%v, want typed refusal", err)
	}
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}
	outcome, err = inspectWindowsProductUpgradeReceiptWithAdmit(stateDir, candidate, target, admit)
	if err != nil || outcome != upgradeGUINewTransactionRequired {
		t.Fatalf("valid different candidate outcome=(%q,%v), want new transaction", outcome, err)
	}
	if err := os.WriteFile(target, []byte("test-owned requested Product"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectWindowsProductUpgradeReceiptWithAdmit(stateDir, candidate, target, admit); err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE") {
		t.Fatalf("same canonical with missing receipt error=%v, want typed refusal", err)
	}
}
