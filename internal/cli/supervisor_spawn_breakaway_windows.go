//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows"
)

// winCreateBreakawayFromJob lets a spawned process escape an inherited
// Windows Job Object whose limit flags include KILL_ON_JOB_CLOSE. The
// MANUAL /api/supervisor/restart spawn path already sets this
// (internal/gui/supervisor_restart_windows.go, with a cascade-kill
// rationale); the two AUTOMATIC supervisor-spawn paths historically did
// NOT — an asymmetry that lets an automatic spawn inherit (and be
// cascade-killed by) the launcher's job while the manual one survives.
// PART 1 of the §5 permanent fix closes that asymmetry.
const winCreateBreakawayFromJob = 0x01000000

// startSupervisorDetachedBreakaway starts an already-detached supervisor
// cmd (the caller has set DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP)
// with CREATE_BREAKAWAY_FROM_JOB added so the long-lived supervisor
// escapes any KILL_ON_JOB_CLOSE job inherited from its launcher (GUI /
// Task Scheduler / install CLI).
//
// Per Microsoft docs, CreateProcess FAILS with ERROR_ACCESS_DENIED when
// the parent job does NOT permit breakaway (no JOB_OBJECT_LIMIT_BREAKAWAY_OK).
// On THAT specific error we retry ONCE flagless via rebuild() — still
// detached, just without breakaway. Only after that retry starts successfully
// do we invoke onDegrade so the (rare, locked-down corp host) loss of
// orphan-protection is operator-visible. A failed retry remains a hard spawn
// failure and reports both CreateProcess attempts.
//
// rebuild MUST return a FRESH equivalent cmd (a started exec.Cmd cannot
// be restarted) that reuses the caller's stdio so the caller's post-spawn
// reads (e.g. the stderr tail buffer) still observe the started process.
// Returns the cmd that actually started so the caller reads Process from it.
func startSupervisorDetachedBreakaway(cmd *exec.Cmd, rebuild func() *exec.Cmd, onDegrade func(error)) (*exec.Cmd, error) {
	return startSupervisorDetachedBreakawayWithStart(cmd, rebuild, onDegrade, func(next *exec.Cmd) error {
		return next.Start()
	})
}

// startSupervisorDetachedBreakawayWithStart owns the retry decision while
// accepting the one operation whose Windows Job behavior cannot be made
// deterministic with a missing executable fixture. Production supplies
// exec.Cmd.Start; tests supply a per-call result without mutable package state.
func startSupervisorDetachedBreakawayWithStart(cmd *exec.Cmd, rebuild func() *exec.Cmd, onDegrade func(error), start func(*exec.Cmd) error) (*exec.Cmd, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &windows.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= winCreateBreakawayFromJob
	err := start(cmd)
	if err == nil {
		return cmd, nil
	}
	// ERROR_ACCESS_DENIED is the documented CreateProcess failure when the
	// parent job lacks JOB_OBJECT_LIMIT_BREAKAWAY_OK. This errno match is
	// precedent-verified (same windows.ERROR_ACCESS_DENIED idiom as the sibling
	// syscall sites) — it is NOT exercised by a unit test because that requires
	// constructing a real breakaway-rejecting parent Job; the live stability
	// window + the locked-down-host field path cover it instead.
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return cmd, err
	}
	// Parent job forbids breakaway. Retry flagless (rebuild constructs a
	// fresh cmd WITHOUT the breakaway flag) so the supervisor still spawns.
	retry := rebuild()
	if e2 := start(retry); e2 != nil {
		return retry, fmt.Errorf("breakaway spawn failed: %w; flagless retry failed: %w", err, e2)
	}
	if onDegrade != nil {
		onDegrade(err)
	}
	return retry, nil
}
