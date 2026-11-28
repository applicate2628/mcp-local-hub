//go:build windows

package binaryadmission

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// These are the accepted h463trim runtime-index bytes. No installed product
// or external input is needed when the focused test executes.
const heldRuntimeIndexFixture = "{\"schema\":\"hub-runtime-payload-v1\",\"target\":\"win32-x64\",\"components\":[{\"id\":\"python\",\"descriptor_path\":\"runtimes/python/4147e919195a64ffe56aafd1441992c0d5ea05f0aab7366af5ea16927f2c1658/release.yaml\",\"descriptor_sha256\":\"ca91d794a70d12d013f5b220a6794dddf3d15de22b12cfefa32683e4db6b9542\",\"root\":\"runtimes/python/4147e919195a64ffe56aafd1441992c0d5ea05f0aab7366af5ea16927f2c1658/root\",\"inventory\":\"runtimes/python/4147e919195a64ffe56aafd1441992c0d5ea05f0aab7366af5ea16927f2c1658/inventory.json\",\"tree_sha256\":\"6f57a5cf3a81c1502241d38b5f2cc560e48363baadcd2f9fac8c4c7cad46386d\"},{\"id\":\"uv\",\"descriptor_path\":\"runtimes/uv/c7b81a1be03e06fe898aa599d908a0539d695538b401dd6864481880b60e69b8/release.yaml\",\"descriptor_sha256\":\"1ca29376ab9dc228ce0ff0f20f497cefa6de8f244f6e7562f8937e6a4b9423ac\",\"root\":\"runtimes/uv/c7b81a1be03e06fe898aa599d908a0539d695538b401dd6864481880b60e69b8/root\",\"inventory\":\"runtimes/uv/c7b81a1be03e06fe898aa599d908a0539d695538b401dd6864481880b60e69b8/inventory.json\",\"tree_sha256\":\"9fc0107fcdc83b3991f2c1723ed12ad1da3285a41c4ac56dfcd1674a306a292b\"}]}\n"
const heldRuntimeIndexFixtureSHA256 = "7f5561da5892891bcf622bd831a95500008b7fdca0cf1649982eb32f9e9170bb"

func writeBoundIndexFixture(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func metadataOnlyRuntimeIndex(t *testing.T, path string) *os.File {
	t.Helper()
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
	})
	h, err := canonicalEntryOpenNative(windows.Handle(parent.Fd()), filepath.Base(path), false, 0, windows.FILE_SHARE_READ, windows.FILE_OPEN)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		t.Fatal(errors.Join(errors.New("metadata-only runtime index file unavailable"), windows.CloseHandle(h)))
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func TestHeldCanonicalRuntimeIndexExtent(t *testing.T) {
	raw := []byte(heldRuntimeIndexFixture)
	if len(raw) != 1062 || RuntimeDigest(raw) != heldRuntimeIndexFixtureSHA256 {
		t.Fatal("retained fixture extent or digest differs")
	}
	t.Run("canonical_h463", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "runtime-payload.json")
		writeBoundIndexFixture(t, path, raw)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
		got, err := readHeldCanonicalRuntimeIndex(context.Background(), f)
		if err != nil || !bytes.Equal(got, raw) || RuntimeDigest(got) != heldRuntimeIndexFixtureSHA256 {
			t.Fatalf("held canonical runtime index differs: bytes=%d error=%v", len(got), err)
		}
		if _, err = ParseRuntimePayload(got); err != nil {
			t.Fatal(err)
		}
		t.Log("received exact retained h463 canonical bytes from the bounded original")
	})
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"short_before_content", raw[:len(raw)-1]},
		{"one_extra_before_content", append(append([]byte(nil), raw...), '\n')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime-payload.json")
			writeBoundIndexFixture(t, path, tc.raw)
			f := metadataOnlyRuntimeIndex(t, path)
			var probe [1]byte
			if n, err := f.ReadAt(probe[:], 0); n != 0 || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Fatalf("metadata-only original permitted content IO: n=%d error=%v", n, err)
			}
			got, err := readHeldCanonicalRuntimeIndex(context.Background(), f)
			if len(got) != 0 || err == nil || err.Error() != "original current runtime index extent differs from the canonical schema" {
				t.Fatalf("expected native extent refusal before denied read: bytes=%d error=%v", len(got), err)
			}
			t.Log("extent refused before content IO on a metadata-only original")
		})
	}
}

