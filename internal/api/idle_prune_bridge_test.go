package api

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PruneFixtureForExternalTest exposes the existing hermetic register harness
// to the external GUI-to-API regression. It is compiled only for api tests.
type PruneFixtureForExternalTest struct {
	Workspace, RegistryPath, IntentPath, StateDir string
	h                                             *registerHarness
	effects                                       *int
}

func NewPruneFixtureForExternalTest(t *testing.T) *PruneFixtureForExternalTest {
	t.Helper()
	h := newRegisterHarness(t)
	t.Cleanup(h.restore)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	stateDir := filepath.Dir(h.regPath)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	workspace := t.TempDir()
	canonical := mustCanonical(t, workspace)
	key := WorkspaceKey(canonical)
	oldPath := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return h.regPath, nil }
	t.Cleanup(func() { defaultRegistryPathFn = oldPath })
	oldKill, oldForce, oldReconcile := killByPortFn, forceKillByPortFn, registerSupervisorReconcileFn
	effects := new(int)
	killByPortFn = func(int, time.Duration) error { *effects++; return nil }
	forceKillByPortFn = func(int, time.Duration) (portKillOutcome, error) { *effects++; return portKillNoListener, nil }
	registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) { *effects++; return ReconcileResponse{}, nil }
	t.Cleanup(func() {
		killByPortFn, forceKillByPortFn, registerSupervisorReconcileFn = oldKill, oldForce, oldReconcile
	})
	oldActivity := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	goRow := WorkspaceEntry{WorkspaceKey: key, WorkspacePath: canonical, Language: "go", Backend: "gopls-mcp", Port: 9424, TaskName: LSPTaskNameForWorkspaceLanguage(key, "go"), Lifecycle: LifecycleConfigured, LastToolsCallAt: oldActivity}
	pyRow := WorkspaceEntry{WorkspaceKey: key, WorkspacePath: canonical, Language: "python", Backend: "mcp-language-server", Port: 9425, TaskName: LSPTaskNameForWorkspaceLanguage(key, "python"), Lifecycle: LifecycleConfigured, LastToolsCallAt: oldActivity}
	serenaRow := WorkspaceEntry{WorkspaceKey: key, WorkspacePath: canonical, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9426, TaskName: SerenaTaskNameForWorkspace(canonical), LastToolsCallAt: oldActivity}
	reg := NewRegistry(h.regPath)
	for _, row := range []WorkspaceEntry{goRow, pyRow} {
		if err := reg.PutLSP(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.PutSerena(serenaRow); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatal(err)
	}
	stop := DaemonIntent{Desired: "stopped", Reason: "user-stop", UpdatedAt: oldActivity}
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{BuildSupervisorDaemonForLSP(goRow, testCanonicalMcphubPathOverride), BuildSupervisorDaemonForLSP(pyRow, testCanonicalMcphubPathOverride), {TaskName: serenaRow.TaskName, Server: "serena", Daemon: key, Workspace: canonical, Command: testCanonicalMcphubPathOverride}}, Stops: map[string]DaemonIntent{LSPIntentTaskNameForWorkspaceLanguage(key, "go"): stop, serenaRow.TaskName: stop}, LegacyStopWatermarks: map[string]DaemonIntent{LSPIntentTaskNameForWorkspaceLanguage(key, "python"): stop}}
	writeSupervisorIntentForRegisterTest(t, intentPath, intent)
	if err := NewAPI().SettingsSet(AutoPruneWorkspacesSettingKey, "true"); err != nil {
		t.Fatal(err)
	}
	if err := NewAPI().SettingsSet(PruneIdleHoursSettingKey, "168"); err != nil {
		t.Fatal(err)
	}
	if err := WriteDefaultWorkspace(stateDir, canonical); err != nil {
		t.Fatal(err)
	}
	return &PruneFixtureForExternalTest{Workspace: canonical, RegistryPath: h.regPath, IntentPath: intentPath, StateDir: stateDir, h: h, effects: effects}
}
func (f *PruneFixtureForExternalTest) DestructiveEffectsForExternalTest() int {
	return *f.effects + len(f.h.fakeSch.deleteNames)
}
func (f *PruneFixtureForExternalTest) BytesForExternalTest(t *testing.T) ([]byte, []byte) {
	t.Helper()
	a, err := os.ReadFile(f.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.IntentPath)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

// EventsForExternalTest reads every record from this fixture's actual event
// files. A missing rotated file is normal; malformed records remain failures.
func (f *PruneFixtureForExternalTest) EventsForExternalTest(t *testing.T) []map[string]any {
	t.Helper()
	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, leaf := range []string{hubMcpLogFileLeaf + ".1", hubMcpLogFileLeaf} {
		raw, err := os.ReadFile(filepath.Join(dir, leaf))
		if os.IsNotExist(err) && leaf != hubMcpLogFileLeaf {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(raw, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("decode fixture event: %v", err)
			}
			events = append(events, event)
		}
	}
	return events
}
