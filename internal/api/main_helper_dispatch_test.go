package api

import (
	"path/filepath"
	"strconv"
	"strings"
)

type apiTestHelperDispatch struct {
	invalid                 bool
	runSelectedTest         bool
	mustExit                bool
	callerStartTimeStateDir string
}

// classifyAPITestHelperDispatch validates only the child invocations issued
// by this package. It must stay free of filesystem, state and process work:
// TestMain calls it before allocating or redirecting any package fixture.
func classifyAPITestHelperDispatch(args []string, getenv func(string) string) apiTestHelperDispatch {
	selector := ""
	if len(args) == 2 && strings.HasPrefix(args[1], "-test.run=") {
		selector = strings.TrimPrefix(args[1], "-test.run=")
	}

	oracleState := getenv(callerStartTimeChildStateDirEnv)
	// The child converts milliseconds to a nanosecond time.Duration. Reject
	// malformed values and multiplication overflow before its first write.
	delayMS, delayErr := strconv.Atoi(getenv(callerStartTimeChildDelayEnv))
	vaultMarker := getenv("MCPHUB_TEST_HOLD_VAULT_LOCK")
	fenceMarker := getenv("MCPHUB_TEST_UPGRADE_FENCE_HOLDER")
	adoptRole := getenv("MCPHUB_A2_ROLE")
	adoptEntry := getenv("MCPHUB_A2_ENTRY")
	stdioMode := getenv(stdioBridgeAdmissionHelperEnv)
	port, portErr := strconv.Atoi(getenv(stdioBridgeAdmissionHelperPortEnv))

	validAdoptRole := false
	switch adoptRole {
	case "A-settle-window", "wait-try", "wait-try-hold", "try", "crash":
		validAdoptRole = true
	}
	validStdioMode := stdioMode == "ready" || stdioMode == "legacy" || stdioMode == "hang"

	type candidate struct {
		active   bool
		valid    bool
		dispatch apiTestHelperDispatch
	}
	candidates := []candidate{
		{
			active: oracleState != "",
			valid: selector == "^TestIntentAudit_CallerStartTimeAgainstIndependentOracle$" &&
				validAPITestHelperPath(oracleState) && delayErr == nil && delayMS >= 0 && int64(delayMS) <= (1<<63-1)/1_000_000,
			dispatch: apiTestHelperDispatch{callerStartTimeStateDir: oracleState},
		},
		{
			active: vaultMarker != "",
			valid: vaultMarker == "1" && selector == "^TestVaultFlockHelperProcess$" &&
				validAPITestHelperPath(getenv("MCPHUB_TEST_VAULT_PATH")) &&
				validAPITestHelperPath(getenv("MCPHUB_TEST_READY_PATH")) &&
				validAPITestHelperPath(getenv("MCPHUB_TEST_RELEASE_PATH")),
			dispatch: apiTestHelperDispatch{runSelectedTest: true, mustExit: true},
		},
		{
			active: fenceMarker != "",
			valid: fenceMarker == "1" && selector == "^TestUpgradeFenceReacquiresAfterHolderProcessDeath$" &&
				validAPITestHelperPath(getenv("MCPHUB_TEST_UPGRADE_FENCE_STATE_DIR")),
			dispatch: apiTestHelperDispatch{runSelectedTest: true, mustExit: true},
		},
		{
			active: adoptRole != "",
			valid: validAdoptRole && selector == "^TestAdoptLeaseRaceChild$" &&
				validAPITestHelperPath(getenv("MCPHUB_A2_STATE")) && CheckManifestName(adoptEntry) == nil &&
				!strings.HasSuffix(adoptEntry, adoptManifestLeaseSuffix),
			dispatch: apiTestHelperDispatch{runSelectedTest: true, mustExit: adoptRole == "crash"},
		},
		{
			active: stdioMode != "",
			valid: validStdioMode && selector == "^TestFrozenStdioBridgeAdmissionHelper$" &&
				portErr == nil && port > 0 && port <= 65535,
			dispatch: apiTestHelperDispatch{runSelectedTest: true, mustExit: true},
		},
	}

	active := 0
	selected := apiTestHelperDispatch{}
	for _, candidate := range candidates {
		if !candidate.active {
			continue
		}
		active++
		if !candidate.valid || active > 1 {
			return apiTestHelperDispatch{invalid: true}
		}
		selected = candidate.dispatch
	}
	if active == 0 {
		switch selector {
		case "^TestVaultFlockHelperProcess$", "^TestAdoptLeaseRaceChild$", "^TestFrozenStdioBridgeAdmissionHelper$":
			return apiTestHelperDispatch{invalid: true}
		}
	}
	return selected
}

// Absolute, clean paths prevent accidental working-directory or resolver
// fallback before the helper's first write. Ownership still comes from the
// parent's private fixture; this predicate does not admit an arbitrary path
// as an owned native object.
func validAPITestHelperPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, '\x00')
}
