package cli

import "mcp-local-hub/internal/api"

var sweepOldBinariesFn = api.SweepOldBinaries
var recoverMissingBinaryFn = api.RecoverMissingBinary

func setSweepOldBinariesFnForTest(fn func(dir string, warn ...func(string, error)) error) func() {
	prev := sweepOldBinariesFn
	sweepOldBinariesFn = fn
	return func() { sweepOldBinariesFn = prev }
}

func setRecoverMissingBinaryFnForTest(fn func(target string) error) func() {
	prev := recoverMissingBinaryFn
	recoverMissingBinaryFn = fn
	return func() { recoverMissingBinaryFn = prev }
}
