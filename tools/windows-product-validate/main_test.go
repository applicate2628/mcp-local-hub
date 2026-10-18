package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureVersion   = "0.4.36-rc.1+qa"
	fixtureCommit    = "0123456789abcdef0123456789abcdef01234567"
	fixtureBuildDate = "2026-09-11T12:34:56Z"
	fixtureMachine   = uint16(0x8664)
)

func appendFixtureUTF16Z(dst []byte, value string) []byte {
	for _, r := range value {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(r))
	}
	return binary.LittleEndian.AppendUint16(dst, 0)
}

func alignFixture4(dst []byte) []byte {
	for len(dst)%4 != 0 {
		dst = append(dst, 0)
	}
	return dst
}

func fixtureVersionBlock(key string, value []byte, valueWords, blockType uint16, children ...[]byte) []byte {
	block := make([]byte, 6)
	binary.LittleEndian.PutUint16(block[2:4], valueWords)
	binary.LittleEndian.PutUint16(block[4:6], blockType)
	block = alignFixture4(appendFixtureUTF16Z(block, key))
	block = alignFixture4(append(block, value...))
	for _, child := range children {
		block = alignFixture4(append(block, child...))
	}
	binary.LittleEndian.PutUint16(block[0:2], uint16(len(block)))
	return block
}

func fixtureVersionString(key, value string) []byte {
	raw := appendFixtureUTF16Z(nil, value)
	return fixtureVersionBlock(key, raw, uint16(len(raw)/2), 1)
}

func productVersionResourceFixture() []byte {
	children := [][]byte{
		fixtureVersionString("ProductName", "mcp-local-hub"),
		fixtureVersionString("ProductVersion", fixtureVersion),
		fixtureVersionString("PrivateBuild", "mcphub-build-v1;commit="+fixtureCommit+";build_date="+fixtureBuildDate),
		fixtureVersionString("SpecialBuild", "mcphub-role-v1;role=product"),
		fixtureVersionString("InternalName", "mcphub"),
		fixtureVersionString("OriginalFilename", "mcphub.exe"),
	}
	stringTable := fixtureVersionBlock("040904B0", nil, 0, 1, children...)
	stringInfo := fixtureVersionBlock("StringFileInfo", nil, 0, 1, stringTable)
	translation := make([]byte, 4)
	binary.LittleEndian.PutUint16(translation[0:2], 0x0409)
	binary.LittleEndian.PutUint16(translation[2:4], 0x04b0)
	varValue := fixtureVersionBlock("Translation", translation, uint16(len(translation)), 0)
	varInfo := fixtureVersionBlock("VarFileInfo", nil, 0, 1, varValue)
	fixed := make([]byte, 52)
	binary.LittleEndian.PutUint32(fixed[0:4], 0xFEEF04BD)
	binary.LittleEndian.PutUint32(fixed[4:8], 0x00010000)
	return fixtureVersionBlock("VS_VERSION_INFO", fixed, uint16(len(fixed)), 0, stringInfo, varInfo)
}

func writeProductPEFixture(t *testing.T, filename string) ([]byte, string) {
	t.Helper()
	versionResource := productVersionResourceFixture()
	const peOffset, optionalSize, rawOffset, resourceRVA, resourceDataOffset = 0x80, 0xf0, 0x200, 0x1000, 0x80
	data := make([]byte, rawOffset+resourceDataOffset+len(versionResource))
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], peOffset)
	copy(data[peOffset:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[peOffset+4:peOffset+6], fixtureMachine)
	binary.LittleEndian.PutUint16(data[peOffset+6:peOffset+8], 1)
	binary.LittleEndian.PutUint16(data[peOffset+20:peOffset+22], optionalSize)
	optional := data[peOffset+24 : peOffset+24+optionalSize]
	binary.LittleEndian.PutUint16(optional[0:2], 0x20b)
	binary.LittleEndian.PutUint16(optional[68:70], 2)
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

	path := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data, path
}

func fixtureArgs(candidate string) []string {
	return []string{
		"--candidate", candidate,
		"--expected-version", fixtureVersion,
		"--expected-commit", fixtureCommit,
		"--expected-build-date", fixtureBuildDate,
	}
}

func TestRunEmitsAdmittedWindowsProductIdentity(t *testing.T) {
	data, candidate := writeProductPEFixture(t, "mcphub.exe")
	wantHashBytes := sha256.Sum256(data)
	wantHash := hex.EncodeToString(wantHashBytes[:])
	want := `{"schema":"windows-product-identity-v1","role":"product","filename":"mcphub.exe","subsystem":2,"machine":34404,"version":"0.4.36-rc.1+qa","commit":"0123456789abcdef0123456789abcdef01234567","build_date":"2026-09-11T12:34:56Z","sha256":"` + wantHash + `"}` + "\n"
	var stdout, stderr bytes.Buffer

	if code := run(fixtureArgs(candidate), &stdout, &stderr); code != 0 {
		t.Fatalf("run exit=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want stdout=%q and empty stderr", stdout.String(), stderr.String(), want)
	}
}

func TestRunRejectsMalformedExpectedCommitAsUsageBeforeCandidateAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		commit string
	}{
		{name: "empty", commit: ""},
		{name: "39 characters", commit: strings.Repeat("a", 39)},
		{name: "41 characters", commit: strings.Repeat("a", 41)},
		{name: "uppercase", commit: strings.Repeat("A", 40)},
		{name: "non-hex", commit: strings.Repeat("a", 39) + "g"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := filepath.Join(t.TempDir(), "mcphub.exe")
			args := fixtureArgs(candidate)
			args[5] = tc.commit
			var stdout, stderr bytes.Buffer

			if code := run(args, &stdout, &stderr); code != 2 {
				t.Fatalf("run exit=%d, want usage exit 2 before candidate admission; stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage:") || strings.Contains(stderr.String(), "E_WINDOWS_PE_FORMAT") {
				t.Fatalf("stdout=%q stderr=%q, want empty stdout and pre-admission usage diagnostic", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunClassifiesUsageAndValidationFailures(t *testing.T) {
	t.Run("missing required flag is usage", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run(nil, &stdout, &stderr); code != 2 {
			t.Fatalf("run exit=%d, want 2; stderr=%q", code, stderr.String())
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage:") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	t.Run("malformed candidate is admission failure", func(t *testing.T) {
		candidate := filepath.Join(t.TempDir(), "mcphub.exe")
		if err := os.WriteFile(candidate, []byte("not-a-pe"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := run(fixtureArgs(candidate), &stdout, &stderr); code != 1 {
			t.Fatalf("run exit=%d, want 1; stderr=%q", code, stderr.String())
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "E_WINDOWS_PE_FORMAT") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	t.Run("wrong filename is validation failure", func(t *testing.T) {
		_, candidate := writeProductPEFixture(t, "other.exe")
		var stdout, stderr bytes.Buffer
		if code := run(fixtureArgs(candidate), &stdout, &stderr); code != 1 {
			t.Fatalf("run exit=%d, want 1; stderr=%q", code, stderr.String())
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "filename") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	t.Run("tuple mismatch is validation failure", func(t *testing.T) {
		_, candidate := writeProductPEFixture(t, "mcphub.exe")
		args := fixtureArgs(candidate)
		args[3] = "0.4.37"
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 1 {
			t.Fatalf("run exit=%d, want 1; stderr=%q", code, stderr.String())
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "build identity mismatch") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})
}
