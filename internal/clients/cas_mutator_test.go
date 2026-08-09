package clients

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const casHubURL = "http://localhost:9121/mcp"

// casURLMatch recognizes the URL-native hub entry (the injected recognizer stand-in
// for liveEntryMatchesManifestBinding). Nil-guards live before deref, mirroring the
// real recognizer contract.
func casURLMatch(e *MCPEntry) bool { return e != nil && e.URL == casHubURL }

// casRelayMatch recognizes the antigravity relay hub entry by RelayServer.
func casRelayMatch(e *MCPEntry) bool { return e != nil && e.RelayServer == "serena" }

func casWriteCfg(t *testing.T, filename, initial string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

// --- Thorough gate-branch coverage on one representative adapter (claude-code) ---
// The gate LOGIC is single-owned in casRestoreFromBytes/casGuardedRemove, so one
// adapter exercises every branch; the per-adapter table below proves dispatch.

func TestCASRestoreGateBranches(t *testing.T) {
	native := []byte(`{"mcpServers":{"serena":{"command":"native-mcp-cmd","args":["start"]}}}`)
	absentSnap := []byte(`{"mcpServers":{}}`)
	hubCfg := `{"mcpServers":{"serena":{"url":"` + casHubURL + `"}}}`
	foreignCfg := `{"mcpServers":{"serena":{"url":"http://evil.example/mcp"}}}`

	t.Run("nil-live-refuses-no-resurrection", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", `{"mcpServers":{}}`)}
		err := c.CASRestoreEntryFromBytes("serena", casURLMatch, native)
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("nil live: err=%v, want ErrCASConflict", err)
		}
		// The empty slot stays empty — no resurrection.
		if e, _ := c.GetEntry("serena"); e != nil {
			t.Errorf("nil-live restore must not create an entry, got %+v", e)
		}
	})

	t.Run("mismatch-refuses-and-preserves", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", foreignCfg)}
		err := c.CASRestoreEntryFromBytes("serena", casURLMatch, native)
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("mismatch: err=%v, want ErrCASConflict", err)
		}
		raw, _ := os.ReadFile(c.path)
		if !strings.Contains(string(raw), "evil.example") {
			t.Errorf("mismatch restore must leave the operator entry intact, got %s", raw)
		}
		if strings.Contains(string(raw), "native-mcp-cmd") {
			t.Errorf("mismatch restore must NOT write the snapshot, got %s", raw)
		}
	})

	t.Run("snapshot-absent-fails-closed-no-remove", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", hubCfg)}
		err := c.CASRestoreEntryFromBytes("serena", casURLMatch, absentSnap)
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("snapshot-absent: err=%v, want ErrCASConflict (B5 fail-closed)", err)
		}
		// B5: restore never removes — the hub entry must still be there.
		if e, _ := c.GetEntry("serena"); e == nil || e.URL != casHubURL {
			t.Errorf("snapshot-absent must leave the live hub entry intact, got %+v", e)
		}
	})

	t.Run("present-and-match-restores-native", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", hubCfg)}
		if err := c.CASRestoreEntryFromBytes("serena", casURLMatch, native); err != nil {
			t.Fatalf("present+match restore: %v", err)
		}
		raw, _ := os.ReadFile(c.path)
		if !strings.Contains(string(raw), "native-mcp-cmd") {
			t.Errorf("restore must write the native snapshot entry, got %s", raw)
		}
		if e, _ := c.GetEntry("serena"); e != nil && casURLMatch(e) {
			t.Errorf("after restore the entry must no longer be the hub entry, got %+v", e)
		}
	})

	t.Run("nil-recognizer-fails-closed", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", hubCfg)}
		err := c.CASRestoreEntryFromBytes("serena", nil, native)
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("nil recognizer: err=%v, want ErrCASConflict (no panic)", err)
		}
		// Live untouched.
		if e, _ := c.GetEntry("serena"); e == nil || e.URL != casHubURL {
			t.Errorf("nil-recognizer restore must not mutate, got %+v", e)
		}
	})
}

