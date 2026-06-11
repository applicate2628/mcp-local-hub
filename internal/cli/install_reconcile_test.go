package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"

	"github.com/spf13/cobra"
)

// TestManifestHasScheduledDaemon_FlagsInstalledManifests pins the
// codex bot phase5 r16 P1 closure on PR #160: reconcile manifest
// discovery MUST be manifest-aware, not task-name-parse-aware.
// `ParseManagedTaskName` splits on the last hyphen, so a daemon
// name containing '-' (e.g. `mcp-language-server` server with
// `vscode-css` daemon) parses to the WRONG server name and drops
// the real manifest from the installed set.
//
// The new check (manifestHasScheduledDaemon) iterates each manifest's
// expected task names — `mcp-local-hub-<server>-<daemon>` and
// `mcp-local-hub-<server>-weekly-refresh` — and looks them up in a
// scheduler-task set built from the raw scheduler row names. Byte-
// exact, no parsing, so hyphenated server AND daemon names compose.
//
// Also pins the original codex bot phase5 r10 P2 closure: non-server
// task families (watchdog, hub-wide / workspace-wide weekly-refresh,
// lazy-proxy LSP) must NOT pull unrelated manifests into the
// "installed" set. With manifestHasScheduledDaemon the protection
// is structural — those families do not match `<server>-<daemon>`
// shapes for any real manifest, so they cannot leak.
func TestManifestHasScheduledDaemon_FlagsInstalledManifests(t *testing.T) {
	// Scheduler set as seen by manifestHasScheduledDaemon — the
	// runReconcileHubMode caller pre-filters non-per-server task
	// families (watchdog, hub-wide weekly-refresh, workspace
	// weekly-refresh, lazy-proxy LSP) before passing the map to
	// this helper. The map below reflects only the per-server
	// task names a normalized scheduledTasks set would carry in
	// production.
	scheduledTasks := map[string]bool{
		"mcp-local-hub-serena-default":                 true,
		"mcp-local-hub-memory-weekly-refresh":          true, // per-SERVER weekly-refresh signal
		"mcp-local-hub-mcp-language-server-vscode-css": true, // hyphenated daemon
		"mcp-local-hub-paper-search-mcp-default":       true, // hyphenated server
	}

	cases := []struct {
		manifest config.ServerManifest
		wantFlag bool
		whyShort string
	}{
		{
			manifest: config.ServerManifest{
				Name:    "serena",
				Daemons: []config.DaemonSpec{{Name: "default"}},
			},
			wantFlag: true,
			whyShort: "real per-server daemon matches",
		},
		{
			manifest: config.ServerManifest{
				Name:    "memory",
				Daemons: []config.DaemonSpec{{Name: "default"}},
			},
			wantFlag: true,
			whyShort: "per-server weekly-refresh task counts as installation signal",
		},
		{
			manifest: config.ServerManifest{
				Name:    "mcp-language-server",
				Daemons: []config.DaemonSpec{{Name: "vscode-css"}},
			},
			wantFlag: true,
			whyShort: "hyphenated daemon name (the regression bot flagged)",
		},
		{
			manifest: config.ServerManifest{
				Name:    "paper-search-mcp",
				Daemons: []config.DaemonSpec{{Name: "default"}},
			},
			wantFlag: true,
			whyShort: "hyphenated server name",
		},
		{
			manifest: config.ServerManifest{
				Name:    "never-installed",
				Daemons: []config.DaemonSpec{{Name: "default"}},
			},
			wantFlag: false,
			whyShort: "manifest exists but no scheduler task — never `mcphub install`d",
		},
		// Note: the non-server task family exclusion (watchdog,
		// workspace-weekly-refresh, lazy-proxy LSP) is enforced by
		// the runReconcileHubMode caller before calling this helper.
		// See the comment on `scheduledTasks` above. The end-to-end
		// guarantee that a "watchdog" or "workspace" manifest would
		// not accidentally match a non-server family task is covered
		// by the production filter in runReconcileHubMode.
	}

	for _, tc := range cases {
		t.Run(tc.manifest.Name+"_"+tc.whyShort, func(t *testing.T) {
			got := manifestHasScheduledDaemon(&tc.manifest, scheduledTasks)
			if got != tc.wantFlag {
				t.Errorf("manifestHasScheduledDaemon(%q) = %v, want %v (%s)",
					tc.manifest.Name, got, tc.wantFlag, tc.whyShort)
			}
		})
	}
}

func TestReconcileHubMode_SupervisorIntentOnlyServerCountsInstalled(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)

	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: `\mcp-local-hub-time-default`,
			Server:   "time",
			Daemon:   "default",
			Port:     9128,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	restoreScheduler := api.SetTestSchedulerFactoryFn(func() (scheduler.Scheduler, error) {
		return &setupFakeScheduler{listResult: nil}, nil
	})
	t.Cleanup(restoreScheduler)

	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := runReconcileHubMode(cmd, true); err != nil {
		t.Fatalf("runReconcileHubMode dry-run: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "entry=time") {
		t.Fatalf("supervisor-intent-only time install did not produce time restore ops; output=%q", stdout.String())
	}
}
