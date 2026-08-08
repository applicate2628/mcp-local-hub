//go:build linux

package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	processinternal "mcp-local-hub/internal/process"
)

func platformInternalCommands() []*cobra.Command {
	return []*cobra.Command{newLinuxProcfsClassifierHelperCmd()}
}

func newLinuxProcfsClassifierHelperCmd() *cobra.Command {
	return &cobra.Command{
		Use:           processinternal.LinuxProcfsClassifierHelperCommand + " <pgid>",
		Hidden:        true,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args: func(_ *cobra.Command, args []string) error {
			_, err := parseLinuxProcfsClassifierPGID(args)
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			pgid, err := parseLinuxProcfsClassifierPGID(args)
			if err != nil {
				return err
			}
			return processinternal.RunLinuxProcfsClassifierHelper(pgid, cmd.OutOrStdout())
		},
	}
}

func parseLinuxProcfsClassifierPGID(args []string) (int, error) {
	if len(args) != 1 || args[0] == "" {
		return 0, fmt.Errorf("%s requires exactly one positive decimal PGID", processinternal.LinuxProcfsClassifierHelperCommand)
	}
	for _, digit := range args[0] {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("%s requires exactly one positive decimal PGID", processinternal.LinuxProcfsClassifierHelperCommand)
		}
	}
	pgid, err := strconv.Atoi(args[0])
	if err != nil || pgid <= 0 || strconv.Itoa(pgid) != args[0] {
		return 0, fmt.Errorf("%s requires exactly one positive decimal PGID", processinternal.LinuxProcfsClassifierHelperCommand)
	}
	return pgid, nil
}
