package api

import (
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
				Args:         []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", `C:\work\alpha`, "--port", "9121", "--task-name", `\mcp-local-hub-serena-deadbeef`},
				Port:         9121,
				Workspace:    `C:\work\alpha`,
				ManifestHash: "sha256:abc",
				RuntimeSpec: &DaemonRuntimeSpec{
					SpecVersion:   DaemonRuntimeSpecVersion,
					ChildCommand:  "uvx",
					ChildArgs:     []string{"--from", "git+https://example/serena", "serena", "start-mcp-server", "--project", `C:\work\alpha`, "--context", "codex"},
					EnvRefs:       map[string]string{"PYTHONUNBUFFERED": "1", "SERENA_TOKEN": "secret:SERENA_TOKEN"},
					UpstreamPort:  19121,
					ExternalPort:  9121,
					WorkspacePath: `C:\work\alpha`,
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
