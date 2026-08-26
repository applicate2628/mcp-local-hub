package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestSerenaReleaseProjectionParity catches a release descriptor that no longer
// matches a live installation surface. It exercises the real checked-in
// manifest and marketplace rows rather than a text-only source assertion.
func TestSerenaReleaseProjectionParity(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate this test")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))

	if err := Check(repoRoot); err != nil {
		t.Fatalf("Serena release projections must agree: %v", err)
	}
}

// TestReleaseCatalogsPassProductionSchema catches a generated target row that
// satisfies release parity but violates the catalog parser shipped to clients.
func TestReleaseCatalogsPassProductionSchema(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate this test")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	for _, relative := range []string{"marketplace/v1/catalog.json", "marketplace/v2/catalog.json"} {
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		if _, err := api.ParseMarketplaceCatalogStrict(data); err != nil {
			t.Fatalf("production strict parser rejected %s: %v", relative, err)
		}
	}
}

// TestSerenaDescriptorAcceptsChangedValidPinWithoutGoConstant catches a
// generator that duplicates release policy in compiled constants instead of
// accepting the canonical immutable pin carried by release.yaml.
func TestSerenaDescriptorAcceptsChangedValidPinWithoutGoConstant(t *testing.T) {
	descriptor := Descriptor{
		SchemaVersion:      1,
		UpstreamRepository: repositoryURL,
		Version:            "1.8.0",
		Commit:             "0123456789abcdef0123456789abcdef01234567",
		ReadmePath:         "README.md",
	}
	if err := descriptor.validate(); err != nil {
		t.Fatalf("valid changed Serena pin must be descriptor-owned: %v", err)
	}
}

// TestCheckRequiresEveryDeclaredManifest catches optional output-existence
// admission: every registry entry is live and therefore its manifest is
// required even when the file is absent.
func TestCheckRequiresEveryDeclaredManifest(t *testing.T) {
	for _, registration := range declaredReleaseProjections {
		registration := registration
		t.Run(registration.serverID, func(t *testing.T) {
			root := t.TempDir()
			writeCurrentSerenaFixture(t, root)
			manifestPath := filepath.Join(root, filepath.FromSlash(registration.manifestPath))
			if err := os.Remove(manifestPath); err != nil {
				t.Fatalf("remove declared %s manifest: %v", registration.serverID, err)
			}

			err := Check(root)
			if err == nil || !strings.Contains(err.Error(), registration.manifestPath) {
				t.Fatalf("Check must fail closed on missing %s manifest: %v", registration.serverID, err)
			}
		})
	}
}

// TestGenerateRequiresEveryDeclaredManifestWithoutMutation catches a
// generator that publishes earlier projections before discovering that a
// later declared server has no manifest.
func TestGenerateRequiresEveryDeclaredManifestWithoutMutation(t *testing.T) {
	for _, registration := range declaredReleaseProjections {
		registration := registration
		t.Run(registration.serverID, func(t *testing.T) {
			root := t.TempDir()
			writeCurrentSerenaFixture(t, root)
			manifestPath := filepath.Join(root, filepath.FromSlash(registration.manifestPath))
			if err := os.Remove(manifestPath); err != nil {
				t.Fatalf("remove declared %s manifest: %v", registration.serverID, err)
			}
			if registration.serverID != "serena" {
				serenaPath := filepath.Join(root, "servers", "serena", "manifest.yaml")
				serena, err := os.ReadFile(serenaPath)
				if err != nil {
					t.Fatalf("read Serena manifest: %v", err)
				}
				stale := bytes.ReplaceAll(serena,
					[]byte("949a27ef1e5fda1a6e7b561e777bcece345c6ffd"),
					[]byte("f0a3a279b7c48d28b9e7e4aea1ed9caed846906b"))
				writeTestFile(t, serenaPath, string(stale))
			}
			before := snapshotExistingProjectionTargets(t, root)

			err := Generate(root)
			if err == nil || !strings.Contains(err.Error(), registration.manifestPath) {
				t.Fatalf("Generate must fail closed on missing %s manifest: %v", registration.serverID, err)
			}
			after := snapshotExistingProjectionTargets(t, root)
			if len(after) != len(before) {
				t.Fatalf("failed generation changed target count: before=%d after=%d", len(before), len(after))
			}
			for relative, want := range before {
				if got := after[relative]; !bytes.Equal(got, want) {
					t.Fatalf("failed generation mutated %s\nbefore:\n%s\nafter:\n%s", relative, want, got)
				}
			}
		})
	}
}

// TestDeclaredReleaseProjectionRegistryIsUniqueAndComplete catches a registry
// that drops, reorders, or duplicates one of the three release owners.
func TestDeclaredReleaseProjectionRegistryIsUniqueAndComplete(t *testing.T) {
	want := []string{"serena", "fetch", "sequential-thinking"}
	if len(declaredReleaseProjections) != len(want) {
		t.Fatalf("declared release registry has %d entries, want %d", len(declaredReleaseProjections), len(want))
	}
	seen := make(map[string]struct{}, len(want))
	for index, registration := range declaredReleaseProjections {
		if registration.serverID != want[index] {
			t.Fatalf("registry entry %d = %q, want %q", index, registration.serverID, want[index])
		}
		if _, exists := seen[registration.serverID]; exists {
			t.Fatalf("registry duplicates %q", registration.serverID)
		}
		seen[registration.serverID] = struct{}{}
		if registration.descriptorPath == "" || registration.manifestPath == "" || registration.load == nil {
			t.Fatalf("registry entry %q is incomplete: %#v", registration.serverID, registration)
		}
	}
	if err := validateDeclaredReleaseProjections(); err != nil {
		t.Fatalf("declared release registry is invalid: %v", err)
	}
}

