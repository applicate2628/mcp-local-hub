package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGeneratedRuntimeImageTimerOnlyConvergence(t *testing.T) {
	root := hardenedTempDir(t)
	legacy := filepath.Join(root, "local", "mcphub.exe")
	image := filepath.Join(root, "npm-native", "mcphub.exe")
	enabled, disabled := true, false
	timers := []MaintenanceTimer{
		{Name: canonicalIntentTaskKey("mcp-local-hub-image-fixture-weekly-refresh"), Kind: "server-weekly-refresh", Server: "image-fixture", Command: legacy, Args: []string{"restart", "--server", "image-fixture"}, Enabled: &disabled},
		{Name: canonicalIntentTaskKey("mcp-local-hub-image-fixture-alpha-weekly-refresh"), Kind: "server-weekly-refresh", Command: legacy, Args: []string{"restart", "--server", "image-fixture-alpha"}},
		{Name: WeeklyRefreshTaskName, Kind: "workspace-weekly-refresh", Command: legacy, Args: []string{"workspace-weekly-refresh"}, Enabled: &enabled},
		{Name: "foreign", Kind: "server-weekly-refresh", Server: "image-fixture", Command: legacy, Args: []string{"restart", "--server", "image-fixture"}},
		{Name: canonicalIntentTaskKey("mcp-local-hub-image-fixture-weekly-refresh"), Kind: "server-weekly-refresh", Server: "other", Command: legacy, Args: []string{"restart", "--server", "image-fixture"}},
		{Name: canonicalIntentTaskKey("mcp-local-hub-other-weekly-refresh"), Kind: "server-weekly-refresh", Command: legacy, Args: []string{"restart", "--server", "image-fixture"}},
		{Name: WeeklyRefreshTaskName, Kind: "workspace-weekly-refresh", Command: legacy, Args: []string{"workspace-weekly-refresh", "--custom"}},
		{Name: WeeklyRefreshTaskName, Kind: "workspace-weekly-refresh", Server: "other", Command: legacy, Args: []string{"workspace-weekly-refresh"}},
		{Name: canonicalIntentTaskKey("mcp-local-hub-image-fixture-weekly-refresh"), Kind: "server-weekly-refresh", Command: "custom-native", Args: []string{"restart", "--server", "image-fixture"}},
		{Name: canonicalIntentTaskKey("mcp-local-hub-weekly-refresh"), Kind: "server-weekly-refresh", Command: legacy, Args: []string{"restart", "--server", ""}},
	}
	path := filepath.Join(root, "supervisor-intent.json")
	intent := &SupervisorIntentFile{Version: 1, OwnerMode: OwnerModeSupervise, StrictMode: true, MaintenanceTimers: timers, Stops: map[string]DaemonIntent{"foreign": {Desired: "stopped", Reason: "operator"}}}
	if err := WriteSupervisorIntent(path, intent); err != nil {
		t.Fatal(err)
	}
	before, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatal(err)
	}
	bytesBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !supervisorIntentHasLegacyImage(before, legacy) {
		t.Error("timer-only legacy intent did not enter image convergence")
	}
	preview := cloneSupervisorIntentFile(before)
	if changed, err := convergeGeneratedSupervisorImage(preview, legacy, image); err != nil || !changed {
		t.Fatalf("timer-only preview: changed=%v error=%v", changed, err)
	}
	want := cloneSupervisorIntentFile(before)
	for i := 0; i < 3; i++ {
		want.MaintenanceTimers[i].Command = image
	}
	if !reflect.DeepEqual(preview, want) {
		t.Fatalf("preview changed timer settings, foreign rows, or other intent fields: got=%#v want=%#v", preview, want)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(bytesBefore, after) {
		t.Fatalf("preview wrote intent: %v", err)
	}
	result, err := MutateSupervisorIntentIfChangedReturning(path, func(f *SupervisorIntentFile) (bool, error) {
		return convergeGeneratedSupervisorImage(f, legacy, image)
	})
	if err != nil || !result.Changed {
		t.Fatalf("timer-only writer: changed=%v error=%v", result.Changed, err)
	}
	want.IntentGeneration, want.UpdatedAt = result.Intent.IntentGeneration, result.Intent.UpdatedAt
	if !reflect.DeepEqual(result.Intent, want) {
		t.Fatalf("writer changed timer settings or foreign intent: got=%#v want=%#v", result.Intent, want)
	}
	settledBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MutateSupervisorIntentIfChangedReturning(path, func(f *SupervisorIntentFile) (bool, error) {
		return convergeGeneratedSupervisorImage(f, legacy, image)
	})
	if err != nil || second.Changed || !reflect.DeepEqual(second.Intent, result.Intent) {
		t.Fatalf("second timer pass changed: %+v error=%v", second, err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(settledBytes, after) {
		t.Fatalf("second timer pass wrote intent: %v", err)
	}
	foreignOnly := cloneSupervisorIntentFile(before)
	foreignOnly.MaintenanceTimers = foreignOnly.MaintenanceTimers[3:]
	if supervisorIntentHasLegacyImage(foreignOnly, legacy) {
		t.Fatal("foreign-only timer intent requested runtime image resolution")
	}
	if _, err := os.Stat(filepath.Dir(legacy)); !os.IsNotExist(err) {
		t.Fatalf("timer convergence created an optional local installation: %v", err)
	}
}

func TestGeneratedRuntimeImageConvergencePreservesObservedIntent(t *testing.T) {
	root := hardenedTempDir(t)
	legacy := filepath.Join(root, "local", "mcphub.exe")
	image := filepath.Join(root, "npm-native", "mcphub.exe")
	manifestDir := filepath.Join(root, "manifests")
	makeFakeManifest(t, filepath.Join(manifestDir, "image-fixture"), "image-fixture", 31013)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	raw, err := os.ReadFile(filepath.Join(manifestDir, "image-fixture", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ws := WorkspaceEntry{WorkspacePath: filepath.Join(root, "workspace"), Language: "go", Backend: "mcp-language-server", Port: 31012}
	ws.WorkspaceKey = WorkspaceKey(ws.WorkspacePath)
	registryPath := filepath.Join(root, "workspaces.yaml")
	originalRegistry := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return registryPath, nil }
	t.Cleanup(func() { defaultRegistryPathFn = originalRegistry })
	reg := NewRegistry(registryPath)
	reg.Workspaces = []WorkspaceEntry{ws}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	global := SupervisorDaemon{
		TaskName: `\mcp-local-hub-image-fixture-default`, Server: "image-fixture", Daemon: "default",
		Command: legacy, Args: []string{"daemon", "--server", "image-fixture", "--daemon", "default"},
		WorkingDir: filepath.Dir(legacy), Port: 31013, ManifestHash: ManifestHashContent(raw),
	}
	workspace := BuildSupervisorDaemonForLSP(ws, legacy)
	builtin := BuildBuiltinRouteDaemon(legacy, 31011)
	custom := SupervisorDaemon{TaskName: `\foreign`, Command: "node", Args: []string{"./mcp-memory-server.js"}, RuntimeSpec: &DaemonRuntimeSpec{SpecVersion: 1, ChildCommand: "custom-native"}}
	unproven := global
	unproven.TaskName = `\mcp-local-hub-image-fixture-custom`
	unproven.Args = append(unproven.Args, "--custom")
	path := filepath.Join(root, "supervisor-intent.json")
	latest := &SupervisorIntentFile{
		Version: 1, StrictMode: true, OwnerMode: OwnerModeSupervise,
		Daemons: []SupervisorDaemon{global, workspace, builtin, custom, unproven},
		Stops:   map[string]DaemonIntent{global.TaskName: {Desired: "stopped", Reason: "operator"}},
	}
	if err := WriteSupervisorIntent(path, latest); err != nil {
		t.Fatal(err)
	}
	before, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := MutateSupervisorIntentIfChangedReturning(path, func(f *SupervisorIntentFile) (bool, error) {
		return convergeGeneratedSupervisorImage(f, legacy, image)
	})
	if err != nil || !result.Changed {
		t.Fatalf("converge: changed=%v error=%v", result.Changed, err)
	}
	want := cloneSupervisorIntentFile(before)
	for i := 0; i < 2; i++ {
		want.Daemons[i].Command = image
	}
	want.Daemons[0].WorkingDir = filepath.Dir(image)
	// The writer owns timestamp/generation; every semantic field is compared.
	want.IntentGeneration = result.Intent.IntentGeneration
	want.UpdatedAt = result.Intent.UpdatedAt
	if !reflect.DeepEqual(result.Intent, want) {
		t.Fatalf("OBSERVED-GENERATION or custom state changed: got=%#v want=%#v", result.Intent, want)
	}
	bytesBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MutateSupervisorIntentIfChangedReturning(path, func(f *SupervisorIntentFile) (bool, error) {
		return convergeGeneratedSupervisorImage(f, legacy, image)
	})
	if err != nil || second.Changed || !reflect.DeepEqual(second.Intent, result.Intent) {
		t.Fatalf("second pass changed observed generation: %+v error=%v", second, err)
	}
	bytesAfter, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatalf("second pass rewrote intent bytes: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(legacy)); !os.IsNotExist(err) {
		t.Fatalf("convergence created an optional installation: %v", err)
	}
	if changed, err := convergeGeneratedSupervisorImage(before, legacy, ""); changed || err == nil {
		t.Fatalf("missing image weakened convergence: changed=%v error=%v", changed, err)
	}
}

func TestSupervisorIntent_RoundTrip(t *testing.T) {
	// v0.5.0 Fix Group 5: WriteSupervisorIntent now flows through
	// the hardened secure-write pipeline (handle-bound DACL,
	// parent-dir gate, post-rename re-verify). Test temp dirs must
	// pass the parent-dir gate, which t.TempDir() alone may not on
	// machines whose %TEMP%/TMPDIR carries Authenticated Users (or
	// equivalent) write rights. hardenedTempDir installs the
	// allowlist-conforming DACL/mode the gate expects.
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	want := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-16T18:00:00.000000000Z",
		Daemons: []SupervisorDaemon{
			{
				TaskName:     `\mcp-local-hub-memory-default`,
				Server:       "memory",
				Daemon:       "default",
				Command:      "node",
				Args:         []string{"./mcp-memory-server.js"},
				Port:         9128,
				ManifestHash: "sha256:abc123",
			},
		},
		StrictMode: false,
	}
	if err := WriteSupervisorIntent(path, &want); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Daemons[0].TaskName != `\mcp-local-hub-memory-default` {
		t.Fatalf("task_name not preserved: %q", got.Daemons[0].TaskName)
	}
	if got.Daemons[0].Port != 9128 {
		t.Fatalf("port not preserved: %d", got.Daemons[0].Port)
	}
}

func TestSupervisorIntent_OwnerModeDefaultsToGUIAndPersistsSupervise(t *testing.T) {
	legacy := &SupervisorIntentFile{Version: 1}
	if got := legacy.EffectiveOwnerMode(); got != OwnerModeGUI {
		t.Fatalf("legacy effective owner mode=%q, want gui", got)
	}

	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")
	if err := WriteSupervisorIntent(path, &SupervisorIntentFile{Version: 1, OwnerMode: OwnerModeSupervise}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.EffectiveOwnerMode() != OwnerModeSupervise {
		t.Fatalf("persisted effective owner mode=%q, want supervise", got.EffectiveOwnerMode())
	}
}

func TestSupervisorDaemonWorkingDirWireIsAdditiveAndRoundTrips(t *testing.T) {
	legacy, err := json.Marshal(SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy, []byte(`"working_dir"`)) {
		t.Fatalf("legacy descriptor grew working_dir: %s", legacy)
	}

	want := SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, WorkingDir: "/frozen/mcphub", StartupBindDeadlineSeconds: 60}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got SupervisorDaemon
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.WorkingDir != want.WorkingDir || got.StartupBindDeadlineSeconds != want.StartupBindDeadlineSeconds {
		t.Fatalf("descriptor round trip = %#v, want %#v", got, want)
	}
}

func TestSupervisorIntent_LegacyStopWatermarkJSONIsAdditiveForSupportedOldReaders(t *testing.T) {
	now := "2026-07-09T09:59:00Z"
	raw := []byte(`{"version":1,"legacy_stop_watermarks":{"\\mcp-local-hub-paper-search-default":{"desired":"stopped","reason":"user-stop","updated_at":"` + now + `"}}}`)

	var oldReaderShape struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &oldReaderShape); err != nil {
		t.Fatalf("supported old reader shape rejected additive legacy_stop_watermarks field: %v", err)
	}
	if oldReaderShape.Version != 1 {
		t.Fatalf("old reader shape version = %d, want 1", oldReaderShape.Version)
	}
	var current SupervisorIntentFile
	if err := json.Unmarshal(raw, &current); err != nil {
		t.Fatalf("current reader rejected legacy_stop_watermarks field: %v", err)
	}
	if _, ok := current.LegacyStopWatermarks[`\mcp-local-hub-paper-search-default`]; !ok {
		t.Fatalf("current reader did not preserve legacy_stop_watermarks: %+v", current.LegacyStopWatermarks)
	}

	emptyRaw, err := json.Marshal(SupervisorIntentFile{Version: 1})
	if err != nil {
		t.Fatalf("marshal empty supervisor intent: %v", err)
	}
	if strings.Contains(string(emptyRaw), "legacy_stop_watermarks") {
		t.Fatalf("empty watermark field should be omitted for back-compat, raw=%s", emptyRaw)
	}
}

func TestWriteSupervisorIntent_NormalizesAbsentOnlyLegacyStopWatermarks(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	activeTask := `\mcp-local-hub-demo-active`
	clearedTask := `\mcp-local-hub-demo-cleared`
	activeStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now}
	clearedStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-time.Minute)}

	if err := WriteSupervisorIntent(path, &SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			activeTask: activeStop,
		},
		LegacyStopWatermarks: map[string]DaemonIntent{
			strings.TrimPrefix(activeTask, `\`):  activeStop,
			strings.TrimPrefix(clearedTask, `\`): clearedStop,
		},
	}); err != nil {
		t.Fatalf("WriteSupervisorIntent: %v", err)
	}

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	assertDaemonIntentEqual(t, got.Stops[activeTask], activeStop)
	if _, ok := got.LegacyStopWatermarks[activeTask]; ok {
		t.Fatalf("redundant active-task watermark survived normalization: %+v", got.LegacyStopWatermarks)
	}
	if _, ok := got.LegacyStopWatermarks[strings.TrimPrefix(activeTask, `\`)]; ok {
		t.Fatalf("bare active-task watermark survived normalization: %+v", got.LegacyStopWatermarks)
	}
	if _, ok := got.LegacyStopWatermarks[strings.TrimPrefix(clearedTask, `\`)]; ok {
		t.Fatalf("cleared-task watermark kept bare key after normalization: %+v", got.LegacyStopWatermarks)
	}
	assertDaemonIntentEqual(t, got.LegacyStopWatermarks[clearedTask], clearedStop)
}

func TestReadSupervisorIntent_CanonicalizesBareStopKeysPrefersCanonicalEntry(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	task := `\mcp-local-hub-demo-canonical`
	bareTask := strings.TrimPrefix(task, `\`)
	bareStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	canonicalStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserDisabled,
		UpdatedAt: time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC),
	}
	writeRawSupervisorIntentFileForTest(t, path, SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			bareTask: bareStop,
			task:     canonicalStop,
		},
	})

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(got.Stops) != 1 {
		t.Fatalf("read stops count = %d, want one canonical entry after bare/canonical merge: %+v", len(got.Stops), got.Stops)
	}
	if _, ok := got.Stops[bareTask]; ok {
		t.Fatalf("bare stop key survived read-boundary canonicalization: %+v", got.Stops)
	}
	assertDaemonIntentEqual(t, got.Stops[task], canonicalStop)
}

func TestReadSupervisorIntent_MergeStopCollisionKeepsBareNewerRecord(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	task := `\mcp-local-hub-demo-rollback-stop`
	bareTask := strings.TrimPrefix(task, `\`)
	canonicalStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserDisabled,
		UpdatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	bareStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC),
	}
	writeRawSupervisorIntentFileForTest(t, path, SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			bareTask: bareStop,
			task:     canonicalStop,
		},
	})

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(got.Stops) != 1 {
		t.Fatalf("read stops count = %d, want one canonical entry after bare/canonical merge: %+v", len(got.Stops), got.Stops)
	}
	if _, ok := got.Stops[bareTask]; ok {
		t.Fatalf("bare stop key survived read-boundary canonicalization: %+v", got.Stops)
	}
	assertDaemonIntentEqual(t, got.Stops[task], bareStop)
}

func TestReadSupervisorIntent_MergeWatermarkCollisionKeepsBareNewerRecord(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	task := `\mcp-local-hub-demo-rollback-watermark`
	bareTask := strings.TrimPrefix(task, `\`)
	canonicalWatermark := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserDisabled,
		UpdatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	bareWatermark := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC),
	}
	writeRawSupervisorIntentFileForTest(t, path, SupervisorIntentFile{
		Version: 1,
		LegacyStopWatermarks: map[string]DaemonIntent{
			bareTask: bareWatermark,
			task:     canonicalWatermark,
		},
	})

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(got.LegacyStopWatermarks) != 1 {
		t.Fatalf("read watermarks count = %d, want one canonical entry after bare/canonical merge: %+v", len(got.LegacyStopWatermarks), got.LegacyStopWatermarks)
	}
	if _, ok := got.LegacyStopWatermarks[bareTask]; ok {
		t.Fatalf("bare watermark key survived read-boundary canonicalization: %+v", got.LegacyStopWatermarks)
	}
	assertDaemonIntentEqual(t, got.LegacyStopWatermarks[task], bareWatermark)
}

func TestReadSupervisorIntent_MergeStopAndWatermarkCollisionTieKeepsCanonicalRecord(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	stopTask := `\mcp-local-hub-demo-tie-stop`
	bareStopTask := strings.TrimPrefix(stopTask, `\`)
	watermarkTask := `\mcp-local-hub-demo-tie-watermark`
	bareWatermarkTask := strings.TrimPrefix(watermarkTask, `\`)
	tiedUpdatedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	bareStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: tiedUpdatedAt}
	canonicalStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: tiedUpdatedAt}
	bareWatermark := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: tiedUpdatedAt}
	canonicalWatermark := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: tiedUpdatedAt}
	writeRawSupervisorIntentFileForTest(t, path, SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			bareStopTask: bareStop,
			stopTask:     canonicalStop,
		},
		LegacyStopWatermarks: map[string]DaemonIntent{
			bareWatermarkTask: bareWatermark,
			watermarkTask:     canonicalWatermark,
		},
	})

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(got.Stops) != 1 {
		t.Fatalf("read stops count = %d, want one canonical entry after bare/canonical merge: %+v", len(got.Stops), got.Stops)
	}
	if len(got.LegacyStopWatermarks) != 1 {
		t.Fatalf("read watermarks count = %d, want one canonical entry after bare/canonical merge: %+v", len(got.LegacyStopWatermarks), got.LegacyStopWatermarks)
	}
	if _, ok := got.Stops[bareStopTask]; ok {
		t.Fatalf("bare stop key survived tie canonicalization: %+v", got.Stops)
	}
	if _, ok := got.LegacyStopWatermarks[bareWatermarkTask]; ok {
		t.Fatalf("bare watermark key survived tie canonicalization: %+v", got.LegacyStopWatermarks)
	}
	assertDaemonIntentEqual(t, got.Stops[stopTask], canonicalStop)
	assertDaemonIntentEqual(t, got.LegacyStopWatermarks[watermarkTask], canonicalWatermark)
}

func TestSupervisorIntent_ReadRejectsSymlinkTarget(t *testing.T) {
	dir := hardenedTempDir(t)
	realPath := filepath.Join(dir, "real-supervisor-intent.json")
	linkPath := filepath.Join(dir, "supervisor-intent.json")

	if err := WriteStateFileAtomic(realPath, &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed real intent: %v", err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	_, err := ReadSupervisorIntent(linkPath)
	if err == nil {
		t.Fatalf("ReadSupervisorIntent followed symlink target; want refusal")
	}
	if !errors.Is(err, ErrIrregularFile) {
		t.Fatalf("ReadSupervisorIntent err = %v, want ErrIrregularFile", err)
	}
}

func TestSupervisorIntent_ReadAllowsLargeIntentFile(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	intent := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-06-20T12:00:00Z",
	}
	var raw []byte
	for i := 0; int64(len(raw)) <= maxStateFileBytes; i++ {
		intent.Daemons = append(intent.Daemons, SupervisorDaemon{
			TaskName:     fmt.Sprintf(`\mcp-local-hub-large-%05d`, i),
			Server:       "large",
			Daemon:       fmt.Sprintf("d-%05d", i),
			Command:      "node",
			Args:         []string{strings.Repeat("x", 2048)},
			Port:         9000 + i,
			ManifestHash: "sha256:large",
		})
		var err error
		raw, err = json.Marshal(intent)
		if err != nil {
			t.Fatalf("marshal intent: %v", err)
		}
		if int64(len(raw)) > maxIntentFileBytes {
			t.Fatalf("test fixture grew past supervisor/daemon intent cap: %d > %d", len(raw), maxIntentFileBytes)
		}
	}
	if err := WriteStateFileBytesAtomic(path, raw); err != nil {
		t.Fatalf("seed large supervisor intent: %v", err)
	}

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent rejected %d-byte intent: %v", len(raw), err)
	}
	if len(got.Daemons) != len(intent.Daemons) {
		t.Fatalf("daemon count = %d, want %d", len(got.Daemons), len(intent.Daemons))
	}
}

func TestSupervisorIntent_FiltersLegacyWatchdogOneshot(t *testing.T) {
	// v0.4.x->v0.5.0 migration captured the `\mcp-local-hub-watchdog`
	// scheduled task into supervisor-intent.json as a daemon
	// descriptor. The watchdog's `--once` argv makes it exit
	// immediately, which combined with the supervisor's reconcile
	// respawn produces a wasteful watchdog spawn loop AND leaves a
	// duplicate "watchdog" row in GUI Dashboard alongside the legacy
	// Task Scheduler entry. ReadSupervisorIntent post-parses the
	// loaded file to strip such one-shot entries so existing broken
	// intent files self-heal on next read.
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	on := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-18T00:00:00.000000000Z",
		Daemons: []SupervisorDaemon{
			{
				TaskName: `\mcp-local-hub-memory-default`,
				Server:   "memory",
				Daemon:   "default",
				Command:  "mcphub",
				Args:     []string{"daemon", "--server", "memory"},
				Port:     9128,
			},
			{
				TaskName: `\mcp-local-hub-watchdog`,
				Command:  "mcphub",
				Args:     []string{"watchdog", "--once"},
			},
			{
				TaskName: `\mcp-local-hub-time-default`,
				Server:   "time",
				Daemon:   "default",
				Command:  "mcphub",
				Args:     []string{"daemon", "--server", "time"},
				Port:     9129,
			},
		},
	}
	if err := WriteSupervisorIntent(path, &on); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Daemons) != 2 {
		names := make([]string, 0, len(got.Daemons))
		for _, d := range got.Daemons {
			names = append(names, d.TaskName)
		}
		t.Fatalf("expected 2 daemons after watchdog filter, got %d: %v", len(got.Daemons), names)
	}
	for _, d := range got.Daemons {
		if d.TaskName == `\mcp-local-hub-watchdog` {
			t.Fatalf("watchdog entry leaked past filter: %+v", d)
		}
	}
	if got.Daemons[0].TaskName != `\mcp-local-hub-memory-default` ||
		got.Daemons[1].TaskName != `\mcp-local-hub-time-default` {
		t.Fatalf("daemon order not preserved across filter: %+v", got.Daemons)
	}
}

// TestSupervisorIntent_FilterStrictWatchdogOnceOnly verifies the
// tightened filter predicate at supervisor_intent.go:isLegacyOneshotDaemon —
// only `["watchdog", "--once"]` (the exact migration artifact) is
// stripped. Defensive: a future long-lived `mcphub watchdog serve`
// daemon variant must NOT be filtered away by a too-broad match. PR
// #212 r2 code-review finding 5.
func TestSupervisorIntent_FilterStrictWatchdogOnceOnly(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	on := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-18T00:00:00.000000000Z",
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-watchdog`, Command: "mcphub", Args: []string{"watchdog", "--once"}},      // FILTERED
			{TaskName: `\mcp-local-hub-watchdog-bare`, Command: "mcphub", Args: []string{"watchdog"}},           // KEPT — not --once
			{TaskName: `\mcp-local-hub-watchdog-serve`, Command: "mcphub", Args: []string{"watchdog", "serve"}}, // KEPT — future variant
			{TaskName: `\mcp-local-hub-watchdog-empty`, Command: "mcphub", Args: []string{}},                    // KEPT — no args
		},
	}
	if err := WriteSupervisorIntent(path, &on); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Daemons) != 3 {
		names := make([]string, 0, len(got.Daemons))
		for _, d := range got.Daemons {
			names = append(names, d.TaskName)
		}
		t.Fatalf("expected 3 daemons (only `watchdog --once` filtered), got %d: %v", len(got.Daemons), names)
	}
	for _, d := range got.Daemons {
		if d.TaskName == `\mcp-local-hub-watchdog` {
			t.Errorf("watchdog --once entry should be filtered, got %+v", d)
		}
	}
}

