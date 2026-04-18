package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// newInstallCmdReal is the concrete cobra.Command wired by root.go's stub
// newInstallCmd. It is a thin wrapper over api.Install — all behavior lives
// in internal/api so CLI and future GUI share one code path.
func newInstallCmdReal() *cobra.Command {
	var server string
	var daemonFilter string
	var dryRun bool
	var all bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install an MCP server as shared daemon(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// If mcphub is not on PATH, try to bootstrap before we hit
			// the API's preflight check. Three-tier fallback:
			//   1. ~/.local/bin already on PATH — silently copy there
			//      (no registry write, no prompt, non-interactive safe).
			//   2. Interactive terminal — prompt "bootstrap? [Y/n]".
			//   3. Non-interactive without canonical dir on PATH —
			//      return the guidance error (preflight would produce
			//      the same message).
			if _, err := exec.LookPath(mcphubShortName); err != nil {
				switch {
				case targetDirOnPath():
					if err := Bootstrap(cmd.OutOrStdout()); err != nil {
						return err
					}
				default:
					if err := maybeBootstrapInteractively(cmd.OutOrStdout(), os.Stdin); err != nil {
						return err
					}
				}
			}
			if all {
				if server != "" || daemonFilter != "" {
					return fmt.Errorf("--all is mutually exclusive with --server/--daemon")
				}
				a := api.NewAPI()
				results := a.InstallAll(dryRun, cmd.OutOrStdout())
				for _, r := range results {
					if r.Err != nil {
						fmt.Fprintf(cmd.OutOrStderr(), "\u2717 %s: %v\n", r.Server, r.Err)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "\u2713 %s\n", r.Server)
					}
				}
				return nil
			}
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			a := api.NewAPI()
			return a.Install(api.InstallOpts{
				Server:       server,
				DaemonFilter: daemonFilter,
				DryRun:       dryRun,
				Writer:       cmd.OutOrStdout(),
			})
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (matches servers/<name>/manifest.yaml)")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "install only this daemon (+ its client bindings); omit to install all")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without making changes")
	c.Flags().BoolVar(&all, "all", false, "install every manifest under servers/")
	return c
}

// maybeBootstrapInteractively asks the user whether to bootstrap mcphub to
// ~/.local/bin when it is not yet on PATH. Returns nil if the user says yes
// (and bootstrap succeeds) or no-ops with a guidance error when the user
// declines. In non-terminal contexts it returns nil immediately; the API
// preflight check will then surface the "not on PATH" error with the same
// guidance, so automation never has its PATH mutated out from under it.
func maybeBootstrapInteractively(w io.Writer, in *os.File) error {
	if !term.IsTerminal(int(in.Fd())) {
		return nil
	}
	fmt.Fprintf(w, "%s not found on PATH. Bootstrap to ~/.local/bin? [Y/n] ", mcphubShortName)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read prompt response: %w", err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" || strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
		return Bootstrap(w)
	}
	return fmt.Errorf("%s not found on PATH — run `mcphub setup` once to install to ~/.local/bin and register in PATH", mcphubShortName)
}