// TestCheckRejectsFetchProjectionDrift catches a release drift gate that still
// validates Serena alone. Fetch's descriptor is authoritative, so an older
// embedded-manifest command must make the all-release Check fail closed.
func TestCheckRejectsFetchProjectionDrift(t *testing.T) {
	root := t.TempDir()
	writeCurrentSerenaFixture(t, root)
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "release.yaml"), `schema_version: 1
server_id: fetch
upstream_repository: https://github.com/modelcontextprotocol/servers
version: "2026.8.18"
source_commit: 644cbe65648f1d6c687b3b647683e1aaa4ed1eba
readme_path: src/fetch/README.md
runtime:
  command: uvx
  args:
    - --with
    - mcp==1.29.0
    - mcp-server-fetch@2026.8.18
artifacts:
  fetch_wheel_sha256: 6642df733a1032e7f37d0f13849af8a944d46c02420d2c070cc14e0948f8fcc2
  mcp_version: "1.29.0"
  mcp_wheel_sha256: f5a075bb611f23d6f4d080c6a1699fa62772eebc562ba9e66b306ddde1c755f7
`)
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "manifest.yaml"), `name: fetch
kind: global
command: uvx
base_args:
  - --with
  - mcp==1.28.1
  - mcp-server-fetch@2026.7.10
transport: stdio-bridge
`)
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "README.md"), "stale fetch release metadata\n")

	err := Check(root)
	if err == nil || !strings.Contains(err.Error(), "servers/fetch/manifest.yaml") {
		t.Fatalf("Check must reject Fetch manifest drift, got: %v", err)
	}
}

// TestFetchDescriptorRejectsMCP2 catches removal of Fetch's published MCP 1.x
// upper-bound enforcement even if a caller also rewrites the runtime args.
func TestFetchDescriptorRejectsMCP2(t *testing.T) {
	descriptor := FetchDescriptor{
		SchemaVersion:      1,
		ServerID:           "fetch",
		UpstreamRepository: "https://github.com/modelcontextprotocol/servers",
		Version:            "2026.8.18",
		SourceCommit:       "644cbe65648f1d6c687b3b647683e1aaa4ed1eba",
		ReadmePath:         "src/fetch/README.md",
		Runtime: runtimeDescriptor{
			Command: "uvx",
			Args:    []string{"--with", "mcp==2.1.1", "mcp-server-fetch@2026.8.18"},
		},
		Artifacts: fetchArtifacts{
			FetchWheelSHA256: "6642df733a1032e7f37d0f13849af8a944d46c02420d2c070cc14e0948f8fcc2",
			MCPVersion:       "2.1.1",
			MCPWheelSHA256:   "f5a075bb611f23d6f4d080c6a1699fa62772eebc562ba9e66b306ddde1c755f7",
		},
	}
	if err := descriptor.validate(); err == nil || !strings.Contains(err.Error(), "MCP 1.x") {
		t.Fatalf("Fetch descriptor accepted forbidden MCP 2.x closure: %v", err)
	}
}

// TestGenerateDerivesFetchProjections catches a generator that records an
// approved Fetch descriptor but leaves any runtime or marketplace surface on
// an older package selection or a floating README reference.
func TestGenerateDerivesFetchProjections(t *testing.T) {
	root := t.TempDir()
	writeCurrentSerenaFixture(t, root)
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "release.yaml"), `schema_version: 1
server_id: fetch
upstream_repository: https://github.com/modelcontextprotocol/servers
version: "2026.8.18"
source_commit: 644cbe65648f1d6c687b3b647683e1aaa4ed1eba
readme_path: src/fetch/README.md
runtime:
  command: uvx
  args:
    - --with
    - mcp==1.29.0
    - mcp-server-fetch@2026.8.18
artifacts:
  fetch_wheel_sha256: 6642df733a1032e7f37d0f13849af8a944d46c02420d2c070cc14e0948f8fcc2
  mcp_version: "1.29.0"
  mcp_wheel_sha256: f5a075bb611f23d6f4d080c6a1699fa62772eebc562ba9e66b306ddde1c755f7
`)
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "manifest.yaml"), `name: fetch
kind: global
command: uvx
base_args:
  - --with
  - mcp==1.28.1
  - mcp-server-fetch@2026.7.10
transport: stdio-bridge
daemons: [{name: default, port: 9133}]
`)
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "README.md"), "stale fetch release metadata\n")
	writeFetchCatalogs(t, root, []string{"mcp-server-fetch==2026.6.4"}, "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/fetch/README.md")

	if err := Generate(root); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "servers", "fetch", "manifest.yaml"))
	if err != nil {
		t.Fatalf("read generated Fetch manifest: %v", err)
	}
	for _, required := range []string{"mcp==1.29.0", "mcp-server-fetch@2026.8.18"} {
		if !bytes.Contains(manifest, []byte(required)) {
			t.Fatalf("generated Fetch manifest lacks %q: %s", required, manifest)
		}
	}
	if bytes.Contains(manifest, []byte("mcp==2.1.1")) || bytes.Contains(manifest, []byte("2026.7.10")) {
		t.Fatalf("generated Fetch manifest retained a forbidden/stale selection: %s", manifest)
	}
	assertGeneratedCatalogEntry(t, filepath.Join(root, "marketplace", "v1", "catalog.json"), "fetch",
		[]string{"--with", "mcp==1.29.0", "mcp-server-fetch@2026.8.18"},
		"https://raw.githubusercontent.com/modelcontextprotocol/servers/644cbe65648f1d6c687b3b647683e1aaa4ed1eba/src/fetch/README.md")
	assertGeneratedCatalogEntry(t, filepath.Join(root, "marketplace", "v2", "catalog.json"), "fetch",
		[]string{"--with", "mcp==1.29.0", "mcp-server-fetch@2026.8.18"},
		"https://raw.githubusercontent.com/modelcontextprotocol/servers/644cbe65648f1d6c687b3b647683e1aaa4ed1eba/src/fetch/README.md")
	readme, err := os.ReadFile(filepath.Join(root, "servers", "fetch", "README.md"))
	if err != nil {
		t.Fatalf("read generated Fetch README: %v", err)
	}
	for _, required := range []string{
		"uvx --with mcp==1.29.0 mcp-server-fetch@2026.8.18",
		"6642df733a1032e7f37d0f13849af8a944d46c02420d2c070cc14e0948f8fcc2",
		"f5a075bb611f23d6f4d080c6a1699fa62772eebc562ba9e66b306ddde1c755f7",
		"644cbe65648f1d6c687b3b647683e1aaa4ed1eba/src/fetch/README.md",
	} {
		if !bytes.Contains(readme, []byte(required)) {
			t.Fatalf("generated Fetch README lacks %q: %s", required, readme)
		}
	}
	if bytes.Contains(readme, []byte("/main/")) || bytes.Contains(readme, []byte("mcp==2.1.1")) {
		t.Fatalf("generated Fetch README retained a floating or forbidden reference: %s", readme)
	}
}

