package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"

	"github.com/spf13/cobra"
)

// Plan describes the side effects that `mcp install --server X` would produce.
// Returned by BuildPlan and rendered by `install --dry-run`.
type Plan struct {
	Server        string
	SchedulerTasks []ScheduledTaskPlan
	ClientUpdates  []ClientUpdatePlan
}

type ScheduledTaskPlan struct {
	Name    string
	Command string
	Args    []string
	Trigger string // human-readable
}

type ClientUpdatePlan struct {
	Client string
	Path   string
	Action string // "add" | "replace"
	URL    string
}

func newInstallCmdReal() *cobra.Command {
	var server string
	var dryRun bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install an MCP server as shared daemon(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			manifestPath := filepath.Join("servers", server, "manifest.yaml")
			f, err := os.Open(manifestPath)
			if err != nil {
				return fmt.Errorf("open %s: %w", manifestPath, err)
			}
			defer f.Close()
			m, err := config.ParseManifest(f)
			if err != nil {
				return err
			}
			plan, err := BuildPlan(m)
			if err != nil {
				return err
			}
			if dryRun {
				return printPlan(cmd, plan)
			}
			return fmt.Errorf("real install not yet implemented — use --dry-run (Task 20 wires this)")
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (matches servers/<name>/manifest.yaml)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without making changes")
	return c
}

// BuildPlan translates a manifest into concrete intended actions.
func BuildPlan(m *config.ServerManifest) (*Plan, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	p := &Plan{Server: m.Name}
	// Scheduler tasks — one per daemon (global) or lazy (workspace-scoped).
	for _, d := range m.Daemons {
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-" + d.Name,
			Command: exe,
			Args:    []string{"daemon", "--server", m.Name, "--daemon", d.Name},
			Trigger: "At logon",
		})
	}
	if m.WeeklyRefresh {
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-weekly-refresh",
			Command: exe,
			Args:    []string{"restart", "--server", m.Name},
			Trigger: "Weekly Sun 03:00",
		})
	}
	// Client updates — one per binding.
	for _, b := range m.ClientBindings {
		daemon, ok := findDaemon(m, b.Daemon)
		if !ok {
			return nil, fmt.Errorf("binding references unknown daemon %q", b.Daemon)
		}
		path, err := clientConfigPath(b.Client)
		if err != nil {
			return nil, err
		}
		urlPath := b.URLPath
		if urlPath == "" {
			urlPath = "/mcp"
		}
		url := fmt.Sprintf("http://localhost:%d%s", daemon.Port, urlPath)
		p.ClientUpdates = append(p.ClientUpdates, ClientUpdatePlan{
			Client: b.Client,
			Path:   path,
			Action: "add/replace",
			URL:    url,
		})
	}
	return p, nil
}

func findDaemon(m *config.ServerManifest, name string) (config.DaemonSpec, bool) {
	for _, d := range m.Daemons {
		if d.Name == name {
			return d, true
		}
	}
	return config.DaemonSpec{}, false
}

func printPlan(cmd *cobra.Command, p *Plan) error {
	cmd.Printf("Install plan for server %q (dry-run):\n\n", p.Server)
	cmd.Printf("  Scheduler tasks to create (%d):\n", len(p.SchedulerTasks))
	for _, t := range p.SchedulerTasks {
		cmd.Printf("    • %s  [%s]\n        %s %v\n", t.Name, t.Trigger, t.Command, t.Args)
	}
	cmd.Printf("\n  Client configs to update (%d):\n", len(p.ClientUpdates))
	for _, u := range p.ClientUpdates {
		cmd.Printf("    • %s (%s)\n        %s  →  %s\n", u.Client, u.Path, u.Action, u.URL)
	}
	cmd.Println("\nNo changes made.")
	_ = clients.Client(nil) // keep import live for later tasks
	return nil
}
