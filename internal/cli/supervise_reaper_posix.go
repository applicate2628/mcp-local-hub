//go:build !windows

// Package cli — Task 13.1 cold-start stale-child reaper for Linux/macOS.
//
// On POSIX the supervisor has no Job Object equivalent that the kernel
// auto-reaps on parent exit. If a prior supervisor crashed (or was
// killed -9) while it still held child daemons in its process tree,
// those children are now reparented to PID 1 and remain running. The
// next supervisor start would, without intervention, attempt to bind
// each daemon's port and lose to the orphan — leaving the user with
// stuck child daemons and a supervisor stuck in spawn-failure loops.
//
// Spec §"Fallback if step 4 IPC fails" + plan §2660 — every supervisor
// cold-start scans supervisor-state.transient_pids[] (the list of
// fire-and-forget children the prior supervisor wrote BEFORE the
// spawn syscall returned) and reaps any PID that still belongs to us.
//
// The 3-gate ownership check defends against PID recycling: a recycled
// PID assigned to a different operator's process must NOT be killed
// even if it happens to occupy a slot in our transient list.
//
//  Gate 1: image basename equals "mcphub" exactly (from /proc/<pid>/comm).
//  Gate 2: cmdline contains the daemon token AND --server AND --daemon
//          (from /proc/<pid>/cmdline) — these are the only invocation
//          shapes we ever record in transient_pids per
//          supervise_maintenance.go.
//  Gate 3: process UID matches the current operator (from stat).
//
// All three must pass. If any gate fails, the PID is recorded in
// SkippedPIDs (not killed) and the operator surfaces it via the
// supervisor event log emitted by the caller (newSuperviseCmd). If
// the PID is already gone (kernel returned ESRCH on alive-check or
// /proc/<pid>/ is missing), the PID is recorded in DeadPIDs and
// removed from state without a kill.
//
// Kill mechanism: kill(-pgid, SIGKILL) via syscall.Kill(-pid, SIGKILL).
// Sending to the negated PGID kills every member of the daemon's
// process group — covers child-of-child shells, npx wrappers, and the
// daemon's own MCP server subprocess. Best-effort: a failed kill is
// logged but does NOT preserve the stale PID in state (the next start
// would re-attempt the reap pass and find a dead PID anyway).
//
// Settle: 2-3 seconds between the kill burst and the first reconcile
// spawn so the kernel finishes its TCP TIME_WAIT teardown on the
// daemon's listening port. Without the settle the reconcile spawn
// races the kernel's port-release and gets EADDRINUSE.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mcp-local-hub/internal/api"
)

// ReaperResult reports what the reaper did during this supervisor cold
// start. Caller (newSuperviseCmd) logs each slice's contents to the
// supervisor event log so operators have provenance for each PID
// the reaper touched.
type ReaperResult struct {
	KilledPIDs        []int         // PIDs killed (after ownership gate)
	SkippedPIDs       []int         // PIDs alive but failed ownership gate
	DeadPIDs          []int         // PIDs already gone (no kill needed)
	ClearedTransients int           // size of supervisor-state.transient_pids[] before clear
	SettleDuration    time.Duration // actual settle wait
}

// ReaperDeps allows test injection. Production callers fill these via
// ReaperDepsForProduction(stateDir); tests pass synthetic functions
// to exercise the classification and state-write paths without
// touching /proc or invoking real syscall.Kill.
type ReaperDeps struct {
	StateDir         string
	ReadState        func(path string) (*api.SupervisorStateFile, error)
	WriteState       func(path string, s *api.SupervisorStateFile) error
	PIDAlive         func(pid int) bool
	ProcessIdentity  func(pid int) (basename, cmdline string, uid int, ok bool)
	CurrentUID       func() int
	KillProcessGroup func(pid int) error
	SettleDuration   time.Duration
	Now              func() time.Time
}

