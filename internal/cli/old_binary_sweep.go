package cli

import "mcp-local-hub/internal/api"

var sweepOldBinariesFn = api.SweepOldBinaries

func setSweepOldBinariesFnForTest(fn func(dir string, warn ...func(string, error)) error) func() {
	prev := sweepOldBinariesFn
	sweepOldBinariesFn = fn
	return func() { sweepOldBinariesFn = prev }
}
