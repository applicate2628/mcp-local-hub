//go:build !windows

package cli

// portableNoopCommand returns a command line that exits successfully
// almost immediately on POSIX. Used by TestProductionSpawnFn_*
// tests as a platform-portable no-op that lets cmd.Start succeed
// without forking a long-lived child.
func portableNoopCommand() (string, []string) {
	return "true", nil
}