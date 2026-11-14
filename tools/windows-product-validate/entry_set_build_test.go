package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func TestEntrySetBuildModeIsExplicitAndSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcphub.exe")
	common := []string{"--expected-entry-set-build-id", strings.Repeat("a", 32), "--expected-canonical-entry-sha256", strings.Repeat("b", 64), "--canonical-entry-reverse-projection", "../mcphub.exe", "--expected-version", fixtureVersion, "--expected-commit", fixtureCommit, "--expected-build-date", fixtureBuildDate}
	portable := append([]string{"--validate-entry-set-build", path}, common...)
	var stderr bytes.Buffer
	options, ok := parseOptions(append(append([]string{}, portable...), "--expected-source-epoch", strings.Repeat("f", 64)), &stderr)
	if !ok || options.validateEntrySetBuild != path || options.validateEntrySet != "" {
		t.Fatalf("portable mode lost: %s", stderr.String())
	}
	for _, args := range [][]string{
		append(append([]string{}, portable...), "--validate-entry-set", path),
		append(append([]string{"--validate-entry-set", path}, common...), "--expected-source-epoch", strings.Repeat("f", 64)),
	} {
		if _, ok := parseOptions(args, &stderr); ok {
			t.Fatalf("native/portable modes mixed: %v", args)
		}
	}
	if runtime.GOOS != "windows" {
		var stdout, nativeError bytes.Buffer
		if code := run(append([]string{"--validate-entry-set", path}, common...), &stdout, &nativeError); code == 0 || !strings.Contains(strings.ToLower(nativeError.String()), "unsupported") {
			t.Fatalf("native mode gained a portable fallback: %d %s", code, nativeError.String())
		}
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		var stdout, portableError bytes.Buffer
		if code := run(portable, &stdout, &portableError); code == 0 || strings.Contains(strings.ToLower(portableError.String()), "unsupported") {
			t.Fatalf("explicit portable mode did not use its file observer: %d %s", code, portableError.String())
		}
	}
}

func TestEntrySetBuildObservationRejectsOriginalInputMismatch(t *testing.T) {
	binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(strings.Repeat("a", 32), strings.Repeat("b", 64), "../mcphub.exe")
	if err != nil {
		t.Fatal(err)
	}
	options := validatorOptions{expectedVersion: fixtureVersion, expectedCommit: fixtureCommit, expectedBuildDate: fixtureBuildDate, expectedSourceEpoch: strings.Repeat("f", 64)}
	identity := []byte("received identity bytes")
	hash := sha256.Sum256(identity)
	observation := binaryadmission.WindowsCanonicalEntryBuildObservationV1{SourceEpoch: options.expectedSourceEpoch, EntrySet: binaryadmission.WindowsCanonicalEntrySetSnapshotV1{Schema: "windows-canonical-entry-set-v1", BuildID: binding.BuildID(), CanonicalEntryRelativePath: "mcphub.exe", ProductRelativePath: "mcphub-product/mcphub.exe", IdentityRelativePath: "windows-product-identity.json", ReverseProjection: binding.ReverseProjection(), CanonicalEntry: binaryadmission.WindowsCanonicalEntryFileV1{SHA256: binding.ExpectedCanonicalEntrySHA256()}, Product: binaryadmission.WindowsCanonicalEntryFileV1{SHA256: strings.Repeat("c", 64)}, Identity: binaryadmission.WindowsCanonicalEntryFileV1{SHA256: hex.EncodeToString(hash[:])}, Machine: fixtureMachine, IdentityBytes: identity, ProductArtifact: binaryadmission.WindowsArtifact{Role: binaryadmission.WindowsArtifactRoleProduct, Subsystem: 2, Machine: fixtureMachine, Version: fixtureVersion, Commit: fixtureCommit, BuildDate: fixtureBuildDate, SHA256: strings.Repeat("c", 64)}}}
	if err := receiveEntrySetBuildObservation(options, binding, observation); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*binaryadmission.WindowsCanonicalEntryBuildObservationV1){
		func(o *binaryadmission.WindowsCanonicalEntryBuildObservationV1) {
			o.EntrySet.ProductArtifact.Version = "foreign"
		},
		func(o *binaryadmission.WindowsCanonicalEntryBuildObservationV1) { o.EntrySet.ProductArtifact.Machine++ },
		func(o *binaryadmission.WindowsCanonicalEntryBuildObservationV1) {
			o.EntrySet.CanonicalEntry.SHA256 = strings.Repeat("d", 64)
		},
		func(o *binaryadmission.WindowsCanonicalEntryBuildObservationV1) {
			o.EntrySet.Identity.SHA256 = strings.Repeat("e", 64)
		},
		func(o *binaryadmission.WindowsCanonicalEntryBuildObservationV1) {
			o.EntrySet.ReverseProjection = "foreign"
		},
		func(o *binaryadmission.WindowsCanonicalEntryBuildObservationV1) {
			o.SourceEpoch = strings.Repeat("e", 64)
		},
	} {
		changed := observation
		mutate(&changed)
		if err := receiveEntrySetBuildObservation(options, binding, changed); err == nil {
			t.Fatal("original build mismatch accepted")
		}
	}
}
