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
//	Gate 1: image basename equals "mcphub" exactly (from /proc/<pid>/comm).
//	Gate 2: cmdline matches the recorded transient kind's invocation
//	        signature: daemon-shaped entries carry daemon AND --server AND
//	        --daemon; maintenance entries carry the argv tokens their timer
//	        Args actually exec (workspace-weekly-refresh → the subcommand
//	        token; server-weekly-refresh → restart AND --server).
//	Gate 3: process UID matches the current operator (from stat).
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
//
// KillErrors (Lane F P0 #5 / Lane B P0) records non-ESRCH errors
// returned by the kill syscall (EPERM, no-such-process-group fallback
// failures, etc.). Entries that appear in KillErrors are RETAINED in
// supervisor-state.transient_pids[] for retry on the next supervisor
// cold start; everything else (Killed/Dead/Skipped) is cleared.
// ESRCH whose follow-up identity re-check confirms the process is
// gone classifies as Dead, not as a kill error.
type ReaperResult struct {
	KilledPIDs        []int         // PIDs killed (after ownership + StartedAt gate)
	SkippedPIDs       []int         // PIDs alive but failed an ownership or StartedAt gate
	DeadPIDs          []int         // PIDs already gone (no kill needed)
	KillErrors        map[int]error // PIDs where kill returned a non-ESRCH error; entries retained in state
	ClearedTransients int           // size of supervisor-state.transient_pids[] before clear
	SettleDuration    time.Duration // actual settle wait
}

