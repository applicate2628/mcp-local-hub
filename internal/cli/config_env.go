package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

func newConfigEnvCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "env",
		Short: "Manage per-daemon environment overrides",
		Long: `Manage per-daemon environment overrides stored under the hub state directory.

Overrides are applied when a supervised daemon is spawned. A restart is required
for a changed value to affect an already-running daemon.

Selectors accept:
  server          when the server has exactly one daemon
  server/daemon   when the server has multiple daemons
  task name       mcp-local-hub-... or \mcp-local-hub-...`,
	}
	c.AddCommand(newConfigEnvSetCmd())
	c.AddCommand(newConfigEnvUnsetCmd())
	c.AddCommand(newConfigEnvListCmd())
	return c
}

func newConfigEnvSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <server|server/daemon|task> <KEY> <VALUE>",
		Short: "Set one environment override",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir, err := stateDirFunc()
			if err != nil {
				return fmt.Errorf("resolve state dir: %w", err)
			}
			return runConfigEnvSet(stateDir, args[0], args[1], args[2], cmd.OutOrStdout())
		},
	}
}

func newConfigEnvUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <server|server/daemon|task> <KEY>",
		Short: "Remove one environment override",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir, err := stateDirFunc()
			if err != nil {
				return fmt.Errorf("resolve state dir: %w", err)
			}
			return runConfigEnvUnset(stateDir, args[0], args[1], cmd.OutOrStdout())
		},
	}
}

func newConfigEnvListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [server|server/daemon|task]",
		Short: "List environment overrides",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir, err := stateDirFunc()
			if err != nil {
				return fmt.Errorf("resolve state dir: %w", err)
			}
			selector := ""
			if len(args) > 0 {
				selector = args[0]
			}
			return runConfigEnvList(stateDir, selector, cmd.OutOrStdout())
		},
	}
}

func runConfigEnvSet(stateDir, selector, key, value string, out io.Writer) error {
	key = strings.TrimSpace(key)
	if err := daemon_env_overlay.ValidateEnvMap(map[string]string{key: value}); err != nil {
		return err
	}
	target, err := resolveSingleConfigEnvTarget(stateDir, selector)
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(stateDir, overlayBaseName)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := daemon_env_overlay.WriteOverlay(overlayPath, func(ov *daemon_env_overlay.Overlay) error {
		row := ov.Daemons[target.TaskName]
		if row.Env == nil {
			row.Env = map[string]string{}
		}
		row.Env[key] = value
		row.Source = "operator"
		row.ModifiedAt = now
		ov.Daemons[target.TaskName] = row
		return nil
	}); err != nil {
		return fmt.Errorf("write env overlay: %w", err)
	}
	fmt.Fprintf(out, "set %s %s. Restart the daemon for this change to take effect.\n", target.TaskName, key)
	return nil
}

func runConfigEnvUnset(stateDir, selector, key string, out io.Writer) error {
	key = strings.TrimSpace(key)
	if !daemon_env_overlay.ValidEnvKey(key) {
		return fmt.Errorf("invalid env key %q: must match [A-Za-z_][A-Za-z0-9_]*", key)
	}
	target, err := resolveSingleConfigEnvTarget(stateDir, selector)
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(stateDir, overlayBaseName)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := daemon_env_overlay.WriteOverlay(overlayPath, func(ov *daemon_env_overlay.Overlay) error {
		row, ok := ov.Daemons[target.TaskName]
		if !ok || row.Env == nil {
			return nil
		}
		delete(row.Env, key)
		if len(row.Env) == 0 {
			delete(ov.Daemons, target.TaskName)
			return nil
		}
		row.Source = "operator"
		row.ModifiedAt = now
		ov.Daemons[target.TaskName] = row
		return nil
	}); err != nil {
		return fmt.Errorf("write env overlay: %w", err)
	}
	fmt.Fprintf(out, "unset %s %s. Restart the daemon for this change to take effect.\n", target.TaskName, key)
	return nil
}

func runConfigEnvList(stateDir, selector string, out io.Writer) error {
	targets, err := resolveConfigEnvTargets(stateDir, selector)
	if err != nil {
		return err
	}
	ov, err := daemon_env_overlay.Load(filepath.Join(stateDir, overlayBaseName))
	if err != nil {
		return fmt.Errorf("load env overlay: %w", err)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].TaskName < targets[j].TaskName })
	for _, target := range targets {
		env := daemon_env_overlay.LookupOverlay(ov, target.TaskName)
		if len(env) == 0 {
			fmt.Fprintf(out, "%s (%s/%s): no overrides\n", target.TaskName, target.Server, target.Daemon)
			continue
		}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(out, "%s (%s/%s)\n", target.TaskName, target.Server, target.Daemon)
		for _, k := range keys {
			fmt.Fprintf(out, "  %s=%s\n", k, env[k])
		}
	}
	return nil
}

type configEnvTarget struct {
	TaskName  string
	Server    string
	Daemon    string
	Workspace string
}

func resolveSingleConfigEnvTarget(stateDir, selector string) (configEnvTarget, error) {
	targets, err := resolveConfigEnvTargets(stateDir, selector)
	if err != nil {
		return configEnvTarget{}, err
	}
	if len(targets) == 0 {
		return configEnvTarget{}, fmt.Errorf("no daemon matches %q", selector)
	}
	if len(targets) > 1 {
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, strings.TrimPrefix(t.TaskName, `\`))
		}
		sort.Strings(names)
		return configEnvTarget{}, fmt.Errorf("selector %q matches multiple daemons (%s); use server/daemon or a full task name", selector, strings.Join(names, ", "))
	}
	return targets[0], nil
}

func resolveConfigEnvTargets(stateDir, selector string) ([]configEnvTarget, error) {
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("supervisor-intent.json not found under %s; install/register daemons before setting env overrides", stateDir)
		}
		return nil, fmt.Errorf("read supervisor-intent.json: %w", err)
	}
	selector = strings.TrimSpace(selector)
	serverSelector := selector
	daemonSelector := ""
	if strings.Contains(selector, "/") {
		parts := strings.Split(selector, "/")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("invalid selector %q: expected server/daemon", selector)
		}
		serverSelector = strings.TrimSpace(parts[0])
		daemonSelector = strings.TrimSpace(parts[1])
	}
	taskSelector := ""
	if selector != "" {
		trimmed := strings.TrimPrefix(selector, `\`)
		if strings.HasPrefix(trimmed, "mcp-local-hub-") {
			taskSelector = daemon_env_overlay.NormalizeOverlayKey(trimmed)
		}
	}

	var out []configEnvTarget
	for _, d := range intent.Daemons {
		taskName := daemon_env_overlay.NormalizeOverlayKey(d.TaskName)
		if taskName == "" || isSupervisorMaintenanceTask(taskName) {
			continue
		}
		server, daemon := api.ParseManagedTaskName(taskName)
		if server == "" {
			server = d.Server
		}
		if daemon == "" {
			daemon = d.Daemon
			if daemon == "" {
				daemon = "default"
			}
		}
		target := configEnvTarget{
			TaskName:  taskName,
			Server:    server,
			Daemon:    daemon,
			Workspace: d.Workspace,
		}
		switch {
		case selector == "":
			out = append(out, target)
		case taskSelector != "":
			if taskName == taskSelector {
				out = append(out, target)
			}
		case daemonSelector != "":
			if server == serverSelector && daemon == daemonSelector {
				out = append(out, target)
			}
		default:
			if server == serverSelector {
				out = append(out, target)
			}
		}
	}
	return out, nil
}
