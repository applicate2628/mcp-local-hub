package cli

import (
	"runtime"

	"github.com/spf13/cobra"
)

// Build metadata set by main.main() via SetBuildInfo. Unset defaults
// ("dev", "unknown") apply to `go run` / non-build-script invocations.
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

// SetBuildInfo is called by main.main() before executing the root
// command. Keeps build-time constants centralized in the cli package
// instead of passed through every subcommand's context.
func SetBuildInfo(version, commit, date string) {
	if version != "" {
		buildVersion = version
	}
	if commit != "" {
		buildCommit = commit
	}
	if date != "" {
		buildDate = date
	}
}

func newVersionCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, build metadata, and project homepage",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Printf("mcp-local-hub %s\n", buildVersion)
			cmd.Printf("  commit:     %s\n", buildCommit)
			cmd.Printf("  build date: %s\n", buildDate)
			cmd.Printf("  go version: %s\n", runtime.Version())
			cmd.Printf("  platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
			cmd.Println()
			cmd.Println("  homepage:   https://github.com/applicate2628/mcp-local-hub")
			cmd.Println("  issues:     https://github.com/applicate2628/mcp-local-hub/issues")
			cmd.Println("  license:    Apache-2.0")
			cmd.Println("  author:     Dmitry Denisenko (@applicate2628)")
		},
	}
}