func TestSupervisorIntent_IgnoresUnknownFields(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")
	body := `{"version":1,"updated_at":"2026-05-16T18:00:00.000000000Z","daemons":[{"task_name":"\\mcp-local-hub-serena-default","server":"serena","daemon":"default","command":"mcphub","args":["daemon","serena-proxy"],"port":9121,"runtime_spec":{"spec_version":1,"child_command":"uvx","child_args":["serena"],"future_runtime_field":"x"},"future_daemon_field":"x"}],"strict_mode":false,"future_top_level":"x"}`
	if err := WriteStateFileAtomic(path, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read with unknown fields: %v", err)
	}
	if len(got.Daemons) != 1 {
		t.Fatalf("got %d daemons; want 1", len(got.Daemons))
	}
	if got.Daemons[0].TaskName != `\mcp-local-hub-serena-default` {
		t.Fatalf("known daemon fields lost: %+v", got.Daemons[0])
	}
	if got.Daemons[0].RuntimeSpec == nil || got.Daemons[0].RuntimeSpec.ChildCommand != "uvx" {
		t.Fatalf("known runtime_spec fields lost: %+v", got.Daemons[0].RuntimeSpec)
	}
}

// TestSupervisorIntent_RoundTrip_NilRuntimeSpecLegacyFile is the additive-
// field regression guard (design claim #2): an existing pre-RuntimeSpec
// supervisor-intent.json must decode through ReadSupervisorIntent with a nil
// RuntimeSpec and NO error, and re-marshal must OMIT the runtime_spec key
// (omitempty) so the on-disk shape is byte-identical for legacy daemons.
func TestSupervisorIntent_RoundTrip_NilRuntimeSpecLegacyFile(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")
	// A legacy intent body written before RuntimeSpec existed: no
	// runtime_spec key anywhere. The lenient supervisor reader must accept it.
	body := `{"version":1,"updated_at":"2026-05-16T18:00:00.000000000Z","daemons":[` +
		`{"task_name":"\\mcp-local-hub-memory-default","server":"memory","daemon":"default","command":"node","args":["./mcp-memory-server.js"],"port":9128,"manifest_hash":"sha256:abc123"}` +
		`],"strict_mode":false}`
	if err := WriteStateFileAtomic(path, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("legacy file must decode with nil RuntimeSpec; got error: %v", err)
	}
	if len(got.Daemons) != 1 {
		t.Fatalf("got %d daemons; want 1", len(got.Daemons))
	}
	if got.Daemons[0].RuntimeSpec != nil {
		t.Errorf("legacy daemon must have nil RuntimeSpec; got %#v", got.Daemons[0].RuntimeSpec)
	}
	// Re-marshal: the absent RuntimeSpec must NOT serialize a runtime_spec
	// key (omitempty), preserving byte-shape for legacy rows.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "runtime_spec") {
		t.Errorf("nil RuntimeSpec must be omitted from JSON (omitempty); got: %s", string(raw))
	}
}

