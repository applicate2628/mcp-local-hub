//go:build windows

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mcp-local-hub/internal/runtimeidentity"
)

type windowsCanonicalNativeActorFixtureInput struct {
	EntrySet              windowsCanonicalEntryFixtureInput `json:"entry_set"`
	RuntimeLibrarySHA256  string                            `json:"runtime_library_sha256"`
	RuntimeIdentitySHA256 string                            `json:"runtime_identity_sha256"`
}

type windowsCanonicalNativeActorFixture struct {
	*windowsCanonicalEntryFixture
	input           windowsCanonicalNativeActorFixtureInput
	runtimeIdentity runtimeidentity.WindowsRuntimeLibraryIdentity
	libraryBytes    []byte
	identityBytes   []byte
}

// The test receiver and the production actor are distinct build operands.
// Native replay must never launch the receiver's TestMain as a supervisor.
func loadWindowsCanonicalNativeActorFixture(t *testing.T) *windowsCanonicalNativeActorFixture {
	t.Helper()
	const inputEnv = "MCPHUB_CANONICAL_NATIVE_ACTOR_FIXTURE"
	required, requiredSet := os.LookupEnv("MCPHUB_CANONICAL_ENTRY_FIXTURE_REQUIRED")
	if requiredSet && required != "1" {
		t.Fatal("MCPHUB_CANONICAL_ENTRY_FIXTURE_REQUIRED must be 1 when supplied")
	}
	inputPath, inputSet := os.LookupEnv(inputEnv)
	_, executingFixtureSet := os.LookupEnv("MCPHUB_CANONICAL_ENTRY_FIXTURE")
	if !inputSet && !requiredSet && !executingFixtureSet {
		t.Skip("requires an explicitly supplied qualified native Product actor")
	}
	if !cleanAbsolutePath(inputPath) {
		t.Fatalf("%s requires a separate absolute native actor fixture input", inputEnv)
	}
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	var input windowsCanonicalNativeActorFixtureInput
	decodeCanonicalNativeActorJSON(t, raw, &input)
	if !validLowerHexSHA256(input.RuntimeLibrarySHA256) || !validLowerHexSHA256(input.RuntimeIdentitySHA256) {
		t.Fatal("native actor fixture requires both exact runtime companion hashes")
	}
	original := receiveWindowsCanonicalEntryFixture(t, input.EntrySet)
	requireCanonicalGUIReplayStateIsolation(t, original.Targets.ProductPath)
	dir := filepath.Dir(original.Targets.ProductPath)
	f := &windowsCanonicalNativeActorFixture{windowsCanonicalEntryFixture: original, input: input}
	f.libraryBytes = readCanonicalNativeActorBytes(t, filepath.Join(dir, "mcphub-owner.dll"), input.RuntimeLibrarySHA256, 64<<20)
	f.identityBytes = readCanonicalNativeActorBytes(t, filepath.Join(dir, "mcphub-owner-identity.json"), input.RuntimeIdentitySHA256, 16<<10)
	decodeCanonicalNativeActorJSON(t, f.identityBytes, &f.runtimeIdentity)
	if f.runtimeIdentity.Schema != runtimeidentity.WindowsRuntimeLibraryIdentitySchemaV1 ||
		f.runtimeIdentity.Filename != "mcphub-owner.dll" ||
		f.runtimeIdentity.SourceCommit != original.Observation.EntrySet.ProductArtifact.Commit ||
		f.runtimeIdentity.Machine != original.Observation.EntrySet.Machine ||
		f.runtimeIdentity.SHA256 != input.RuntimeLibrarySHA256 ||
		!runtimeidentity.IsWindowsProcessOwnerBorrowExportProfileV1(f.runtimeIdentity.Exports) {
		t.Fatal("native actor runtime identity differs from its qualified Product and exact profile")
	}
	f.verifyRuntimeLibrary(t, filepath.Join(dir, "mcphub-owner.dll"))
	return f
}

func decodeCanonicalNativeActorJSON(t *testing.T, raw []byte, value any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatalf("native actor input must be one JSON object: %v", err)
	}
}

func readCanonicalNativeActorBytes(t *testing.T, path, expectedSHA256 string, maximum int64) []byte {
	t.Helper()
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() <= 0 || before.Size() > maximum {
		t.Fatalf("native actor companion is not an ordinary bounded file: %s: %v", path, err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, statErr := file.Stat()
	raw, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if err := errors.Join(statErr, readErr, closeErr); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, opened) || !os.SameFile(opened, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		int64(len(raw)) != before.Size() || fmt.Sprintf("%x", sha256.Sum256(raw)) != expectedSHA256 {
		t.Fatalf("native actor companion changed or differs from its issued digest: %s: %v", path, err)
	}
	return raw
}

func (f *windowsCanonicalNativeActorFixture) verifyRuntimeLibrary(t *testing.T, path string) {
	t.Helper()
	actual, err := runtimeidentity.AdmitWindowsRuntimeLibrary(path, f.runtimeIdentity.Machine, f.runtimeIdentity.Exports, f.runtimeIdentity.SourceCommit)
	if err != nil || !reflect.DeepEqual(actual, f.runtimeIdentity) {
		t.Fatalf("native actor DLL receiving differs from its original identity: %v", err)
	}
}

