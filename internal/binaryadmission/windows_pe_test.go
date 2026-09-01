package binaryadmission

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendUTF16Z(dst []byte, value string) []byte {
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

func versionFixtureBlock(key string, value []byte, valueWords uint16, blockType uint16, children ...[]byte) []byte {
	block := make([]byte, 6)
	binary.LittleEndian.PutUint16(block[2:4], valueWords)
	binary.LittleEndian.PutUint16(block[4:6], blockType)
	block = appendUTF16Z(block, key)
	block = alignFixture4(block)
	block = append(block, value...)
	block = alignFixture4(block)
	for _, child := range children {
		block = append(block, child...)
		block = alignFixture4(block)
	}
	binary.LittleEndian.PutUint16(block[0:2], uint16(len(block)))
	return block
}

func versionStringFixture(key, value string) []byte {
	raw := appendUTF16Z(nil, value)
	return versionFixtureBlock(key, raw, uint16(len(raw)/2), 1)
}

func buildVersionResourceFixture(role WindowsArtifactRole, version, commit, buildDate string, duplicateProductName bool) []byte {
	filename, internalName, subsystemRole := "mcphub.exe", "mcphub-cli", "cli"
	if role == WindowsArtifactRoleWindowless {
		filename, internalName, subsystemRole = "mcphub-windowless.exe", "mcphub-windowless", "windowless"
	}
	children := [][]byte{
		versionStringFixture("ProductName", "mcp-local-hub"),
		versionStringFixture("ProductVersion", version),
		versionStringFixture("PrivateBuild", "mcphub-build-v1;commit="+commit+";build_date="+buildDate),
		versionStringFixture("SpecialBuild", "mcphub-role-v1;role="+subsystemRole),
		versionStringFixture("InternalName", internalName),
		versionStringFixture("OriginalFilename", filename),
	}
	if duplicateProductName {
		children = append(children, versionStringFixture("ProductName", "mcp-local-hub"))
	}
	stringTable := versionFixtureBlock("040904B0", nil, 0, 1, children...)
	stringInfo := versionFixtureBlock("StringFileInfo", nil, 0, 1, stringTable)
	translation := make([]byte, 4)
	binary.LittleEndian.PutUint16(translation[0:2], 0x0409)
	binary.LittleEndian.PutUint16(translation[2:4], 0x04b0)
	varValue := versionFixtureBlock("Translation", translation, uint16(len(translation)), 0)
	varInfo := versionFixtureBlock("VarFileInfo", nil, 0, 1, varValue)
	fixed := make([]byte, 52)
	binary.LittleEndian.PutUint32(fixed[0:4], 0xFEEF04BD)
	binary.LittleEndian.PutUint32(fixed[4:8], 0x00010000)
	return versionFixtureBlock("VS_VERSION_INFO", fixed, uint16(len(fixed)), 0, stringInfo, varInfo)
}

func writeWindowsPEVersionFixture(t *testing.T, role WindowsArtifactRole, version, commit, buildDate string, duplicateProductName bool) string {
	t.Helper()
	subsystem := WindowsCUISubsystem
	if role == WindowsArtifactRoleWindowless {
		subsystem = WindowsGUISubsystem
	}
	versionResource := buildVersionResourceFixture(role, version, commit, buildDate, duplicateProductName)
	const peOffset, optionalSize, rawOffset, resourceRVA, resourceDataOffset = 0x80, 0xf0, 0x200, 0x1000, 0x80
	data := make([]byte, rawOffset+resourceDataOffset+len(versionResource))
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], peOffset)
	copy(data[peOffset:], "PE\x00\x00")
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
	path := filepath.Join(t.TempDir(), filepath.Base(map[WindowsArtifactRole]string{WindowsArtifactRoleCLI: "mcphub.exe", WindowsArtifactRoleWindowless: "mcphub-windowless.exe"}[role]))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeWindowsPEFixture(t *testing.T, magic, subsystem uint16, mutate func([]byte)) string {
	t.Helper()
	data := make([]byte, 0x200)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], 0x80)
	copy(data[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[0x80+20:0x80+22], 0xf0)
	binary.LittleEndian.PutUint16(data[0x80+24:0x80+26], magic)
	binary.LittleEndian.PutUint16(data[0x80+24+68:0x80+24+70], subsystem)
	if mutate != nil {
		mutate(data)
	}
	path := filepath.Join(t.TempDir(), "candidate.exe")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWindowsPEAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		magic uint16
	}{
		{"PE32", 0x10b},
		{"PE32+", 0x20b},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeWindowsPEFixture(t, tc.magic, WindowsGUISubsystem, nil)
			if err := AdmitWindowsGUI(path); err != nil {
				t.Fatalf("AdmitWindowsGUI: %v", err)
			}
		})
	}

	path := writeWindowsPEFixture(t, 0x20b, 3, nil)
	err := AdmitWindowsGUI(path)
	if err == nil || !strings.Contains(err.Error(), WindowsPESubsystemErrorID) ||
		!strings.Contains(err.Error(), "expected 2, actual 3") {
		t.Fatalf("CUI error=%v, want %s with expected/actual", err, WindowsPESubsystemErrorID)
	}
}

