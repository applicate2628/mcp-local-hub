//go:build windows

// Package cli — Task 13.1 Windows cold-start stale-child reaper.
//
// DAEMON children on Windows are owned through per-task Job Objects
// (see internal/process). When the supervisor exits — gracefully or by
// crash — each Job Object's KILL_ON_JOB_CLOSE limit terminates the
// daemon's whole tree, so a Windows supervisor restart never finds
// orphan DAEMON children: the prior generation's Job handles die with
// the process and the kernel reaps the children before the new
// supervisor binds its lock.
//
// MAINTENANCE-timer transients are different. Per spec
// (§supervisor-state.json) `transient_pids[*]` are "fire-and-forget
// children OUTSIDE Job Object" — they are spawned with plain
// CreateProcess (process.NewProcessGroup is a no-op on Windows). If the
// supervisor crashes or is force-killed while a weekly refresh is
// running, that child is NOT reaped by any Job Object and its recorded
// PID lingers in supervisor-state.json. This reaper walks
// `transient_pids` on cold start and kills + clears those stale
// maintenance children (PR #243 bot round-2 P2). It does NOT touch
// daemons — they are not in `transient_pids`.
//
// Spec source: §"Fallback if step 4 IPC fails" + plan §2660.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// ReaperResult reports what the reaper did during this supervisor cold
// start. Fields mirror the POSIX surface so cross-platform tests compile
// against the same API. See the POSIX impl for field semantics.
type ReaperResult struct {
	KilledPIDs        []int         // maintenance transients killed (after the start-time gate)
	SkippedPIDs       []int         // alive but failed/unverifiable identity gate (not killed)
	DeadPIDs          []int         // PIDs already gone or PID<=0 claim slots (no kill needed)
	KillErrors        map[int]error // PIDs where the tree-kill returned an error; retained in state
	ClearedTransients int           // size of supervisor-state.transient_pids[] before reconcile
	SettleDuration    time.Duration // actual settle wait
}

// ReaperDeps allows test injection. Production wires only StateDir; the
// rest fall back to the Windows defaults below. The struct is kept
// identical to the POSIX surface so cross-platform tests compile against
// the same API.
type ReaperDeps struct {
	StateDir           string
	ReadState          func(path string) (*api.SupervisorStateFile, error)
	WriteState         func(path string, s *api.SupervisorStateFile) error
	PIDAlive           func(pid int) bool
	ProcessIdentity    func(pid int) (basename, cmdline string, uid int, ok bool)
	CurrentUID         func() int
	KillProcessGroup   func(pid int) error
	KillProcess        func(pid int) error
	ProcessStartTime   func(pid int) (time.Time, bool)
	StartedAtTolerance time.Duration
	SettleDuration     time.Duration
	Now                func() time.Time
}

