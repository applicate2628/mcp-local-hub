package clients

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestCodexDesiredTransportInvalidFailsBeforeLayerRead(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global", "config.toml")
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalPath, []byte("not TOML {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: globalPath}
	_, err := c.ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena",
		DesiredTransport: CodexTransportNone,
		ProjectRoot:      filepath.Join(root, "missing-root"),
		WorkingDir:       filepath.Join(root, "missing-workdir"),
	})
	if !errors.Is(err, ErrCodexDesiredTransportInvalid) {
		t.Fatalf("ResolveTransportTarget error = %v, want CODEX_DESIRED_TRANSPORT_INVALID", err)
	}
}

func TestCodexTransportPublicResultsDoNotExposePathsAndRelocationSettlesExact(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[CodexTransportTarget](),
		reflect.TypeFor[CodexHTTPRelocationResult](),
	} {
		for field := range typ.NumField() {
			name := typ.Field(field).Name
			if name == "GlobalPath" || name == "ProjectPaths" || name == "Path" {
				t.Fatalf("%s exposes machine path field %s", typ.Name(), name)
			}
		}
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[mcp_servers.serena]\ncommand = 'uvx'\nargs = ['serena']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := (&codexCLI{path: path}).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena",
		TargetEntryName: "serena",
		ExpectedSource:  CodexTransportStdio,
		Entry:           MCPEntry{Name: "serena", URL: "http://127.0.0.1:9300/mcp"},
	})
	if err != nil {
		t.Fatalf("RelocateHTTPEntry: %v", err)
	}
	if result.Outcome != CodexHTTPRelocationCommitted || result.Readback != CodexHTTPRelocationReadbackExact {
		t.Fatalf("result = %+v, want committed exact settlement", result)
	}
}

func TestCodexRelocationPreservesCompleteDocumentOutsideAuthorizedMove(t *testing.T) {
	path := setupCodexConfig(t, `title = "operator configuration"
features = ["alpha", "beta"]

[metadata]
owner = "platform"

[metadata.nested]
enabled = true

[mcp_servers.serena]
command = "uvx"
args = ["serena", "start"]

[mcp_servers.unrelated]
url = "http://127.0.0.1:9999/mcp"
startup_timeout_sec = 7
http_headers = { "X-Unrelated" = "keep" }
`)
	before, err := (&codexCLI{path: path}).readTOML()
	if err != nil {
		t.Fatal(err)
	}
	expected := cloneCodexDocument(before)
	expectedServers := expected["mcp_servers"].(map[string]any)
	delete(expectedServers, "serena")
	expectedServers["serena-mcphub"] = map[string]any{
		"url":                 "http://127.0.0.1:9300/mcp",
		"startup_timeout_sec": 10.0,
		"http_headers":        map[string]any{"Authorization": "Bearer test-token"},
	}

	result, err := (&codexCLI{path: path}).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena", TargetEntryName: "serena-mcphub",
		Entry:          MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9300/mcp", Headers: map[string]string{"Authorization": "Bearer test-token"}},
		ExpectedSource: CodexTransportStdio,
	})
	if err != nil || result.Outcome != CodexHTTPRelocationCommitted || result.Readback != CodexHTTPRelocationReadbackExact {
		t.Fatalf("RelocateHTTPEntry result=%#v err=%v", result, err)
	}
	after, err := (&codexCLI{path: path}).readTOML()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, expected) {
		t.Fatalf("decoded document changed outside authorized relocation:\n got: %#v\nwant: %#v", after, expected)
	}
	servers := after["mcp_servers"].(map[string]any)
	if _, present := servers["serena"]; present {
		t.Fatalf("source table remains after relocation: %#v", servers)
	}
	if target, ok := servers["serena-mcphub"].(map[string]any); !ok ||
		!reflect.DeepEqual(target["http_headers"], map[string]any{"Authorization": "Bearer test-token"}) {
		t.Fatalf("target/header shape = %#v", target)
	}
}

