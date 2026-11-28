package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sequentialSecondDocument = "\n---\nartifacts:\n  node_engine: null\nlock:\n  resolved_protocol_dependencies: null\n"

const splitSequentialFixture = `schema_version: 2
server_id: sequential-thinking
upstream_repository: https://github.com/modelcontextprotocol/servers
version: "2.2.0"
source_commit: c799c8552b0f27b7a33a7b9233773da144dddcf5
readme_path: src/sequentialthinking/README.md
runtime:
  command: npx
  args: [-y, "@modelcontextprotocol/server-sequential-thinking@2.2.0"]
artifacts:
  package_integrity: sha512-Tv3AniI/gc+X2J4YjJmZ0BCP/82clUTvOt7Z9z8PWHEdE4/123iHUyJJk0dRMCE/mlskmOuBAgHv+pDq6KvBDA==
  node_engine: ">=24"
lock:
  lockfile_version: 3
  package_count: 388
  sha256: 923d2b0e484e31e1fdeb103bb8dbec2637df3bb14fdc41a02836f1e1ecb712a7
  resolved_protocol_dependencies:
    "@modelcontextprotocol/server":
      version: &server_version "2.4.0"
      integrity: sha512-rgR9yxSaAJ+bcsi/XroT/S2AfPiIWloXAnrdfYSCms9ivVju0ishu/wW0J21Ho6+ZOR1r7RyeRbjiRuF87ix5Q==
    "@modelcontextprotocol/node":
      version: "2.1.2"
      integrity: sha512-IIcIqBpr6NCQQs5DgJTgvbSOAw+3iaR/uquqg+e0nXCdudfjPg/80x0TseUnQx14AUAtE4fsJCEpJKPyb6WPVA==
`

func TestSequentialV1UsesLegacyFirstDocument(t *testing.T) {
	root := t.TempDir()
	writeCurrentSerenaFixture(t, root)
	if err := Generate(root); err != nil {
		t.Fatalf("generate baseline with the first document only: %v", err)
	}
	if err := Check(root); err != nil {
		t.Fatalf("legacy fixture must start synchronized: %v", err)
	}
	before := snapshotExistingProjectionTargets(t, root)
	path := filepath.Join(root, "servers", "sequential-thinking", "release.yaml")
	legacy, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data string
	}{
		{"reserved fields in second document", string(legacy) + sequentialSecondDocument},
		{"malformed second document", string(legacy) + "\n---\n["},
		{"historical unknown keys and YAML integer coercion", strings.NewReplacer(
			"schema_version: 1", "schema_version: 1.0\nhistorical_note: [one, two]",
			"artifacts:\n", "artifacts:\n  old_evidence: null\n",
		).Replace(string(legacy))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeTestFile(t, path, tc.data)
			if err := Check(root); err != nil {
				t.Fatalf("Check rejected accepted V1 data: %v", err)
			}
			if err := Generate(root); err != nil {
				t.Fatalf("Generate rejected accepted V1 data: %v", err)
			}
			if after := snapshotExistingProjectionTargets(t, root); !maps.EqualFunc(before, after, bytes.Equal) {
				t.Fatal("V1 projection bytes changed")
			}
		})
	}
	for _, tc := range []struct{ branch, field string }{
		{"artifacts", "node_engine"},
		{"lock", "resolved_protocol_dependencies"},
	} {
		t.Run(tc.field+" in first document", func(t *testing.T) {
			header := tc.branch + ":\n"
			writeTestFile(t, path, strings.Replace(string(legacy), header, header+"  "+tc.field+": null\n", 1))
			assertSequentialDescriptorFailure(t, root, before, tc.branch+"."+tc.field)
		})
	}
}

