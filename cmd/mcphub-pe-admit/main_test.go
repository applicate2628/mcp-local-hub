package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func TestRunRequiresExplicitArtifactRole(t *testing.T) {
	original := admitWindowsRoleFn
	admitWindowsRoleFn = func(artifact binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		return artifact, nil
	}
	t.Cleanup(func() { admitWindowsRoleFn = original })
	cli := writePEFixture(t, 3)
	windowless := writePEFixture(t, 2)
	for _, tc := range []struct {
		role string
		path string
		want string
	}{
		{"cli", cli, "role cli: PE subsystem 3"},
		{"windowless", windowless, "role windowless: PE subsystem 2"},
		{"upgrade-prior", windowless, "role upgrade-prior: PE subsystem 2"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := run([]string{tc.role, tc.path}, &out, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("out=%q want %q", out.String(), tc.want)
			}
		})
	}
	var out, stderr bytes.Buffer
	if code := run([]string{cli}, &out, &stderr); code != 2 || !strings.Contains(stderr.String(), "<cli|windowless|upgrade-prior>") {
		t.Fatalf("legacy argv code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunRejectsWrongRole(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := run([]string{"cli", writePEFixture(t, 2)}, &out, &stderr); code != 1 || !strings.Contains(stderr.String(), "E_WINDOWS_PE_SUBSYSTEM") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func writePEFixture(t *testing.T, subsystem uint16) string {
	t.Helper()
	body := make([]byte, 0x200)
	copy(body, "MZ")
	binary.LittleEndian.PutUint32(body[0x3c:0x40], 0x80)
	copy(body[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(body[0x80+20:0x80+22], 0xf0)
	binary.LittleEndian.PutUint16(body[0x80+24:0x80+26], 0x20b)
	binary.LittleEndian.PutUint16(body[0x80+24+68:0x80+24+70], subsystem)
	p := filepath.Join(t.TempDir(), "candidate.exe")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
