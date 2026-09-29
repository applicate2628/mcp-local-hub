package kimi

import "github.com/spf13/cobra"

func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "kimi",
		Short:  "Kimi wrapper MCP server (stdio)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