func TestSequentialV1PreservesLegacySDKLeadingZero(t *testing.T) {
	root := t.TempDir()
	writeCurrentSerenaFixture(t, root)
	if err := Generate(root); err != nil {
		t.Fatalf("generate V1 baseline: %v", err)
	}
	want := snapshotExistingProjectionTargets(t, root)
	path := filepath.Join(root, "servers", "sequential-thinking", "release.yaml")
	legacy, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, strings.Replace(string(legacy), `version: "1.30.0"`, `version: "01.30.0"`, 1))
	if err := Generate(root); err != nil {
		t.Fatalf("Generate rejected the legacy SDK pin: %v", err)
	}
	if err := Check(root); err != nil {
		t.Fatalf("Check rejected the legacy SDK pin: %v", err)
	}
	const readme = "servers/sequential-thinking/README.md"
	want[readme] = bytes.Replace(want[readme], []byte("@modelcontextprotocol/sdk 1.30.0"), []byte("@modelcontextprotocol/sdk 01.30.0"), 1)
	after := snapshotExistingProjectionTargets(t, root)
	if !bytes.Contains(after[readme], []byte("@modelcontextprotocol/sdk 01.30.0")) ||
		!maps.EqualFunc(want, after, bytes.Equal) {
		t.Fatal("V1 must render the legacy SDK pin verbatim and preserve the other projection bytes")
	}
}

func TestGenerateSequentialV2ProjectsDescriptorEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, data, engine, node, server string
	}{
		{"reviewed release", splitSequentialFixture, ">=24", "2.1.2", "2.4.0"},
		{"different evidence", strings.NewReplacer(
			">=24", ">=31",
			"2.1.2", "7.1.2",
			"2.4.0", "9.4.0",
		).Replace(splitSequentialFixture), ">=31", "7.1.2", "9.4.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCurrentSerenaFixture(t, root)
			writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "release.yaml"), tc.data)
			if err := Generate(root); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if err := Check(root); err != nil {
				t.Fatalf("Check: %v", err)
			}
			readmeBytes, err := os.ReadFile(filepath.Join(root, "servers", "sequential-thinking", "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			readme := string(readmeBytes)
			for _, expected := range []string{
				"Published Node.js engine: `" + tc.engine + "`",
				"Upstream monorepo npm lockfile at the source commit: v3, 388 lockfile entries",
				"Resolved protocol dependencies for workspace `src/sequentialthinking`",
				"npx launches the pinned outer release and resolves its dependencies at launch.",
				"2.2.0",
			} {
				if !strings.Contains(readme, expected) {
					t.Errorf("README lacks %q", expected)
				}
			}
			node := strings.Index(readme, "`@modelcontextprotocol/node@"+tc.node+"`, integrity `sha512-IIcIqBpr")
			server := strings.Index(readme, "`@modelcontextprotocol/server@"+tc.server+"`, integrity `sha512-rgR9yxSa")
			if node < 0 || server < 0 || node >= server {
				t.Fatalf("README must retain both pins in package-name order:\n%s", readme)
			}
		})
	}
}

