package api

import "testing"


// TestIsHubDaemonSchedulerTaskName_WeeklyRefreshExcluded pins that the
// per-server weekly-refresh maintenance task is NEVER classified as a daemon
// task by the shared predicate the Stop/Restart/StopAll loops rely on (bot PR
// #288 r29 raised the concern; the -weekly-refresh suffix is caught by
// isMaintenanceTaskName via isHubInfrastructureTaskName — this test turns the
// empirical refutation into a permanent regression pin).
func TestIsHubDaemonSchedulerTaskName_WeeklyRefreshExcluded(t *testing.T) {
	for _, name := range []string{
		`\mcp-local-hub-demo-weekly-refresh`,
		"mcp-local-hub-demo-weekly-refresh",
		`\mcp-local-hub-serena-weekly-refresh`,
		`\mcp-local-hub-supervisor`,
		`\mcp-local-hub-liveness`,
	} {
		if isHubDaemonSchedulerTaskName(name) {
			t.Fatalf("maintenance/infrastructure task %q classified as a daemon", name)
		}
	}
	if !isHubDaemonSchedulerTaskName(`\mcp-local-hub-demo-alpha`) {
		t.Fatalf("genuine daemon task not classified as a daemon")
	}
}
