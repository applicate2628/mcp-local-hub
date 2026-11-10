package binaryadmission

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func rebuildPortableBuildVersionFixture(block versionInfoBlock) []byte {
	children := make([][]byte, 0, len(block.children))
	for _, child := range block.children {
		children = append(children, rebuildPortableBuildVersionFixture(child))
	}
	return versionFixtureBlock(block.key, block.value, block.valueWords, block.blockType, children...)
}

func portableCanonicalEntryBuildFixture(t *testing.T) (WindowsCanonicalEntrySetTargetsV1, WindowsCanonicalEntryBuildBindingV1) {
	return portableCanonicalEntryBuildCommentsFixture(t, "portable build fixture\r\n\nmcphub-source-v1;sha256="+strings.Repeat("f", 64))
}

func portableCanonicalEntryBuildCommentsFixture(t *testing.T, existingComments string) (WindowsCanonicalEntrySetTargetsV1, WindowsCanonicalEntryBuildBindingV1) {
	t.Helper()
	targets, err := CanonicalEntryTargetsV1(filepath.Join(t.TempDir(), "mcphub.exe"))
	if err != nil {
		t.Fatal(err)
	}
	entryFixture := writeWindowsPEFixture(t, 0x20b, WindowsCUISubsystem, func(raw []byte) { binary.LittleEndian.PutUint16(raw[0x84:0x86], 0x8664) })
	entry, err := os.ReadFile(entryFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(targets.CanonicalEntryPath, entry, 0o600); err != nil {
		t.Fatal(err)
	}
	binding, err := NewWindowsCanonicalEntryBuildBindingV1(strings.Repeat("a", 32), canonicalEntrySHA256(entry), WindowsCanonicalEntryReverseProjectionV1)
	if err != nil {
		t.Fatal(err)
	}
	const version, commit, date = "0.4.41", "dddddddddddddddddddddddddddddddddddddddd", "2026-11-10T12:34:56Z"
	fixture := writeWindowsPEVersionFixture(t, WindowsArtifactRoleProduct, version, commit, date, false)
	product, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	resource := buildVersionResourceFixture(WindowsArtifactRoleProduct, version, commit, date, false)
	block, _, err := parseVersionInfoBlock(resource, 0)
	if err != nil {
		t.Fatal(err)
	}
	comments, err := EncodeWindowsCanonicalEntryCommentsV1(existingComments, binding)
	if err != nil {
		t.Fatal(err)
	}
	comment, _, err := parseVersionInfoBlock(versionStringFixture("Comments", comments), 0)
	if err != nil {
		t.Fatal(err)
	}
	block.children[0].children[0].children = append(block.children[0].children[0].children, comment)
	resource = rebuildPortableBuildVersionFixture(block)
	product = append(product[:0x280:0x280], resource...)
	binary.LittleEndian.PutUint32(product[0x80+24+132:0x80+24+136], uint32(0x80+len(resource)))
	section := 0x80 + 24 + 0xf0
	binary.LittleEndian.PutUint32(product[section+8:section+12], uint32(0x80+len(resource)))
	binary.LittleEndian.PutUint32(product[section+16:section+20], uint32(0x80+len(resource)))
	binary.LittleEndian.PutUint32(product[0x264:0x268], uint32(len(resource)))
	if err = os.Mkdir(filepath.Dir(targets.ProductPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(targets.ProductPath, product, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := WindowsProductIdentityV1{"windows-product-identity-v1", WindowsArtifactRoleProduct, "mcphub.exe", WindowsGUISubsystem, 0x8664, version, commit, date, canonicalEntrySHA256(product)}
	raw, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(targets.IdentityPath, append(raw, '\r', '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return targets, binding
}

func TestPortableCanonicalEntryBuildObservationDataAndIndependentBinding(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("host explicitly unsupported for native file-attribute build observations")
	}
	targets, binding := portableCanonicalEntryBuildFixture(t)
	observation, err := CheckWindowsCanonicalEntryBuildV1(context.Background(), targets.CanonicalEntryPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	if observation.SourceEpoch != strings.Repeat("f", 64) || observation.EntrySet.ProductArtifact.Path != targets.ProductPath || observation.EntrySet.CanonicalEntry.SHA256 != binding.ExpectedCanonicalEntrySHA256() || observation.EntrySet.ProductArtifact.Commit != strings.Repeat("d", 40) {
		t.Fatalf("build observation=%+v", observation)
	}
	wrong, err := NewWindowsCanonicalEntryBuildBindingV1(strings.Repeat("b", 32), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = CheckWindowsCanonicalEntryBuildV1(context.Background(), targets.CanonicalEntryPath, wrong); err == nil {
		t.Fatal("observed files promoted into expected issuer binding")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = CheckWindowsCanonicalEntryBuildV1(ctx, targets.CanonicalEntryPath, binding); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
	if runtime.GOOS != "windows" {
		owner, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
		if err == nil || owner != nil {
			t.Fatal("portable DATA became unsupported-host native admission")
		}
	}
}

func TestPortableCanonicalEntryBuildObservationRefusesHardlinkAlias(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("host explicitly unsupported for native file-attribute build observations")
	}
	targets, binding := portableCanonicalEntryBuildFixture(t)
	if err := os.Link(targets.IdentityPath, targets.IdentityPath+".alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckWindowsCanonicalEntryBuildV1(context.Background(), targets.CanonicalEntryPath, binding); err == nil {
		t.Fatal("hardlink alias accepted as a unique ordinary build artifact")
	}
}

func TestPortableCanonicalEntryBuildObservationOptionalSourceEpoch(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("host explicitly unsupported for native file-attribute build observations")
	}
	for _, tc := range []struct {
		name      string
		comments  string
		wantError bool
	}{
		{"absent_no_expected_epoch", "portable build fixture\r\n", false},
		{"malformed_present", "portable build fixture\nmcphub-source-v1;sha256=invalid", true},
		{"unknown_source_version", "portable build fixture\nmcphub-source-v2;sha256=" + strings.Repeat("f", 64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targets, binding := portableCanonicalEntryBuildCommentsFixture(t, tc.comments)
			observation, err := CheckWindowsCanonicalEntryBuildV1(context.Background(), targets.CanonicalEntryPath, binding)
			if tc.wantError {
				if err == nil {
					t.Fatal("present malformed/reserved source metadata was treated as absence")
				}
				return
			}
			if err != nil || observation.SourceEpoch != "" {
				t.Fatalf("unmarked optional source epoch=%q error=%v", observation.SourceEpoch, err)
			}
		})
	}
}
