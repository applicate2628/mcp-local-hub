package api

import (
	"testing"

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