// TestSupervisorIntent_RoundTrip_WithRuntimeSpec asserts a daemon carrying a
// fully-populated RuntimeSpec marshals, decodes through the supervisor intent
// reader, and equals the original — every RuntimeSpec field is preserved
// (design §3 schema).
func TestSupervisorIntent_RoundTrip_WithRuntimeSpec(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	want := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-29T12:00:00.000000000Z",
		Daemons: []SupervisorDaemon{
			{
				TaskName:     `\mcp-local-hub-serena-deadbeef`,
				Server:       "serena",
				Daemon:       "deadbeef",
				Command:      `C:\bin\mcphub.exe`,
				Args:         []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", "fixture-workspace-alpha", "--port", "9121", "--task-name", `\mcp-local-hub-serena-deadbeef`},
				Port:         9121,
				Workspace:    "fixture-workspace-alpha",
				ManifestHash: "sha256:abc",
				RuntimeSpec: &DaemonRuntimeSpec{
					SpecVersion:   DaemonRuntimeSpecVersion,
					ChildCommand:  "uvx",
					ChildArgs:     []string{"--from", "git+https://example/serena", "serena", "start-mcp-server", "--project", "fixture-workspace-alpha", "--context", "codex"},
					EnvRefs:       map[string]string{"PYTHONUNBUFFERED": "1", "SERENA_TOKEN": "secret:SERENA_TOKEN"},
					UpstreamPort:  19121,
					ExternalPort:  9121,
					WorkspacePath: "fixture-workspace-alpha",
				},
			},
		},
		StrictMode: false,
	}
	if err := WriteSupervisorIntent(path, &want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Daemons) != 1 {
		t.Fatalf("got %d daemons; want 1", len(got.Daemons))
	}
	if !reflect.DeepEqual(got.Daemons[0].RuntimeSpec, want.Daemons[0].RuntimeSpec) {
		t.Errorf("RuntimeSpec round-trip mismatch:\n got=%#v\nwant=%#v", got.Daemons[0].RuntimeSpec, want.Daemons[0].RuntimeSpec)
	}
}