// ReaperDeps allows test injection. Production callers fill these via
// ReaperDepsForProduction(stateDir); tests pass synthetic functions
// to exercise the classification and state-write paths without
// touching /proc or invoking real syscall.Kill.
//
// ProcessStartTime (Lane F P0 #4) returns the kernel-recorded wall
// clock start time of a PID. The reaper compares this against
// state.transient_pids[].started_at; PIDs whose recorded vs computed
// start time differ by more than the StartedAtTolerance window are
// skipped (treated as PID recycling). On Darwin the production
// implementation returns (zero, false) — every PID then fails the
// gate and is skipped, the safe failure mode for a platform with no
// /proc.
//
// KillProcess (Lane F P0 #5) is the per-PID kill fallback used when
// KillProcessGroup returns ESRCH while the process is still alive
// (no process-group leader because POSIX spawn paths do not currently
// call Setpgid — see comment near KillProcessGroup invocation).
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
	if d.KillProcess == nil {
		d.KillProcess = killProcessSIGKILL
	}
	if d.ProcessStartTime == nil {
		d.ProcessStartTime = processStartTime
	}
	if d.StartedAtTolerance == 0 {
		d.StartedAtTolerance = 2 * time.Second
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
	// retained collects TransientPID entries that must SURVIVE the
	// reaper pass — currently only entries whose kill returned a
	// non-ESRCH error (Lane F P0 #5 / Lane B P0). They are re-attempted
	// on the next supervisor cold start.
	var retained []api.TransientPID

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
		if !ok || !ownershipGate(t.Kind, basename, cmdline, procUID, uid) {
			res.SkippedPIDs = append(res.SkippedPIDs, pid)
			continue
		}
		// Lane F P0 #4 — StartedAt gate. Compare recorded vs computed
		// start time; reject if outside tolerance (treat as PID
		// recycling). On Darwin processStartTime returns ok=false,
		// causing every PID to fail this gate and be skipped — the
		// safe failure mode (no kills > wrong kills on a platform
		// where we can't probe identity reliably).
		if !startedAtGate(t.StartedAt, pid, deps) {
			res.SkippedPIDs = append(res.SkippedPIDs, pid)
			continue
		}
		// All gates passed — kill the process group, then handle the
		// outcome per Lane F P0 #5 / Lane B P0:
		//
		//   nil err     → KilledPIDs, entry cleared from state.
		//   ESRCH       → identity re-check: PIDAlive==false confirms
		//                 the process is gone (kernel reaped it
		//                 between alive-check and kill — common
		//                 benign race) → DeadPIDs, cleared.
		//                 Otherwise it's the "no such process group"
		//                 case — POSIX spawn paths in this codebase
		//                 do NOT currently call Setpgid (audited as
		//                 of v0.5.0 phase 16; see also
		//                 internal/process/pdeathsig_linux.go which
		//                 only sets Pdeathsig). Fall back to per-PID
		//                 kill via KillProcess. If that succeeds the
		//                 PID is killed; if it ALSO returns ESRCH
		//                 the process is gone; on any other error,
		//                 KillErrors + retain in state.
		//   other err   → KillErrors[pid]=err; retain entry for the
		//                 next cold-start to re-try.
		err := deps.KillProcessGroup(pid)
		if err == nil {
			res.KilledPIDs = append(res.KilledPIDs, pid)
			killAttempted = true
			continue
		}
		if errors.Is(err, syscall.ESRCH) {
			// Differentiate "process gone" from "no pgroup leader".
			if !deps.PIDAlive(pid) {
				res.DeadPIDs = append(res.DeadPIDs, pid)
				continue
			}
			// Process is alive — pgroup kill failed because the daemon
			// never became a process-group leader. Fall back.
			fallbackErr := deps.KillProcess(pid)
			if fallbackErr == nil {
				res.KilledPIDs = append(res.KilledPIDs, pid)
				killAttempted = true
				continue
			}
			if errors.Is(fallbackErr, syscall.ESRCH) {
				// Race won by kernel between pgroup-ESRCH and per-PID.
				res.DeadPIDs = append(res.DeadPIDs, pid)
				continue
			}
			if res.KillErrors == nil {
				res.KillErrors = map[int]error{}
			}
			res.KillErrors[pid] = fallbackErr
			retained = append(retained, t)
			continue
		}
		if res.KillErrors == nil {
			res.KillErrors = map[int]error{}
		}
		res.KillErrors[pid] = err
		retained = append(retained, t)
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

	// Rewrite transient_pids[] preserving only entries whose kill
	// returned an unrecoverable error (so the next cold-start can
	// retry). Everything else — killed / dead / skipped — is cleared.
	// Skipped PIDs surface via res.SkippedPIDs for the caller's event
	// log; the operator must intervene manually for those.
	state.TransientPIDs = retained
	if err := deps.WriteState(statePath, state); err != nil {
		return res, fmt.Errorf("write supervisor state: %w", err)
	}
	return res, nil
}

// startedAtGate returns true when the kernel-computed process start
// time agrees with the supervisor-state.transient_pids[].started_at
// timestamp within deps.StartedAtTolerance. A mismatch (or an
// unparseable recorded timestamp, or a probe that returned ok=false)
// fails the gate — the caller treats this as PID recycling and skips
// the kill (Lane F P0 #4).
func startedAtGate(recordedRFC3339 string, pid int, deps ReaperDeps) bool {
	recorded, err := time.Parse(time.RFC3339Nano, recordedRFC3339)
	if err != nil {
		return false
	}
	computed, ok := deps.ProcessStartTime(pid)
	if !ok {
		return false
	}
	delta := recorded.Sub(computed)
	if delta < 0 {
		delta = -delta
	}
	return delta <= deps.StartedAtTolerance
}