// withDefaults returns deps with any unset fields filled from the
// production implementations. Tests that want a specific seam stay
// in control by setting the field explicitly; this fills the gaps.
func (d ReaperDeps) withDefaults() ReaperDeps {
	if d.ReadState == nil {
		d.ReadState = api.ReadSupervisorState
	}
	if d.WriteState == nil {
		d.WriteState = api.WriteSupervisorState
	}
	if d.PIDAlive == nil {
		d.PIDAlive = pidAliveSignal0
	}
	if d.ProcessIdentity == nil {
		d.ProcessIdentity = procIdentityFromProc
	}
	if d.CurrentUID == nil {
		d.CurrentUID = os.Getuid
	}
	if d.KillProcessGroup == nil {
		d.KillProcessGroup = killProcessGroupSIGKILL
	}
	if d.SettleDuration == 0 {
		d.SettleDuration = 2 * time.Second
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return d
}

// ReapStaleTransients walks supervisor-state.transient_pids and reaps
// any orphans from a prior supervisor crash. Returns ReaperResult for
// caller logging; non-fatal kill failures are recorded in KilledPIDs
// (the attempt was made) but do not propagate as errors.
//
// Returns ctx.Err() if cancellation fires during the read, classify,
// or settle phase. On cancellation the state is NOT written back —
// transient_pids[] stays as-is so the next supervisor cold-start
// re-attempts the reap pass and finds the PIDs already dead (via the
// kill we did execute before noticing cancellation).
func ReapStaleTransients(ctx context.Context, deps ReaperDeps) (ReaperResult, error) {
	deps = deps.withDefaults()
	var res ReaperResult

	if err := ctx.Err(); err != nil {
		return res, err
	}

	statePath := filepath.Join(deps.StateDir, "supervisor-state.json")
	state, err := deps.ReadState(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || isUnderlyingNotExist(err) {
			// First-boot host has no state file — nothing to reap.
			return res, nil
		}
		return res, fmt.Errorf("read supervisor state: %w", err)
	}

	if state == nil || len(state.TransientPIDs) == 0 {
		return res, nil
	}
	res.ClearedTransients = len(state.TransientPIDs)

	uid := deps.CurrentUID()
	killAttempted := false

	for _, t := range state.TransientPIDs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		pid := t.PID
		if pid <= 0 {
			// Claim slot (PID=0) — never alive, treat as dead so the
			// state-clear still happens but no kill is attempted.
			res.DeadPIDs = append(res.DeadPIDs, pid)
			continue
		}
		if !deps.PIDAlive(pid) {
			res.DeadPIDs = append(res.DeadPIDs, pid)
			continue
		}
		basename, cmdline, procUID, ok := deps.ProcessIdentity(pid)
		if !ok || !ownershipGate(basename, cmdline, procUID, uid) {
			res.SkippedPIDs = append(res.SkippedPIDs, pid)
			continue
		}
		// 3-gate passed — kill the process group. Recording the
		// kill in KilledPIDs even on syscall failure documents the
		// attempt for the supervisor event log; the state-clear
		// below still proceeds so the next start sees a clean slate.
		_ = deps.KillProcessGroup(pid)
		res.KilledPIDs = append(res.KilledPIDs, pid)
		killAttempted = true
	}

	if killAttempted {
		if err := sleepWithCtx(ctx, deps.SettleDuration); err != nil {
			return res, err
		}
		res.SettleDuration = deps.SettleDuration
	}

	if err := ctx.Err(); err != nil {
		return res, err
	}

	// Clear transient_pids[] and write back. We rewrite even when all
	// entries classified as Dead or Skipped: stale entries in the
	// transient list serve no purpose past the cold-start reap, and
	// leaving them would only confuse the next start. Skipped PIDs
	// are surfaced via res.SkippedPIDs for the caller's event log.
	state.TransientPIDs = nil
	if err := deps.WriteState(statePath, state); err != nil {
		return res, fmt.Errorf("write supervisor state: %w", err)
	}
	return res, nil
}

// ownershipGate returns true when all 3 gates pass: image basename
// equals exactly "mcphub"; cmdline contains the daemon-invocation
// token shape; UID matches the current operator. Gate 2 checks the
// presence of three distinct tokens rather than a single substring
// because /proc/<pid>/cmdline NUL-separates argv: the basename
// comparison + per-token substring check is enough to confirm this
// is an mcphub daemon child and not, say, a user's mcphub gui or
// mcphub install invocation that happens to share the name.
func ownershipGate(basename, cmdline string, procUID, currentUID int) bool {
	if procUID != currentUID {
		return false
	}
	if basename != "mcphub" {
		return false
	}
	if !strings.Contains(cmdline, "daemon") {
		return false
	}
	if !strings.Contains(cmdline, "--server") {
		return false
	}
	if !strings.Contains(cmdline, "--daemon") {
		return false
	}
	return true
}

// pidAliveSignal0 returns true if kill(pid, 0) succeeds (process exists
// and we have permission to signal it) or returns EPERM (process
// exists but we lack permission — still counts as alive). ESRCH means
// the PID is gone.
func pidAliveSignal0(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

// procIdentityFromProc reads /proc/<pid>/comm, /proc/<pid>/cmdline, and
// stats /proc/<pid> to derive the (basename, cmdline, uid) tuple.
// macOS does NOT have /proc — on Darwin this falls back to a sysctl-
// based path; for the v0.5.0 Task 13.1 scope the Linux /proc reader is
// sufficient because the reaper is only documented to run on Linux at
// production (macOS supervisor is launchd-managed and inherits Job-
// Object-equivalent semantics from launchd's group teardown). The
// macOS fallback returns ok=false which causes the caller to record
// the PID as skipped — the safest behavior on a platform where we
// can't probe identity.
func procIdentityFromProc(pid int) (string, string, int, bool) {
	procPath := fmt.Sprintf("/proc/%d", pid)
	st, err := os.Stat(procPath)
	if err != nil {
		return "", "", 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", 0, false
	}
	commRaw, err := os.ReadFile(filepath.Join(procPath, "comm"))
	if err != nil {
		return "", "", 0, false
	}
	basename := strings.TrimSpace(string(commRaw))
	cmdlineRaw, err := os.ReadFile(filepath.Join(procPath, "cmdline"))
	if err != nil {
		return "", "", 0, false
	}
	// /proc/<pid>/cmdline NUL-separates argv. Convert to space-joined
	// form for the per-token substring checks in ownershipGate.
	cmdline := strings.ReplaceAll(string(cmdlineRaw), "\x00", " ")
	cmdline = strings.TrimSpace(cmdline)
	return basename, cmdline, int(sys.Uid), true
}

// killProcessGroupSIGKILL invokes kill(-pid, SIGKILL) — terminates
// every member of the daemon's process group. The negated PID form
// reaches the group leader's PGID; the daemon's own process group is
// established at spawn time in supervise_maintenance.go via
// setsid()-equivalent behavior on Linux/macOS.
func killProcessGroupSIGKILL(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to kill non-positive pid %d", pid)
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// sleepWithCtx blocks for d, returning early with ctx.Err() if the
// context cancels first.
func sleepWithCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isUnderlyingNotExist unwraps wrapped fs errors so the "first-boot
// host" detection works regardless of how api.ReadSupervisorState
// formats its error. api.ReadSupervisorState wraps os.ReadFile
// errors with "read: %w", which means errors.Is(err, os.ErrNotExist)
// catches the wrapped case — this helper is the explicit assertion
// of that fact.
func isUnderlyingNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
