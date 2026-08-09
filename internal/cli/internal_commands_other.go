//go:build !linux

package cli

import "github.com/spf13/cobra"

func platformInternalCommands() []*cobra.Command {
	return nil
}