func TestWindowsUpgradePriorAdmission(t *testing.T) {
	for _, subsystem := range []uint16{WindowsGUISubsystem, WindowsCUISubsystem} {
		path := writeWindowsPEFixture(t, 0x20b, subsystem, nil)
		if err := AdmitWindowsUpgradePrior(path); err != nil {
			t.Fatalf("AdmitWindowsUpgradePrior(subsystem=%d): %v", subsystem, err)
		}
	}

	path := writeWindowsPEFixture(t, 0x20b, 9, nil)
	err := AdmitWindowsUpgradePrior(path)
	if err == nil || !strings.Contains(err.Error(), WindowsPESubsystemErrorID) ||
		!strings.Contains(err.Error(), "expected one of [2 3], actual 9") {
		t.Fatalf("unsupported subsystem error=%v, want %s with allowed/actual values", err, WindowsPESubsystemErrorID)
	}
}

func TestWindowsProductPairAdmissionBindsRolesMetadataAndHashes(t *testing.T) {
	cli := writeWindowsPEVersionFixture(t, WindowsArtifactRoleCLI, "0.4.36", "abc1234", "2026-09-01T12:34:56Z", false)
	windowless := writeWindowsPEVersionFixture(t, WindowsArtifactRoleWindowless, "0.4.36", "abc1234", "2026-09-01T12:34:56Z", false)

	pair, err := AdmitWindowsProductPair(
		WindowsArtifact{Path: cli, Role: WindowsArtifactRoleCLI},
		WindowsArtifact{Path: windowless, Role: WindowsArtifactRoleWindowless},
	)
	if err != nil {
		t.Fatalf("AdmitWindowsProductPair: %v", err)
	}
	if pair.CLI.Role != WindowsArtifactRoleCLI || pair.Windowless.Role != WindowsArtifactRoleWindowless {
		t.Fatalf("roles = %q/%q", pair.CLI.Role, pair.Windowless.Role)
	}
	if len(pair.CLI.SHA256) != 64 || len(pair.Windowless.SHA256) != 64 || pair.CLI.SHA256 == pair.Windowless.SHA256 {
		t.Fatalf("hashes = %q/%q", pair.CLI.SHA256, pair.Windowless.SHA256)
	}
}

