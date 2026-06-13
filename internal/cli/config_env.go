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
		if _, present := row.Env[key]; !present {
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

	// installedServers is resolved lazily (only for a blank-field
	// daemonSelector decision) so the common selector paths never pay the
	// catalog read. It carries the longest-installed-prefix disambiguator's
	// reference set; a read failure leaves it empty, which the predicate
	// treats as "no sibling proof exists" (claim any prefix-matching row).
	var installedServers map[string]struct{}
	installedServersResolved := false
	ensureInstalledServers := func() map[string]struct{} {
		if installedServersResolved {
			return installedServers
		}
		installedServersResolved = true
		installedServers = map[string]struct{}{}
		if names, err := api.NewAPI().ManifestList(); err == nil {
			for _, n := range names {
				installedServers[n] = struct{}{}
			}
		}
		return installedServers
	}

	var out []configEnvTarget
	for _, d := range intent.Daemons {
		taskName := daemon_env_overlay.NormalizeOverlayKey(d.TaskName)
		if taskName == "" || isSupervisorMaintenanceTask(taskName) {
			continue
		}
		server := strings.TrimSpace(d.Server)
		daemon := strings.TrimSpace(d.Daemon)
		if server == "" {
			parsedServer, parsedDaemon := api.ParseManagedTaskName(taskName)
			server = parsedServer
			if daemon == "" {
				daemon = parsedDaemon
			}
		}
		if daemon == "" {
			_, parsedDaemon := api.ParseManagedTaskName(taskName)
			daemon = parsedDaemon
		}
		if daemon == "" {
			daemon = "default"
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
			// Both identity components are KNOWN here (server/daemon selector).
			//
			// A populated-field row carries its true split, so the precise field
			// compare is exact and correct (unchanged path).
			//
			// A blank-field row (Server=="" || Daemon=="") derived its (server,
			// daemon) via api.ParseManagedTaskName, whose last-hyphen split
			// misattributes hyphenated daemon names: \mcp-local-hub-demo-alpha-beta
			// (real server "demo", daemon "alpha-beta") parses as demo-alpha/beta.
			// A field compare would MISS the real demo/alpha-beta row and let
			// demo-alpha/beta wrongly claim it. Reconstruction alone can't fix it
			// either — both demo/alpha-beta and demo-alpha/beta rebuild the SAME
			// canonical name \mcp-local-hub-demo-alpha-beta, so a bare want-compare
			// would let either selector claim the row.
			//
			// Mirror the landed sibling family's authoritative disambiguator
			// api.blankServerRowOwnedByLongestInstalledPrefix
			// (internal/api/install_parsed_manifest.go, r33-2): the blank-field
			// row's true server is the LONGEST installed-server-name prefix of its
			// canonical task portion. demo/alpha-beta claims the row iff "demo" is
			// the longest installed prefix (demo-alpha not installed); demo-alpha/beta
			// claims it iff "demo-alpha" is installed. The reconstructed-name match
			// (taskName == want) AND the longest-prefix ownership of serverSelector
			// must BOTH hold. installedServers empty (catalog read failed) → no
			// sibling proof, any prefix-matching row is claimed (safe outcome).
			if d.Server == "" || d.Daemon == "" {
				want := daemon_env_overlay.NormalizeOverlayKey("mcp-local-hub-" + serverSelector + "-" + daemonSelector)
				if taskName == want && blankServerTaskOwnedByLongestInstalledPrefix(taskName, serverSelector, ensureInstalledServers()) {
					out = append(out, target)
				}
			} else if server == serverSelector && daemon == daemonSelector {
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

// blankServerTaskOwnedByLongestInstalledPrefix decides whether a blank-field
// supervisor-intent row whose canonical task name is `\mcp-local-hub-<X>` is
// owned by `server` under the longest-installed-prefix disambiguator. It is the
// config-env-side mirror of api.blankServerRowOwnedByLongestInstalledPrefix
// (internal/api/install_parsed_manifest.go, r33-2) — kept in-package because the
// api predicate is unexported.
//
// One adaptation vs the api predicate: there the `server` argument is always the
// manifest's OWN name (m.Name), so the caller has already proved `server` is an
// installed server and the predicate only needs the longer-sibling check. Here
// `server` is the OPERATOR's free-form selector, which may name a server that is
// not installed at all (`demo-alpha/beta` against a row that is really
// demo/alpha-beta). So when the catalog is non-empty we additionally require
// `server` itself to be installed: otherwise both demo/alpha-beta AND
// demo-alpha/beta (which rebuild the same canonical name) would claim the row.
// When the catalog is EMPTY (read failed) the membership and sibling checks are
// both vacuous and any prefix-matching row is claimed — the same safe
// no-sibling-proof outcome as the api predicate.
//
// Returns true IFF `<X>` starts with `server+"-"`, AND (catalog empty OR `server`
// is installed), AND no OTHER installed server name S (S != server,
// len(S) > len(server)) is also a prefix of `<X>` in the same `S+"-"` form.
// taskName is already canonical (NormalizeOverlayKey applied by the caller).
func blankServerTaskOwnedByLongestInstalledPrefix(taskName, server string, installedServers map[string]struct{}) bool {
	const taskPrefix = `\mcp-local-hub-`
	canonical := daemon_env_overlay.NormalizeOverlayKey(taskName)
	portion, ok := strings.CutPrefix(canonical, taskPrefix)
	if !ok {
		return false
	}
	// `<X>` must be `server-<daemon...>`: starts with server followed by a
	// hyphen (a bare `server` with no daemon segment is degenerate and not a
	// daemon row this prefix server should claim).
	if !strings.HasPrefix(portion, server+"-") {
		return false
	}
	if len(installedServers) > 0 {
		if _, ok := installedServers[server]; !ok {
			// The operator named a server that is not installed; a longer or
			// shorter installed sibling owns the row, not this selector.
			return false
		}
	}
	for s := range installedServers {
		if s == server || len(s) <= len(server) {
			continue
		}
		if strings.HasPrefix(portion, s+"-") {
			// A longer installed server name is also a prefix — it owns the
			// row, so `server` must not claim it.
			return false
		}
	}
	return true
}
