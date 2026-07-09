package api

import (
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

func TestLSPSupervisorIntentUpsertRollbackRemovesOnlyItsDescriptor(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	entryA := WorkspaceEntry{WorkspaceKey: "aaa11111", WorkspacePath: "D:/repo/a", Language: "python", Port: 9201}
	entryB := WorkspaceEntry{WorkspaceKey: "bbb22222", WorkspacePath: "D:/repo/b", Language: "go", Port: 9202}

	restoreA, err := NewAPI().upsertLSPSupervisorIntent(entryA, "mcphub.exe")
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if _, err := NewAPI().upsertLSPSupervisorIntent(entryB, "mcphub.exe"); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	restoreA()

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(entryA.WorkspaceKey, entryA.Language)); row != nil {
		t.Fatalf("rollback for A left A descriptor behind: %+v", row)
	}
	if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(entryB.WorkspaceKey, entryB.Language)); row == nil {
		t.Fatalf("rollback for A clobbered concurrent B descriptor; rows=%+v", intent.Daemons)
	}
}

func TestLSPSupervisorIntentUpsertRollbackRestoresPriorDescriptorOnReplace(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	prior := WorkspaceEntry{WorkspaceKey: "aaa11111", WorkspacePath: "D:/repo/a", Language: "python", Port: 9201}
	replacement := prior
	replacement.Port = 9301

	if _, err := NewAPI().upsertLSPSupervisorIntent(prior, "old-mcphub.exe"); err != nil {
		t.Fatalf("upsert prior: %v", err)
	}
	restoreReplacement, err := NewAPI().upsertLSPSupervisorIntent(replacement, "new-mcphub.exe")
	if err != nil {
		t.Fatalf("upsert replacement: %v", err)
	}

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	taskName := LSPIntentTaskNameForWorkspaceLanguage(prior.WorkspaceKey, prior.Language)
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent before rollback: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row == nil || row.Port != replacement.Port {
		t.Fatalf("replacement precondition row = %+v, want port %d", row, replacement.Port)
	}

	restoreReplacement()

	intent, err = ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after rollback: %v", err)
	}
	row := intent.FindSupervisorDaemonByTaskName(taskName)
	if row == nil {
		t.Fatalf("replace rollback deleted prior descriptor %s", taskName)
	}
	if row.Port != prior.Port || row.Command != "old-mcphub.exe" {
		t.Fatalf("replace rollback row = %+v, want prior port %d and old command", row, prior.Port)
	}
}

func TestLSPSupervisorIntentRemoveRollbackPreservesConcurrentDescriptors(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	entryA := WorkspaceEntry{WorkspaceKey: "aaa11111", WorkspacePath: "D:/repo/a", Language: "python", Port: 9201}
	entryB := WorkspaceEntry{WorkspaceKey: "bbb22222", WorkspacePath: "D:/repo/b", Language: "go", Port: 9202}
	entryC := WorkspaceEntry{WorkspaceKey: "ccc33333", WorkspacePath: "D:/repo/c", Language: "rust", Port: 9203}
	if _, err := NewAPI().upsertLSPSupervisorIntent(entryA, "mcphub.exe"); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if _, err := NewAPI().upsertLSPSupervisorIntent(entryB, "mcphub.exe"); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	restoreA, removed, err := NewAPI().removeLSPSupervisorIntent(entryA.WorkspaceKey, entryA.Language)
	if err != nil {
		t.Fatalf("remove A: %v", err)
	}
	if !removed {
		t.Fatal("remove A reported removed=false")
	}
	if _, err := NewAPI().upsertLSPSupervisorIntent(entryC, "mcphub.exe"); err != nil {
		t.Fatalf("upsert concurrent C: %v", err)
	}

	restoreA()

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	for _, entry := range []WorkspaceEntry{entryA, entryB, entryC} {
		taskName := LSPIntentTaskNameForWorkspaceLanguage(entry.WorkspaceKey, entry.Language)
		if row := intent.FindSupervisorDaemonByTaskName(taskName); row == nil {
			t.Fatalf("intent missing %s after remove rollback; rows=%+v", taskName, intent.Daemons)
		}
	}
}