func TestCodexRelocationHeaderReadbackRejectsValueAndTypeDrift(t *testing.T) {
	original := []byte(`[mcp_servers.serena]
command = "uvx"
`)
	for name, mutate := range map[string]func(map[string]any){
		"header_value": func(headers map[string]any) { headers["Authorization"] = "Bearer changed" },
		"header_type":  func(headers map[string]any) { headers["Authorization"] = int64(7) },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := (&codexCLI{path: path}).RelocateHTTPEntry(CodexHTTPRelocation{
				SourceEntryName: "serena", TargetEntryName: "serena-mcphub",
				ExpectedSource: CodexTransportStdio,
				Entry:          MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9300/mcp", Headers: map[string]string{"Authorization": "Bearer test-token"}},
				WriteConfig: func(path string, data []byte) error {
					var document map[string]any
					if err := toml.Unmarshal(data, &document); err != nil {
						return err
					}
					headers := document["mcp_servers"].(map[string]any)["serena-mcphub"].(map[string]any)["http_headers"].(map[string]any)
					mutate(headers)
					drifted, err := toml.Marshal(document)
					if err != nil {
						return err
					}
					return os.WriteFile(path, drifted, 0o600)
				},
			})
			if !errors.Is(err, ErrCodexConfigReadbackFailed) || !reflect.DeepEqual(result, CodexHTTPRelocationResult{}) {
				t.Fatalf("RelocateHTTPEntry result=%#v err=%v, want zero/CODEX_CLIENT_CONFIG_READBACK_FAILED", result, err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(after, original) {
				t.Fatalf("header drift rollback bytes=%q err=%v, want %q", after, readErr, original)
			}
		})
	}
}