// TestCheckRejectsSequentialProjectionDrift catches a drift gate that omits
// Sequential Thinking even when its exact npm closure is canonical.
func TestCheckRejectsSequentialProjectionDrift(t *testing.T) {
	root := t.TempDir()
	writeCurrentSerenaFixture(t, root)
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "release.yaml"), `schema_version: 1
server_id: sequential-thinking
upstream_repository: https://github.com/modelcontextprotocol/servers
version: "2026.7.4"
source_commit: 6dd0a683e198783e30feabf7abaf42f925bd18b1
readme_path: src/sequentialthinking/README.md
runtime:
  command: npx
  args:
    - -y
    - "@modelcontextprotocol/server-sequential-thinking@2026.7.4"
artifacts:
  package_integrity: sha512-tmR/ieGaeweffLNBrDp1H1w4sn4M6TN5yWSbMS+YMfS+0GDyPjnNKzqCl2uqfdRiX3D44PJUhwiDGqtJp6tFhw==
lock:
  lockfile_version: 3
  package_count: 112
  sha256: a9ae0791d302ff944fa3d43c6909539425bfc7aa04a75322963a165a06280843
  resolved_sdk:
    version: "1.30.0"
    integrity: sha512-xKd8OIzlqNzcqcNumGAa6g+PW2kjD5vrpcKOnfldAUPP3j7lnqMPwlTXQm8gF+UwH72z0lqaRbjr9hqGz0eITA==
`)
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "manifest.yaml"), `name: sequential-thinking
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "@modelcontextprotocol/server-sequential-thinking@2025.12.18"
`)
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "README.md"), "stale Sequential Thinking release metadata\n")

	err := Check(root)
	if err == nil || !strings.Contains(err.Error(), "servers/sequential-thinking/manifest.yaml") {
		t.Fatalf("Check must reject Sequential Thinking manifest drift, got: %v", err)
	}
}

// TestGenerateDerivesSequentialProjections catches a generator that updates an
// outer npm pin without publishing the immutable source and exact resolved-lock
// evidence carried by the same canonical descriptor.
func TestGenerateDerivesSequentialProjections(t *testing.T) {
	root := t.TempDir()
	writeCurrentSerenaFixture(t, root)
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "release.yaml"), `schema_version: 1
server_id: sequential-thinking
upstream_repository: https://github.com/modelcontextprotocol/servers
version: "2026.7.4"
source_commit: 6dd0a683e198783e30feabf7abaf42f925bd18b1
readme_path: src/sequentialthinking/README.md
runtime:
  command: npx
  args:
    - -y
    - "@modelcontextprotocol/server-sequential-thinking@2026.7.4"
artifacts:
  package_integrity: sha512-tmR/ieGaeweffLNBrDp1H1w4sn4M6TN5yWSbMS+YMfS+0GDyPjnNKzqCl2uqfdRiX3D44PJUhwiDGqtJp6tFhw==
lock:
  lockfile_version: 3
  package_count: 112
  sha256: a9ae0791d302ff944fa3d43c6909539425bfc7aa04a75322963a165a06280843
  resolved_sdk:
    version: "1.30.0"
    integrity: sha512-xKd8OIzlqNzcqcNumGAa6g+PW2kjD5vrpcKOnfldAUPP3j7lnqMPwlTXQm8gF+UwH72z0lqaRbjr9hqGz0eITA==
`)
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "manifest.yaml"), `name: sequential-thinking
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "@modelcontextprotocol/server-sequential-thinking@2025.12.18"
daemons: [{name: default, port: 9124}]
`)
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "README.md"), "stale Sequential Thinking release metadata\n")
	writeSequentialCatalogs(t, root, []string{"-y", "@modelcontextprotocol/server-sequential-thinking@2025.12.18"}, "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/sequentialthinking/README.md")

	if err := Generate(root); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "servers", "sequential-thinking", "manifest.yaml"))
	if err != nil {
		t.Fatalf("read generated Sequential manifest: %v", err)
	}
	if !bytes.Contains(manifest, []byte("@modelcontextprotocol/server-sequential-thinking@2026.7.4")) || bytes.Contains(manifest, []byte("2025.12.18")) {
		t.Fatalf("generated Sequential manifest retained a stale release: %s", manifest)
	}
	for _, relative := range []string{"marketplace/v1/catalog.json", "marketplace/v2/catalog.json"} {
		assertGeneratedCatalogEntry(t, filepath.Join(root, filepath.FromSlash(relative)), "sequential-thinking",
			[]string{"-y", "@modelcontextprotocol/server-sequential-thinking@2026.7.4"},
			"https://raw.githubusercontent.com/modelcontextprotocol/servers/6dd0a683e198783e30feabf7abaf42f925bd18b1/src/sequentialthinking/README.md")
	}
	readme, err := os.ReadFile(filepath.Join(root, "servers", "sequential-thinking", "README.md"))
	if err != nil {
		t.Fatalf("read generated Sequential README: %v", err)
	}
	for _, required := range []string{
		"npx -y @modelcontextprotocol/server-sequential-thinking@2026.7.4",
		"sha512-tmR/ieGaeweffLNBrDp1H1w4sn4M6TN5yWSbMS+YMfS+0GDyPjnNKzqCl2uqfdRiX3D44PJUhwiDGqtJp6tFhw==",
		"a9ae0791d302ff944fa3d43c6909539425bfc7aa04a75322963a165a06280843",
		"@modelcontextprotocol/sdk 1.30.0",
		"sha512-xKd8OIzlqNzcqcNumGAa6g+PW2kjD5vrpcKOnfldAUPP3j7lnqMPwlTXQm8gF+UwH72z0lqaRbjr9hqGz0eITA==",
		"6dd0a683e198783e30feabf7abaf42f925bd18b1/src/sequentialthinking/README.md",
	} {
		if !bytes.Contains(readme, []byte(required)) {
			t.Fatalf("generated Sequential README lacks %q: %s", required, readme)
		}
	}
	if bytes.Contains(readme, []byte("/main/")) {
		t.Fatalf("generated Sequential README retained a floating reference: %s", readme)
	}
}

