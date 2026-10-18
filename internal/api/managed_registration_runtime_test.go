package api

import (
	"context"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
)

// TestW10_ManagedRegistrationFrozenInstallRuntimeRejectsIncompleteInput catches
// a constructor that fills frozen install facts from a manifest, state file, or
// other ambient source instead of rejecting incomplete input before any write.
func TestW10_ManagedRegistrationFrozenInstallRuntimeRejectsIncompleteInput(t *testing.T) {
	valid := w10FrozenRuntimeInput()
	cases := []struct {
		name   string
		mutate func(*managedRegistrationFrozenInstallInput)
	}{
		{"missing backup io", func(in *managedRegistrationFrozenInstallInput) { in.BackupIO = nil }},
		{"typed nil backup io", func(in *managedRegistrationFrozenInstallInput) {
			var backup *w10RuntimeBackupIO
			in.BackupIO = backup
		}},
		{"non UTC capture", func(in *managedRegistrationFrozenInstallInput) { in.CapturedAtUTC = time.Now() }},
		{"non absolute state root", func(in *managedRegistrationFrozenInstallInput) { in.StateRoot = "state"; in.JournalRoot = "state" }},
		{"unproven action", func(in *managedRegistrationFrozenInstallInput) { in.Targets[0].Action = ClientUpdateRemove }},
		{"desired name differs from physical key", func(in *managedRegistrationFrozenInstallInput) { in.Targets[0].Desired.Name = "different" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			in.Targets = append([]managedRegistrationFrozenInstallTarget(nil), valid.Targets...)
			tc.mutate(&in)
			if runtime, err := newManagedRegistrationRuntime(in); err == nil || runtime != nil {
				t.Fatalf("newManagedRegistrationRuntime() = (%v, %v), want rejected input", runtime, err)
			}
		})
	}
}

// TestW10_ManagedRegistrationFrozenInstallRuntimeClonesCapturedInput catches a
// runtime that retains caller-owned maps or slices and can therefore change
// after plan capture without another explicit composition step.
func TestW10_ManagedRegistrationFrozenInstallRuntimeClonesCapturedInput(t *testing.T) {
	in := w10FrozenRuntimeInput()
	runtime, err := newManagedRegistrationRuntime(in)
	if err != nil {
		t.Fatalf("newManagedRegistrationRuntime: %v", err)
	}
	wantReceipt := runtime.SourceReceipt()
	in.Targets[0].Desired.Headers["X-Test"] = "changed"
	in.Targets[0].Desired.Env["KEEP"] = "changed"
	in.ClientConstructionFailures[0] = "codex-cli"
	if got := runtime.frozen.Targets[0].Desired.Headers["X-Test"]; got != "captured" {
		t.Fatalf("runtime header = %q, want captured value", got)
	}
	if got := runtime.frozen.Targets[0].Desired.Env["KEEP"]; got != "captured" {
		t.Fatalf("runtime env = %q, want captured value", got)
	}
	if got := runtime.SourceReceipt(); got != wantReceipt {
		t.Fatalf("source receipt changed after caller mutation: got %q, want %q", got, wantReceipt)
	}
	if got := in.BackupIO.(*w10RuntimeBackupIO).calls; got != 0 {
		t.Fatalf("constructor invoked backup I/O %d times", got)
	}
}

func w10FrozenRuntimeInput() managedRegistrationFrozenInstallInput {
	return managedRegistrationFrozenInstallInput{
		Server:                     "serena",
		ReleaseIdentity:            managedRegistrationReleaseIdentity{Version: "0.4.36", Commit: "test-commit"},
		CapturedAtUTC:              time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
		StateRoot:                  "C:\\state",
		JournalRoot:                "C:\\state",
		BackupIO:                   &w10RuntimeBackupIO{},
		ClientPopulation:           map[string]clients.Client{},
		ClientConstructionFailures: []string{"claude-code"},
		Targets: []managedRegistrationFrozenInstallTarget{{
			ClientID:          "claude-code",
			LogicalEntryName:  "serena",
			PhysicalEntryName: "serena",
			Action:            ClientUpdateAddReplace,
			DaemonName:        "default",
			TaskName:          "mcp-local-hub-serena-default",
			Port:              9121,
			URLPath:           "/mcp",
			Desired: clients.MCPEntry{
				Name:    "serena",
				URL:     "http://127.0.0.1:9121/mcp",
				Headers: map[string]string{"X-Test": "captured"},
				Env:     map[string]string{"KEEP": "captured"},
			},
		}},
	}
}

type w10RuntimeBackupIO struct{ calls int }

func (io *w10RuntimeBackupIO) Create(_ context.Context, _ string, _ string, _ []byte, _ string) (clients.ManagedRegistrationBackupCreateReceipt, error) {
	io.calls++
	return clients.ManagedRegistrationBackupCreateReceipt{}, nil
}

func (io *w10RuntimeBackupIO) Read(_ context.Context, _ string, _ string, _ string) ([]byte, error) {
	io.calls++
	return nil, nil
}