func TestLSPSupervisorIntentRemoveRollbackRestoresStopAndPreservesSiblingWatermark(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	entryA := WorkspaceEntry{WorkspaceKey: "aaa11111", WorkspacePath: "D:/repo/a", Language: "python", Port: 9201}
	entryB := WorkspaceEntry{WorkspaceKey: "bbb22222", WorkspacePath: "D:/repo/b", Language: "go", Port: 9202}
	if _, err := NewAPI().upsertLSPSupervisorIntent(entryA, "mcphub.exe"); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if _, err := NewAPI().upsertLSPSupervisorIntent(entryB, "mcphub.exe"); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	taskA := LSPIntentTaskNameForWorkspaceLanguage(entryA.WorkspaceKey, entryA.Language)
	taskB := LSPIntentTaskNameForWorkspaceLanguage(entryB.WorkspaceKey, entryB.Language)
	now := time.Unix(1700000000, 0).UTC()
	stopA := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now}
	stopB := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(time.Minute)}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent before seed: %v", err)
	}
	intent.Stops = map[string]DaemonIntent{taskA: stopA}
	intent.LegacyStopWatermarks = map[string]DaemonIntent{taskB: stopB}
	if err := WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatalf("seed stop artifacts: %v", err)
	}

	restoreA, removed, err := NewAPI().removeLSPSupervisorIntent(entryA.WorkspaceKey, entryA.Language)
	if err != nil {
		t.Fatalf("remove A: %v", err)
	}
	if !removed {
		t.Fatal("remove A reported removed=false")
	}
	intent, err = ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after remove: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskA); row != nil {
		t.Fatalf("remove left A descriptor behind: %+v", row)
	}
	if _, ok := intent.Stops[taskA]; ok {
		t.Fatalf("remove left A stop behind: %+v", intent.Stops)
	}
	if _, ok := intent.LegacyStopWatermarks[taskA]; ok {
		t.Fatalf("remove left A legacy-stop watermark behind: %+v", intent.LegacyStopWatermarks)
	}
	assertDaemonIntentEqual(t, intent.LegacyStopWatermarks[taskB], stopB)

	restoreA()

	intent, err = ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after rollback: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskA); row == nil {
		t.Fatalf("rollback did not restore A descriptor; rows=%+v", intent.Daemons)
	}
	assertDaemonIntentEqual(t, intent.Stops[taskA], stopA)
	if _, ok := intent.LegacyStopWatermarks[taskA]; ok {
		t.Fatalf("rollback restored redundant A watermark beside present stop: %+v", intent.LegacyStopWatermarks)
	}
	assertDaemonIntentEqual(t, intent.LegacyStopWatermarks[taskB], stopB)
}

func TestLSPSupervisorIntentUpsertRollbackRestoresPreExistingStop(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	entry := WorkspaceEntry{WorkspaceKey: "aaa11111", WorkspacePath: "D:/repo/a", Language: "python", Port: 9201}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	taskName := LSPIntentTaskNameForWorkspaceLanguage(entry.WorkspaceKey, entry.Language)
	stop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Stops:   map[string]DaemonIntent{taskName: stop},
	}); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	restore, err := NewAPI().upsertLSPSupervisorIntent(entry, "mcphub.exe")
	if err != nil {
		t.Fatalf("upsert descriptor: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after upsert: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row == nil {
		t.Fatalf("descriptor precondition not met after upsert; rows=%+v", intent.Daemons)
	}

	restore()

	intent, err = ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after rollback: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row != nil {
		t.Fatalf("upsert rollback left descriptor behind: %+v", row)
	}
	assertDaemonIntentEqual(t, intent.Stops[taskName], stop)
	if _, ok := intent.LegacyStopWatermarks[taskName]; ok {
		t.Fatalf("upsert rollback restored redundant watermark beside present stop: %+v", intent.LegacyStopWatermarks)
	}
}

func TestLSPSupervisorIntentUpsertRollbackRestoresWatermarkOnlyPriorArtifact(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	entry := WorkspaceEntry{WorkspaceKey: "aaa11111", WorkspacePath: "D:/repo/a", Language: "python", Port: 9201}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	taskName := LSPIntentTaskNameForWorkspaceLanguage(entry.WorkspaceKey, entry.Language)
	watermark := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version:              1,
		LegacyStopWatermarks: map[string]DaemonIntent{taskName: watermark},
	}); err != nil {
		t.Fatalf("seed supervisor-intent watermark: %v", err)
	}

	restore, err := NewAPI().upsertLSPSupervisorIntent(entry, "mcphub.exe")
	if err != nil {
		t.Fatalf("upsert descriptor: %v", err)
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("simulate descriptor and watermark removed before rollback: %v", err)
	}

	restore()

	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after rollback: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row != nil {
		t.Fatalf("upsert rollback left descriptor behind: %+v", row)
	}
	if _, ok := intent.Stops[taskName]; ok {
		t.Fatalf("watermark-only rollback restored a stop: %+v", intent.Stops)
	}
	assertDaemonIntentEqual(t, intent.LegacyStopWatermarks[taskName], watermark)
}

func TestLSPSupervisorIntentUpsertRollbackSkipsWatermarkRestoreWhenConcurrentStopExists(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	entry := WorkspaceEntry{WorkspaceKey: "aaa11111", WorkspacePath: "D:/repo/a", Language: "python", Port: 9201}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	taskName := LSPIntentTaskNameForWorkspaceLanguage(entry.WorkspaceKey, entry.Language)
	now := time.Now().UTC().Add(-time.Minute)
	watermark := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}
	freshStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserDisabled,
		UpdatedAt: now.Add(time.Minute),
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version:              1,
		LegacyStopWatermarks: map[string]DaemonIntent{taskName: watermark},
	}); err != nil {
		t.Fatalf("seed supervisor-intent watermark: %v", err)
	}

	restore, err := NewAPI().upsertLSPSupervisorIntent(entry, "mcphub.exe")
	if err != nil {
		t.Fatalf("upsert descriptor: %v", err)
	}
	if err := NewAPI().WriteStopIntent(taskName, freshStop, "tester"); err != nil {
		t.Fatalf("concurrent WriteStopIntent: %v", err)
	}

	restore()

	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after rollback: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row != nil {
		t.Fatalf("upsert rollback left descriptor behind: %+v", row)
	}
	assertDaemonIntentEqual(t, intent.Stops[taskName], freshStop)
	if _, ok := intent.LegacyStopWatermarks[taskName]; ok {
		t.Fatalf("upsert rollback restored stale watermark beside concurrent stop: %+v", intent.LegacyStopWatermarks)
	}
}