func TestSequentialV2RejectsInvalidDescriptorBeforeWriting(t *testing.T) {
	const nodePin = "    \"@modelcontextprotocol/node\":\n      version: \"2.1.2\"\n      integrity: sha512-IIcIqBpr6NCQQs5DgJTgvbSOAw+3iaR/uquqg+e0nXCdudfjPg/80x0TseUnQx14AUAtE4fsJCEpJKPyb6WPVA==\n"
	for _, tc := range []struct {
		name, old, replacement, diagnostic string
	}{
		{"second document", "", sequentialSecondDocument, "single YAML document"},
		{"empty second document", "", "\n---\n", "single YAML document"},
		{"trailing parse error", "", "\n---\n[", "trailing YAML"},
		{"missing schema", "schema_version: 2\n", "", "schema_version"},
		{"unknown schema", "schema_version: 2", "schema_version: 3", "schema_version"},
		{"float schema", "schema_version: 2", "schema_version: 2.0", "schema_version"},
		{"unknown top level field", "", "hint: unknown\n", "hint"},
		{"duplicate metadata", "  package_count: 388", "  package_count: 388\n  package_count: 389", "duplicate field \"package_count\""},
		{"mistyped lock count", "package_count: 388", "package_count: 388.0", "package_count"},
		{"nonpositive lock count", "package_count: 388", "package_count: 0", "package_count"},
		{"invalid lock digest", "sha256: 923d2b0e", "sha256: Z23d2b0e", "lock.sha256"},
		{"wrong collection tag", "artifacts:\n", "artifacts: !!str\n", "artifacts"},
		{"argument scalar type", "[-y,", "[true,", "runtime.args[0]"},
		{"missing engine", "  node_engine: \">=24\"\n", "", "node_engine"},
		{"empty engine", "node_engine: \">=24\"", "node_engine: \"\"", "node_engine"},
		{"invalid engine expression", "node_engine: \">=24\"", "node_engine: \">=24.0.0\"", "node_engine"},
		{"zero engine major", "node_engine: \">=24\"", "node_engine: \">=0\"", "node_engine"},
		{"invalid package digest length", "sha512-Tv3AniI/gc+X2J4YjJmZ0BCP/82clUTvOt7Z9z8PWHEdE4/123iHUyJJk0dRMCE/mlskmOuBAgHv+pDq6KvBDA==", "sha512-AA==", "package_integrity"},
		{"legacy lock only", "resolved_protocol_dependencies:", "resolved_sdk:", "resolved_sdk"},
		{"mixed lock", "  resolved_protocol_dependencies:", "  resolved_sdk: null\n  resolved_protocol_dependencies:", "resolved_sdk"},
		{"missing pin", nodePin, "", "@modelcontextprotocol/node"},
		{"duplicate pin", nodePin, nodePin + nodePin, "duplicate field \"@modelcontextprotocol/node\""},
		{"unknown pin", "@modelcontextprotocol/node", "@modelcontextprotocol/sdk", "@modelcontextprotocol/sdk"},
		{"mistyped pin", "version: \"2.1.2\"", "version: 212", ".version"},
		{"null pin", nodePin, "    \"@modelcontextprotocol/node\": null\n", "@modelcontextprotocol/node"},
		{"range pin", "version: \"2.1.2\"", "version: \"^2.1.2\"", "@modelcontextprotocol/node version"},
		{"invalid pin digest length", "sha512-IIcIqBpr6NCQQs5DgJTgvbSOAw+3iaR/uquqg+e0nXCdudfjPg/80x0TseUnQx14AUAtE4fsJCEpJKPyb6WPVA==", "sha512-AA==", "@modelcontextprotocol/node integrity"},
		{"alias pin version", "version: \"2.1.2\"", "version: *server_version", ".version"},
		{"merge key", "  node_engine:", "  <<: {}\n  node_engine:", "invalid YAML key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCurrentSerenaFixture(t, root)
			before := snapshotExistingProjectionTargets(t, root)
			data := splitSequentialFixture
			if tc.old == "" {
				data += tc.replacement
			} else {
				if !strings.Contains(data, tc.old) {
					t.Fatalf("invalid fixture edit: %q is missing", tc.old)
				}
				data = strings.Replace(data, tc.old, tc.replacement, 1)
			}
			writeTestFile(t, filepath.Join(root, "servers", "sequential-thinking", "release.yaml"), data)
			assertSequentialDescriptorFailure(t, root, before, tc.diagnostic)
		})
	}
}

func TestSequentialV2RejectsLeadingZerosBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name, old, version, diagnostic string
	}{
		{"outer version and matching argument", "2.2.0", "2.02.0", "Sequential version"},
		{"node lock pin", "2.1.2", "2.01.2", "@modelcontextprotocol/node version"},
		{"server lock pin", "2.4.0", "02.4.0", "@modelcontextprotocol/server version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCurrentSerenaFixture(t, root)
			path := filepath.Join(root, "servers", "sequential-thinking", "release.yaml")
			writeTestFile(t, path, splitSequentialFixture)
			if err := Generate(root); err != nil {
				t.Fatalf("generate V2 baseline: %v", err)
			}
			before := snapshotExistingProjectionTargets(t, root)
			// Keep the outer package argument aligned with the outer release version.
			writeTestFile(t, path, strings.ReplaceAll(splitSequentialFixture, tc.old, tc.version))
			assertSequentialDescriptorFailure(t, root, before, tc.diagnostic)
		})
	}
}

func assertSequentialDescriptorFailure(t *testing.T, root string, before map[string][]byte, diagnostic string) {
	t.Helper()
	for _, operation := range []struct {
		run    func(string) error
		prefix string
	}{
		{Check, "RELEASE_PROJECTION_DRIFT"},
		{Generate, "RELEASE_GENERATION_FAILED"},
	} {
		err := operation.run(root)
		if err == nil || !strings.Contains(err.Error(), operation.prefix+": servers/sequential-thinking/release.yaml:") ||
			!strings.Contains(err.Error(), diagnostic) {
			t.Errorf("expected %s for %q, got %v", operation.prefix, diagnostic, err)
		}
		if after := snapshotExistingProjectionTargets(t, root); !maps.EqualFunc(before, after, bytes.Equal) {
			t.Errorf("%s changed a projection", operation.prefix)
		}
	}
}