func TestWindowsProductPairAdmissionRejectsRoleAndMetadataMismatch(t *testing.T) {
	cui := writeWindowsPEFixture(t, 0x20b, WindowsCUISubsystem, nil)
	gui := writeWindowsPEFixture(t, 0x20b, WindowsGUISubsystem, nil)
	mixedCLI := writeWindowsPEVersionFixture(t, WindowsArtifactRoleCLI, "0.4.36", "abc1234", "2026-09-01T12:34:56Z", false)
	mixedWindowless := writeWindowsPEVersionFixture(t, WindowsArtifactRoleWindowless, "0.4.37", "abc1234", "2026-09-01T12:34:56Z", false)

	for _, tc := range []struct {
		name       string
		cli        WindowsArtifact
		windowless WindowsArtifact
	}{
		{
			name:       "subsystems swapped",
			cli:        WindowsArtifact{Path: gui, Role: WindowsArtifactRoleCLI, Version: "v", Commit: "c", BuildDate: "d"},
			windowless: WindowsArtifact{Path: cui, Role: WindowsArtifactRoleWindowless, Version: "v", Commit: "c", BuildDate: "d"},
		},
		{
			name:       "metadata differs",
			cli:        WindowsArtifact{Path: mixedCLI, Role: WindowsArtifactRoleCLI},
			windowless: WindowsArtifact{Path: mixedWindowless, Role: WindowsArtifactRoleWindowless},
		},
		{
			name:       "duplicate role",
			cli:        WindowsArtifact{Path: cui, Role: WindowsArtifactRoleCLI, Version: "v", Commit: "c", BuildDate: "d"},
			windowless: WindowsArtifact{Path: gui, Role: WindowsArtifactRoleCLI, Version: "v", Commit: "c", BuildDate: "d"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := AdmitWindowsProductPair(tc.cli, tc.windowless); err == nil || !strings.Contains(err.Error(), WindowsProductPairErrorID) {
				t.Fatalf("error=%v, want %s", err, WindowsProductPairErrorID)
			}
		})
	}
}

func TestWindowsProductPairAdmissionDerivesStrictIdentityFromBothArtifacts(t *testing.T) {
	cli := writeWindowsPEVersionFixture(t, WindowsArtifactRoleCLI, "0.4.36-rc.1+qa", "abc1234", "2026-09-01T12:34:56Z", false)
	windowless := writeWindowsPEVersionFixture(t, WindowsArtifactRoleWindowless, "0.4.36-rc.1+qa", "abc1234", "2026-09-01T12:34:56Z", false)
	pair, err := AdmitWindowsProductPair(
		WindowsArtifact{Path: cli, Role: WindowsArtifactRoleCLI},
		WindowsArtifact{Path: windowless, Role: WindowsArtifactRoleWindowless},
	)
	if err != nil {
		t.Fatalf("AdmitWindowsProductPair: %v", err)
	}
	if pair.CLI.Version != "0.4.36-rc.1+qa" || pair.CLI.Commit != "abc1234" || pair.CLI.BuildDate != "2026-09-01T12:34:56Z" {
		t.Fatalf("derived CLI identity = %+v", pair.CLI)
	}
	if pair.Windowless.Version != pair.CLI.Version || pair.Windowless.Commit != pair.CLI.Commit || pair.Windowless.BuildDate != pair.CLI.BuildDate {
		t.Fatalf("pair identity mismatch = %+v", pair)
	}
}

func TestWindowsProductPairAdmissionRejectsDuplicateAndMixedArtifactIdentity(t *testing.T) {
	validCLI := writeWindowsPEVersionFixture(t, WindowsArtifactRoleCLI, "0.4.36", "abc1234", "2026-09-01T12:34:56Z", false)
	duplicateWindowless := writeWindowsPEVersionFixture(t, WindowsArtifactRoleWindowless, "0.4.36", "abc1234", "2026-09-01T12:34:56Z", true)
	if _, err := AdmitWindowsProductPair(WindowsArtifact{Path: validCLI, Role: WindowsArtifactRoleCLI}, WindowsArtifact{Path: duplicateWindowless, Role: WindowsArtifactRoleWindowless}); err == nil || !strings.Contains(err.Error(), WindowsProductPairErrorID) {
		t.Fatalf("duplicate resource error = %v", err)
	}
	mixedWindowless := writeWindowsPEVersionFixture(t, WindowsArtifactRoleWindowless, "0.4.36", "def5678", "2026-09-01T12:34:56Z", false)
	if _, err := AdmitWindowsProductPair(WindowsArtifact{Path: validCLI, Role: WindowsArtifactRoleCLI}, WindowsArtifact{Path: mixedWindowless, Role: WindowsArtifactRoleWindowless}); err == nil || !strings.Contains(err.Error(), "identity differs") {
		t.Fatalf("mixed build error = %v", err)
	}
}

