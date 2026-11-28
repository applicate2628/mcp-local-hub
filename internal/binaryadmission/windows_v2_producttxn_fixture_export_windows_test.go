//go:build windows

package binaryadmission

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"gopkg.in/yaml.v3"
)

// Controlled portable DATA fixture: no executable is run and no native source
// or install receipt is constructed. It uses the actual runtime wire owners.
func v2TxnDonorRuntimeFixture(t *testing.T, productDir string, emptyDirectories ...string) string {
	t.Helper()
	payload := RuntimePayloadV1{Schema: RuntimePayloadSchemaV1, Target: "win32-x64"}
	for _, component := range []string{"python", "uv"} {
		tool, notice := []byte("synthetic runtime "+component), []byte("synthetic notice "+component)
		free := false
		d := RuntimeReleaseDescriptorV1{Schema: RuntimeReleaseSchemaV1, Component: component, Version: "1.0.0", Release: "fixture", Target: payload.Target, RuntimeArchiveSource: RuntimeArchiveSource{ArchiveURL: "https://github.com/astral-sh/fixture", ArchiveSHA256: strings.Repeat("a", 64), ArchiveSize: 1, ArchiveFormat: "zip", Layout: []RuntimeArchiveMapping{{Source: "fixture"}}}, OriginalArtifacts: []RuntimeOriginalArtifact{{ArchiveURL: "https://github.com/astral-sh/fixture", ArchiveSHA256: strings.Repeat("b", 64)}}, OriginalFiles: []RuntimeOriginalFile{{SourcePath: "fixture/NOTICE.txt", Path: "NOTICE.txt", SHA256: RuntimeDigest(notice)}}, Executable: "tool.exe", NativeRequirements: []string{"fixture"}, Licenses: []string{"NOTICE.txt"}}
		if component == "python" {
			d.Version = "3.13.99"
			d.Implementation = "cpython"
			d.Family = "3.13"
			d.ABI = "cp313"
			d.FreeThreaded = &free
		}
		descriptor, err := yaml.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = ParseRuntimeReleaseDescriptor(descriptor); err != nil {
			t.Fatal(err)
		}
		n1, n2 := int64(len(notice)), int64(len(tool))
		inventory := RuntimeInventoryV1{Schema: RuntimeInventorySchemaV1, Target: payload.Target, Objects: []RuntimeObjectV1{{Path: "NOTICE.txt", Kind: "file", Size: &n1, SHA256: RuntimeDigest(notice)}, {Path: "tool.exe", Kind: "file", Size: &n2, SHA256: RuntimeDigest(tool)}}}
		for _, directory := range emptyDirectories {
			inventory.Objects = append(inventory.Objects, RuntimeObjectV1{Path: directory, Kind: "directory"})
		}
		sort.Slice(inventory.Objects, func(i, j int) bool { return inventory.Objects[i].Path < inventory.Objects[j].Path })
		raw, err := CanonicalRuntimeJSON(inventory)
		if err != nil {
			t.Fatal(err)
		}
		id, err := RuntimeComponentContentID(payload.Target, RuntimeDigest(descriptor), RuntimeDigest(raw))
		if err != nil {
			t.Fatal(err)
		}
		prefix := "runtimes/" + component + "/" + id
		c := RuntimeComponentV1{ID: component, DescriptorPath: prefix + "/release.yaml", DescriptorSHA256: RuntimeDigest(descriptor), Root: prefix + "/root", Inventory: prefix + "/inventory.json", TreeSHA256: RuntimeDigest(raw)}
		payload.Components = append(payload.Components, c)
		for name, data := range map[string][]byte{c.DescriptorPath: descriptor, c.Inventory: raw, c.Root + "/NOTICE.txt": notice, c.Root + "/tool.exe": tool} {
			p := filepath.Join(productDir, filepath.FromSlash(name))
			if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		for _, directory := range emptyDirectories {
			if err := os.MkdirAll(filepath.Join(productDir, filepath.FromSlash(c.Root), filepath.FromSlash(directory)), 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	raw, err := CanonicalRuntimeJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(productDir, "runtime-payload.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return RuntimeDigest(raw)
}

func v2TxnDonorEntryFixture(t *testing.T, emptyDirectories ...string) (string, WindowsCanonicalEntryBuildBindingV1, WindowsProductIdentityV2) {
	t.Helper()
	targets, binding := portableCanonicalEntryBuildFixture(t)
	productDir := filepath.Dir(targets.ProductPath)
	runtimeHash := v2TxnDonorRuntimeFixture(t, productDir, emptyDirectories...)
	product, err := os.ReadFile(targets.ProductPath)
	if err != nil {
		t.Fatal(err)
	}
	const version, commit, date = "0.4.41", "dddddddddddddddddddddddddddddddddddddddd", "2026-11-10T12:34:56Z"
	resource := buildVersionResourceFixture(WindowsArtifactRoleProduct, version, commit, date, false)
	block, _, err := parseVersionInfoBlock(resource, 0)
	if err != nil {
		t.Fatal(err)
	}
	comments, err := EncodeWindowsCanonicalEntryCommentsV1("native current fixture\nmcphub-source-v1;sha256="+strings.Repeat("f", 64)+"\nmcphub-runtime-v1;sha256="+runtimeHash, binding)
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
	if err = os.WriteFile(targets.ProductPath, product, 0600); err != nil {
		t.Fatal(err)
	}
	identity := WindowsProductIdentityV2{Schema: "windows-product-identity-v2", Role: WindowsArtifactRoleProduct, Filename: "mcphub.exe", Subsystem: WindowsGUISubsystem, Machine: 0x8664, Version: version, Commit: commit, BuildDate: date, SHA256: canonicalEntrySHA256(product), SourceEpoch: strings.Repeat("f", 64), LauncherSHA256: binding.ExpectedCanonicalEntrySHA256(), RuntimePayloadPath: "runtime-payload.json", RuntimePayloadSHA256: runtimeHash}
	writeV2TxnDonorIdentity(t, productDir, identity)
	// The old root identity remains a real, valid historical decoy. The current
	// receiver may neither consume it nor retry it after a nested read failure.
	return targets.CanonicalEntryPath, binding, identity
}

func writeV2TxnDonorIdentity(t *testing.T, productDir string, identity WindowsProductIdentityV2) {
	t.Helper()
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(productDir, "windows-product-identity.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// TestExportWindowsV2ProductTxnFixture prepares inert bytes for a second,
// explicitly selected CLI test Job. The original Job owns the separate export
// root and keeps its temporary working directory disposable.
func TestExportWindowsV2ProductTxnFixture(t *testing.T) {
	armed, set := os.LookupEnv("MCPHUB_EXPORT_V2_TXN_FIXTURE")
	if !set {
		t.Skip("private V2 transaction fixture export was not selected")
	}
	private := os.Getenv("MCPHUB_V2_TXN_PRIVATE_TEMP")
	if armed != "1" || !filepath.IsAbs(private) || filepath.Clean(private) != private ||
		private != os.TempDir() || private != os.Getenv("TEMP") || private != os.Getenv("TMP") ||
		private != os.Getenv("TMPDIR") || private != os.Getenv("GOTMPDIR") {
		t.Fatal("fixture export requires value 1 and all exact owner-bound private TEMP values")
	}
	exportRoot := os.Getenv("MCPHUB_V2_TXN_EXPORT_ROOT")
	if !filepath.IsAbs(exportRoot) || filepath.Clean(exportRoot) != exportRoot ||
		strings.EqualFold(exportRoot, private) || !strings.EqualFold(filepath.Dir(exportRoot), filepath.Dir(private)) {
		t.Fatal("fixture export root must be a separate sibling of private TEMP")
	}
	for _, root := range []string{private, exportRoot} {
		name, err := windows.UTF16PtrFromString(root)
		if err != nil {
			t.Fatal(err)
		}
		attrs, err := windows.GetFileAttributes(name)
		if err != nil || attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			t.Fatal("fixture export requires ordinary owner-bound directories", err)
		}
	}
	if entries, err := os.ReadDir(exportRoot); err != nil || len(entries) != 0 {
		t.Fatal("fixture export root must already exist and be vacant", err)
	}
	deadline, finite := t.Deadline()
	if !finite {
		t.Fatal("fixture export requires a runner deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	t.Cleanup(cancel)
	entry, binding, _ := v2TxnDonorEntryFixture(t)
	source, err := AdmitWindowsCanonicalEntrySetV2(ctx, entry, binding)
	if source != nil {
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(exportRoot, "v2-producttxn-fixture")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	cohort := filepath.Join(output, "cohort")
	if err := os.Mkdir(cohort, 0700); err != nil {
		t.Fatal(err)
	}
	target, err := CanonicalEntryTargetsV2(filepath.Join(cohort, WindowsCanonicalEntryRelativePathV1))
	if err != nil {
		t.Fatal(err)
	}
	copyMember := func(from, to, digest string) {
		raw, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if RuntimeDigest(raw) != digest {
			t.Fatal("owned synthetic input changed before export")
		}
		if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	copyMember(entry, target.CanonicalEntryPath, snapshot.CanonicalEntry.SHA256)
	for _, object := range snapshot.BundleInventory {
		path := filepath.FromSlash(object.Path)
		if object.Attributes != 0 {
			t.Fatal("synthetic fixture has unsupported portable attributes")
		}
		switch object.Kind {
		case "directory":
			if err := os.MkdirAll(filepath.Join(target.BundlePath, path), 0700); err != nil {
				t.Fatal(err)
			}
		case "file":
			copyMember(filepath.Join(filepath.Dir(snapshot.Product.Path), path), filepath.Join(target.BundlePath, path), object.SHA256)
		default:
			t.Fatal("unexpected synthetic fixture object kind")
		}
	}
	exported, err := AdmitWindowsCanonicalEntrySetV2(ctx, target.CanonicalEntryPath, binding)
	if exported != nil {
		t.Cleanup(func() {
			if err := exported.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	actual, err := exported.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if actual.CanonicalEntry.SHA256 != snapshot.CanonicalEntry.SHA256 ||
		actual.BundleTreeSHA256 != snapshot.BundleTreeSHA256 ||
		actual.RuntimePayload.SHA256 != snapshot.RuntimePayload.SHA256 {
		t.Fatal("exported private V4 cohort differs from the admitted synthetic source")
	}
	raw, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	if err := exported.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "snapshot.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("retained synthetic V4 input snapshot SHA-256 %s", RuntimeDigest(raw))
}
