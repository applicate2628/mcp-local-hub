package api

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mcp-local-hub/internal/process"
)

type providerProcessObservationStateV1 string

const (
	providerProcessObservationComplete    providerProcessObservationStateV1 = "complete"
	providerProcessObservationUnavailable providerProcessObservationStateV1 = "unavailable"
)

type providerDirectProcessIdentityV1 struct {
	ExecutablePath string
	Args           []string
}

type providerProcessGenerationV1 struct {
	PID, ParentPID, RootPID     int
	StartedAt                   time.Time
	ExecutablePath, CommandLine string
}

type providerDirectProcessObservationV1 struct {
	Active    []providerProcessGenerationV1
	State     providerProcessObservationStateV1
	FailureID string
	Err       error
}

type providerDirectProcessObserver func(context.Context, providerDirectProcessIdentityV1, []providerProcessGenerationV1) providerDirectProcessObservationV1

func observeProviderDirectProcessTree(ctx context.Context, identity providerDirectProcessIdentityV1, prior []providerProcessGenerationV1) providerDirectProcessObservationV1 {
	if runtime.GOOS != "windows" {
		return providerDirectProcessObservationUnavailable(identity, "unsupported-platform", errors.New("provider process observation is Windows-only"))
	}
	if err := ctx.Err(); err != nil {
		return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", err)
	}
	snapshot := takeProcessSnapshotContext(ctx)
	if snapshot.reasonID != "" || snapshot.snapshot.raw == "" {
		return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New(snapshot.reasonID))
	}
	rows, err := parseProcessSnapshotRows(strings.NewReader(strings.Join(snapshot.snapshot.lines, "\n")))
	if err != nil || len(rows) == 0 {
		return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", err)
	}
	return observeProviderDirectProcessRows(identity, prior, rows)
}

func observeProviderDirectProcessRows(identity providerDirectProcessIdentityV1, prior []providerProcessGenerationV1, rows []procRow) providerDirectProcessObservationV1 {
	if !providerDirectProcessIdentityValid(identity) {
		return providerDirectProcessObservationUnavailable(identity, "provider-lifecycle-unsupported", errors.New("provider executable identity is not absolute"))
	}
	byPID := make(map[int]procRow, len(rows))
	for _, row := range rows {
		if row.pid == 0 {
			return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New("process row missing pid"))
		}
		byPID[row.pid] = row
	}
	rootPIDs := map[int]int{}
	active := make([]providerProcessGenerationV1, 0)
	for _, row := range rows {
		match, unavailable := providerExactRootMatch(identity, row)
		if unavailable {
			return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New("possible provider root lacks identity evidence"))
		}
		if match {
			rootPIDs[row.pid] = row.pid
		}
	}
	for changed := true; changed; {
		changed = false
		for _, row := range rows {
			if _, known := rootPIDs[row.pid]; known {
				continue
			}
			if root, ok := rootPIDs[row.ppid]; ok {
				rootPIDs[row.pid] = root
				changed = true
			}
		}
	}
	for _, row := range rows {
		if root, ok := rootPIDs[row.pid]; ok {
			if !providerRowComplete(row) {
				return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New("provider tree row lacks identity evidence"))
			}
			active = append(active, providerGenerationFromRow(row, root))
		}
	}
	for _, generation := range prior {
		row, ok := byPID[generation.PID]
		if !ok {
			continue
		}
		if !providerRowComplete(row) {
			return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New("prior provider generation lacks identity evidence"))
		}
		if providerSameGeneration(generation, row) && !providerGenerationPresent(active, generation) {
			active = append(active, providerGenerationFromRow(row, generation.RootPID))
		}
	}
	return providerDirectProcessObservationV1{Active: active, State: providerProcessObservationComplete}
}

func providerDirectProcessIdentityValid(identity providerDirectProcessIdentityV1) bool {
	return identity.ExecutablePath != "" && filepath.IsAbs(identity.ExecutablePath)
}

func providerExactRootMatch(identity providerDirectProcessIdentityV1, row procRow) (match, unavailable bool) {
	if row.exePath == "" {
		// A process snapshot can omit image identity. It is not an absence claim
		// when its ordered args could still belong to the selected provider root;
		// unrelated incomplete rows remain non-candidates.
		return false, providerCommandLineMatchesArgs(identity, row)
	}
	if !providerExecutableEqual(identity.ExecutablePath, row.exePath) {
		return false, false
	}
	if row.cmdline == "" || row.created.IsZero() {
		return false, true
	}
	argv := process.TokenizeWindowsCommandLine(row.cmdline)
	if len(argv) == 0 {
		return false, true
	}
	return providerArgsMatch(identity.Args, argv), false
}

func providerCommandLineMatchesArgs(identity providerDirectProcessIdentityV1, row procRow) bool {
	if row.cmdline == "" || row.created.IsZero() {
		return false
	}
	argv := process.TokenizeWindowsCommandLine(row.cmdline)
	return len(argv) > 0 && providerArgsMatch(identity.Args, argv)
}

func providerArgsMatch(expected, argv []string) bool {
	if len(argv)-1 != len(expected) {
		return false
	}
	for i, arg := range expected {
		if argv[i+1] != arg {
			return false
		}
	}
	return true
}

func providerExecutableEqual(expected, actual string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(expected), filepath.Clean(actual))
	}
	return filepath.Clean(expected) == filepath.Clean(actual)
}

func providerRowComplete(row procRow) bool {
	return row.pid != 0 && !row.created.IsZero() && row.exePath != "" && row.cmdline != ""
}

func providerGenerationFromRow(row procRow, rootPID int) providerProcessGenerationV1 {
	return providerProcessGenerationV1{PID: row.pid, ParentPID: row.ppid, RootPID: rootPID, StartedAt: row.created, ExecutablePath: row.exePath, CommandLine: row.cmdline}
}

func providerSameGeneration(generation providerProcessGenerationV1, row procRow) bool {
	return generation.StartedAt.Equal(row.created) && providerExecutableEqual(generation.ExecutablePath, row.exePath) && generation.CommandLine == row.cmdline
}

func providerGenerationPresent(generations []providerProcessGenerationV1, target providerProcessGenerationV1) bool {
	for _, generation := range generations {
		if generation.PID == target.PID && providerSameGeneration(target, procRow{created: generation.StartedAt, exePath: generation.ExecutablePath, cmdline: generation.CommandLine}) {
			return true
		}
	}
	return false
}

func providerDirectProcessObservationUnavailable(_ providerDirectProcessIdentityV1, failureID string, cause error) providerDirectProcessObservationV1 {
	if cause == nil {
		cause = errors.New(failureID)
	}
	return providerDirectProcessObservationV1{State: providerProcessObservationUnavailable, FailureID: failureID, Err: cause}
}