// TestGeneratePreservesManifestWhitespaceOutsideSerenaFields catches a
// generator that rewrites comments or formatting while changing the release
// source. The release writer is allowed to change only the source argument and
// the one live pinned-revision comment.
func TestGeneratePreservesManifestWhitespaceOutsideSerenaFields(t *testing.T) {
	root := t.TempDir()
	writeCurrentNonSerenaFixtures(t, root)
	oldCommit := "f0a3a279b7c48d28b9e7e4aea1ed9caed846906b"
	newCommit := "949a27ef1e5fda1a6e7b561e777bcece345c6ffd"
	oldSource := "git+https://github.com/oraios/serena@" + oldCommit
	newSource := "git+https://github.com/oraios/serena@" + newCommit

	writeTestFile(t, filepath.Join(root, "servers", "serena", "release.yaml"), `schema_version: 1
upstream_repository: https://github.com/oraios/serena
version: 1.7.0
commit: 949a27ef1e5fda1a6e7b561e777bcece345c6ffd
readme_path: README.md
`)
	manifest := "name: serena\nkind: global\ntransport: native-http\ncommand: uvx\nbase_args: [--from, " + oldSource + ", serena, start-mcp-server, --transport, streamable-http]\n# pinned serena commit " + oldCommit + "\nenv:\n  PYTHONUNBUFFERED: \"1\"\n\n# formatting sentinel\ndaemons: [{name: unified, context: codex, port: 9121}]\n"
	writeTestFile(t, filepath.Join(root, "servers", "serena", "manifest.yaml"), manifest)

	catalog := testCatalog(oldSource, "https://raw.githubusercontent.com/oraios/serena/main/README.md")
	writeTestFile(t, filepath.Join(root, "marketplace", "v1", "catalog.json"), catalog)
	writeTestFile(t, filepath.Join(root, "marketplace", "v2", "catalog.json"), catalog)

	if err := Generate(root); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "servers", "serena", "manifest.yaml"))
	if err != nil {
		t.Fatalf("read generated manifest: %v", err)
	}
	want := strings.ReplaceAll(strings.ReplaceAll(manifest, oldSource, newSource), oldCommit, newCommit)
	if string(got) != want {
		t.Fatalf("generator rewrote manifest bytes outside the approved release fields\nwant:\n%s\ngot:\n%s", want, got)
	}
	if err := Check(root); err != nil {
		t.Fatalf("generated test repository must pass Check: %v", err)
	}
}

// TestGenerateRepairsOnlyTheDriftedProjection catches a batch publisher that
// associates a temporary file with its ordinal position instead of its target
// path. A partially synchronized release must repair the remaining catalog,
// never replace the already-current manifest.
func TestGenerateRepairsOnlyTheDriftedProjection(t *testing.T) {
	root := t.TempDir()
	writeCurrentNonSerenaFixtures(t, root)
	oldCommit := "f0a3a279b7c48d28b9e7e4aea1ed9caed846906b"
	newCommit := "949a27ef1e5fda1a6e7b561e777bcece345c6ffd"
	oldSource := "git+https://github.com/oraios/serena@" + oldCommit
	newSource := "git+https://github.com/oraios/serena@" + newCommit

	writeTestFile(t, filepath.Join(root, "servers", "serena", "release.yaml"), `schema_version: 1
upstream_repository: https://github.com/oraios/serena
version: 1.7.0
commit: 949a27ef1e5fda1a6e7b561e777bcece345c6ffd
readme_path: README.md
`)
	manifest := "name: serena\nkind: global\ntransport: native-http\ncommand: uvx\nbase_args: [--from, " + newSource + ", serena, start-mcp-server, --transport, streamable-http]\ndaemons: [{name: unified, context: codex, port: 9121}]\n"
	writeTestFile(t, filepath.Join(root, "servers", "serena", "manifest.yaml"), manifest)
	currentCatalog := testCatalog(newSource, "https://raw.githubusercontent.com/oraios/serena/"+newCommit+"/README.md")
	staleCatalog := testCatalog(oldSource, "https://raw.githubusercontent.com/oraios/serena/main/README.md")
	writeTestFile(t, filepath.Join(root, "marketplace", "v1", "catalog.json"), currentCatalog)
	writeTestFile(t, filepath.Join(root, "marketplace", "v2", "catalog.json"), staleCatalog)

	if err := Generate(root); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	gotManifest, err := os.ReadFile(filepath.Join(root, "servers", "serena", "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(gotManifest) != manifest {
		t.Fatalf("already-current manifest changed or was replaced: %s", gotManifest)
	}
	if err := Check(root); err != nil {
		t.Fatalf("partially synchronized repository must pass after Generate: %v", err)
	}
}

// TestGenerateNormalizesV2CatalogLineEndings keeps the generated Serena row
// safe for plain git diff --check without reformatting other catalog rows.
func TestGenerateNormalizesV2CatalogLineEndings(t *testing.T) {
	root := t.TempDir()
	writeCurrentNonSerenaFixtures(t, root)
	newCommit := "949a27ef1e5fda1a6e7b561e777bcece345c6ffd"
	newSource := "git+https://github.com/oraios/serena@" + newCommit

	writeTestFile(t, filepath.Join(root, "servers", "serena", "release.yaml"), `schema_version: 1
upstream_repository: https://github.com/oraios/serena
version: 1.7.0
commit: 949a27ef1e5fda1a6e7b561e777bcece345c6ffd
readme_path: README.md
`)
	writeTestFile(t, filepath.Join(root, "servers", "serena", "manifest.yaml"), "name: serena\nkind: global\ntransport: native-http\ncommand: uvx\nbase_args: [--from, "+newSource+", serena, start-mcp-server, --transport, streamable-http]\ndaemons: [{name: unified, context: codex, port: 9121}]\n")
	v1Catalog := strings.Replace(testCatalog(newSource, "https://raw.githubusercontent.com/oraios/serena/"+newCommit+"/README.md"), "  \"entries\": [\n", "  \"entries\": [\n    {\n      \"id\": \"other\"\n    },\n", 1)
	v2Catalog := strings.ReplaceAll(v1Catalog, "\n", "\r\n")
	writeTestFile(t, filepath.Join(root, "marketplace", "v1", "catalog.json"), v1Catalog)
	writeTestFile(t, filepath.Join(root, "marketplace", "v2", "catalog.json"), v2Catalog)

	if err := Check(root); err == nil || !strings.Contains(err.Error(), "Serena catalog row must use LF line endings") {
		t.Fatalf("Check must reject a CRLF v2 catalog before generation: %v", err)
	}
	if err := Generate(root); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "marketplace", "v2", "catalog.json"))
	if err != nil {
		t.Fatalf("read generated v2 catalog: %v", err)
	}
	row, _, err := catalogRow(got, "serena")
	if err != nil {
		t.Fatalf("read generated Serena row: %v", err)
	}
	if bytes.Contains(row, []byte("\r")) {
		t.Fatalf("generated Serena row retained CRLF bytes")
	}
	if !bytes.Contains(got, []byte("\"id\": \"other\"\r\n")) {
		t.Fatalf("generator reformatted a non-Serena catalog row")
	}
	if !strings.Contains(string(got), "\"id\": \"serena\"") || !strings.Contains(string(got), newCommit) {
		t.Fatalf("generated v2 catalog did not retain its Serena projection: %s", got)
	}
	if err := Check(root); err != nil {
		t.Fatalf("generated repository must pass Check: %v", err)
	}
}

