//go:build windows

package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestPrivateSerenaMigrationSeedIsExplicitPinnedInput(t *testing.T) {
	root := `D:\dev\ChatGPT\seed-input-data`
	digest := strings.Repeat("a", 64)
	artifact := func(name string) privateSerenaMigrationArtifact {
		return privateSerenaMigrationArtifact{Path: filepath.Join(root, name), SHA256: digest}
	}
	input := privateSerenaMigrationInput{
		Schema: "private-serena-migration-input-v1", Purpose: privateSerenaMigrationPurpose,
		SourceRoot: filepath.Join(root, "source"), SourceEpoch: digest, SourceCommit: strings.Repeat("b", 40),
		Image: artifact("private.exe"), ProductFixture: artifact("product.json"),
		UV: artifact("uv.exe"), UVX: artifact("uvx.exe"), Python: artifact("python.exe"),
		IssuanceReceipt: artifact("issued.json"), EvidenceParent: filepath.Join(root, "evidence"),
	}
	input.Catalog = privateSerenaMigrationArtifact{Path: filepath.Join(input.SourceRoot, "servers", "serena", "manifest.yaml"), SHA256: digest}
	decode := func(v privateSerenaMigrationInput) (privateSerenaMigrationInput, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return privateSerenaMigrationInput{}, err
		}
		return privateSerenaMigrationDecodeInput(raw)
	}
	cold, err := decode(input)
	if err != nil || cold.CacheSeed != nil || len(cold.artifacts()) != 7 {
		t.Fatal("old input changed", err)
	}
	seed := artifact("seed-descriptor.json")
	input.CacheSeed = &seed
	bound, err := decode(input)
	if err != nil || bound.CacheSeed == nil || *bound.CacheSeed != seed || len(bound.artifacts()) != 8 {
		t.Fatal("descriptor not retained as pinned input", err)
	}
	for _, wrong := range []privateSerenaMigrationArtifact{
		{Path: seed.Path}, {SHA256: digest}, {Path: `C:\seed.json`, SHA256: digest}, {Path: "relative", SHA256: digest},
	} {
		input.CacheSeed = &wrong
		if _, err = decode(input); err == nil {
			t.Fatal("unbound/out-of-scope seed accepted", wrong)
		}
	}
	input.CacheSeed = nil
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"cache_seed":null}`)...)
	if _, err = privateSerenaMigrationDecodeInput(raw); err == nil {
		t.Fatal("explicit null silently became cold input")
	}
}

func TestPrivateSerenaMigrationSeedDeclaresOfflineBeforeProjection(t *testing.T) {
	catalog := strings.Replace(legacy2DaemonManifestYAML, "command: go", "command: uvx", 1)
	catalog = strings.Replace(catalog, "git+https://example/serena", "git+https://example/serena@"+strings.Repeat("a", 40), 1)
	lookup := func(string) (string, bool) { return "", false }
	for _, original := range []string{legacy2DaemonManifestYAML, alreadyMigratedManifestYAML} {
		cold, err := privateSerenaMigrationManifest([]byte(catalog), original)
		if err != nil {
			t.Fatal(err)
		}
		absent, err := privateSerenaMigrationManifest([]byte(catalog), original, false)
		if err != nil || !bytes.Equal(cold, absent) {
			t.Fatal("no-seed bytes changed", err)
		}
		seeded, err := privateSerenaMigrationManifest([]byte(catalog), original, true)
		if err != nil {
			t.Fatal(err)
		}
		base, err := config.ParseManifestWithEnvironment(bytes.NewReader(cold), lookup)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := config.ParseManifestWithEnvironment(bytes.NewReader(seeded), lookup)
		if err != nil || actual.Env["UV_OFFLINE"] != "true" {
			t.Fatal("offline declaration absent from original parsed manifest", err)
		}
		if _, exists := actual.Env["UV_CACHE_DIR"]; exists {
			t.Fatal("ambient/external cache path injected")
		}
		delete(actual.Env, "UV_OFFLINE")
		if len(actual.Env) == 0 && base.Env == nil {
			actual.Env = nil
		}
		if !reflect.DeepEqual(actual, base) {
			t.Fatal("seed declaration changed the original scenario beyond offline preparation")
		}
	}
}