func TestSupervisorRuntimeIntentQualifiedSnapshotAndEffectiveStopDrift(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	route := BuildBuiltinRouteDaemon("mcphub.exe", 9140)
	intent := SupervisorIntentFile{Version: 1, IntentGeneration: 7, Daemons: []SupervisorDaemon{
		{TaskName: `\current`, Command: "mcphub.exe", Port: 9137, Args: []string{"daemon", "--server", "fixture", "--daemon", "default"}},
		{TaskName: WeeklyRefreshTaskName, Command: "mcphub.exe", Args: []string{"workspace-weekly-refresh"}},
		route,
	}, Stops: map[string]DaemonIntent{
		`\current`:           {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-24*time.Hour + time.Second)},
		BuiltinRouteTaskName: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-24*time.Hour + time.Second)},
	}}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hardenedTempDir(t), "supervisor-intent.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ObserveSupervisorRuntimeIntent(context.Background(), path, now)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got.SHA256 != hex.EncodeToString(sum[:]) || got.Generation != 7 || !got.ObservedAt.Equal(now) || len(got.Ports) != 2 || !got.Stopped[`\current`] || !got.Stopped[BuiltinRouteTaskName] {
		t.Fatalf("snapshot not bound to actual bytes/stops: %+v", got)
	}
	later, err := ObserveSupervisorRuntimeIntent(context.Background(), path, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if later.SHA256 != got.SHA256 || later.StopSHA256 == got.StopSHA256 || later.Stopped[`\current`] || later.Stopped[BuiltinRouteTaskName] {
		t.Fatal("effective stop TTL change hidden behind unchanged raw bytes")
	}
	if _, err := ObserveSupervisorRuntimeIntent(context.Background(), path+"-missing", now); !errors.Is(err, ErrSupervisorRuntimeIntentUnavailable) {
		t.Fatalf("missing snapshot=%v", err)
	}
}