// TestPublishTargetsRollsBackAndCleansTempsOnLateRenameFailure catches a
// release transaction that leaves earlier replacements live when a later
// target cannot be replaced. The injected third-target rename failure happens
// only after the first two real replacements, so rollback and temp cleanup are
// deterministic on every platform.
func TestPublishTargetsRollsBackAndCleansTempsOnLateRenameFailure(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.yaml")
	second := filepath.Join(root, "second.json")
	third := filepath.Join(root, "third.json")
	firstBefore := []byte("first-before\n")
	secondBefore := []byte("second-before\n")
	thirdBefore := []byte("third-before\n")
	writeTestFile(t, first, string(firstBefore))
	writeTestFile(t, second, string(secondBefore))
	writeTestFile(t, third, string(thirdBefore))
	operations := systemPublishOps()
	realRename := operations.rename
	laterRenameInjected := false
	operations.rename = func(from, to string) error {
		if to == third && !laterRenameInjected {
			laterRenameInjected = true
			firstLive, readErr := os.ReadFile(first)
			if readErr != nil {
				t.Fatalf("read first target during injected later failure: %v", readErr)
			}
			if string(firstLive) != "first-after\n" {
				t.Fatalf("later failure was injected before first replacement: got %q", firstLive)
			}
			return errors.New("injected later target rename failure")
		}
		return realRename(from, to)
	}

	err := publishTargetsWithOps([]target{
		{path: first, data: []byte("first-after\n")},
		{path: second, data: []byte("second-after\n")},
		{path: third, data: []byte("third-after\n")},
	}, operations)
	if err == nil {
		t.Fatal("publishTargets succeeded despite an injected later rename failure")
	}
	if !laterRenameInjected {
		t.Fatal("test did not inject the later rename failure")
	}
	for _, check := range []struct {
		path string
		want []byte
	}{
		{first, firstBefore},
		{second, secondBefore},
		{third, thirdBefore},
	} {
		got, readErr := os.ReadFile(check.path)
		if readErr != nil {
			t.Fatalf("read restored target %s: %v", check.path, readErr)
		}
		if string(got) != string(check.want) {
			t.Fatalf("target %s was not restored: got %q want %q", check.path, got, check.want)
		}
	}
	temps, err := filepath.Glob(filepath.Join(root, ".*.releasegen-*"))
	if err != nil {
		t.Fatalf("glob temporary files: %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("release transaction left temporary files: %v", temps)
	}
}

// TestPublishTargetsCleansTempsWhenLaterPreimageReadFails catches the staging
// exit path before any replacement: a later read failure must remove the
// sibling temporary files created for earlier targets.
func TestPublishTargetsCleansTempsWhenLaterPreimageReadFails(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.yaml")
	second := filepath.Join(root, "second.json")
	third := filepath.Join(root, "third.json")
	writeTestFile(t, first, "first-before\n")
	writeTestFile(t, second, "second-before\n")
	writeTestFile(t, third, "third-before\n")
	operations := systemPublishOps()
	realRead := operations.readFile
	operations.readFile = func(path string) ([]byte, error) {
		if path == third {
			return nil, errors.New("injected later preimage read failure")
		}
		return realRead(path)
	}

	err := publishTargetsWithOps([]target{
		{path: first, data: []byte("first-after\n")},
		{path: second, data: []byte("second-after\n")},
		{path: third, data: []byte("third-after\n")},
	}, operations)
	if err == nil {
		t.Fatal("publishTargets succeeded despite an injected later preimage read failure")
	}
	temps, err := filepath.Glob(filepath.Join(root, ".*.releasegen-*"))
	if err != nil {
		t.Fatalf("glob temporary files: %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("preimage-read failure left temporary files: %v", temps)
	}
}

// TestPublishTargetsRollsBackNewFileOnLateFailure catches a transaction that
// treats a newly generated projection as if it had an empty preimage. Rollback
// must remove the new file, not leave an empty or partially published README.
func TestPublishTargetsRollsBackNewFileOnLateFailure(t *testing.T) {
	root := t.TempDir()
	created := filepath.Join(root, "README.md")
	second := filepath.Join(root, "second.json")
	third := filepath.Join(root, "third.json")
	writeTestFile(t, second, "second-before\n")
	writeTestFile(t, third, "third-before\n")
	operations := systemPublishOps()
	realRename := operations.rename
	laterRenameInjected := false
	operations.rename = func(from, to string) error {
		if to == third && !laterRenameInjected {
			laterRenameInjected = true
			if _, statErr := os.Stat(created); statErr != nil {
				t.Fatalf("new projection was not published before late failure: %v", statErr)
			}
			return errors.New("injected late catalog rename failure")
		}
		return realRename(from, to)
	}

	err := publishTargetsWithOps([]target{
		{path: created, data: []byte("generated README\n")},
		{path: second, data: []byte("second-after\n")},
		{path: third, data: []byte("third-after\n")},
	}, operations)
	if err == nil || !laterRenameInjected {
		t.Fatalf("transaction did not reach the injected late failure: %v", err)
	}
	if _, statErr := os.Stat(created); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rollback retained newly created projection: %v", statErr)
	}
	for path, want := range map[string]string{second: "second-before\n", third: "third-before\n"} {
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read restored target %s: %v", path, readErr)
		}
		if string(got) != want {
			t.Fatalf("target %s was not restored: got %q want %q", path, got, want)
		}
	}
}

