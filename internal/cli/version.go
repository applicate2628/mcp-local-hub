package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/buildinfo"
)

// normalizeExePath resolves symlinks and cleans an executable path so two
// references to the same on-disk binary compare equal. Best-effort: a
// resolve error returns the cleaned-but-unresolved path so the caller can
// still compare, and the caller treats a normalize failure as "no warning".
func normalizeExePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// pathShadowDiagnostic compares the running executable against the `mcphub`
// the shell PATH resolves to. When they are DIFFERENT on-disk binaries (a
// shadow — e.g. a stale npm-global `mcphub` ahead of a fresher dev deploy in
// ~/.local/bin), it returns an operator warning naming both locations;
// otherwise "". Pure + best-effort: any empty/unresolvable input yields ""
// (never a false alarm). Paths are compared case-insensitively only on
// Windows, where executable paths are case-insensitive; POSIX keeps exact
// case so case-only distinct paths on case-sensitive filesystems still warn.
func pathShadowDiagnostic(runningExe, pathResolved string) string {
	if runningExe == "" || pathResolved == "" {
		return ""
	}
	a, errA := normalizeExePath(runningExe)
	b, errB := normalizeExePath(pathResolved)
	if errA != nil || errB != nil {
		return ""
	}
	if sameExePath(a, b) {
		return ""
	}
	return fmt.Sprintf("the 'mcphub' on your PATH (%s) is NOT this running binary (%s); "+
		"a fresh shell may run the shadowed one. Reconcile the two install locations, "+
		"or invoke this binary by its full path.", b, a)
}

func sameExePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

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
/ commercial licensing / license / author links.

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
			// Running-binary location + PATH-shadow diagnostic: surfaces the
			// case where a stale `mcphub` on PATH (e.g. an npm-global install)
			// shadows a fresher dev deploy (~/.local/bin). Best-effort — a
			// lookup failure prints nothing extra.
			runningExe, _ := os.Executable()
			if runningExe != "" {
				cmd.Printf("  running:    %s\n", runningExe)
			}
			if resolved, lerr := exec.LookPath("mcphub"); lerr == nil {
				if warn := pathShadowDiagnostic(runningExe, resolved); warn != "" {
					cmd.Printf("  ⚠ shadow:   %s\n", warn)
				}
			}
			cmd.Println()
			cmd.Println("  homepage:   https://github.com/applicate2628/mcp-local-hub")
			cmd.Println("  issues:     https://github.com/applicate2628/mcp-local-hub/issues")
			cmd.Println("  commercial: https://github.com/applicate2628/mcp-local-hub/blob/master/README.md#license")
			cmd.Println("  license:    MPL-2.0")
			cmd.Println("  author:     Dmitry Denisenko (@applicate2628)")
		},
	}
}