// ReapStaleTransients walks supervisor-state.transient_pids on Windows
// cold start and reaps stale maintenance-timer children left behind by a
// prior supervisor that crashed mid-fire. Per-transient outcome:
//
//   - PID<=0 (claim slot)            → DeadPIDs, cleared (never a kill target).
//   - PID not alive                  → DeadPIDs, cleared (already gone).
//   - alive, start time matches the recorded started_at within tolerance
//     → our orphaned child → tree-kill (taskkill /F /T), KilledPIDs, cleared.
//   - alive, start time mismatches   → PID recycled to an unrelated
//     process → SkippedPIDs, entry cleared (not ours; never kill it).
//   - alive, start time unverifiable → SkippedPIDs, entry RETAINED for
//     the next cold start (conservative: do not kill what we cannot
//     identify).
//   - tree-kill returns an error     → KillErrors, entry retained.
//
// The start-time gate is the PID-recycling guard: a recycled PID's
// creation time differs from the recorded fire timestamp by far more
// than the tolerance, so it is never killed.
func ReapStaleTransients(ctx context.Context, deps ReaperDeps) (ReaperResult, error) {
	res := ReaperResult{}

	customStateIO := deps.ReadState != nil || deps.WriteState != nil
	readState := deps.ReadState
	if readState == nil {
		readState = api.ReadSupervisorState
	}
	writeState := deps.WriteState
	if writeState == nil {
		writeState = api.WriteSupervisorState
	}
	pidAlive := deps.PIDAlive
	if pidAlive == nil {
		pidAlive = isPIDAlive
	}
	startTime := deps.ProcessStartTime
	if startTime == nil {
		startTime = process.ProcessStartTime
	}
	killTree := deps.KillProcess
	if killTree == nil {
		killTree = process.TreeKillByPID
	}
	tolerance := deps.StartedAtTolerance
	if tolerance <= 0 {
		tolerance = 2 * time.Second
	}

	statePath := filepath.Join(deps.StateDir, "supervisor-state.json")
	reapState := func(state *api.SupervisorStateFile) (bool, error) {
		if state == nil || len(state.TransientPIDs) == 0 {
			return false, nil
		}
		res.ClearedTransients = len(state.TransientPIDs)

		var retained []api.TransientPID
		killAttempted := false
		for _, t := range state.TransientPIDs {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			pid := t.PID
			if pid <= 0 {
				// Claim slot — never alive, no kill; clear it.
				res.DeadPIDs = append(res.DeadPIDs, pid)
				continue
			}
			if !pidAlive(pid) {
				res.DeadPIDs = append(res.DeadPIDs, pid)
				continue
			}

			// Alive: verify this is still OUR maintenance child before
			// killing, so a recycled PID belonging to an unrelated process
			// is never terminated.
			observed, ok := startTime(pid)
			recorded, perr := time.Parse(time.RFC3339Nano, t.StartedAt)
			if !ok || perr != nil {
				// Cannot verify identity — leave the process alone and
				// retain the entry for the next cold start to retry.
				res.SkippedPIDs = append(res.SkippedPIDs, pid)
				retained = append(retained, t)
				continue
			}
			delta := observed.UTC().Sub(recorded.UTC())
			if delta < 0 {
				delta = -delta
			}
			if delta > tolerance {
				// Start times disagree → PID recycled to an unrelated
				// process. Not ours: do NOT kill; clear the stale entry.
				res.SkippedPIDs = append(res.SkippedPIDs, pid)
				continue
			}

			// Confirmed: our orphaned maintenance child. Tree-kill it.
			//
			// Residual TOCTOU (PR #243 bot round-2 review): the start-time
			// probe and the taskkill below are separate operations on a
			// PID, not one handle-atomic op, so in principle the PID could
			// exit and be reused between them. This window is the same one
			// the POSIX reaper (killProcessGroupSIGKILL after its gates) and
			// `mcphub gui --force --kill` (start-time-precedes-mtime gate)
			// already accept. It is not practically reachable here: the
			// recorded started_at belongs to a PRIOR (crashed) supervisor
			// generation, so a process that grabbed this PID after the crash
			// has a start time of ~now — far outside the 2s tolerance — and
			// fails the gate above. The only way past it is a recycle within
			// 2s of the original fire AND surviving until this cold-start
			// reap, which the crash→restart→reap latency makes implausible.
			if kerr := killTree(pid); kerr != nil {
				if res.KillErrors == nil {
					res.KillErrors = map[int]error{}
				}
				res.KillErrors[pid] = kerr
				retained = append(retained, t)
				continue
			}
			res.KilledPIDs = append(res.KilledPIDs, pid)
			killAttempted = true
		}

		if killAttempted && deps.SettleDuration > 0 {
			timer := time.NewTimer(deps.SettleDuration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-timer.C:
			}
			res.SettleDuration = deps.SettleDuration
		}

		state.TransientPIDs = retained
		return true, nil
	}

	if !customStateIO {
		if err := api.MutateSupervisorStateIfChanged(statePath, reapState); err != nil {
			return res, fmt.Errorf("mutate supervisor state: %w", err)
		}
		return res, nil
	}

	state, err := readState(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("read supervisor state: %w", err)
	}
	changed, err := reapState(state)
	if err != nil {
		return res, err
	}
	if changed {
		if err := writeState(statePath, state); err != nil {
			return res, fmt.Errorf("write supervisor state: %w", err)
		}
	}
	return res, nil
}
