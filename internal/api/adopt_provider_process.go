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
	ExecutablePath         string
	Args                   []string
	WorkingDirectoryObject providerDirectoryObjectIdentityV1
}

type providerDirectoryObjectIdentityV1 struct {
	volumeSerialNumber uint64
	fileID             [16]byte
	valid              bool
}

type providerProcessWorkingDirectoryStateV1 string

const (
	providerProcessWorkingDirectoryEqual     providerProcessWorkingDirectoryStateV1 = "equal"
	providerProcessWorkingDirectoryDifferent providerProcessWorkingDirectoryStateV1 = "different"
	providerProcessWorkingDirectoryUnknown   providerProcessWorkingDirectoryStateV1 = "unknown"
)

type providerProcessWorkingDirectoryCandidateV1 struct {
	PID             int
	SnapshotStarted time.Time
	ExecutablePath  string
}

type providerProcessWorkingDirectoryResultV1 struct {
	State     providerProcessWorkingDirectoryStateV1
	FailureID string
	Err       error
}

type providerProcessWorkingDirectoryProbe func(context.Context, providerProcessWorkingDirectoryCandidateV1, providerDirectoryObjectIdentityV1) providerProcessWorkingDirectoryResultV1

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

func providerDirectProcessObservationSupported() bool {
	return runtime.GOOS == "windows"
}

func observeProviderDirectProcessTree(ctx context.Context, identity providerDirectProcessIdentityV1, prior []providerProcessGenerationV1) providerDirectProcessObservationV1 {
	if !providerDirectProcessObservationSupported() {
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
	return observeProviderDirectProcessRowsWithProbe(ctx, identity, prior, rows, probeProviderProcessWorkingDirectory)
}

func observeProviderDirectProcessRowsWithProbe(ctx context.Context, identity providerDirectProcessIdentityV1, prior []providerProcessGenerationV1, rows []procRow, probe providerProcessWorkingDirectoryProbe) providerDirectProcessObservationV1 {
	if !providerDirectProcessIdentityValid(identity) {
		return providerDirectProcessObservationUnavailable(identity, "provider-lifecycle-unsupported", errors.New("provider executable identity is not absolute"))
	}
	if err := ctx.Err(); err != nil {
		return providerDirectProcessObservationUnavailable(identity, "provider-working-directory-unstable", err)
	}
	byPID := make(map[int]procRow, len(rows))
	for _, row := range rows {
		if row.pid == 0 {
			return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New("process row missing pid"))
		}
		if existing, duplicate := byPID[row.pid]; duplicate {
			if existing != row {
				return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New("process snapshot has conflicting pid rows"))
			}
			continue
		}
		byPID[row.pid] = row
	}
	rootPIDs := map[int]int{}
	classifiedRootPIDs := map[int]struct{}{}
	active := make([]providerProcessGenerationV1, 0)
	for _, row := range rows {
		if _, classified := classifiedRootPIDs[row.pid]; classified {
			continue
		}
		classifiedRootPIDs[row.pid] = struct{}{}
		match, unavailable := providerExactRootMatch(identity, row)
		if unavailable {
			return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New("possible provider root lacks identity evidence"))
		}
		if match {
			if probe == nil {
				return providerDirectProcessObservationUnavailable(identity, "provider-working-directory-layout-unsupported", errors.New("provider working-directory probe is unavailable"))
			}
			result := probe(ctx, providerProcessWorkingDirectoryCandidateV1{PID: row.pid, SnapshotStarted: row.created, ExecutablePath: row.exePath}, identity.WorkingDirectoryObject)
			switch result.State {
			case providerProcessWorkingDirectoryEqual:
				rootPIDs[row.pid] = row.pid
			case providerProcessWorkingDirectoryDifferent:
				continue
			default:
				failureID := result.FailureID
				if failureID == "" {
					failureID = "provider-working-directory-layout-unsupported"
				}
				return providerDirectProcessObservationUnavailable(identity, failureID, result.Err)
			}
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
	activePIDs := map[int]struct{}{}
	for _, row := range rows {
		if root, ok := rootPIDs[row.pid]; ok {
			if _, present := activePIDs[row.pid]; present {
				continue
			}
			if !providerRowComplete(row) {
				return providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", errors.New("provider tree row lacks identity evidence"))
			}
			active = append(active, providerGenerationFromRow(row, root))
			activePIDs[row.pid] = struct{}{}
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
	return identity.ExecutablePath != "" && filepath.IsAbs(identity.ExecutablePath) && identity.WorkingDirectoryObject.valid
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
