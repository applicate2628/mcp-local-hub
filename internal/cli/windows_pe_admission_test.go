//go:build windows

package cli

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
