package cli

import (
	"os"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/tray"
)

// newTrayCmdReal builds the hidden `mcphub tray` subcommand. The
// `mcphub gui` parent process spawns this as a child (`<self> tray`)
// to keep the system tray icon and its right-click menu running on
// a fresh OS thread that owns no shared state with the GUI.
//
// See internal/tray/tray.go package docs for the IPC architecture.
//
// Hidden because end users never invoke it directly; if it shows
// up in --help, that's noise and an attractive nuisance.
func newTrayCmdReal() *cobra.Command {
	c := &cobra.Command{
		Use:    "tray",
		Short:  "internal: subprocess for the system tray icon (spawned by `mcphub gui`)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tray.RunChild(os.Stdin, os.Stdout)
		},
	}
	return c
}
