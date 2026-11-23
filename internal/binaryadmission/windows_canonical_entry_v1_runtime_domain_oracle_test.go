package binaryadmission

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestPortableCanonicalEntryBuildV1RejectsRuntimeMarkedProduct(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("host explicitly unsupported for native file-attribute build observations")
	}

	legacyTargets, legacyBinding := portableCanonicalEntryBuildFixture(t)
	legacy, err := CheckWindowsCanonicalEntryBuildV1(t.Context(), legacyTargets.CanonicalEntryPath, legacyBinding)
	if err != nil {
		t.Fatalf("original legacy fixture was refused: %v", err)
	}
	if legacy.SourceEpoch != strings.Repeat("f", 64) || legacy.EntrySet.ProductArtifact.Path != legacyTargets.ProductPath {
		t.Fatal("original legacy fixture did not produce a matching V1 observation")
	}
	legacyIdentity, err := ParseWindowsProductIdentity(legacy.EntrySet.IdentityBytes)
	if err != nil {
		t.Fatalf("parse original V1 identity: %v", err)
	}

	runtimeSHA := strings.Repeat("e", 64)
	comments := "portable build fixture\r\n\nmcphub-source-v1;sha256=" + legacy.SourceEpoch +
		"\nmcphub-runtime-v1;sha256=" + runtimeSHA
	markedTargets, markedBinding := portableCanonicalEntryBuildCommentsFixture(t, comments)
	if markedBinding != legacyBinding {
		t.Fatal("runtime-marked fixture changed the launcher binding")
	}
	product, err := os.ReadFile(markedTargets.ProductPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := readWindowsBuildIdentityAt(bytes.NewReader(product), int64(len(product)))
	if err != nil {
		t.Fatalf("parse runtime-marked synthetic PE: %v", err)
	}
	composition, err := parseWindowsBuildCommentsComposition(metadata.Comments)
	if err != nil || composition.sourceEpoch != legacy.SourceEpoch || composition.runtimeSHA256 != runtimeSHA ||
		!composition.hasEntry || composition.entry != markedBinding || composition.backendRunHoldV1 {
		t.Fatalf("runtime-marked PE did not contain the valid source/runtime/entry lineage: %v", err)
	}

	identityRaw, err := os.ReadFile(markedTargets.IdentityPath)
	if err != nil {
		t.Fatal(err)
	}
	markedIdentity, err := ParseWindowsProductIdentity(identityRaw)
	if err != nil {
		t.Fatalf("parse regenerated V1 identity: %v", err)
	}
	if markedIdentity.SHA256 == legacyIdentity.SHA256 || markedIdentity.SHA256 != canonicalEntrySHA256(product) {
		t.Fatal("regenerated V1 identity did not bind the changed Product bytes")
	}
	markedIdentity.SHA256 = legacyIdentity.SHA256
	if markedIdentity != legacyIdentity {
		t.Fatal("regenerated V1 identity changed metadata outside the Product digest")
	}
	t.Log("original V1 build accepted; matched runtime-marked PE and regenerated V1 identity verified")

	got, err := CheckWindowsCanonicalEntryBuildV1(t.Context(), markedTargets.CanonicalEntryPath, markedBinding)
	if err == nil || !reflect.DeepEqual(got, WindowsCanonicalEntryBuildObservationV1{}) {
		t.Fatalf("V1 build observation admitted runtime-marked Product: error=%v, returned_schema=%q", err, got.EntrySet.Schema)
	}
}
