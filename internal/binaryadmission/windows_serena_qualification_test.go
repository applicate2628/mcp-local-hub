package binaryadmission

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// This is a byte-parser fixture. It does not issue source authority or claim
// that a native process, package preparation, or namespace was observed.
func serenaQualificationPEFixture(t *testing.T, fields map[string]string) (string, []byte) {
	t.Helper()
	const version, commit, date = "0.0.0-serena-qualification", "dddddddddddddddddddddddddddddddddddddddd", "2026-11-14T18:00:00Z"
	path := writeWindowsPEVersionFixture(t, WindowsArtifactRoleSerenaQualification, version, commit, date, false)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	resource := buildVersionResourceFixture(WindowsArtifactRoleSerenaQualification, version, commit, date, false)
	block, _, err := parseVersionInfoBlock(resource, 0)
	if err != nil {
		t.Fatal(err)
	}
	stringsBlock := &block.children[0].children[0]
	values := map[string]string{"Comments": "Serena qualification\nmcphub-source-v1;sha256=" + strings.Repeat("f", 64)}
	for key, value := range fields {
		values[key] = value
	}
	for key, value := range values {
		replacement, _, err := parseVersionInfoBlock(versionStringFixture(key, value), 0)
		if err != nil {
			t.Fatal(err)
		}
		replaced := false
		for i := range stringsBlock.children {
			if stringsBlock.children[i].key == key {
				stringsBlock.children[i], replaced = replacement, true
				break
			}
		}
		if !replaced {
			stringsBlock.children = append(stringsBlock.children, replacement)
		}
	}
	resource = rebuildPortableBuildVersionFixture(block)
	raw = append(raw[:0x280:0x280], resource...)
	binary.LittleEndian.PutUint32(raw[0x80+24+132:0x80+24+136], uint32(0x80+len(resource)))
	section := 0x80 + 24 + 0xf0
	binary.LittleEndian.PutUint32(raw[section+8:section+12], uint32(0x80+len(resource)))
	binary.LittleEndian.PutUint32(raw[section+16:section+20], uint32(0x80+len(resource)))
	binary.LittleEndian.PutUint32(raw[0x264:0x268], uint32(len(resource)))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func TestSerenaQualificationPublisherIdentityIsDistinctFromProduct(t *testing.T) {
	path, raw := serenaQualificationPEFixture(t, nil)
	identity, err := ParseWindowsPublisherSourceIdentity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Role != WindowsArtifactRoleSerenaQualification || identity.SourceEpoch != strings.Repeat("f", 64) ||
		identity.Version != "0.0.0-serena-qualification" || identity.Commit != strings.Repeat("d", 40) ||
		identity.BuildDate != "2026-11-14T18:00:00Z" || identity.Subsystem != WindowsGUISubsystem || identity.Machine != 0x8664 {
		t.Fatalf("qualification identity = %+v", identity)
	}
	for _, role := range []WindowsArtifactRole{WindowsArtifactRoleProduct, WindowsArtifactRoleCLI, WindowsArtifactRoleWindowless, WindowsArtifactRoleSerenaQualification} {
		t.Run(string(role), func(t *testing.T) {
			if _, err := AdmitWindowsRole(WindowsArtifact{Path: path, Role: role}); err == nil {
				t.Fatal("qualification test binary admitted by ordinary product admission")
			}
		})
	}
}

func TestSerenaQualificationPublisherIdentityRejectsMismatchedMetadata(t *testing.T) {
	for name, fields := range map[string]map[string]string{
		"product internal name": {"InternalName": "mcphub"},
		"product filename":      {"OriginalFilename": "mcphub.exe"},
		"product role":          {"SpecialBuild": "mcphub-role-v1;role=product"},
		"unknown role":          {"SpecialBuild": "mcphub-role-v1;role=serena-qualification-extra"},
		"missing source":        {"Comments": "Serena qualification"},
		"duplicate source":      {"Comments": "source\nmcphub-source-v1;sha256=" + strings.Repeat("f", 64) + "\nmcphub-source-v1;sha256=" + strings.Repeat("f", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			_, raw := serenaQualificationPEFixture(t, fields)
			if _, err := ParseWindowsPublisherSourceIdentity(raw); err == nil {
				t.Fatal("invalid qualification metadata accepted")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"console subsystem", func(raw []byte) { binary.LittleEndian.PutUint16(raw[0x80+24+68:0x80+24+70], WindowsCUISubsystem) }},
		{"DLL", func(raw []byte) { binary.LittleEndian.PutUint16(raw[0x80+22:0x80+24], windowsPEDLLCharacteristic) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, raw := serenaQualificationPEFixture(t, nil)
			tc.mutate(raw)
			if _, err := ParseWindowsPublisherSourceIdentity(raw); err == nil {
				t.Fatal("non-process qualification image accepted")
			}
		})
	}
}
