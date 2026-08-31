//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

func writeAdmissionPEFixture(t *testing.T, subsystem uint16, malformed bool) string {
	t.Helper()
	data := make([]byte, 0x200)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], 0x80)
	copy(data[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[0x80+20:0x80+22], 0xf0)
	binary.LittleEndian.PutUint16(data[0x80+24:0x80+26], 0x20b)
	binary.LittleEndian.PutUint16(data[0x80+24+68:0x80+24+70], subsystem)
	if malformed {
		data = data[:31]
	}
	p := filepath.Join(t.TempDir(), "candidate.exe")
	if err := os.WriteFile(p, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeAdmissionPEFixtureWithTag(t *testing.T, subsystem uint16, tag string) string {
	t.Helper()
	path := writeAdmissionPEFixture(t, subsystem, false)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tag); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertRejectedWithoutDestinationMutation(t *testing.T, mutate func(src, dst string) error) {
	t.Helper()
	for _, tc := range []struct {
		name      string
		subsystem uint16
		malformed bool
		id        string
	}{
		{"CUI", 3, false, binaryadmission.WindowsPESubsystemErrorID},
		{"malformed", 0, true, binaryadmission.WindowsPEFormatErrorID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := writeAdmissionPEFixture(t, tc.subsystem, tc.malformed)
			dst := filepath.Join(dir, "mcphub.exe")
			prior := []byte("PRIOR-EXACT-BYTES")
			if err := os.WriteFile(dst, prior, 0o755); err != nil {
				t.Fatal(err)
			}
			err := mutate(src, dst)
			if err == nil || !strings.Contains(err.Error(), tc.id) {
				t.Fatalf("error=%v, want %s", err, tc.id)
			}
			got, readErr := os.ReadFile(dst)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(got, prior) {
				t.Fatalf("destination mutated: got %q want %q", got, prior)
			}
			matches, globErr := filepath.Glob(filepath.Join(dir, "mcphub.exe.*.tmp"))
			if globErr != nil || len(matches) != 0 {
				t.Fatalf("temp artifacts after rejection: %v, err=%v", matches, globErr)
			}
		})
	}
}

func TestWindowsPEAdmissionSetup(t *testing.T) {
	assertRejectedWithoutDestinationMutation(t, copyExe)
}

func TestWindowsPEAdmissionCanonicalize(t *testing.T) {
	assertRejectedWithoutDestinationMutation(t, func(src, dst string) error {
		return canonicalizeBinaryToTarget(&bytes.Buffer{}, src, dst)
	})
}

func TestWindowsPEAdmissionMigration(t *testing.T) {
	assertRejectedWithoutDestinationMutation(t, func(src, dst string) error {
		_, err := stageV5UpgradeBinary(src, dst)
		return err
	})
}

func TestWindowsPEAdmissionUpgrade(t *testing.T) {
	assertRejectedWithoutDestinationMutation(t, func(src, dst string) error {
		_, err := stageV5UpgradeBinary(src, dst)
		return err
	})
}

func TestV5UpgradeDepsRetainedRollbackExactBytes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mcphub.exe")
	priorFixture := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "PRIOR-EXACT")
	newFixture := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "SUCCESSOR")
	prior, err := os.ReadFile(priorFixture)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := os.ReadFile(newFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, prior, 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "mcphub.exe.new")
	if err := os.WriteFile(staged, successor, 0o755); err != nil {
		t.Fatal(err)
	}

	deps := &v5UpgradeDeps{}
	retained, err := deps.RenameAsideBinary(target, staged)
	if err != nil {
		t.Fatal(err)
	}
	gotSuccessor, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSuccessor, successor) {
		t.Fatal("swap did not install successor bytes")
	}
	retainedBytes, err := os.ReadFile(retained)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retainedBytes, prior) {
		t.Fatal("retained artifact differs from exact prior bytes")
	}
	if err := deps.RestoreRetainedBinary(target, retained); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, prior) {
		t.Fatal("restored canonical binary differs from exact prior bytes")
	}
	if stillRetained, err := os.ReadFile(retained); err != nil || !bytes.Equal(stillRetained, prior) {
		t.Fatalf("rollback must preserve retained artifact: bytes=%d err=%v", len(stillRetained), err)
	}
}

func TestV5UpgradeDepsReplacesConsoleSubsystemPrior(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mcphub.exe")
	priorFixture := writeAdmissionPEFixtureWithTag(t, 3, "PRIOR-CUI")
	successorFixture := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "SUCCESSOR-GUI")
	prior, err := os.ReadFile(priorFixture)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := os.ReadFile(successorFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, prior, 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "mcphub.exe.new")
	if err := os.WriteFile(staged, successor, 0o755); err != nil {
		t.Fatal(err)
	}

	retained, err := (&v5UpgradeDeps{}).RenameAsideBinary(target, staged)
	if err != nil {
		t.Fatal(err)
	}
	gotSuccessor, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSuccessor, successor) {
		t.Fatal("swap did not install GUI successor bytes")
	}
	retainedPrior, err := os.ReadFile(retained)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retainedPrior, prior) {
		t.Fatal("retained artifact differs from console-subsystem prior bytes")
	}
}

