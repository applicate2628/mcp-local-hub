package binaryadmission

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These are PE byte/parser fixtures only. They contain no Go entrypoint,
// original source issuer, native observation, or publication authority.
func privatePublisherPEFixture(t *testing.T, comments, internalName string) []byte {
	t.Helper()
	const version, commit, date = "0.4.60", "dddddddddddddddddddddddddddddddddddddddd", "2026-11-20T00:00:00Z"
	p := writeWindowsPEVersionFixture(t, WindowsArtifactRoleProduct, version, commit, date, false)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	block, _, err := parseVersionInfoBlock(buildVersionResourceFixture(WindowsArtifactRoleProduct, version, commit, date, false), 0)
	if err != nil {
		t.Fatal(err)
	}
	table := &block.children[0].children[0]
	replacements := map[string]string{
		"SpecialBuild":     "mcphub-role-v1;role=publisher-test",
		"InternalName":     internalName,
		"OriginalFilename": "cli.test.exe",
	}
	for n := range table.children {
		if value, ok := replacements[table.children[n].key]; ok {
			next, _, err := parseVersionInfoBlock(versionStringFixture(table.children[n].key, value), 0)
			if err != nil {
				t.Fatal(err)
			}
			table.children[n] = next
		}
	}
	if comments != "" {
		next, _, err := parseVersionInfoBlock(versionStringFixture("Comments", comments), 0)
		if err != nil {
			t.Fatal(err)
		}
		table.children = append(table.children, next)
	}
	resource := rebuildPortableBuildVersionFixture(block)
	raw = append(raw[:0x280:0x280], resource...)
	binary.LittleEndian.PutUint32(raw[0x80+24+132:0x80+24+136], uint32(0x80+len(resource)))
	section := 0x80 + 24 + 0xf0
	binary.LittleEndian.PutUint32(raw[section+8:section+12], uint32(0x80+len(resource)))
	binary.LittleEndian.PutUint32(raw[section+16:section+20], uint32(0x80+len(resource)))
	binary.LittleEndian.PutUint32(raw[0x264:0x268], uint32(len(resource)))
	return raw
}

func TestPrivatePublisherMetadataNeverAdmitsOrdinaryProduct(t *testing.T) {
	epoch := strings.Repeat("e", 64)
	raw := privatePublisherPEFixture(t, "diagnostic\nmcphub-source-v1;sha256="+epoch, "mcphub-cli-publisher-test")
	parsed, err := ParseWindowsPublisherSourceIdentity(raw)
	if err != nil || parsed.Role != WindowsArtifactRolePublisherTest || parsed.SourceEpoch != epoch || parsed.Subsystem != WindowsGUISubsystem {
		t.Fatalf("private metadata=%+v err=%v", parsed, err)
	}
	p := filepath.Join(t.TempDir(), "cli.test.exe")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AdmitWindowsRole(WindowsArtifact{Path: p, Role: WindowsArtifactRoleProduct}); err == nil || !strings.Contains(err.Error(), "disagrees with expected role") {
		t.Fatalf("private bytes admitted as ordinary Product: %v", err)
	}
	if _, err := AdmitWindowsRole(WindowsArtifact{Path: p, Role: WindowsArtifactRolePublisherTest}); err == nil || !strings.Contains(err.Error(), "invalid artifact role") {
		t.Fatalf("private role admitted as distributable artifact: %v", err)
	}
}

func TestPrivatePublisherMetadataRefusesMissingSourceAndWrongIdentity(t *testing.T) {
	epoch := strings.Repeat("e", 64)
	for _, tc := range []struct {
		name, comments, internalName, want string
		subsystem                          uint16
	}{
		{"unmarked", "", "mcphub-cli-publisher-test", "PUBLISHER_SOURCE_EPOCH_UNMARKED", WindowsGUISubsystem},
		{"malformed epoch", "diagnostic\nmcphub-source-v1;sha256=bad", "mcphub-cli-publisher-test", "PUBLISHER_SOURCE_EPOCH_MISMATCH", WindowsGUISubsystem},
		{"duplicate epoch", "diagnostic\nmcphub-source-v1;sha256=" + epoch + "\nmcphub-source-v1;sha256=" + epoch, "mcphub-cli-publisher-test", "PUBLISHER_SOURCE_EPOCH_MISMATCH", WindowsGUISubsystem},
		{"wrong entry name", "diagnostic\nmcphub-source-v1;sha256=" + epoch, "mcphub", "role/name mismatch", WindowsGUISubsystem},
		{"console subsystem", "diagnostic\nmcphub-source-v1;sha256=" + epoch, "mcphub-cli-publisher-test", "role/subsystem", WindowsCUISubsystem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := privatePublisherPEFixture(t, tc.comments, tc.internalName)
			binary.LittleEndian.PutUint16(raw[0x80+24+68:0x80+24+70], tc.subsystem)
			if _, err := ParseWindowsPublisherSourceIdentity(raw); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid private metadata accepted: %v", err)
			}
		})
	}
}
