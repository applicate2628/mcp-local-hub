// Package cli — G7 `mcphub import vscode-workspace` subcommand.
//
// Projects <workspace>/.vscode/mcp.json onto a draft mcp-local-hub
// manifest and prints the result to stdout. No write side effects.
// Operators pipe the output to a file (or paste into the GUI Add
// server screen) and run `mcphub manifest create` separately.

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

func newImportCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "import",
		Short: "Import MCP server config from external sources (VS Code workspaces, …)",
		Long: `Read a non-mcp-local-hub MCP configuration and project it onto a
draft mcp-local-hub manifest. No write side effects — output is
printed to stdout for inspection. Pipe to a file or paste into the
GUI Add server screen, then run 'mcphub manifest create' or use the
GUI to install.`,
	}
	root.AddCommand(newImportVSCodeWorkspaceCmd())
	return root
}

func newImportVSCodeWorkspaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "vscode-workspace <path>",
		Short: "Project <path>/.vscode/mcp.json onto a draft manifest YAML",
		Long: `Read the VS Code workspace MCP configuration at
<path>/.vscode/mcp.json and project it onto a draft mcp-local-hub
manifest. Supports both the new 'servers' and legacy 'mcpServers'
top-level keys. Tolerates // and /* */ comments and trailing commas.

Expands the four standard VS Code placeholders:
  ${env:VAR}           → environment variable
  ${workspaceFolder}   → <path>
  ${userHome}          → user's home directory
  ${pathSeparator}     → OS path separator

External config is treated as untrusted — the output is a draft, not
an applied install. Inspect, edit if needed, then run
'mcphub manifest create' or paste into the GUI Add server screen.
Stderr carries non-fatal warnings (unknown server types, undefined
env vars, schema collisions).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workspacePath, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolve %s: %w", args[0], err)
			}
			a := api.NewAPI()
			result, err := a.ImportVSCodeWorkspace(workspacePath, api.VSCodeImportOpts{})
			if err != nil {
				return err
			}
			// Warnings go to stderr so the YAML on stdout pipes
			// cleanly to files / clipboards.
			for _, w := range result.Warnings {
				fmt.Fprintf(os.Stderr, "warn: %s\n", w)
			}
			if result.EmptyResult {
				// Exit 0 — empty isn't a failure; the operator
				// pointed at a workspace without any MCP entries.
				// Stderr warning already informs them.
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), result.YAML)
			return nil
		},
	}
	return c
}
