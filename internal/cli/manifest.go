package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"

	"github.com/spf13/cobra"
)

func newManifestCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "manifest",
		Short: "Manage server manifests under servers/*/manifest.yaml",
		Long: `Inspect and author server manifests. Manifests are the source-of-truth
for each server — daemon name(s), port(s), command + args, env
references, client bindings, and weekly-refresh policy.

Manifests are embedded into the mcphub binary via //go:embed at build
time (servers/embed.go). This lets the canonical ~/.local/bin/mcphub.exe
resolve manifests without needing a sibling servers/ directory on disk —
at the cost of requiring a rebuild to pick up manifest edits.

Subcommands:
  manifest list                 # names of all embedded manifests
  manifest show <name>          # print a manifest's YAML content
  manifest create <name>        # scaffold a new manifest interactively
  manifest edit <name>          # open in $EDITOR (source file, not embed)
  manifest test-remote <name>   # smoke-check a remote-http manifest's URL+headers

After editing / creating a manifest, rebuild + 'mcphub install --server <n>'.

See also: install, scan (flags un-manifested clients as "unknown").`,
	}
	root.AddCommand(newManifestListCmd())
	root.AddCommand(newManifestShowCmd())
	root.AddCommand(newManifestCreateCmd())
	root.AddCommand(newManifestEditCmd())
	root.AddCommand(newManifestValidateCmd())
	root.AddCommand(newManifestDeleteCmd())
	root.AddCommand(newManifestExtractCmd())
	root.AddCommand(newManifestTestRemoteCmd())
	return root
}

func newManifestListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List server names with manifests",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			names, err := a.ManifestList()
			if err != nil {
				return err
			}
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}
}

func newManifestShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print the YAML of a server's manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			yaml, err := a.ManifestGet(args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), yaml)
			return nil
		},
	}
}

func newManifestCreateCmd() *cobra.Command {
	var fromFile string
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new manifest (from --from-file or stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var yaml []byte
			var err error
			if fromFile != "" {
				yaml, err = os.ReadFile(fromFile)
				if err != nil {
					return err
				}
			} else {
				yaml, err = readAllStdin()
				if err != nil {
					return err
				}
			}
			a := api.NewAPI()
			if err := a.ManifestCreate(args[0], string(yaml)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Created manifest %s\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&fromFile, "from-file", "", "read YAML from this file instead of stdin")
	return c
}

func newManifestEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <name>",
		Short: "Open the manifest in $EDITOR and re-validate on save",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			yaml, err := a.ManifestGet(args[0])
			if err != nil {
				return err
			}
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "notepad"
			}
			tmp, err := os.CreateTemp("", args[0]+"-*.yaml")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			if _, err := tmp.WriteString(yaml); err != nil {
				tmp.Close()
				return err
			}
			tmp.Close()
			editorCmd := exec.Command(editor, tmp.Name())
			editorCmd.Stdin = os.Stdin
			editorCmd.Stdout = os.Stdout
			editorCmd.Stderr = os.Stderr
			if err := editorCmd.Run(); err != nil {
				return fmt.Errorf("editor %s: %w", editor, err)
			}
			edited, err := os.ReadFile(tmp.Name())
			if err != nil {
				return err
			}
			if err := a.ManifestEdit(args[0], string(edited)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Saved manifest %s\n", args[0])
			return nil
		},
	}
}

func newManifestValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <name>",
		Short: "Check a manifest for structural issues",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			yaml, err := a.ManifestGet(args[0])
			if err != nil {
				return err
			}
			warnings := a.ManifestValidate(yaml)
			if len(warnings) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s: valid\n", args[0])
				return nil
			}
			for _, w := range warnings {
				fmt.Fprintf(cmd.OutOrStderr(), "  %s\n", w)
			}
			return fmt.Errorf("%d validation issue(s)", len(warnings))
		},
	}
}

func newManifestDeleteCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove a manifest (uninstall first or use --force)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				fmt.Fprintln(cmd.OutOrStderr(),
					"refusing to delete without --force; if the server is installed, run `mcphub uninstall --server "+args[0]+"` first")
				return fmt.Errorf("missing --force")
			}
			a := api.NewAPI()
			if err := a.ManifestDelete(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Deleted manifest %s\n", args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "skip the uninstall-first reminder")
	return c
}

// manifestExtractSupportedClients is the SINGLE OWNER of the source clients
// `mcphub manifest extract --client` wires a config path for (the
// claude/codex/gemini/antigravity/opencode/mimocode subset of
// ExtractManifestFromClient whose ScanOpts paths this command actually
// populates below). Both the required-client error and the --client flag help
// render from it so the two can't drift again.
var manifestExtractSupportedClients = []string{"claude-code", "codex-cli", "gemini-cli", "antigravity", "opencode", "mimocode"}

// manifestExtractClientsHelp renders the supported-client list as a
// pipe-separated string for the error message and flag help.
func manifestExtractClientsHelp() string {
	return strings.Join(manifestExtractSupportedClients, " | ")
}

func newManifestExtractCmd() *cobra.Command {
	var clientFlag string
	c := &cobra.Command{
		Use:   "extract <server>",
		Short: "Print a draft manifest YAML derived from an existing client's stdio entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if clientFlag == "" {
				return fmt.Errorf("--client is required (%s)", manifestExtractClientsHelp())
			}
			a := api.NewAPI()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			// opencode/mimocode config paths are resolved through the adapter
			// (the SAME resolver the GUI extract path uses via
			// guiClientConfigPath → clients.ConfigPathForName), so extract
			// populates their named ScanOpts fields instead of failing the
			// empty-path guards.
			openCodePath, _ := clients.ConfigPathForName("opencode")
			mimoCodePath, _ := clients.ConfigPathForName("mimocode")
			yaml, err := a.ExtractManifestFromClient(clientFlag, args[0], api.ScanOpts{
				ClaudeConfigPath:      filepath.Join(home, ".claude.json"),
				CodexConfigPath:       filepath.Join(home, ".codex", "config.toml"),
				GeminiConfigPath:      filepath.Join(home, ".gemini", "settings.json"),
				AntigravityConfigPath: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
				OpenCodeConfigPath:    openCodePath,
				MimoCodeConfigPath:    mimoCodePath,
				ManifestDir:           scanManifestDir(),
			})
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), yaml)
			return nil
		},
	}
	c.Flags().StringVar(&clientFlag, "client", "", "source client ("+manifestExtractClientsHelp()+")")
	return c
}