func boundRuntimeV4Registry(t *testing.T) WindowsProductRegistryV4 {
	t.Helper()
	wire := canonicalEntryRegistryV1()
	wire.SchemaVersion = 4
	wire.IdentityManifest.Schema = "windows-product-identity-v2"
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	const runtime = ",\"runtimePayload\":{\"schema\":\"hub-runtime-payload-v1\",\"releaseDescriptors\":[\"runtimes/python/release.yaml\",\"runtimes/uv/release.yaml\"],\"filename\":\"mcphub-product/runtime-payload.json\"}}"
	raw = append(raw[:len(raw)-1], runtime...)
	registry, err := parseCompiledWindowsProductRegistry(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func boundRuntimeV2Fixture(t *testing.T) (WindowsCanonicalEntrySetTargetsV1, WindowsCanonicalEntryBuildBindingV1, string) {
	t.Helper()
	const epoch = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	comments := "native bounded runtime fixture\nmcphub-source-v1;sha256=" + epoch +
		"\nmcphub-runtime-v1;sha256=" + heldRuntimeIndexFixtureSHA256
	targets, binding := portableCanonicalEntryBuildCommentsFixture(t, comments)
	legacyBytes, err := os.ReadFile(targets.IdentityPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := ParseWindowsProductIdentity(legacyBytes)
	if err != nil {
		t.Fatal(err)
	}
	current := WindowsProductIdentityV2{
		Schema: "windows-product-identity-v2", Role: legacy.Role, Filename: legacy.Filename,
		Subsystem: legacy.Subsystem, Machine: legacy.Machine, Version: legacy.Version,
		Commit: legacy.Commit, BuildDate: legacy.BuildDate, SHA256: legacy.SHA256,
		SourceEpoch: epoch, LauncherSHA256: binding.ExpectedCanonicalEntrySHA256(),
		RuntimePayloadPath: "runtime-payload.json", RuntimePayloadSHA256: heldRuntimeIndexFixtureSHA256,
	}
	raw, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	productDir := filepath.Dir(targets.ProductPath)
	writeBoundIndexFixture(t, filepath.Join(productDir, "windows-product-identity.json"), raw)
	runtimePath := filepath.Join(productDir, current.RuntimePayloadPath)
	writeBoundIndexFixture(t, runtimePath, []byte(heldRuntimeIndexFixture))
	return targets, binding, runtimePath
}

func keepBoundRuntimeOwnerForCleanup(t *testing.T, o *WindowsCanonicalEntryDispatchSetV2) {
	t.Helper()
	if o == nil {
		t.Fatal("expected the original or partial held owner")
	}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Error(err)
		}
	})
}