// TestPublishTargetsJoinsCreateAndPriorCleanupFailures catches a staging path
// that returns the create failure while hiding cleanup failure for an earlier
// temporary file.
func TestPublishTargetsJoinsCreateAndPriorCleanupFailures(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.json")
	second := filepath.Join(root, "second.json")
	writeTestFile(t, first, "first-before\n")
	writeTestFile(t, second, "second-before\n")
	createFailure := errors.New("injected create failure")
	cleanupFailure := errors.New("injected prior cleanup failure")
	operations := systemPublishOps()
	realCreate := operations.createTemp
	realRemove := operations.remove
	created := ""
	createCalls := 0
	operations.createTemp = func(dir, pattern string) (*os.File, error) {
		createCalls++
		if createCalls == 2 {
			return nil, createFailure
		}
		file, err := realCreate(dir, pattern)
		if err == nil {
			created = file.Name()
		}
		return file, err
	}
	operations.remove = func(path string) error {
		if path == created {
			return cleanupFailure
		}
		return realRemove(path)
	}
	t.Cleanup(func() {
		if created != "" {
			_ = os.Remove(created)
		}
	})

	err := publishTargetsWithOps([]target{
		{path: first, data: []byte("first-after\n")},
		{path: second, data: []byte("second-after\n")},
	}, operations)
	if !errors.Is(err, createFailure) || !errors.Is(err, cleanupFailure) {
		t.Fatalf("create and cleanup failures must both remain observable: %v", err)
	}
}

// TestPublishTargetsJoinsWriteAndCleanupFailures catches a failed temporary
// write that hides failure to remove that same temporary file.
func TestPublishTargetsJoinsWriteAndCleanupFailures(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.json")
	second := filepath.Join(root, "second.json")
	writeTestFile(t, first, "first-before\n")
	writeTestFile(t, second, "second-before\n")
	writeFailure := errors.New("injected write failure")
	currentCleanupFailure := errors.New("injected current cleanup failure")
	priorCleanupFailure := errors.New("injected prior cleanup failure")
	operations := systemPublishOps()
	realCreate := operations.createTemp
	realWrite := operations.writeTemp
	created := make([]*os.File, 0, 2)
	operations.createTemp = func(dir, pattern string) (*os.File, error) {
		file, err := realCreate(dir, pattern)
		if err == nil {
			created = append(created, file)
		}
		return file, err
	}
	writeCalls := 0
	operations.writeTemp = func(file *os.File, data []byte) error {
		writeCalls++
		if writeCalls == 2 {
			return writeFailure
		}
		return realWrite(file, data)
	}
	operations.remove = func(path string) error {
		if len(created) > 1 && path == created[1].Name() {
			return currentCleanupFailure
		}
		if len(created) > 0 && path == created[0].Name() {
			return priorCleanupFailure
		}
		return nil
	}
	t.Cleanup(func() {
		for _, file := range created {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	})

	err := publishTargetsWithOps([]target{
		{path: first, data: []byte("first-after\n")},
		{path: second, data: []byte("second-after\n")},
	}, operations)
	if !errors.Is(err, writeFailure) || !errors.Is(err, currentCleanupFailure) || !errors.Is(err, priorCleanupFailure) {
		t.Fatalf("write and both cleanup failures must remain observable: %v", err)
	}
}

// TestRestorePublishedJoinsRestoreAndTempCleanupFailures catches rollback
// staging that reports only its write failure and suppresses cleanup failure.
func TestRestorePublishedJoinsRestoreAndTempCleanupFailures(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		stage := stage
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			targetPath := filepath.Join(root, "projection.json")
			restoreFailure := errors.New("injected restore " + stage + " failure")
			cleanupFailure := errors.New("injected restore cleanup failure")
			operations := systemPublishOps()
			realCreate := operations.createTemp
			realWrite := operations.writeTemp
			var created *os.File
			operations.createTemp = func(dir, pattern string) (*os.File, error) {
				file, err := realCreate(dir, pattern)
				created = file
				return file, err
			}
			if stage == "write" {
				operations.writeTemp = func(*os.File, []byte) error { return restoreFailure }
			} else {
				operations.writeTemp = realWrite
				operations.rename = func(string, string) error { return restoreFailure }
			}
			operations.remove = func(string) error { return cleanupFailure }
			t.Cleanup(func() {
				if created != nil {
					_ = created.Close()
					_ = os.Remove(created.Name())
				}
			})

			err := restorePublished([]stagedTarget{{
				target:   targetPath,
				preimage: []byte("before\n"),
				existed:  true,
			}}, operations)
			if !errors.Is(err, restoreFailure) || !errors.Is(err, cleanupFailure) {
				t.Fatalf("restore %s and cleanup failures must both remain observable: %v", stage, err)
			}
		})
	}
}