// ownershipGate returns true when all 3 gates pass: image basename
// equals exactly "mcphub"; cmdline matches the recorded transient
// kind's invocation signature; UID matches the current operator.
// Gate 2 accepts both daemon-shaped transients and maintenance timer
// children. Maintenance children are NOT all invoked as `mcphub <kind>`:
// workspace-weekly-refresh runs `mcphub workspace-weekly-refresh`, but
// server-weekly-refresh runs `mcphub restart --server <name>`, so the
// gate matches each kind's real argv tokens (maintenanceCmdlineSignature).
// A bare kind-token match would skip-and-forget live server-weekly-refresh
// orphans after a supervisor crash.
func ownershipGate(kind, basename, cmdline string, procUID, currentUID int) bool {
	if procUID != currentUID {
		return false
	}
	if basename != "mcphub" {
		return false
	}
	if sig, ok := maintenanceCmdlineSignature(kind); ok {
		for _, tok := range sig {
			if !cmdlineHasToken(cmdline, tok) {
				return false
			}
		}
		return true
	}
	return cmdlineHasToken(cmdline, "daemon") &&
		cmdlineHasToken(cmdline, "--server") &&
		cmdlineHasToken(cmdline, "--daemon")
}

func cmdlineHasToken(cmdline, token string) bool {
	for _, field := range strings.Fields(cmdline) {
		if field == token {
			return true
		}
	}
	return false
}

// maintenanceCmdlineSignature maps a maintenance transient Kind to the
// argv tokens its `mcphub` invocation always carries, so Gate 2 can
// confirm a live PID is the maintenance child we recorded — not an
// unrelated recycled mcphub PID — before killing it. The tokens mirror
// the timer Args the supervisor execs verbatim via exec.Command:
//   - workspace-weekly-refresh → `mcphub workspace-weekly-refresh`
//     (internal/cli/supervise_maintenance.go maintenance-timer Args)
//   - server-weekly-refresh    → `mcphub restart --server <name>`
//     (internal/api/install.go weekly-refresh task Args)
func maintenanceCmdlineSignature(kind string) ([]string, bool) {
	switch kind {
	case "workspace-weekly-refresh":
		return []string{"workspace-weekly-refresh"}, true
	case "server-weekly-refresh":
		return []string{"restart", "--server"}, true
	default:
		return nil, false
	}
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
// reaches the group leader's PGID and cascades to child shells / npx
// wrappers / MCP server subprocesses spawned underneath.
//
// SPAWN-SIDE GAP (Lane F P0 #4): POSIX daemon spawn paths in this
// codebase do NOT currently call SysProcAttr.Setpgid = true. Audited
// surfaces as of v0.5.0 phase 16:
//
//   - internal/cli/* — no Setpgid use anywhere; daemon.go's
//     exec.CommandContext call inherits the supervisor's pgroup.
//   - internal/process/pdeathsig_linux.go — only sets Pdeathsig, not
//     SysProcAttr.Setpgid.
//   - internal/process/start_with_job_windows.go — Windows-only.
//
// Consequence: a daemon's PID is typically not a process-group
// leader, so kill(-pid, SIGKILL) returns ESRCH ("no such process
// group" rather than "no such process"). The reaper handles this by
// re-checking PIDAlive: if the PID is gone the kernel already reaped
// it (DeadPIDs); if it's still alive the reaper falls back to per-
// PID kill via deps.KillProcess.
//
// The wider fix — calling SysProcAttr.Setpgid = true at every POSIX
// spawn site so kill(-pid) reliably reaches the daemon's children —
// is tracked separately. The reaper's per-PID fallback is the
// orphan-recovery mechanism in the interim.
func killProcessGroupSIGKILL(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to kill non-positive pid %d", pid)
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// killProcessSIGKILL invokes kill(pid, SIGKILL) — terminates only the
// daemon process itself, not its children. Used as the fallback when
// kill(-pid) returns ESRCH because the daemon never became a process-
// group leader (see SPAWN-SIDE GAP comment on killProcessGroupSIGKILL).
//
// This narrower kill leaves any grandchildren orphaned. That is
// strictly worse than the cascading process-group kill the reaper
// prefers, but the orphan-grandchild problem already exists in the
// current spawn path (without Setpgid the supervisor cannot kill its
// daemon's children through any single syscall anyway), so the per-
// PID fallback does not regress beyond the existing surface.
func killProcessSIGKILL(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to kill non-positive pid %d", pid)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
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