func TestWindowsProductPairAdmissionIgnoresCallerMetadataAssertions(t *testing.T) {
	cli := writeWindowsPEVersionFixture(t, WindowsArtifactRoleCLI, "0.4.36", "abc1234", "2026-09-01T12:34:56Z", false)
	windowless := writeWindowsPEVersionFixture(t, WindowsArtifactRoleWindowless, "0.4.36", "abc1234", "2026-09-01T12:34:56Z", false)
	pair, err := AdmitWindowsProductPair(
		WindowsArtifact{Path: cli, Role: WindowsArtifactRoleCLI, Version: "forged", Commit: "forged", BuildDate: "forged"},
		WindowsArtifact{Path: windowless, Role: WindowsArtifactRoleWindowless, Version: "forged", Commit: "forged", BuildDate: "forged"},
	)
	if err != nil {
		t.Fatalf("caller assertions affected admission: %v", err)
	}
	if pair.CLI.Version != "0.4.36" || bytes.Equal([]byte(pair.CLI.Commit), []byte("forged")) {
		t.Fatalf("caller assertion survived: %+v", pair.CLI)
	}
}

func TestWindowsPEMalformed(t *testing.T) {
	base := writeWindowsPEFixture(t, 0x20b, WindowsGUISubsystem, nil)
	valid, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"truncated DOS", valid[:32]},
		{"missing MZ", append([]byte("NO"), valid[2:]...)},
		{"offset before DOS end", func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(b[0x3c:0x40], 32)
			return b
		}()},
		{"offset beyond bound", func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(b[0x3c:0x40], maxWindowsPEHeaderOffset+1)
			return b
		}()},
		{"truncated PE", valid[:0x80+10]},
		{"missing signature", func() []byte { b := append([]byte(nil), valid...); copy(b[0x80:], "PX\x00\x00"); return b }()},
		{"small optional header", func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(b[0x80+20:0x80+22], 60)
			return b
		}()},
		{"unsupported magic", func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(b[0x80+24:0x80+26], 0x999)
			return b
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate.exe")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			err := AdmitWindowsGUI(path)
			if err == nil || !strings.Contains(err.Error(), WindowsPEFormatErrorID) {
				t.Fatalf("error=%v, want %s", err, WindowsPEFormatErrorID)
			}
		})
	}
}

type boundedReaderAt struct {
	data    []byte
	maxRead int
	reads   int
}

func (r *boundedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	if len(p) > r.maxRead {
		return 0, errors.New("read exceeded bound")
	}
	if off < 0 || off >= int64(len(r.data)) {
		return 0, os.ErrInvalid
	}
	n := copy(p, r.data[off:])
	if n != len(p) {
		return n, errors.New("short read")
	}
	return n, nil
}

func TestWindowsPEBoundedRead(t *testing.T) {
	data := make([]byte, 0x200)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], 0x80)
	copy(data[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[0x80+20:0x80+22], 0xf0)
	binary.LittleEndian.PutUint16(data[0x80+24:0x80+26], 0x20b)
	binary.LittleEndian.PutUint16(data[0x80+24+68:0x80+24+70], WindowsGUISubsystem)
	r := &boundedReaderAt{data: data, maxRead: windowsPEMaxSingleRead}
	subsystem, err := ReadWindowsPESubsystem(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if subsystem != WindowsGUISubsystem {
		t.Fatalf("subsystem=%d", subsystem)
	}
	if r.reads != 2 {
		t.Fatalf("read calls=%d, want exactly 2 bounded reads", r.reads)
	}
}
