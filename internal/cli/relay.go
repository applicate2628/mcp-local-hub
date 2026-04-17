package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"

	"github.com/spf13/cobra"
)

// newRelayCmdReal wires up the `mcp relay` subcommand. Spawned as a
// child process by stdio-only MCP clients (e.g. Antigravity) that
// cannot connect directly to loopback HTTP MCP daemons. Relay forwards
// JSON-RPC between the client's stdin/stdout and an HTTP endpoint.
//
// Two modes:
//
//	mcp relay --server serena --daemon claude   # manifest-aware
//	mcp relay --url http://localhost:9121/mcp   # direct / escape-hatch
func newRelayCmdReal() *cobra.Command {
	var server, daemonName, url, projectPath string
	c := &cobra.Command{
		Use:   "relay",
		Short: "Forward stdio MCP to an HTTP MCP endpoint (for clients lacking HTTP support)",
		Long: `Relay runs as a child process of a stdio-only MCP client and
forwards JSON-RPC 2.0 messages to a Streamable HTTP MCP endpoint.
Use --server and --daemon together to look up the URL from the
installed manifest, or --url to point at any HTTP endpoint directly.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedURL, err := resolveRelayURL(server, daemonName, url)
			if err != nil {
				return err
			}
			r := &daemon.HTTPToStdioRelay{
				URL:    resolvedURL,
				Stdin:  cmd.InOrStdin(),
				Stdout: cmd.OutOrStdout(),
				Stderr: cmd.ErrOrStderr(),
			}
			_ = projectPath // reserved for Phase 3 workspace-scoped routing (pass-through only for now)
			return r.Run(cmd.Context())
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (looks up servers/<name>/manifest.yaml next to mcp.exe)")
	c.Flags().StringVar(&daemonName, "daemon", "", "daemon name within the server manifest")
	c.Flags().StringVar(&url, "url", "", "direct HTTP URL (mutually exclusive with --server/--daemon)")
	c.Flags().StringVar(&projectPath, "project", "", "optional workspace path (Phase 3 reserved; pass-through only)")
	return c
}

// resolveRelayURL returns an HTTP endpoint for the relay to forward to.
// Either --url is specified, or both --server and --daemon. Mixing is
// rejected so misconfigured invocations fail fast before any network I/O.
func resolveRelayURL(server, daemonName, explicitURL string) (string, error) {
	if explicitURL != "" {
		if server != "" || daemonName != "" {
			return "", errors.New("--url is mutually exclusive with --server/--daemon")
		}
		return explicitURL, nil
	}
	if server == "" || daemonName == "" {
		return "", errors.New("either --url or both --server and --daemon are required")
	}
	// Locate manifest relative to the binary, not the working directory —
	// stdio clients may invoke relay from any cwd (typically the user's
	// project root, not the mcp-local-hub repo).
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	exeDir := filepath.Dir(exe)
	manifestPath := filepath.Join(exeDir, "servers", server, "manifest.yaml")
	if _, statErr := os.Stat(manifestPath); statErr != nil {
		alt := filepath.Join(exeDir, "..", "servers", server, "manifest.yaml")
		if _, statErr2 := os.Stat(alt); statErr2 == nil {
			manifestPath = alt
		}
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return "", fmt.Errorf("open manifest %s: %w", manifestPath, err)
	}
	defer f.Close()
	m, err := config.ParseManifest(f)
	if err != nil {
		return "", fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	for _, d := range m.Daemons {
		if d.Name == daemonName {
			return fmt.Sprintf("http://localhost:%d/mcp", d.Port), nil
		}
	}
	return "", fmt.Errorf("no daemon %q in manifest %s", daemonName, server)
}
