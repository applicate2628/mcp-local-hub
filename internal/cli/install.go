package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"

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
	Client     string
	Path       string
	Action     string // "add" | "replace"
	URL        string
	DaemonName string // manifest daemon this binding points at (for relay-aware adapters)
}

func newInstallCmdReal() *cobra.Command {
	var server string
	var daemonFilter string
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
			if err := Preflight(m, daemonFilter); err != nil {
				return err
			}
			plan, err := BuildPlan(m, daemonFilter)
			if err != nil {
				return err
			}
			if dryRun {
				return printPlan(cmd, plan)
			}
			return executeInstall(cmd, m, plan)
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (matches servers/<name>/manifest.yaml)")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "install only this daemon (+ its client bindings); omit to install all")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without making changes")
	return c
}

// BuildPlan translates a manifest into concrete intended actions.
// If daemonFilter is non-empty, only that daemon and its referencing client
// bindings are included; weekly refresh is skipped because a partial install
// does not imply a full-server restart. An unknown daemonFilter is an error
// surfaced before any side effects.
func BuildPlan(m *config.ServerManifest, daemonFilter string) (*Plan, error) {
	if daemonFilter != "" {
		if _, ok := findDaemon(m, daemonFilter); !ok {
			return nil, fmt.Errorf("no daemon %q in manifest %s", daemonFilter, m.Name)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	p := &Plan{Server: m.Name}
	// Scheduler tasks — one per daemon (global) or lazy (workspace-scoped).
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-" + d.Name,
			Command: exe,
			Args:    []string{"daemon", "--server", m.Name, "--daemon", d.Name},
			Trigger: "At logon",
		})
	}
	// Weekly refresh restarts the whole server, so it only makes sense for full installs.
	if m.WeeklyRefresh && daemonFilter == "" {
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-weekly-refresh",
			Command: exe,
			Args:    []string{"restart", "--server", m.Name},
			Trigger: "Weekly Sun 03:00",
		})
	}
	// Client updates — one per binding; with a filter, only bindings pointing at the chosen daemon.
	for _, b := range m.ClientBindings {
		if daemonFilter != "" && b.Daemon != daemonFilter {
			continue
		}
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
			Client:     b.Client,
			Path:       path,
			Action:     "add/replace",
			URL:        url,
			DaemonName: b.Daemon,
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

func executeInstall(cmd *cobra.Command, m *config.ServerManifest, p *Plan) error {
	sch, err := scheduler.New()
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	repoDir, err := os.Getwd()
	if err != nil {
		return err
	}
	// 1. Create scheduler tasks.
	for _, t := range p.SchedulerTasks {
		spec := scheduler.TaskSpec{
			Name:             t.Name,
			Description:      "mcp-local-hub: " + m.Name,
			Command:          t.Command,
			Args:             t.Args,
			WorkingDir:       repoDir,
			RestartOnFailure: true,
		}
		if t.Trigger == "At logon" {
			spec.LogonTrigger = true
		} else if t.Trigger == "Weekly Sun 03:00" {
			spec.WeeklyTrigger = &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
		}
		// Delete any previous instance so Create is idempotent.
		_ = sch.Delete(spec.Name)
		if err := sch.Create(spec); err != nil {
			return fmt.Errorf("create task %s: %w", spec.Name, err)
		}
		cmd.Printf("✓ Scheduler task created: %s\n", spec.Name)
	}
	// 2. Backup + update client configs.
	// Populate relay-related fields so adapters for stdio-only clients
	// (e.g. Antigravity) can produce their `command`+`args` entry shape
	// invoking `mcp.exe relay`. HTTP-native adapters ignore these fields.
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	allClients := mustAllClients()
	for _, u := range p.ClientUpdates {
		client := allClients[u.Client]
		if client == nil {
			return fmt.Errorf("unknown client %q in binding", u.Client)
		}
		if !client.Exists() {
			cmd.Printf("⚠ Client %s not installed on this machine — skipping\n", u.Client)
			continue
		}
		bak, err := client.Backup()
		if err != nil {
			return fmt.Errorf("backup %s: %w", u.Client, err)
		}
		cmd.Printf("  backup: %s\n", bak)
		entry := clients.MCPEntry{
			Name:         m.Name,
			URL:          u.URL,
			RelayServer:  m.Name,
			RelayDaemon:  u.DaemonName,
			RelayExePath: exePath,
		}
		if err := client.AddEntry(entry); err != nil {
			return fmt.Errorf("add entry to %s: %w", u.Client, err)
		}
		cmd.Printf("✓ %s → %s\n", u.Client, u.URL)
	}
	// 3. Start daemons immediately (without waiting for next logon).
	for _, t := range p.SchedulerTasks {
		// Skip weekly refresh — it's triggered on schedule, not on install.
		if t.Trigger != "At logon" {
			continue
		}
		if err := sch.Run(t.Name); err != nil {
			cmd.Printf("⚠ failed to start %s immediately: %v (will start at next logon)\n", t.Name, err)
		} else {
			cmd.Printf("✓ Started: %s\n", t.Name)
		}
	}
	cmd.Println("\nInstall complete.")
	return nil
}

func mustAllClients() map[string]clients.Client {
	result := map[string]clients.Client{}
	for _, factory := range []func() (clients.Client, error){
		clients.NewClaudeCode, clients.NewCodexCLI, clients.NewGeminiCLI, clients.NewAntigravity,
	} {
		c, err := factory()
		if err != nil {
			continue
		}
		result[c.Name()] = c
	}
	return result
}
