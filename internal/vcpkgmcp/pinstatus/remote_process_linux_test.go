//go:build linux

package pinstatus

import (
	"os"
	"strconv"
	"testing"

	hubprocess "mcp-local-hub/internal/process"
)

// TestMain gives the package test binary the same hidden procfs-classifier
// dispatch that the production CLI root owns. RunContainedStream intentionally
// self-executes the current binary, so the real pin-status lifecycle test must
// expose that production command instead of accidentally running the full test
// suite as the classifier child.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == hubprocess.LinuxProcfsClassifierHelperCommand {
		pgid, err := strconv.Atoi(os.Args[2])
		if err != nil || pgid <= 0 {
			os.Exit(2)
		}
		if err := hubprocess.RunLinuxProcfsClassifierHelper(pgid, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