func TestCodexRelocationReadbackRejectsEveryUntouchedSemanticDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte(`top_level = "preserved"
ordered = [1, 2, 3]

[unrelated]
value = "preserved"

[mcp_servers.serena]
command = "uvx"
args = ["serena"]

[mcp_servers.unrelated]
command = "node"
args = ["server.js"]
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(map[string]any){
		"top_level_scalar": func(document map[string]any) { document["top_level"] = "changed" },
		"array_order":      func(document map[string]any) { document["ordered"] = []any{int64(3), int64(2), int64(1)} },
		"nested_table": func(document map[string]any) {
			document["unrelated"].(map[string]any)["value"] = "changed"
		},
		"unrelated_mcp_field": func(document map[string]any) {
			document["mcp_servers"].(map[string]any)["unrelated"].(map[string]any)["args"] = []any{"changed.js"}
		},
		"unrelated_mcp_table": func(document map[string]any) { delete(document["mcp_servers"].(map[string]any), "unrelated") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := (&codexCLI{path: path}).RelocateHTTPEntry(CodexHTTPRelocation{
				SourceEntryName: "serena",
				TargetEntryName: "serena-mcphub",
				ExpectedSource:  CodexTransportStdio,
				Entry:           MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9300/mcp"},
				WriteConfig: func(path string, data []byte) error {
					var document map[string]any
					if err := toml.Unmarshal(data, &document); err != nil {
						return err
					}
					mutate(document)
					drifted, err := toml.Marshal(document)
					if err != nil {
						return err
					}
					return os.WriteFile(path, drifted, 0o600)
				},
			})
			if !errors.Is(err, ErrCodexConfigReadbackFailed) {
				t.Fatalf("RelocateHTTPEntry error = %v, want CODEX_CLIENT_CONFIG_READBACK_FAILED", err)
			}
			if result.Outcome != "" || result.Readback != "" || result.SourceSnapshot != nil ||
				result.LogicalSource != "" || result.TargetEntry != "" || result.WriteTarget != "" ||
				result.DesiredTransport != "" || result.CollisionReason != "" || result.Action != "" {
				t.Fatalf("RelocateHTTPEntry result = %#v, want zero result", result)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != string(original) {
				t.Fatalf("rollback bytes differ\nwant:\n%s\ngot:\n%s", original, after)
			}
		})
	}
}

func TestCodexTransportPresentMalformedShapeFailsClosed(t *testing.T) {
	for name, config := range map[string]string{
		"transportless": `[mcp_servers.serena]
`,
		"wrong_url_type": `[mcp_servers.serena]
url = 7
`,
		"wrong_command_type": `[mcp_servers.serena]
command = ["uvx"]
`,
		"named_entry_not_table": `[mcp_servers]
serena = "not-a-table"
`,
		"container_not_table": `mcp_servers = "not-a-table"
`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			globalPath := filepath.Join(root, "global", "config.toml")
			if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(globalPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			c := &codexCLI{path: globalPath}
			_, err := c.ResolveTransportTarget(CodexTransportTargetRequest{
				LogicalEntryName: "serena",
				DesiredTransport: CodexTransportHTTP,
				DesiredEntry:     MCPEntry{Name: "serena", URL: "http://127.0.0.1:9300/mcp"},
				ProjectRoot:      root,
				WorkingDir:       root,
			})
			if !errors.Is(err, ErrCodexLayerParseFailed) {
				t.Fatalf("ResolveTransportTarget error = %v, want CODEX_LAYER_PARSE_FAILED", err)
			}
			bytes, readErr := os.ReadFile(globalPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.TrimSpace(string(bytes)) != strings.TrimSpace(config) {
				t.Fatalf("malformed fixture changed\nwant:\n%s\ngot:\n%s", config, bytes)
			}
		})
	}
}

// Catches a matcher that treats a present malformed alias as an ordinary
// ownership conflict instead of propagating the strict transport classifier.
func TestCodexAliasOccupancyMalformedFailsClosedEveryLayer(t *testing.T) {
	malformed := map[string]struct {
		body   string
		reason string
	}{
		"transport_missing":  {body: "", reason: "transport_missing"},
		"transport_mixed":    {body: "url = 'http://127.0.0.1:9300/mcp'\ncommand = 'uvx'\n", reason: "transport_mixed"},
		"url_wrong_type":     {body: "url = 7\n", reason: "transport_type_invalid"},
		"command_wrong_type": {body: "command = ['uvx']\n", reason: "transport_type_invalid"},
	}
	for layer := range map[string]struct{}{"global": {}, "root_project": {}, "nested_project": {}} {
		for shape, fixture := range malformed {
			t.Run(layer+"/"+shape, func(t *testing.T) {
				root := t.TempDir()
				globalPath := filepath.Join(root, "global", "config.toml")
				if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
					t.Fatal(err)
				}
				global := "[mcp_servers.serena]\nurl = 'http://127.0.0.1:9300/mcp'\n"
				if layer == "global" {
					global += "\n[mcp_servers.serena-mcphub]\n" + fixture.body
				}
				if err := os.WriteFile(globalPath, []byte(global), 0o600); err != nil {
					t.Fatal(err)
				}
				rootConfig := "[mcp_servers.serena]\ncommand = 'uvx'\n"
				if layer == "root_project" {
					rootConfig += "\n[mcp_servers.serena-mcphub]\n" + fixture.body
				}
				rootConfigPath := filepath.Join(root, ".codex", "config.toml")
				if err := os.MkdirAll(filepath.Dir(rootConfigPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(rootConfigPath, []byte(rootConfig), 0o600); err != nil {
					t.Fatal(err)
				}
				working := root
				var nestedConfigPath string
				if layer == "nested_project" {
					working = filepath.Join(root, "nested")
					nestedConfigPath = filepath.Join(working, ".codex", "config.toml")
					if err := os.MkdirAll(filepath.Dir(nestedConfigPath), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(nestedConfigPath, []byte("[mcp_servers.serena-mcphub]\n"+fixture.body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				beforeGlobal, err := os.ReadFile(globalPath)
				if err != nil {
					t.Fatal(err)
				}
				beforeRoot, err := os.ReadFile(rootConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				beforeNested, err := os.ReadFile(nestedConfigPath)
				if nestedConfigPath != "" && err != nil {
					t.Fatal(err)
				}

				_, err = (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
					LogicalEntryName: "serena", DesiredTransport: CodexTransportHTTP,
					DesiredEntry: MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9300/mcp"},
					ProjectRoot:  root, WorkingDir: working,
				})
				if !errors.Is(err, ErrCodexLayerParseFailed) || !strings.Contains(err.Error(), fixture.reason) {
					t.Fatalf("ResolveTransportTarget error = %v, want CODEX_LAYER_PARSE_FAILED:%s", err, fixture.reason)
				}
				if errors.Is(err, ErrCodexTargetNameConflict) {
					t.Fatalf("ResolveTransportTarget classified malformed alias as target conflict: %v", err)
				}
				assertCodexLayerRefusalDidNotMutate(t, globalPath, beforeGlobal)
				assertCodexLayerRefusalDidNotMutate(t, rootConfigPath, beforeRoot)
				if nestedConfigPath != "" {
					assertCodexLayerRefusalDidNotMutate(t, nestedConfigPath, beforeNested)
				}
			})
		}
	}
}

// Catches the repeat branch accepting a malformed occupied alias as an
// already-configured relocation.
func TestCodexRelocationRepeatMalformedAliasUsesStrictClassifier(t *testing.T) {
	path := setupCodexConfig(t, "[mcp_servers.serena-mcphub]\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	result, err := (&codexCLI{path: path}).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena", TargetEntryName: "serena-mcphub",
		Entry:          MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9300/mcp"},
		ExpectedSource: CodexTransportStdio, SourceSnapshot: map[string]any{"command": "uvx"},
		WriteConfig: func(string, []byte) error { writes++; return nil },
	})
	if !errors.Is(err, ErrCodexLayerParseFailed) || !strings.Contains(err.Error(), "transport_missing") {
		t.Fatalf("RelocateHTTPEntry error = %v, want CODEX_LAYER_PARSE_FAILED:transport_missing", err)
	}
	if !reflect.DeepEqual(result, CodexHTTPRelocationResult{}) || writes != 0 {
		t.Fatalf("result/writes = %#v/%d, want zero/0", result, writes)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("malformed repeat changed bytes: read=%v before=%q after=%q", readErr, before, after)
	}
}

// Catches the source-present locked apply path returning target conflict before
// the shared strict matcher has classified a malformed occupied alias.
func TestCodexRelocationSourcePresentMalformedTargetUsesStrictClassifier(t *testing.T) {
	path := setupCodexConfig(t, "[mcp_servers.serena]\ncommand = 'uvx'\n\n[mcp_servers.serena-mcphub]\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	result, err := (&codexCLI{path: path}).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena", TargetEntryName: "serena-mcphub",
		Entry:          MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9300/mcp"},
		ExpectedSource: CodexTransportStdio,
		WriteConfig:    func(string, []byte) error { writes++; return nil },
	})
	if !errors.Is(err, ErrCodexLayerParseFailed) || !strings.Contains(err.Error(), "transport_missing") {
		t.Fatalf("RelocateHTTPEntry error = %v, want CODEX_LAYER_PARSE_FAILED:transport_missing", err)
	}
	if errors.Is(err, ErrCodexTargetNameConflict) {
		t.Fatalf("RelocateHTTPEntry classified malformed target as conflict: %v", err)
	}
	if !reflect.DeepEqual(result, CodexHTTPRelocationResult{}) || writes != 0 {
		t.Fatalf("result/writes = %#v/%d, want zero/0", result, writes)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("malformed source-present target changed bytes: read=%v before=%q after=%q", readErr, before, after)
	}
}

// Catches the inverse owner check downgrading a malformed target to a generic
// target conflict before the strict classifier runs.
func TestCodexInverseRelocationMalformedAliasUsesStrictClassifier(t *testing.T) {
	path := setupCodexConfig(t, "[mcp_servers.serena-mcphub]\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	result, err := (&codexCLI{path: path}).RestoreRelocatedHTTPEntry(CodexHTTPInverseRelocation{
		SourceEntryName: "serena", TargetEntryName: "serena-mcphub",
		Target:         MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9300/mcp"},
		SourceSnapshot: map[string]any{"command": "uvx"},
		WriteConfig:    func(string, []byte) error { writes++; return nil },
	})
	if !errors.Is(err, ErrCodexLayerParseFailed) || !strings.Contains(err.Error(), "transport_missing") {
		t.Fatalf("RestoreRelocatedHTTPEntry error = %v, want CODEX_LAYER_PARSE_FAILED:transport_missing", err)
	}
	if result != (CodexHTTPInverseResult{}) || writes != 0 {
		t.Fatalf("result/writes = %#v/%d, want zero/0", result, writes)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("malformed inverse changed bytes: read=%v before=%q after=%q", readErr, before, after)
	}
}

// Catches a rollback path that reports readback drift without also making the
// failed exact restoration observable through CODEX_CLIENT_CONFIG_ROLLBACK_INCOMPLETE.
func TestCodexRelocationRollbackReadbackMismatchJoinsFailureIDs(t *testing.T) {
	path := setupCodexConfig(t, "[mcp_servers.serena]\ncommand = 'uvx'\n")
	previousWriter := WriteConfigFile
	WriteConfigFile = func(path string, _ []byte) error {
		return os.WriteFile(path, []byte("rollback = 'not-the-snapshot'\n"), 0o600)
	}
	t.Cleanup(func() { WriteConfigFile = previousWriter })
	result, err := (&codexCLI{path: path}).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena", TargetEntryName: "serena-mcphub",
		ExpectedSource: CodexTransportStdio,
		Entry:          MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9300/mcp"},
		WriteConfig: func(path string, data []byte) error {
			var doc map[string]any
			if err := toml.Unmarshal(data, &doc); err != nil {
				return err
			}
			doc["drift"] = "forward"
			drifted, err := toml.Marshal(doc)
			if err != nil {
				return err
			}
			return os.WriteFile(path, drifted, 0o600)
		},
	})
	if !errors.Is(err, ErrCodexConfigReadbackFailed) || !errors.Is(err, ErrCodexConfigRollbackIncomplete) {
		t.Fatalf("RelocateHTTPEntry error = %v, want joined readback and rollback-incomplete failures", err)
	}
	if !reflect.DeepEqual(result, CodexHTTPRelocationResult{}) {
		t.Fatalf("RelocateHTTPEntry result = %#v, want zero result", result)
	}
}
