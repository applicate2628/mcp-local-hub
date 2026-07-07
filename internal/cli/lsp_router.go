package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

type lspRouterCLIAPI interface {
	LSPRouterDisabledClientSet() (map[string]bool, error)
	SetLSPRouterDisabledClients([]string) error
	RollbackLSPRouterClientEntriesForClient(string, api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error)
	EnsureLSPRouterClientEntries(api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error)
	LSPRouterClientStatuses(api.LSPClientRouterOpts) ([]api.LSPRouterClientStatus, error)
}

var newLSPRouterCLIAPI = func() lspRouterCLIAPI { return api.NewAPI() }

func newLSPRouterCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "lsp-router",
		Short: "Manage shared LSP router entries in MCP client configs",
		Long: `Manage the shared mcp-language-server router entries that
mcphub setup writes into eligible present MCP client configs.

Disabling a client persists an opt-out in gui-preferences.yaml and removes
that client's current router entries immediately. Future 'mcphub setup'
runs will skip disabled clients. Enabling clears the opt-out and runs the
normal ensure pass so eligible present clients can receive router entries again.`,
	}
	root.AddCommand(newLSPRouterDisableCmd())
	root.AddCommand(newLSPRouterEnableCmd())
	root.AddCommand(newLSPRouterStatusCmd())
	return root
}

func newLSPRouterDisableCmd() *cobra.Command {
	var clientName string
	c := &cobra.Command{
		Use:   "disable --client <name>",
		Short: "Disable shared LSP router maintenance for one client",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLSPRouterDisable(cmd, clientName)
		},
	}
	c.Flags().StringVar(&clientName, "client", "", "supported client id to disable")
	return c
}

func newLSPRouterEnableCmd() *cobra.Command {
	var clientName string
	c := &cobra.Command{
		Use:   "enable --client <name>",
		Short: "Enable shared LSP router maintenance for one client",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLSPRouterEnable(cmd, clientName)
		},
	}
	c.Flags().StringVar(&clientName, "client", "", "supported client id to enable")
	return c
}

func newLSPRouterStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show disabled clients and current router entry presence",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLSPRouterStatus(cmd)
		},
	}
}

func runLSPRouterDisable(cmd *cobra.Command, rawClient string) error {
	clientName, err := validateLSPRouterClientName(rawClient)
	if err != nil {
		return err
	}
	a := newLSPRouterCLIAPI()
	disabled, err := a.LSPRouterDisabledClientSet()
	if err != nil {
		return err
	}
	if err := a.SetLSPRouterDisabledClients(lspRouterDisabledNames(disabled, clientName, "")); err != nil {
		return err
	}
	report, err := a.RollbackLSPRouterClientEntriesForClient(clientName, api.LSPClientRouterOpts{})
	if report == nil {
		report = &api.LSPClientRouterReport{}
	}
	printLSPClientRouterReport(cmd.OutOrStdout(), report, "disable")
	if err != nil {
		return err
	}
	if len(report.Removed) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No existing LSP router entries found for %s.\n", clientName)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ %s disabled for LSP router setup; future `mcphub setup` runs will not re-add router entries.\n", clientName)
	return nil
}

func runLSPRouterEnable(cmd *cobra.Command, rawClient string) error {
	clientName, err := validateLSPRouterClientName(rawClient)
	if err != nil {
		return err
	}
	a := newLSPRouterCLIAPI()
	disabled, err := a.LSPRouterDisabledClientSet()
	if err != nil {
		return err
	}
	if err := a.SetLSPRouterDisabledClients(lspRouterDisabledNames(disabled, "", clientName)); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ %s enabled for LSP router setup.\n", clientName)
	report, err := a.EnsureLSPRouterClientEntries(api.LSPClientRouterOpts{})
	if report == nil {
		report = &api.LSPClientRouterReport{}
	}
	printLSPClientRouterReport(cmd.OutOrStdout(), report, "wiring")
	if err != nil {
		return err
	}
	if len(report.Backups) == 0 && len(report.Applied) == 0 && len(report.Removed) == 0 && len(report.Restored) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No LSP router entry changes were needed.")
	}
	return nil
}

func runLSPRouterStatus(cmd *cobra.Command) error {
	a := newLSPRouterCLIAPI()
	disabled, err := a.LSPRouterDisabledClientSet()
	if err != nil {
		return err
	}
	statuses, err := a.LSPRouterClientStatuses(api.LSPClientRouterOpts{})
	if err != nil {
		return err
	}
	disabledNames := lspRouterDisabledNames(disabled, "", "")
	if len(disabledNames) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Disabled clients: <none>")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Disabled clients: %s\n", strings.Join(disabledNames, ", "))
	}
	if len(statuses) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No present MCP client configs found.")
		return nil
	}
	for _, status := range statuses {
		state := "enabled"
		if status.Disabled {
			state = "disabled"
		}
		total := len(status.ExistingEntries) + len(status.MissingEntries)
		fmt.Fprintf(cmd.OutOrStdout(), "%s (%s): %s, %d/%d router entries present\n",
			status.Client, status.ConfigPath, state, len(status.ExistingEntries), total)
		if len(status.MissingEntries) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  missing: %s\n", strings.Join(status.MissingEntries, ", "))
		}
	}
	return nil
}

func validateLSPRouterClientName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("--client is required")
	}
	for _, supported := range clients.SupportedClientNames() {
		if name == supported {
			return name, nil
		}
	}
	return "", fmt.Errorf("unknown client %q (expected %s)", name, strings.Join(clients.SupportedClientNames(), " | "))
}

func lspRouterDisabledNames(disabled map[string]bool, add, remove string) []string {
	names := make([]string, 0, len(disabled)+1)
	for _, name := range clients.SupportedClientNames() {
		on := disabled[name]
		if name == add {
			on = true
		}
		if name == remove {
			on = false
		}
		if on {
			names = append(names, name)
		}
	}
	return names
}
