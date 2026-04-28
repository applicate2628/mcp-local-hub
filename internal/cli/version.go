package cli

import (
	"runtime"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/buildinfo"
)

// SetBuildInfo retains the cli package's existing public surface so
// main.main() doesn't need to switch to a new import path. It
// forwards into the canonical buildinfo store, which both this
// package's `mcphub version` command and the gui's /api/version
// handler read from.
func SetBuildInfo(version, commit, date string) {
	buildinfo.Set(version, commit, date)
}

func newVersionCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, build metadata, and project homepage",
		Long: `Print build metadata: semantic version, short git commit, build date,
Go toolchain version, target platform, plus homepage / issue tracker
/ license / author links.

Values are baked in at build time via build.sh / build.ps1 (which
injects ldflags). A bare 'go build ./cmd/mcphub' produces a binary
that shows version=dev / commit=unknown / build-date=unknown — run
the build scripts to get real values.

Example:
  mcphub version
  → mcp-local-hub 0.3.0
      commit:     38f6349
      build date: 2026-04-18T20:56:14Z
      go version: go1.26.2
      platform:   windows/amd64`,
		Run: func(cmd *cobra.Command, args []string) {
			version, commit, date := buildinfo.Get()
			cmd.Printf("mcp-local-hub %s\n", version)
			cmd.Printf("  commit:     %s\n", commit)
			cmd.Printf("  build date: %s\n", date)
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