func testCatalog(source, readmeURL string) string {
	return fmt.Sprintf(`{
  "entries": [
    {
      "id": "serena",
      "readme_url": %q,
      "transport": "native-http",
      "command": "uvx",
      "args": ["--from", %q, "serena", "start-mcp-server", "--transport", "streamable-http"]
    },
    {
      "id": "fetch",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/644cbe65648f1d6c687b3b647683e1aaa4ed1eba/src/fetch/README.md",
      "transport": "stdio",
      "command": "uvx",
      "args": ["--with", "mcp==1.29.0", "mcp-server-fetch@2026.8.18"]
    },
    {
      "id": "sequential-thinking",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/6dd0a683e198783e30feabf7abaf42f925bd18b1/src/sequentialthinking/README.md",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-sequential-thinking@2026.7.4"]
    }
  ]
}
`, readmeURL, source)
}

func writeCurrentSerenaFixture(t *testing.T, root string) {
	t.Helper()
	commit := "949a27ef1e5fda1a6e7b561e777bcece345c6ffd"
	source := "git+https://github.com/oraios/serena@" + commit
	writeTestFile(t, filepath.Join(root, "servers", "serena", "release.yaml"), `schema_version: 1
upstream_repository: https://github.com/oraios/serena
version: 1.7.0
commit: 949a27ef1e5fda1a6e7b561e777bcece345c6ffd
readme_path: README.md
`)
	writeTestFile(t, filepath.Join(root, "servers", "serena", "manifest.yaml"), "name: serena\nkind: global\ntransport: native-http\ncommand: uvx\nbase_args: [--from, "+source+", serena, start-mcp-server, --transport, streamable-http]\ndaemons: [{name: unified, context: codex, port: 9121}]\n")
	writeCurrentNonSerenaFixtures(t, root)
	catalog := testCatalog(source, "https://raw.githubusercontent.com/oraios/serena/"+commit+"/README.md")
	writeTestFile(t, filepath.Join(root, "marketplace", "v1", "catalog.json"), catalog)
	writeTestFile(t, filepath.Join(root, "marketplace", "v2", "catalog.json"), catalog)
}

func writeCurrentNonSerenaFixtures(t *testing.T, root string) {
	t.Helper()
	fetchDescriptor := FetchDescriptor{
		SchemaVersion:      1,
		ServerID:           "fetch",
		UpstreamRepository: serversRepositoryURL,
		Version:            "2026.8.18",
		SourceCommit:       "644cbe65648f1d6c687b3b647683e1aaa4ed1eba",
		ReadmePath:         "src/fetch/README.md",
		Runtime: runtimeDescriptor{
			Command: "uvx",
			Args:    []string{"--with", "mcp==1.29.0", "mcp-server-fetch@2026.8.18"},
		},
		Artifacts: fetchArtifacts{
			FetchWheelSHA256: "6642df733a1032e7f37d0f13849af8a944d46c02420d2c070cc14e0948f8fcc2",
			MCPVersion:       "1.29.0",
			MCPWheelSHA256:   "f5a075bb611f23d6f4d080c6a1699fa62772eebc562ba9e66b306ddde1c755f7",
		},
	}
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "release.yaml"), `schema_version: 1
server_id: fetch
upstream_repository: https://github.com/modelcontextprotocol/servers
version: "2026.8.18"
source_commit: 644cbe65648f1d6c687b3b647683e1aaa4ed1eba
readme_path: src/fetch/README.md
runtime:
  command: uvx
  args:
    - --with
    - mcp==1.29.0
    - mcp-server-fetch@2026.8.18
artifacts:
  fetch_wheel_sha256: 6642df733a1032e7f37d0f13849af8a944d46c02420d2c070cc14e0948f8fcc2
  mcp_version: "1.29.0"
  mcp_wheel_sha256: f5a075bb611f23d6f4d080c6a1699fa62772eebc562ba9e66b306ddde1c755f7
`)
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "manifest.yaml"), `name: fetch
kind: global
transport: stdio-bridge
command: uvx
base_args: [--with, mcp==1.29.0, mcp-server-fetch@2026.8.18]
`)
	writeTestFile(t, filepath.Join(root, "servers", "fetch", "README.md"), string(renderFetchReadme(fetchDescriptor)))

	sequentialDescriptor := SequentialDescriptor{
		SchemaVersion:      1,
		ServerID:           "sequential-thinking",
		UpstreamRepository: serversRepositoryURL,
		Version:            "2026.7.4",
		SourceCommit:       "6dd0a683e198783e30feabf7abaf42f925bd18b1",
		ReadmePath:         "src/sequentialthinking/README.md",
		Runtime: runtimeDescriptor{
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-sequential-thinking@2026.7.4"},
		},
		Artifacts: sequentialArtifacts{
			PackageIntegrity: "sha512-tmR/ieGaeweffLNBrDp1H1w4sn4M6TN5yWSbMS+YMfS+0GDyPjnNKzqCl2uqfdRiX3D44PJUhwiDGqtJp6tFhw==",
		},
		Lock: sequentialLockDescriptor{
			LockfileVersion: 3,
			PackageCount:    112,
			SHA256:          "a9ae0791d302ff944fa3d43c6909539425bfc7aa04a75322963a165a06280843",
			ResolvedSDK: resolvedSDKDescriptor{
				Version:   "1.30.0",
				Integrity: "sha512-xKd8OIzlqNzcqcNumGAa6g+PW2kjD5vrpcKOnfldAUPP3j7lnqMPwlTXQm8gF+UwH72z0lqaRbjr9hqGz0eITA==",
			},
		},
	}
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "release.yaml"), `schema_version: 1
server_id: sequential-thinking
upstream_repository: https://github.com/modelcontextprotocol/servers
version: "2026.7.4"
source_commit: 6dd0a683e198783e30feabf7abaf42f925bd18b1
readme_path: src/sequentialthinking/README.md
runtime:
  command: npx
  args:
    - -y
    - "@modelcontextprotocol/server-sequential-thinking@2026.7.4"
artifacts:
  package_integrity: sha512-tmR/ieGaeweffLNBrDp1H1w4sn4M6TN5yWSbMS+YMfS+0GDyPjnNKzqCl2uqfdRiX3D44PJUhwiDGqtJp6tFhw==
lock:
  lockfile_version: 3
  package_count: 112
  sha256: a9ae0791d302ff944fa3d43c6909539425bfc7aa04a75322963a165a06280843
  resolved_sdk:
    version: "1.30.0"
    integrity: sha512-xKd8OIzlqNzcqcNumGAa6g+PW2kjD5vrpcKOnfldAUPP3j7lnqMPwlTXQm8gF+UwH72z0lqaRbjr9hqGz0eITA==
`)
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "manifest.yaml"), `name: sequential-thinking
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y", "@modelcontextprotocol/server-sequential-thinking@2026.7.4"]
`)
	writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "README.md"), string(renderSequentialReadme(sequentialDescriptor)))
}

