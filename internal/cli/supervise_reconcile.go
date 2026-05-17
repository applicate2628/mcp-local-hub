// Package cli — Task 7.1 reconcile loop diff primitive.
//
// Spec §"Reconcile loop" + plan Task 7.1.
//
// The supervisor process (Task 6.1) holds two intent inputs:
//
//   - supervisor-intent.json — the authoritative list of daemons that
//     SHOULD be running, with full descriptor (command, args, env,
//     port, workspace). Written by `mcphub install` / manifest-edit
//     paths.
//   - daemon-intent.json — the per-daemon override file carrying
//     user-stop (TTL), user-disabled (permanent), and chronic-failure
//     (watchdog quarantine) decisions. Read with `DaemonIntent.IsActiveStop`
//     which encodes the full TTL + clock-skew + stale-bound decision
//     tree (`internal/api/daemon_intent.go:289-324`).
//
// `Reconciler.Reconcile` is the pure diff step:
//
//   - in intent + NOT IsActiveStop + NOT running → spawn (state-machine
//     `EvStart` fan-out)
//   - in intent + IsActiveStop + running        → terminate (state-machine
//     `EvIntentUpdate{stopped}` fan-out)
//   - in intent + IsActiveStop + NOT running    → no-op (quarantine respected)
//   - in intent + NOT IsActiveStop + running    → no-op (steady state)
//
// Orphans (running but NOT in intent) are intentionally NOT terminated
// here — Reconciler has no daemon descriptor to fan a terminate
// signal through. Cold-start reaper Task 13.1 owns orphan handling
// via the supervisor-state.json `Daemons` map which carries last-known
// PID + canonical task name for every supervised child.
//
// Pure separation: no I/O, no file reads, no clock calls. `Reconcile`
// receives the parsed intent files and a snapshot of currently-running
// task names, and returns nothing — side effects flow through the
// caller-supplied SpawnFunc / TerminateFunc closures. Tests inject
// recording closures; production wires them into the FIFO event loop
// (`internal/api.NewEventLoop`) which routes through the per-daemon
// state machine (`internal/api/supervisor_state_machine.go`).
package cli

import (
	"time"

	"mcp-local-hub/internal/api"
)

// SpawnFunc starts a daemon child. Returns the spawn error so the
// reconciler can record it (currently swallowed; a follow-up task may
// surface per-daemon failures through the audit log).
type SpawnFunc func(d api.SupervisorDaemon) error

// TerminateFunc signals a running daemon child to stop. Returns the
// termination error.
type TerminateFunc func(d api.SupervisorDaemon) error

// Reconciler diffs intent against currently-running daemons and fans
// out spawn/terminate actions. Construction is via NewReconciler; the
// zero value is intentionally not usable because nil fan-out funcs
// would silently swallow every reconcile decision.
type Reconciler struct {
	spawn     SpawnFunc
	terminate TerminateFunc
}

// NewReconciler builds a Reconciler with the supplied fan-out
// closures. Both must be non-nil; tests can supply no-op closures
// to exercise just one side of the diff.
func NewReconciler(spawn SpawnFunc, terminate TerminateFunc) *Reconciler {
	return &Reconciler{spawn: spawn, terminate: terminate}
}

// Reconcile applies one diff pass against the supplied intent and
// running snapshot.
//
// Inputs:
//
//   - intent          — parsed supervisor-intent.json (the daemons we want)
//   - daemonIntent    — parsed daemon-intent.json (user-stop / chronic-failure
//     overrides). May be nil — caller treats missing as "no overrides".
//   - currentRunning  — map of canonical task_name → true for daemons
//     currently running under the supervisor. Caller (the lifecycle
//     side of supervise.go) is the source of truth.
//   - now             — reference time threaded into `DaemonIntent.IsActiveStop`
//     for TTL + clock-skew checks. Tests inject a fixed clock; production
//     passes `time.Now().UTC()`.
//
// The function is intentionally side-effect-free outside the supplied
// fan-out closures: it returns nothing, swallows per-daemon spawn /
// terminate errors (the closures themselves own the error path), and
// never mutates its inputs.
func (r *Reconciler) Reconcile(
	intent *api.SupervisorIntentFile,
	daemonIntent *api.DaemonIntentFile,
	currentRunning map[string]bool,
	now time.Time,
) {
	if intent == nil {
		return
	}
	for _, d := range intent.Daemons {
		// daemon-intent uses canonical leading-backslash keys; the
		// SupervisorDaemon.TaskName field is canonical by construction
		// (writer guarantees the leading backslash per
		// internal/api/daemon_intent.go:271-283 NormalizeTaskName).
		isStopped := false
		if daemonIntent != nil {
			entry, ok := daemonIntent.Tasks[d.TaskName]
			if ok {
				stopped, _ := entry.IsActiveStop(now)
				isStopped = stopped
			}
		}

		running := currentRunning[d.TaskName]

		switch {
		case !isStopped && !running:
			_ = r.spawn(d)
		case isStopped && running:
			_ = r.terminate(d)
		}
		// !isStopped && running  → steady state, no-op
		// isStopped  && !running → quarantine respected, no-op
	}
	// Orphans (in currentRunning but NOT in intent) are NOT handled
	// here — Reconciler has no descriptor to terminate them through.
	// Task 13.1 cold-start reaper owns orphan cleanup via the
	// supervisor-state.json `Daemons` map.
}
