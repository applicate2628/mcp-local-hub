// Build metadata injection happens through ldflags at link time — see
// build.ps1 / build.sh in repo root. Binary version info for Windows
// Explorer Properties is embedded via cmd/mcp/resource.syso, regenerated
// from versioninfo.json whenever the file changes:
//
//go:generate go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest -64 -o resource.syso versioninfo.json
package main

import (
	"fmt"
	"os"

	"mcp-local-hub/internal/cli"
)

// These are populated at build time via `-ldflags "-X ..."` (see build
// scripts). Defaults are useful for `go run` / unmarked dev builds.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	cli.SetBuildInfo(version, commit, buildDate)
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
