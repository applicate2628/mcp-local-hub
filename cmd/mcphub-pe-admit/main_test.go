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

func TestRunChecksOptionalExpectedBuildIdentity(t *testing.T) {
	const (
		version   = "0.4.36"
		buildDate = "2026-09-07T16:20:42Z"
	)
	commit := strings.Repeat("a", 40)
	original := admitWindowsRoleFn
	admitWindowsRoleFn = func(artifact binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		artifact.Version = version
		artifact.Commit = commit
		artifact.BuildDate = buildDate
		return artifact, nil
	}
	t.Cleanup(func() { admitWindowsRoleFn = original })
	upgradePrior := writePEFixture(t, 2)

	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{
			name: "matching tuple",
			args: []string{"cli", "candidate.exe", version, commit, buildDate},
			code: 0,
			want: "role cli: PE subsystem 3",
		},
		{
			name: "version mismatch",
			args: []string{"cli", "candidate.exe", "0.4.37", commit, buildDate},
			code: 1,
			want: "VERSIONINFO version mismatch",
		},
		{
			name: "commit mismatch",
			args: []string{"cli", "candidate.exe", version, strings.Repeat("b", 40), buildDate},
			code: 1,
			want: "VERSIONINFO commit mismatch",
		},
		{
			name: "build date mismatch",
			args: []string{"cli", "candidate.exe", version, commit, "2026-09-07T16:20:43Z"},
			code: 1,
			want: "VERSIONINFO build date mismatch",
		},
		{
			name: "matching windowless tuple",
			args: []string{"windowless", "candidate.exe", version, commit, buildDate},
			code: 0,
			want: "role windowless: PE subsystem 2",
		},
		{
			name: "windowless mismatch",
			args: []string{"windowless", "candidate.exe", version, strings.Repeat("b", 40), buildDate},
			code: 1,
			want: "VERSIONINFO commit mismatch",
		},
		{
			name: "partial tuple usage",
			args: []string{"cli", "candidate.exe", version},
			code: 2,
			want: "usage: mcphub-pe-admit",
		},
		{
			name: "upgrade prior rejects expected tuple",
			args: []string{"upgrade-prior", upgradePrior, version, commit, buildDate},
			code: 2,
			want: "expected build identity is supported only for cli and windowless roles",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := run(tc.args, &out, &stderr); code != tc.code {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
			}
			if got := out.String() + stderr.String(); !strings.Contains(got, tc.want) {
				t.Fatalf("output=%q want %q", got, tc.want)
			}
		})
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
