package clients

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// T15: every adopt-reachable adapter dispatches classification through its own
// physical section and recognizer projection. The shared classifier owns all
// verdict logic, so this table also compile-proves the complete allowlist.
func TestClassifyEntryUnderLockPerAdapter(t *testing.T) {
	for _, tc := range casAdapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			concrete := tc.build(t)
			wrapped := newLockingClient(concrete)
			mut, ok := AsCASEntryMutator(wrapped)
			if !ok {
				t.Fatalf("AsCASEntryMutator(%s) rejected adopt-reachable adapter", tc.name)
			}

			snapshotSubtree, present, err := mut.EntryRawSubtree(tc.native, "serena")
			if err != nil || !present {
				t.Fatalf("EntryRawSubtree(snapshot): subtree=%v present=%v err=%v", snapshotSubtree, present, err)
			}

			if err := wrapped.AddEntry(tc.hubEntry); err != nil {
				t.Fatalf("seed hub entry: %v", err)
			}
			before, err := os.ReadFile(wrapped.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			verdict, err := mut.ClassifyEntryUnderLock("serena", tc.match, snapshotSubtree)
			if err != nil || verdict != ClassifyStillHub {
				t.Fatalf("hub-present classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyStillHub)
			}
			after, err := os.ReadFile(wrapped.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("classification mutated config:\n before: %s\n after: %s", before, after)
			}

			if err := os.WriteFile(wrapped.ConfigPath(), tc.native, 0600); err != nil {
				t.Fatal(err)
			}
			verdict, err = mut.ClassifyEntryUnderLock("serena", tc.match, snapshotSubtree)
			if err != nil || verdict != ClassifyRestoreDone {
				t.Fatalf("restored classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyRestoreDone)
			}

			operatorChanged := bytes.ReplaceAll(tc.native, []byte("native-mcp-cmd"), []byte("operator-mcp-cmd"))
			if bytes.Equal(operatorChanged, tc.native) {
				t.Fatal("test fixture did not produce a distinct operator subtree")
			}
			if err := os.WriteFile(wrapped.ConfigPath(), operatorChanged, 0600); err != nil {
				t.Fatal(err)
			}
			verdict, err = mut.ClassifyEntryUnderLock("serena", tc.match, snapshotSubtree)
			if err != nil || verdict != ClassifyGenuineConflict {
				t.Fatalf("operator-changed classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyGenuineConflict)
			}
		})
	}
}

// Codex's physical de-adopt preview must project the same HTTP timeout that
// the guarded mutation re-reads. Otherwise an exact managed entry can be
// previewed as changed even though its strict CAS matcher accepts it.
func TestCodexClassifyEntryUnderLockProjectsToolTimeoutSec(t *testing.T) {
	matchTimeout := func(timeout int) func(*MCPEntry) bool {
		return func(entry *MCPEntry) bool {
			return entry != nil && entry.URL == casHubURL && entry.ToolTimeoutSec == timeout
		}
	}
	configForTimeout := func(timeoutLine string) string {
		return "[mcp_servers.serena]\n" +
			"url = \"" + casHubURL + "\"\n" +
			"startup_timeout_sec = 10\n" + timeoutLine
	}

	t.Run("timeout-30-matches-classification-and-guarded-read", func(t *testing.T) {
		c := &codexCLI{path: casWriteCfg(t, "config.toml", configForTimeout("tool_timeout_sec = 30\n"))}
		entry, err := c.GetEntry("serena")
		if err != nil || entry == nil || entry.ToolTimeoutSec != 30 {
			t.Fatalf("GetEntry timeout = (%+v, %v), want timeout 30", entry, err)
		}

		verdict, err := c.ClassifyEntryUnderLock("serena", matchTimeout(30), nil)
		if err != nil || verdict != ClassifyStillHub {
			t.Fatalf("timeout-30 classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyStillHub)
		}
	})

	t.Run("absent-timeout-projects-zero", func(t *testing.T) {
		c := &codexCLI{path: casWriteCfg(t, "config.toml", configForTimeout(""))}
		entry, err := c.GetEntry("serena")
		if err != nil || entry == nil || entry.ToolTimeoutSec != 0 {
			t.Fatalf("GetEntry absent timeout = (%+v, %v), want timeout 0", entry, err)
		}

		verdict, err := c.ClassifyEntryUnderLock("serena", matchTimeout(0), nil)
		if err != nil || verdict != ClassifyStillHub {
			t.Fatalf("absent-timeout classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyStillHub)
		}
	})

	t.Run("real-timeout-mismatch-remains-conflict-and-refuses-remove", func(t *testing.T) {
		c := &codexCLI{path: casWriteCfg(t, "config.toml", configForTimeout("tool_timeout_sec = 31\n"))}
		before, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}

		verdict, err := c.ClassifyEntryUnderLock("serena", matchTimeout(30), nil)
		if err != nil || verdict != ClassifyGenuineConflict {
			t.Fatalf("timeout mismatch classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyGenuineConflict)
		}
		if err := c.CASGuardedRemoveEntry("serena", matchTimeout(30)); !errors.Is(err, ErrCASConflict) {
			t.Fatalf("timeout mismatch remove = %v, want ErrCASConflict", err)
		}
		after, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatalf("timeout mismatch remove mutated config:\n before: %s\n after: %s", before, after)
		}
	})
}

// T16: a lower MiMoCode layer re-emerges in GetEntry after the hub's write-target
// entry is removed. Classification must ignore that merged view and judge the
// physical write target absent/restored.
func TestClassifyEntryUnderLockMimoCodeUsesWriteTargetPhysical(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTarget := filepath.Join(dir, "mimocode.json")
	if err := os.WriteFile(writeTarget, []byte(`{"mcp":{"serena":{"type":"remote","url":"`+casHubURL+`","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"mcp":{"serena":{"type":"local","command":"operator-lower"}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	concrete := &mimoCodeClient{path: writeTarget, claudeHome: ""}
	wrapped := newLockingClient(concrete)
	mut, ok := AsCASEntryMutator(wrapped)
	if !ok {
		t.Fatal("mimocode must expose CASEntryMutator")
	}
	if err := mut.CASGuardedRemoveEntry("serena", casURLMatch); err != nil {
		t.Fatalf("remove write-target hub entry: %v", err)
	}
	merged, err := concrete.GetEntry("serena")
	if err != nil {
		t.Fatalf("merged GetEntry: %v", err)
	}
	if merged == nil || casURLMatch(merged) {
		t.Fatalf("precondition: lower operator entry must re-emerge as non-hub in merged view, got %+v", merged)
	}

	verdict, err := mut.ClassifyEntryUnderLock("serena", casURLMatch, nil)
	if err != nil || verdict != ClassifyRestoreDone {
		t.Fatalf("write-target-physical classify = (%v, %v), want (%v, nil); merged view would be a false conflict", verdict, err, ClassifyRestoreDone)
	}
}

// T17/P3-a: absence is a clean empty config state, and the read-selection lock
// must not create the missing parent directory or a lock file.
func TestClassifyEntryUnderLockAbsentConfigIsNotUnreadableAndHasNoFSSideEffects(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "missing")
	configPath := filepath.Join(configDir, "settings.json")
	wrapped := newLockingClient(&claudeCode{path: configPath})
	mut, ok := AsCASEntryMutator(wrapped)
	if !ok {
		t.Fatal("claude-code must expose CASEntryMutator")
	}

	verdict, err := mut.ClassifyEntryUnderLock("serena", casURLMatch, nil)
	if err != nil || verdict != ClassifyRestoreDone {
		t.Fatalf("absent-origin classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyRestoreDone)
	}
	var typedNilSnapshot map[string]any
	verdict, err = mut.ClassifyEntryUnderLock("serena", casURLMatch, typedNilSnapshot)
	if err != nil || verdict != ClassifyRestoreDone {
		t.Fatalf("absent-origin typed-nil snapshot classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyRestoreDone)
	}
	snapshotSubtree, present, err := mut.EntryRawSubtree([]byte(`{"mcpServers":{"serena":{"command":"native-mcp-cmd"}}}`), "serena")
	if err != nil || !present {
		t.Fatalf("extract present snapshot: present=%v err=%v", present, err)
	}
	verdict, err = mut.ClassifyEntryUnderLock("serena", casURLMatch, snapshotSubtree)
	if err != nil || verdict != ClassifyGenuineConflict {
		t.Fatalf("absent live vs present snapshot classify = (%v, %v), want (%v, nil)", verdict, err, ClassifyGenuineConflict)
	}

	for _, path := range []string{configDir, configPath, configPath + ".lock"} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("read-only classify created %s: stat err=%v", path, statErr)
		}
	}
}

func TestClassifyEntryUnderLockMalformedPresentConfigIsUnreadable(t *testing.T) {
	path := casWriteCfg(t, "settings.json", `{this is not json`)
	mut := newLockingClient(&claudeCode{path: path}).(CASEntryMutator)
	verdict, err := mut.ClassifyEntryUnderLock("serena", casURLMatch, nil)
	if err == nil || verdict != ClassifyUnreadable {
		t.Fatalf("malformed present config classify = (%v, %v), want (%v, non-nil error)", verdict, err, ClassifyUnreadable)
	}
}
