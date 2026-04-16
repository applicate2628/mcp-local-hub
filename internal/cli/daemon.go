package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/secrets"

	"github.com/spf13/cobra"
)

func newDaemonCmdReal() *cobra.Command {
	var server, daemonName string
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run a daemon (invoked by scheduler, not by humans)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" || daemonName == "" {
				return fmt.Errorf("--server and --daemon are required")
			}
			manifestPath := filepath.Join("servers", server, "manifest.yaml")
			f, err := os.Open(manifestPath)
			if err != nil {
				return err
			}
			defer f.Close()
			m, err := config.ParseManifest(f)
			if err != nil {
				return err
			}
			var spec *config.DaemonSpec
			for i := range m.Daemons {
				if m.Daemons[i].Name == daemonName {
					spec = &m.Daemons[i]
					break
				}
			}
			if spec == nil {
				return fmt.Errorf("no daemon %q in %s manifest", daemonName, server)
			}
			// Resolve env.
			vault, _ := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			resolver := secrets.NewResolver(vault, nil) // TODO config.local.yaml in later task
			env, err := resolver.ResolveMap(m.Env)
			if err != nil {
				return err
			}
			// Build launch spec.
			logPath := filepath.Join(logBaseDir(), server+"-"+daemonName+".log")
			childArgs := append([]string{}, m.BaseArgs...)
			childArgs = append(childArgs, spec.ExtraArgs...)
			if m.Transport == config.TransportNativeHTTP {
				childArgs = append(childArgs, "--port", fmt.Sprintf("%d", spec.Port))
				ls := daemon.LaunchSpec{
					Command: m.Command,
					Args:    childArgs,
					Env:     env,
					LogPath: logPath,
				}
				code, err := daemon.Launch(ls)
				if err != nil {
					return err
				}
				os.Exit(code)
			} else if m.Transport == config.TransportStdioBridge {
				ls := daemon.BuildBridgeSpec(m.Command, childArgs, spec.Port, env, logPath)
				code, err := daemon.Launch(ls)
				if err != nil {
					return err
				}
				os.Exit(code)
			} else {
				return fmt.Errorf("unsupported transport %q", m.Transport)
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	c.Flags().StringVar(&daemonName, "daemon", "", "daemon name within the server manifest")
	return c
}

// logBaseDir returns the per-OS directory for daemon logs.
// Windows: %LOCALAPPDATA%\mcp-local-hub\logs
// Linux/macOS: $XDG_STATE_HOME/mcp-local-hub/logs (or ~/.local/state/mcp-local-hub/logs)
func logBaseDir() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "logs")
}
