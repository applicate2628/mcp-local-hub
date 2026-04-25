package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper: write a tiny manifest YAML to <dir>/<name>/manifest.yaml.
func writeManifest(t *testing.T, dir, name, body string) {
	t.Helper()
	subdir := filepath.Join(dir, name)
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", subdir, err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestScanManifestEnv_AggregatesSecretRefs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	writeManifest(t, dir, "server-a", `name: server-a
env:
  OPENAI_API_KEY: secret:K1
  HOME: $HOME
  LITERAL: hello
`)
	writeManifest(t, dir, "server-b", `name: server-b
env:
  OPENAI_API_KEY: secret:K1
  WOLFRAM: secret:K2
  LOCAL: file:my_local_key
`)
	writeManifest(t, dir, "server-c", `name: server-c
env:
  PLAIN: just_a_value
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("manifest_errors: want empty, got %v", errs)
	}
	got1 := usage["K1"]
	if len(got1) != 2 {
		t.Fatalf("usage[K1] len = %d, want 2", len(got1))
	}
	if got1[0].Server != "server-a" || got1[0].EnvVar != "OPENAI_API_KEY" {
		t.Errorf("usage[K1][0] = %+v", got1[0])
	}
	if got1[1].Server != "server-b" || got1[1].EnvVar != "OPENAI_API_KEY" {
		t.Errorf("usage[K1][1] = %+v", got1[1])
	}
	got2 := usage["K2"]
	if len(got2) != 1 || got2[0].Server != "server-b" || got2[0].EnvVar != "WOLFRAM" {
		t.Errorf("usage[K2] = %+v", got2)
	}
}

func TestScanManifestEnv_MalformedEnvProducesManifestError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	// env block where one value is itself a YAML mapping → strict typing
	// rejects it as map[string]string; the whole env block fails to
	// unmarshal under our narrow projection.
	writeManifest(t, dir, "broken-env", `name: broken-env
env:
  GOOD: secret:should_not_appear
  BAD:
    nested: value
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("manifest_errors len = %d, want 1: %+v", len(errs), errs)
	}
	if errs[0].Path != "broken-env/manifest.yaml" {
		t.Errorf("manifest_errors[0].Path = %q", errs[0].Path)
	}
	if errs[0].Name != "broken-env" {
		t.Errorf("manifest_errors[0].Name = %q, want broken-env (memo §7.1 + plan-R1 P2)", errs[0].Name)
	}
	if _, exists := usage["should_not_appear"]; exists {
		t.Errorf("usage leaked refs from a manifest whose env failed to parse: %+v", usage)
	}
}

func TestScanManifestEnv_TolerantOnYAMLError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	writeManifest(t, dir, "valid-server", `name: valid-server
env:
  OPENAI: secret:K1
`)
	writeManifest(t, dir, "broken-yaml", `name: broken-yaml
env:
  OPENAI: secret:K2
unclosed_list: [
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("manifest_errors len = %d, want 1: %+v", len(errs), errs)
	}
	if errs[0].Path != "broken-yaml/manifest.yaml" {
		t.Errorf("manifest_errors[0].Path = %q", errs[0].Path)
	}
	if got := usage["K1"]; len(got) != 1 || got[0].Server != "valid-server" {
		t.Errorf("usage[K1] = %+v, want [{valid-server, OPENAI}]", got)
	}
	if _, exists := usage["K2"]; exists {
		t.Errorf("usage leaked K2 from broken-yaml manifest: %+v", usage)
	}
}

func TestScanManifestEnv_IgnoresNonSecretPrefixes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	writeManifest(t, dir, "no-secrets", `name: no-secrets
env:
  HOME: $HOME
  LOCAL: file:my_key
  PLAIN: literal_value
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("manifest_errors should be empty, got %v", errs)
	}
	if len(usage) != 0 {
		t.Errorf("usage should be empty, got %v", usage)
	}
}

func TestScanManifestEnv_MissingNameProducesError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	writeManifest(t, dir, "no-name", `env:
  OPENAI: secret:K1
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("manifest_errors len = %d, want 1: %+v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error, "missing name field") {
		t.Errorf("manifest_errors[0].Error = %q, want containing 'missing name field'", errs[0].Error)
	}
	if _, exists := usage["K1"]; exists {
		t.Errorf("usage should not contain refs from name-less manifest")
	}
}