func TestCASRollbackBypassRestoreGateBranches(t *testing.T) {
	const liveRouterURL = "http://127.0.0.1:9137/serena/mcp"
	const legacyHubURL = "http://localhost:9121/mcp"
	liveCfg := `{"mcpServers":{"serena":{"url":"` + liveRouterURL + `"}}}`
	legacySnapshot := []byte(`{"mcpServers":{"serena":{"url":"` + legacyHubURL + `"}}}`)
	matchLiveRouter := func(e *MCPEntry) bool {
		return e != nil && e.URL == liveRouterURL
	}

	t.Run("ordinary-restore-keeps-demigration-guard", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", liveCfg)}
		err := c.CASRestoreEntryFromBytes("serena", matchLiveRouter, legacySnapshot)
		if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
			t.Fatalf("ordinary CAS restore err=%v, want ErrBackupEntryAlreadyMigrated", err)
		}
		entry, getErr := c.GetEntry("serena")
		if getErr != nil {
			t.Fatal(getErr)
		}
		if entry == nil || entry.URL != liveRouterURL {
			t.Fatalf("ordinary refusal changed the live entry: %+v", entry)
		}
	})

	t.Run("rollback-restore-allows-exact-legacy-hub-snapshot", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", liveCfg)}
		if err := c.CASRestoreEntryFromBytesForRollback("serena", matchLiveRouter, legacySnapshot); err != nil {
			t.Fatalf("rollback CAS restore: %v", err)
		}
		entry, getErr := c.GetEntry("serena")
		if getErr != nil {
			t.Fatal(getErr)
		}
		if entry == nil || entry.URL != legacyHubURL {
			t.Fatalf("rollback did not restore the legacy hub snapshot: %+v", entry)
		}
	})

	t.Run("rollback-still-refuses-concurrent-edit", func(t *testing.T) {
		const operatorURL = "https://operator.example/serena/mcp"
		c := &claudeCode{path: casWriteCfg(t, "cfg.json",
			`{"mcpServers":{"serena":{"url":"`+operatorURL+`"}}}`)}
		err := c.CASRestoreEntryFromBytesForRollback("serena", matchLiveRouter, legacySnapshot)
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("rollback CAS restore after concurrent edit err=%v, want ErrCASConflict", err)
		}
		entry, getErr := c.GetEntry("serena")
		if getErr != nil {
			t.Fatal(getErr)
		}
		if entry == nil || entry.URL != operatorURL {
			t.Fatalf("rollback overwrote the operator entry: %+v", entry)
		}
	})
}

func TestCASRemoveGateBranches(t *testing.T) {
	hubCfg := `{"mcpServers":{"serena":{"url":"` + casHubURL + `"}}}`
	foreignCfg := `{"mcpServers":{"serena":{"url":"http://evil.example/mcp"}}}`

	t.Run("nil-live-idempotent-success", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", `{"mcpServers":{}}`)}
		if err := c.CASGuardedRemoveEntry("serena", casURLMatch); err != nil {
			t.Fatalf("nil-live remove must be idempotent success, got %v", err)
		}
	})

	t.Run("mismatch-refuses-and-preserves", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", foreignCfg)}
		err := c.CASGuardedRemoveEntry("serena", casURLMatch)
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("mismatch remove: err=%v, want ErrCASConflict", err)
		}
		if e, _ := c.GetEntry("serena"); e == nil {
			t.Errorf("mismatch remove must preserve the operator entry")
		}
	})

	t.Run("match-removes", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", hubCfg)}
		if err := c.CASGuardedRemoveEntry("serena", casURLMatch); err != nil {
			t.Fatalf("match remove: %v", err)
		}
		if e, _ := c.GetEntry("serena"); e != nil {
			t.Errorf("match remove must delete the hub entry, got %+v", e)
		}
	})

	t.Run("nil-recognizer-fails-closed", func(t *testing.T) {
		c := &claudeCode{path: casWriteCfg(t, "cfg.json", hubCfg)}
		err := c.CASGuardedRemoveEntry("serena", nil)
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("nil recognizer: err=%v, want ErrCASConflict (no panic)", err)
		}
		if e, _ := c.GetEntry("serena"); e == nil {
			t.Errorf("nil-recognizer remove must not mutate")
		}
	})
}