type realBinaryUpgradeDeps struct {
	*v5UpgradeDeps
}

func (*realBinaryUpgradeDeps) QuiesceTimers(context.Context, string, int) (api.IPCResponse, error) {
	return api.IPCResponse{Result: map[string]any{"still_running": []any{}}}, nil
}

func (*realBinaryUpgradeDeps) ExitGraceful(context.Context, string, int) (api.IPCResponse, error) {
	return api.IPCResponse{}, nil
}

func (*realBinaryUpgradeDeps) ForceKillSupervisor(string) error { return nil }
func (*realBinaryUpgradeDeps) StartSupervisor(string) error     { return nil }

func TestInstallUpgradeRestoresConsolePriorAfterSuccessorReadinessFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mcphub.exe")
	priorFixture := writeAdmissionPEFixtureWithTag(t, 3, "PRIOR-CUI-EXACT")
	successorFixture := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "SUCCESSOR-GUI")
	prior, err := os.ReadFile(priorFixture)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := os.ReadFile(successorFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, prior, 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "mcphub.exe.new")
	if err := os.WriteFile(staged, successor, 0o755); err != nil {
		t.Fatal(err)
	}

	readyCalls := 0
	err = RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: target,
		NewBinary:  staged,
		PipePath:   "test-pipe",
		WithRollbackStopSettlementFence: func(_ context.Context, critical func() error) error {
			return critical()
		},
		WaitSupervisorReady: func(context.Context, time.Duration) error {
			readyCalls++
			if readyCalls == 1 {
				return errors.New("forced successor readiness failure")
			}
			return nil
		},
		Deps: &realBinaryUpgradeDeps{v5UpgradeDeps: &v5UpgradeDeps{}},
	})
	if err == nil || !strings.Contains(err.Error(), "automatic rollback restored") {
		t.Fatalf("error=%v, want successful automatic rollback report", err)
	}
	restored, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(restored, prior) {
		t.Fatal("automatic rollback did not restore exact console-prior bytes")
	}
	retained, globErr := filepath.Glob(target + ".old-*")
	if globErr != nil || len(retained) != 0 {
		t.Fatalf("automatic rollback left retained artifacts: %v, err=%v", retained, globErr)
	}
	temps, globErr := filepath.Glob(target + ".*.tmp")
	if globErr != nil || len(temps) != 0 {
		t.Fatalf("automatic rollback left temp artifacts: %v, err=%v", temps, globErr)
	}
}

func TestV5UpgradeDepsRejectsMalformedPriorBeforeSwap(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mcphub.exe")
	malformed := []byte("not-a-portable-executable")
	if err := os.WriteFile(target, malformed, 0o755); err != nil {
		t.Fatal(err)
	}
	successorFixture := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "SUCCESSOR-GUI")
	successor, err := os.ReadFile(successorFixture)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "mcphub.exe.new")
	if err := os.WriteFile(staged, successor, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = (&v5UpgradeDeps{}).RenameAsideBinary(target, staged)
	if err == nil || !strings.Contains(err.Error(), binaryadmission.WindowsPEFormatErrorID) {
		t.Fatalf("error=%v, want %s", err, binaryadmission.WindowsPEFormatErrorID)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, malformed) {
		t.Fatal("malformed prior rejection mutated canonical target")
	}
	if _, statErr := os.Stat(staged); statErr != nil {
		t.Fatalf("malformed prior rejection consumed staged successor: %v", statErr)
	}
}

func TestRollbackInstallUpgradeRefusesToDeleteCanonicalAlias(t *testing.T) {
	target := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := os.WriteFile(target, []byte("canonical-prior"), 0o755); err != nil {
		t.Fatal(err)
	}
	mock := &fakeUpgradeDeps{}
	err := rollbackInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: target,
		PipePath:   "fake-pipe",
		Deps:       mock,
		WithRollbackStopSettlementFence: func(_ context.Context, critical func() error) error {
			return critical()
		},
	}, target, time.Second, errors.New("forced successor failure"))
	if err == nil || !strings.Contains(err.Error(), "refusing retained-artifact cleanup") {
		t.Fatalf("error=%v, want canonical-alias cleanup refusal", err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "canonical-prior" {
		t.Fatalf("canonical path was deleted or changed: bytes=%q err=%v", got, readErr)
	}
}
