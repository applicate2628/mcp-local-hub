// Package cli — Task 7.1 reconcile-loop diff tests.
//
// Spec §"Reconcile loop" + plan Task 7.1.
//
// These tests pin the three contracts the Reconciler must hold:
//
//  1. Daemons in intent + NOT IsActiveStop + NOT running → spawn fans
//     out exactly once.
//  2. Daemons in intent + IsActiveStop (user-disabled / chronic-failure /
//     within-TTL user-stop) → spawn MUST NOT fire. This is the
//     quarantine-respect path; a regression here would auto-revive a
//     daemon the operator explicitly stopped.
//  3. Daemons in currentRunning but NOT in intent are NOT terminated
//     by the reconciler — orphan handling belongs to Task 13.1 cold-
//     start reaper since the reconciler has no descriptor to fan a
//     terminate through.
//  4. LSP workspace-proxy descriptors whose backing registry row is gone
//     are excluded from the desired set; if one is already running, the
//     reconciler uses the descriptor it still has to terminate it.
package cli

import (
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestReconcile_SpawnsMissing(t *testing.T) {
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Command: "fake-noop"},
		},
	}
	daemonIntent := &api.DaemonIntentFile{} // no stops

	spawned := []string{}
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawned = append(spawned, d.TaskName)
		return nil
	}
	r := NewReconciler(fakeSpawn, func(d api.SupervisorDaemon) error { return nil })
	r.Reconcile(intent, daemonIntent, map[string]bool{}, time.Now())
	if len(spawned) != 1 || spawned[0] != `\mcp-local-hub-memory-default` {
		t.Fatalf("expected spawn of memory-default, got %v", spawned)
	}
}

func TestReconcile_SkipsIsActiveStop(t *testing.T) {
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Command: "fake-noop"},
		},
	}
	now := time.Now().UTC()
	daemonIntent := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			`\mcp-local-hub-memory-default`: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserDisabled,
				UpdatedAt: now,
			},
		},
	}

	spawned := []string{}
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawned = append(spawned, d.TaskName)
		return nil
	}
	r := NewReconciler(fakeSpawn, func(d api.SupervisorDaemon) error { return nil })
	r.Reconcile(intent, daemonIntent, map[string]bool{}, now)
	if len(spawned) != 0 {
		t.Fatalf("user-disabled daemon should not spawn: %v", spawned)
	}
}

func TestReconcile_TerminatesExtras(t *testing.T) {
	// Intent says no daemons should run, but the "current" set says one is running.
	intent := &api.SupervisorIntentFile{Version: 1}
	daemonIntent := &api.DaemonIntentFile{}
	currentRunning := map[string]bool{
		`\mcp-local-hub-orphan-default`: true,
	}

	terminated := []string{}
	fakeTerminate := func(d api.SupervisorDaemon) error {
		terminated = append(terminated, d.TaskName)
		return nil
	}
	r := NewReconciler(func(d api.SupervisorDaemon) error { return nil }, fakeTerminate)
	r.Reconcile(intent, daemonIntent, currentRunning, time.Now())
	// Terminating an orphan needs the daemon descriptor we don't have in
	// intent; reconciler must skip terminating daemons it has no
	// descriptor for (caller's responsibility to clean up orphans via
	// the separate cold-start reaper Task 13.1).
	if len(terminated) != 0 {
		t.Fatalf("orphan termination should be deferred to cold-start reaper, got %v", terminated)
	}
}

func TestReconcile_TerminatesRunningOrphanedLSPDescriptor(t *testing.T) {
	descriptor := api.BuildSupervisorDaemonForLSP(api.WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "/tmp/mcphub-lsp",
		Language:      "go",
		Port:          33051,
	}, "mcphub")
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{descriptor},
	}

	spawned := []string{}
	terminated := []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error {
			spawned = append(spawned, d.TaskName)
			return nil
		},
		func(d api.SupervisorDaemon) error {
			terminated = append(terminated, d.TaskName)
			return nil
		},
	)
	r.LSPRegistryHasRow = func(d api.SupervisorDaemon) bool {
		if d.TaskName != descriptor.TaskName {
			t.Fatalf("registry predicate saw task %q, want %q", d.TaskName, descriptor.TaskName)
		}
		return false
	}

	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{descriptor.TaskName: true}, time.Now())

	if len(spawned) != 0 {
		t.Fatalf("running orphan LSP descriptor must not spawn; got %v", spawned)
	}
	if len(terminated) != 1 || terminated[0] != descriptor.TaskName {
		t.Fatalf("running orphan LSP descriptor terminated = %v, want [%s]", terminated, descriptor.TaskName)
	}
}

func TestReconcile_LSPRegistryReadErrorFailOpenLeavesRunningDescriptor(t *testing.T) {
	descriptor := api.BuildSupervisorDaemonForLSP(api.WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "/tmp/mcphub-lsp",
		Language:      "go",
		Port:          33052,
	}, "mcphub")
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{descriptor},
	}

	spawned := []string{}
	terminated := []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error {
			spawned = append(spawned, d.TaskName)
			return nil
		},
		func(d api.SupervisorDaemon) error {
			terminated = append(terminated, d.TaskName)
			return nil
		},
	)
	r.LSPRegistryHasRow = func(d api.SupervisorDaemon) bool {
		// api.LSPRegistryRowBacksDescriptor fails open on registry read/lock
		// errors; the reconciler seam sees that transient read failure as true.
		return true
	}

	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{descriptor.TaskName: true}, time.Now())

	if len(spawned) != 0 || len(terminated) != 0 {
		t.Fatalf("registry read-error fail-open must leave running descriptor alone; spawned=%v terminated=%v", spawned, terminated)
	}
}