func writeFetchCatalogs(t *testing.T, root string, args []string, readmeURL string) {
	t.Helper()
	commit := "949a27ef1e5fda1a6e7b561e777bcece345c6ffd"
	serenaSource := "git+https://github.com/oraios/serena@" + commit
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal test Fetch args: %v", err)
	}
	catalog := fmt.Sprintf(`{
  "entries": [
    {
      "id": "serena",
      "readme_url": "https://raw.githubusercontent.com/oraios/serena/%s/README.md",
      "transport": "native-http",
      "command": "uvx",
      "args": ["--from", %q, "serena", "start-mcp-server", "--transport", "streamable-http"]
    },
    {
      "id": "fetch",
      "name": "Fetch MCP server",
      "summary": "Fetch arbitrary HTTP URLs and return content.",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/fetch",
      "readme_url": %q,
      "transport": "stdio",
      "command": "uvx",
      "args": %s,
      "env": {},
      "categories": ["http", "io"],
      "license": "MIT"
    },
    {
      "id": "sequential-thinking",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/6dd0a683e198783e30feabf7abaf42f925bd18b1/src/sequentialthinking/README.md",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-sequential-thinking@2026.7.4"]
    }
  ]
}
`, commit, serenaSource, readmeURL, argsJSON)
	writeTestFile(t, filepath.Join(root, "marketplace", "v1", "catalog.json"), catalog)
	writeTestFile(t, filepath.Join(root, "marketplace", "v2", "catalog.json"), catalog)
}

func writeSequentialCatalogs(t *testing.T, root string, args []string, readmeURL string) {
	t.Helper()
	commit := "949a27ef1e5fda1a6e7b561e777bcece345c6ffd"
	serenaSource := "git+https://github.com/oraios/serena@" + commit
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal test Sequential args: %v", err)
	}
	catalog := fmt.Sprintf(`{
  "entries": [
    {
      "id": "serena",
      "readme_url": "https://raw.githubusercontent.com/oraios/serena/%s/README.md",
      "transport": "native-http",
      "command": "uvx",
      "args": ["--from", %q, "serena", "start-mcp-server", "--transport", "streamable-http"]
    },
    {
      "id": "fetch",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/644cbe65648f1d6c687b3b647683e1aaa4ed1eba/src/fetch/README.md",
      "transport": "stdio",
      "command": "uvx",
      "args": ["--with", "mcp==1.29.0", "mcp-server-fetch@2026.8.18"]
    },
    {
      "id": "sequential-thinking",
      "name": "Sequential Thinking MCP server",
      "summary": "Structured step-by-step reasoning.",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/sequentialthinking",
      "readme_url": %q,
      "transport": "stdio",
      "command": "npx",
      "args": %s,
      "env": {},
      "categories": ["reasoning", "agent"],
      "license": "MIT"
    }
  ]
}
`, commit, serenaSource, readmeURL, argsJSON)
	writeTestFile(t, filepath.Join(root, "marketplace", "v1", "catalog.json"), catalog)
	writeTestFile(t, filepath.Join(root, "marketplace", "v2", "catalog.json"), catalog)
}

func assertGeneratedCatalogEntry(t *testing.T, path, id string, wantArgs []string, wantReadmeURL string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated catalog %s: %v", path, err)
	}
	var document struct {
		Entries []struct {
			ID        string   `json:"id"`
			Args      []string `json:"args"`
			ReadmeURL string   `json:"readme_url"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode generated catalog %s: %v", path, err)
	}
	for _, entry := range document.Entries {
		if entry.ID != id {
			continue
		}
		if !equalStrings(entry.Args, wantArgs) || entry.ReadmeURL != wantReadmeURL {
			t.Fatalf("generated %s row mismatch: args=%v readme_url=%q", id, entry.Args, entry.ReadmeURL)
		}
		return
	}
	t.Fatalf("generated catalog %s lacks %s row", path, id)
}

func snapshotExistingProjectionTargets(t *testing.T, root string) map[string][]byte {
	t.Helper()
	paths := append([]string(nil), releaseCatalogPaths[:]...)
	for _, registration := range declaredReleaseProjections {
		paths = append(paths, registration.manifestPath)
		if registration.readmePath != "" {
			paths = append(paths, registration.readmePath)
		}
	}
	snapshot := make(map[string][]byte, len(paths))
	for _, relative := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("snapshot %s: %v", relative, err)
		}
		snapshot[relative] = data
	}
	return snapshot
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