func TestV2CanonicalRuntimeIndexHeldOriginals(t *testing.T) {
	t.Run("four_originals_and_recheck", func(t *testing.T) {
		targets, binding, runtimePath := boundRuntimeV2Fixture(t)
		o, err := acquireCurrentCanonicalEntryOriginal(context.Background(), targets.CanonicalEntryPath, boundRuntimeV4Registry(t), binding.BuildID())
		keepBoundRuntimeOwnerForCleanup(t, o)
		if err != nil {
			t.Fatal(err)
		}
		got, err := o.DispatchSnapshot()
		identityPath := filepath.Join(filepath.Dir(targets.ProductPath), "windows-product-identity.json")
		if err != nil || got.CanonicalEntry.Path != targets.CanonicalEntryPath || got.Product.Path != targets.ProductPath ||
			got.Identity.Path != identityPath || got.RuntimeIndex.Path != runtimePath ||
			got.RuntimeIndex.SHA256 != heldRuntimeIndexFixtureSHA256 || got.BuildID != binding.BuildID() {
			t.Fatalf("four original file roles or authenticated snapshot differ: %+v error=%v", got, err)
		}
		if err = o.VerifyUnchanged(); err != nil {
			t.Fatal(err)
		}
		if f, err := os.OpenFile(runtimePath, os.O_WRONLY, 0); err == nil {
			f.Close()
			t.Fatal("runtime-index original opened for writing while held")
		}
		if err = o.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = o.DispatchSnapshot(); err == nil {
			t.Fatal("closed original still lent a snapshot")
		}
		t.Log("original launcher, Product, nested identity and runtime index received and rechecked")
	})
	t.Run("same_extent_authenticated_digest_mismatch", func(t *testing.T) {
		targets, binding, runtimePath := boundRuntimeV2Fixture(t)
		original := []byte(heldRuntimeIndexFixture)
		changed, err := ParseRuntimePayload(original)
		if err != nil {
			t.Fatal(err)
		}
		first := changed.Components[0]
		changed.Components[0], err = runtimePayloadComponentV1(changed.Target, first.ID,
			RuntimeDigest([]byte("mismatched descriptor fixture")), first.TreeSHA256)
		if err != nil {
			t.Fatal(err)
		}
		replacement, err := CanonicalRuntimeJSON(changed)
		if err != nil || len(replacement) != len(original) || RuntimeDigest(replacement) == heldRuntimeIndexFixtureSHA256 {
			t.Fatalf("same-size alternate index fixture invalid: bytes=%d error=%v", len(replacement), err)
		}
		if _, err = ParseRuntimePayload(replacement); err != nil {
			t.Fatal(err)
		}
		writeBoundIndexFixture(t, runtimePath, replacement)
		o, err := acquireCurrentCanonicalEntryOriginal(context.Background(), targets.CanonicalEntryPath, boundRuntimeV4Registry(t), binding.BuildID())
		keepBoundRuntimeOwnerForCleanup(t, o)
		if err == nil || err.Error() != "original current runtime index differs from authenticated digest" || o.ready || o.runtimeIndex == nil {
			t.Fatalf("same-size different runtime original did not fail at digest receiving: %v", err)
		}
		if _, err = o.DispatchSnapshot(); err == nil {
			t.Fatal("partial owner with rejected runtime digest lent a snapshot")
		}
		if err = o.Close(); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(runtimePath, runtimePath+".released"); err != nil {
			t.Fatalf("partial runtime original did not release: %v", err)
		}
		t.Log("same-size canonical runtime bytes reached the authenticated digest refusal")
	})
	t.Run("linked_runtime_locator_refused_on_open", func(t *testing.T) {
		targets, binding, runtimePath := boundRuntimeV2Fixture(t)
		if err := os.Remove(runtimePath); err != nil {
			t.Fatal(err)
		}
		// Keep the other three original roles ordinary and single-linked.
		// The unrelated, owned file has exactly the canonical runtime bytes.
		donor := filepath.Join(t.TempDir(), "linked-runtime-donor.json")
		writeBoundIndexFixture(t, donor, []byte(heldRuntimeIndexFixture))
		if err := os.Link(donor, runtimePath); err != nil {
			t.Fatal(err)
		}
		o, err := acquireCurrentCanonicalEntryOriginal(context.Background(), targets.CanonicalEntryPath, boundRuntimeV4Registry(t), binding.BuildID())
		keepBoundRuntimeOwnerForCleanup(t, o)
		if err == nil || !strings.Contains(err.Error(), "canonical entry object is linked, reparse, or wrong kind") ||
			o.ready || o.runtimeIndex != nil {
			t.Fatalf("linked runtime locator did not refuse at the native open: %v", err)
		}
		identityPath := filepath.Join(filepath.Dir(targets.ProductPath), "windows-product-identity.json")
		for _, role := range []struct {
			original *canonicalEntryNativeFile
			path     string
		}{{o.entry, targets.CanonicalEntryPath}, {o.product, targets.ProductPath}, {o.identity, identityPath}} {
			if role.original == nil || role.original.path != role.path {
				t.Fatalf("expected ordinary original acquired before runtime refusal: %s", role.path)
			}
			info, statErr := role.original.file.Stat()
			if statErr != nil || !info.Mode().IsRegular() {
				t.Fatalf("held base original is not ordinary: %s: %v", role.path, statErr)
			}
		}
		if _, err = o.DispatchSnapshot(); err == nil {
			t.Fatal("partially acquired original owner lent a snapshot")
		}
		if err = o.Close(); err != nil {
			t.Fatal(err)
		}
		released := runtimePath + ".released"
		if err = os.Rename(runtimePath, released); err != nil {
			t.Fatalf("failed runtime open left its original guard held: %v", err)
		}
		got, err := os.ReadFile(released)
		if err != nil || !bytes.Equal(got, []byte(heldRuntimeIndexFixture)) {
			t.Fatalf("released fixture does not retain the canonical runtime bytes: %v", err)
		}
		if err = os.Remove(released); err != nil {
			t.Fatal(err)
		}
		got, err = os.ReadFile(donor)
		if err != nil || !bytes.Equal(got, []byte(heldRuntimeIndexFixture)) {
			t.Fatalf("independent owned donor changed after runtime guard release: %v", err)
		}
		t.Log("three ordinary originals acquired; linked runtime locator refused at native open and released")
	})
}

func TestV2RuntimeReaderRetainsPartialCloseFailure(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "owned-runtime-close")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	o := &WindowsCanonicalEntryDispatchSetV2{files: []*os.File{f}}
	first := o.Close()
	if !errors.Is(first, os.ErrClosed) || o.Close() != first {
		t.Fatalf("first actual file close failure not retained: %v", first)
	}
	t.Log("partial V2 owner returned the same actual first close failure")
}

func TestBoundRuntimeIndexRegistryDomain(t *testing.T) {
	legacy, err := json.Marshal(canonicalEntryRegistryV1())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseWindowsProductRegistry(legacy); err != nil {
		t.Fatal(err)
	}
	extension := append(append([]byte(nil), legacy[:len(legacy)-1]...), ",\"runtimePayload\":null}"...)
	if _, err = ParseWindowsProductRegistry(extension); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("V1 typed wire decoder did not refuse the V4-only null field: %v", err)
	}
	for _, tc := range []struct{ name, wire string }{
		{"compiled_missing", ""},
		{"compiled_legacy", base64.StdEncoding.EncodeToString(legacy)},
		{"compiled_schema4_missing_runtime", base64.StdEncoding.EncodeToString(bytes.Replace(legacy, []byte("\"schemaVersion\":3"), []byte("\"schemaVersion\":4"), 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCompiledWindowsProductRegistry(tc.wire); err == nil {
				t.Fatal("invalid compiled V4 domain accepted")
			}
			t.Log("invalid compiled V4 domain refused without legacy fallback")
		})
	}
}