func TestSupervisorRuntimeIntentEmptyMaintenanceAndInvalidInput(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	maintenance, err := json.Marshal(SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{TaskName: WeeklyRefreshTaskName, Command: "mcphub.exe", Args: []string{"workspace-weekly-refresh"}}}})
	if err != nil {
		t.Fatal(err)
	}
	marshalDaemon := func(d SupervisorDaemon) []byte {
		raw, err := json.Marshal(SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{d}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	badRouteArgv := BuildBuiltinRouteDaemon("mcphub.exe", 9140)
	badRouteArgv.Args = []string{"daemon", "--server", BuiltinRouteServer, "--daemon", BuiltinRouteDaemonName}
	badRouteName := BuildBuiltinRouteDaemon("mcphub.exe", 9140)
	badRouteName.TaskName = `\foreign-route`
	badRouteIdentity := BuildBuiltinRouteDaemon("mcphub.exe", 9140)
	badRouteIdentity.Server = "foreign"
	badRoutePort := BuildBuiltinRouteDaemon("mcphub.exe", 9140)
	badRoutePort.Port++
	for _, tc := range []struct {
		name  string
		raw   []byte
		valid bool
	}{
		{"explicit empty", []byte(`{"version":1,"daemons":[]}`), true},
		{"recognized maintenance", maintenance, true},
		{"reserved route with ordinary argv", marshalDaemon(badRouteArgv), false},
		{"route with foreign task name", marshalDaemon(badRouteName), false},
		{"route with foreign identity", marshalDaemon(badRouteIdentity), false},
		{"route with conflicting argv port", marshalDaemon(badRoutePort), false},
		{"null", []byte(`null`), false},
		{"missing daemons", []byte(`{"version":1}`), false},
		{"malformed", []byte(`{`), false},
		{"unknown role", []byte(`{"version":1,"daemons":[{"task_name":"x","command":"mcphub.exe","args":["unknown"],"port":9137}]}`), false},
		{"unresolved ordinary", []byte(`{"version":1,"daemons":[{"task_name":"x","command":"mcphub.exe","args":["daemon","--server","missing-fixture","--daemon","default"]}]}`), false},
		{"duplicate task", []byte(`{"version":1,"daemons":[{"task_name":"x","command":"mcphub.exe","args":["daemon","--server","fixture","--daemon","default"],"port":9137},{"task_name":"x","command":"mcphub.exe","args":["daemon","--server","fixture","--daemon","default"],"port":9138}]}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeSupervisorRuntimeIntent("synthetic intent", tc.raw, now)
			if tc.valid {
				if err != nil || got == nil || got.Ports == nil || len(got.Ports) != 0 {
					t.Fatalf("known empty=%+v error=%v", got, err)
				}
			} else if err == nil || got != nil {
				t.Fatalf("invalid intent accepted: %+v %v", got, err)
			}
		})
	}
}