func TestMimoCodeCASCheckActDivergenceRefusesAndPreservesWriteTarget(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTarget := filepath.Join(dir, "mimocode.json")
	writeTargetBefore := []byte(`{"mcp":{"serena":{"type":"local","command":"operator-native"}}}`)
	if err := os.WriteFile(writeTarget, writeTargetBefore, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "mimocode.jsonc"),
		[]byte(`{"mcp":{"serena":{"type":"remote","url":"`+casHubURL+`","enabled":true}}}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	c := &mimoCodeClient{path: writeTarget, claudeHome: ""}

	// Prove the divergence precondition: the merged view is hub-shaped even
	// though the physical write target is the operator-native entry above.
	if merged, err := c.GetEntry("serena"); err != nil {
		t.Fatalf("read merged serena: %v", err)
	} else if !casURLMatch(merged) {
		t.Fatalf("merged serena must be the higher-layer hub entry, got %+v", merged)
	}

	snapshot := []byte(`{"mcp":{"serena":{"type":"local","command":"snapshot-native"}}}`)
	if err := c.CASRestoreEntryFromBytes("serena", casURLMatch, snapshot); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("restore divergence: err=%v, want ErrCASConflict", err)
	}
	if got, err := os.ReadFile(writeTarget); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, writeTargetBefore) {
		t.Fatalf("restore divergence mutated write target:\n got: %s\nwant: %s", got, writeTargetBefore)
	}

	if err := c.CASGuardedRemoveEntry("serena", casURLMatch); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("remove divergence: err=%v, want ErrCASConflict", err)
	}
	if got, err := os.ReadFile(writeTarget); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, writeTargetBefore) {
		t.Fatalf("remove divergence mutated write target:\n got: %s\nwant: %s", got, writeTargetBefore)
	}
	if _, present, err := mimoCodeFileEntryValue(writeTarget, "serena"); err != nil {
		t.Fatal(err)
	} else if !present {
		t.Fatal("remove divergence deleted the write-target serena entry")
	}

	// Exercise the higher-layer hook itself: even when the physical write target
	// matches the recognizer, the winning jsonc layer must pre-refuse both writes.
	hubWriteTarget := []byte(`{"mcp":{"serena":{"type":"remote","url":"` + casHubURL + `","enabled":true}}}`)
	if err := os.WriteFile(writeTarget, hubWriteTarget, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.CASRestoreEntryFromBytes("serena", casURLMatch, snapshot); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("shadowed restore: err=%v, want ErrCASConflict", err)
	}
	if got, err := os.ReadFile(writeTarget); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, hubWriteTarget) {
		t.Fatalf("shadowed restore mutated write target:\n got: %s\nwant: %s", got, hubWriteTarget)
	}
	if err := c.CASGuardedRemoveEntry("serena", casURLMatch); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("shadowed remove: err=%v, want ErrCASConflict", err)
	}
	if got, err := os.ReadFile(writeTarget); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, hubWriteTarget) {
		t.Fatalf("shadowed remove mutated write target:\n got: %s\nwant: %s", got, hubWriteTarget)
	}
}

func TestMimoCodeCASRemoveAllowsDisableOnlyHigherLayer(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTarget := filepath.Join(dir, "mimocode.json")
	if err := os.WriteFile(
		writeTarget,
		[]byte(`{"mcp":{"serena":{"type":"remote","url":"`+casHubURL+`","enabled":true}}}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "mimocode.jsonc"),
		[]byte(`{"mcp":{"serena":{"enabled":false}}}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	c := &mimoCodeClient{path: writeTarget, claudeHome: ""}

	if err := c.CASGuardedRemoveEntry("serena", casURLMatch); err != nil {
		t.Fatalf("disable-only shadow must not block CAS remove: %v", err)
	}
	if _, present, err := mimoCodeFileEntryValue(writeTarget, "serena"); err != nil {
		t.Fatal(err)
	} else if present {
		t.Fatal("CAS remove left the write-target serena entry present")
	}
}

func TestCASRecognizerPanicFailsClosedWithoutMutation(t *testing.T) {
	hubCfg := []byte(`{"mcpServers":{"serena":{"url":"` + casHubURL + `"}}}`)
	nativeSnapshot := []byte(`{"mcpServers":{"serena":{"command":"native-mcp-cmd"}}}`)
	panicMatch := func(*MCPEntry) bool { panic("recognizer boom") }

	t.Run("restore", func(t *testing.T) {
		path := casWriteCfg(t, "settings.json", string(hubCfg))
		c := &claudeCode{path: path}
		if err := c.CASRestoreEntryFromBytes("serena", panicMatch, nativeSnapshot); !errors.Is(err, ErrCASConflict) {
			t.Fatalf("panicking restore recognizer: err=%v, want ErrCASConflict", err)
		}
		if got, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		} else if !bytes.Equal(got, hubCfg) {
			t.Fatalf("panicking restore recognizer mutated config:\n got: %s\nwant: %s", got, hubCfg)
		}
	})

	t.Run("remove", func(t *testing.T) {
		path := casWriteCfg(t, "settings.json", string(hubCfg))
		c := &claudeCode{path: path}
		if err := c.CASGuardedRemoveEntry("serena", panicMatch); !errors.Is(err, ErrCASConflict) {
			t.Fatalf("panicking remove recognizer: err=%v, want ErrCASConflict", err)
		}
		if got, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		} else if !bytes.Equal(got, hubCfg) {
			t.Fatalf("panicking remove recognizer mutated config:\n got: %s\nwant: %s", got, hubCfg)
		}
	})
}

// --- Per-adapter dispatch: proves each adopt-reachable adapter's CAS methods
// resolve its OWN GetEntry + compose its OWN (or promoted-base) restore core. ---

type casAdapterCase struct {
	name     string
	build    func(t *testing.T) Client // empty config on a temp path
	hubEntry MCPEntry
	match    func(*MCPEntry) bool
	native   []byte // snapshot bytes with the native pre-adopt entry
	absent   []byte // snapshot bytes lacking the entry
}

func casAdapterCases() []casAdapterCase {
	jsonNative := func(section string) []byte {
		return []byte(`{"` + section + `":{"serena":{"command":"native-mcp-cmd","args":["start"]}}}`)
	}
	jsonAbsent := func(section string) []byte { return []byte(`{"` + section + `":{}}`) }
	urlHub := MCPEntry{Name: "serena", URL: casHubURL}

	return []casAdapterCase{
		{
			name:     "claude-code",
			build:    func(t *testing.T) Client { return &claudeCode{path: casWriteCfg(t, "settings.json", "{}")} },
			hubEntry: urlHub, match: casURLMatch,
			native: jsonNative("mcpServers"), absent: jsonAbsent("mcpServers"),
		},
		{
			name:     "codex-cli",
			build:    func(t *testing.T) Client { return &codexCLI{path: casWriteCfg(t, "config.toml", "")} },
			hubEntry: urlHub, match: casURLMatch,
			native: []byte("[mcp_servers.serena]\ncommand = \"native-mcp-cmd\"\nargs = [\"start\"]\n"),
			absent: []byte("[mcp_servers]\n"),
		},
		{
			name: "cursor",
			build: func(t *testing.T) Client {
				p := casWriteCfg(t, "mcp.json", "{}")
				return &cursorClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "cursor", urlField: "url"}}
			},
			hubEntry: urlHub, match: casURLMatch,
			native: jsonNative("mcpServers"), absent: jsonAbsent("mcpServers"),
		},
		{
			name: "gemini-cli",
			build: func(t *testing.T) Client {
				p := casWriteCfg(t, "settings.json", "{}")
				return &geminiCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "gemini-cli", urlField: "url"}}
			},
			hubEntry: urlHub, match: casURLMatch,
			native: jsonNative("mcpServers"), absent: jsonAbsent("mcpServers"),
		},
		{
			name: "qwen-cli",
			build: func(t *testing.T) Client {
				p := casWriteCfg(t, "settings.json", "{}")
				return &qwenCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "qwen-cli", urlField: "httpUrl"}}
			},
			hubEntry: urlHub, match: casURLMatch,
			native: jsonNative("mcpServers"), absent: jsonAbsent("mcpServers"),
		},
		{
			name:     "vscode",
			build:    func(t *testing.T) Client { return &vscodeClient{path: casWriteCfg(t, "mcp.json", "{}")} },
			hubEntry: urlHub, match: casURLMatch,
			native: jsonNative("servers"), absent: jsonAbsent("servers"),
		},
		{
			name:     "opencode",
			build:    func(t *testing.T) Client { return &openCodeClient{path: casWriteCfg(t, "opencode.json", "{}")} },
			hubEntry: urlHub, match: casURLMatch,
			native: jsonNative("mcp"), absent: jsonAbsent("mcp"),
		},
		{
			name:     "mimocode",
			build:    func(t *testing.T) Client { return &mimoCodeClient{path: casWriteCfg(t, "mimocode.json", "{}")} },
			hubEntry: urlHub, match: casURLMatch,
			native: jsonNative("mcp"), absent: jsonAbsent("mcp"),
		},
		{
			name: "antigravity",
			build: func(t *testing.T) Client {
				p := casWriteCfg(t, "mcp_config.json", "{}")
				return &antigravityClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "antigravity", urlField: "command"}}
			},
			hubEntry: MCPEntry{Name: "serena", RelayServer: "serena", RelayDaemon: "claude",
				RelayExePath: filepath.Join(os.TempDir(), "mcphub.exe")},
			match:  casRelayMatch,
			native: jsonNative("mcpServers"), absent: jsonAbsent("mcpServers"),
		},
	}
}

func TestCASPerAdapterRestore(t *testing.T) {
	for _, tc := range casAdapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.build(t)
			if err := c.AddEntry(tc.hubEntry); err != nil {
				t.Fatalf("seed hub entry: %v", err)
			}
			mut, ok := c.(CASEntryMutator)
			if !ok {
				t.Fatalf("%s concrete must satisfy CASEntryMutator", tc.name)
			}
			// snapshot-absent fails closed (never removes).
			if err := mut.CASRestoreEntryFromBytes("serena", tc.match, tc.absent); !errors.Is(err, ErrCASConflict) {
				t.Fatalf("snapshot-absent: err=%v, want ErrCASConflict", err)
			}
			if e, _ := c.GetEntry("serena"); e == nil || !tc.match(e) {
				t.Fatalf("snapshot-absent must leave the hub entry intact, got %+v", e)
			}
			// present+match restores the native entry.
			if err := mut.CASRestoreEntryFromBytes("serena", tc.match, tc.native); err != nil {
				t.Fatalf("restore: %v", err)
			}
			raw, _ := os.ReadFile(c.ConfigPath())
			if !strings.Contains(string(raw), "native-mcp-cmd") {
				t.Errorf("restore must write native entry, got %s", raw)
			}
			if e, _ := c.GetEntry("serena"); e != nil && tc.match(e) {
				t.Errorf("after restore the entry must not be the hub entry, got %+v", e)
			}
		})
	}
}

func TestCASPerAdapterRemove(t *testing.T) {
	for _, tc := range casAdapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.build(t)
			mut := c.(CASEntryMutator)
			// remove on an empty config is idempotent success.
			if err := mut.CASGuardedRemoveEntry("serena", tc.match); err != nil {
				t.Fatalf("idempotent remove on empty: %v", err)
			}
			// seed hub then remove.
			if err := c.AddEntry(tc.hubEntry); err != nil {
				t.Fatalf("seed hub entry: %v", err)
			}
			if err := mut.CASGuardedRemoveEntry("serena", tc.match); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if e, _ := c.GetEntry("serena"); e != nil {
				t.Errorf("remove must delete the hub entry, got %+v", e)
			}
		})
	}
}

// --- Lock ownership: the lockingClient forwarder holds withConfigLock; the
// concrete body runs under it and is lock-free. A concrete body that re-entered
// withConfigLock would self-deadlock the non-reentrant per-path mutex — so a
// forwarded call that COMPLETES is the proof of correct lock ownership. ---

func TestCASLockingClientForwarderNoDeadlock(t *testing.T) {
	hubCfg := `{"mcpServers":{"serena":{"url":"` + casHubURL + `"}}}`
	native := []byte(`{"mcpServers":{"serena":{"command":"native-mcp-cmd"}}}`)

	wrapped := newLockingClient(&claudeCode{path: casWriteCfg(t, "settings.json", hubCfg)})
	mut, ok := wrapped.(CASEntryMutator)
	if !ok {
		t.Fatal("lockingClient must satisfy CASEntryMutator via its forwarders")
	}
	// Restore through the wrapper (holds the lock, dispatches the lock-free body).
	if err := mut.CASRestoreEntryFromBytes("serena", casURLMatch, native); err != nil {
		t.Fatalf("forwarded restore under lock: %v", err)
	}
	if raw, _ := os.ReadFile(wrapped.ConfigPath()); !strings.Contains(string(raw), "native-mcp-cmd") {
		t.Errorf("forwarded restore must have written native entry, got %s", raw)
	}

	// Remove through the wrapper.
	wrapped2 := newLockingClient(&claudeCode{path: casWriteCfg(t, "settings.json", hubCfg)})
	mut2 := wrapped2.(CASEntryMutator)
	if err := mut2.CASGuardedRemoveEntry("serena", casURLMatch); err != nil {
		t.Fatalf("forwarded remove under lock: %v", err)
	}
	if e, _ := wrapped2.GetEntry("serena"); e != nil {
		t.Errorf("forwarded remove must delete the entry, got %+v", e)
	}
}

// --- Allowlist: windsurf (a jsonMCPClient embedder that is NOT adopt-reachable)
// must NOT be treated as CAS-capable at the consuming site. ---

func newWindsurfConcrete(t *testing.T) *windsurfClient {
	t.Helper()
	p := casWriteCfg(t, "mcp_config.json", "{}")
	return &windsurfClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "windsurf", urlField: windsurfURLField}}
}

func TestCASAllowlistExcludesWindsurf(t *testing.T) {
	ws := newWindsurfConcrete(t)

	// 1. Structural: the windsurf CONCRETE does NOT satisfy CASEntryMutator,
	//    because CAS methods live on each adopt-reachable concrete, never on the
	//    shared jsonMCPClient base windsurf embeds. This is the load-bearing gate.
	if _, ok := interface{}(ws).(CASEntryMutator); ok {
		t.Fatal("windsurfClient must NOT satisfy CASEntryMutator (CAS must not be on the shared base)")
	}

	// 2. The trap this defends against: the lockingClient WRAPPER satisfies
	//    CASEntryMutator via its forwarders regardless of the wrapped concrete,
	//    so a bare assert would wrongly succeed for a windsurf-wrapped client.
	wrapped := newLockingClient(ws)
	if _, ok := wrapped.(CASEntryMutator); !ok {
		t.Fatal("precondition: the lockingClient wrapper satisfies CASEntryMutator via forwarders (that is why AsCASEntryMutator is needed)")
	}

	// 3. AsCASEntryMutator is the correct gate: it inspects the CONCRETE and
	//    returns (nil,false) for windsurf, wrapped or bare.
	if m, ok := AsCASEntryMutator(wrapped); ok || m != nil {
		t.Errorf("AsCASEntryMutator(wrapped windsurf) = (%v,%v), want (nil,false)", m, ok)
	}
	if m, ok := AsCASEntryMutator(ws); ok || m != nil {
		t.Errorf("AsCASEntryMutator(bare windsurf) = (%v,%v), want (nil,false)", m, ok)
	}
}

func TestCASAllowlistAdmitsAdoptReachable(t *testing.T) {
	for _, tc := range casAdapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Wrap in the locking decorator as production does; AsCASEntryMutator
			// must admit it AND return the lock-holding wrapper (not the concrete).
			wrapped := newLockingClient(tc.build(t))
			mut, ok := AsCASEntryMutator(wrapped)
			if !ok || mut == nil {
				t.Fatalf("AsCASEntryMutator(%s) must admit an adopt-reachable adapter", tc.name)
			}
			if _, isLocking := mut.(*lockingClient); !isLocking {
				t.Errorf("AsCASEntryMutator(%s) must return the lock-holding wrapper, got %T", tc.name, mut)
			}
		})
	}
}
