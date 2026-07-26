// internal/api/mcp_front_port_ownership.go
//
// The SUPERVISOR-OWNERSHIP half of the `mcphub install --reconcile-mcp-front`
// pre-write gate.
//
// WHY THIS EXISTS. The forward cutover already proves two things before it
// rewrites a client config:
//
//   - the port is not claimed by a DIFFERENT known owner
//     (internal/cli's assertMCPFrontPortNotForeignOwned), and
//   - something at the port answers the /serena/mcp protocol shape
//     (defaultRouterReadinessPing's HEAD + `initialize` round-trip).
//
// Neither proves the listener will still be there tomorrow. A bare
// `mcphub route --port <N>` typed in a terminal satisfies BOTH — it is the
// real route server, so it answers the probe perfectly — and nothing restarts
// it when the operator closes the shell. The cutover would then have rewritten
// every in-scope client onto a port with no listener and no owner: the exact
// failure mode ("the data plane dies with the process that happens to host
// it") the whole front-daemon decision exists to eliminate. Protocol shape is
// not ownership, and only ownership carries the survival guarantee.
//
// So this gate requires the port to be served by a SUPERVISED child:
//
//  1. a live supervisor  — flock-authoritative (SupervisorRunningUnderStateDir).
//     This is the actor that restarts the daemon; without it there is no
//     survival guarantee no matter who holds the port right now.
//  2. the canonical descriptor — supervisor-intent.json must carry the
//     reserved built-in route row (BuiltinRouteTaskName + the reserved
//     Server/Daemon identity) AND that row's Port must be exactly the port
//     about to be written into client configs. A supervisor that is managing
//     some OTHER port cannot keep THIS one alive.
//  3. a live supervisor-child PID — supervisor-state.json must show that
//     descriptor `running` with a current_pid that is actually alive.
//  4. an OS-level binding — the kernel's owner of the loopback LISTENING
//     socket must BE that PID. This is the unforgeable link between "the
//     supervisor says it spawned a route child" and "the thing answering on
//     this port is that child". Same primitive, and same fail-closed posture,
//     as defaultGUIPidportIdentityCheck (bot PR #252 P1: never trust a
//     self-reported PID).
//  5. image identity — the owning process must be the mcphub binary, on every
//     platform that can resolve a PID's image (imageIdentityProbeSupported).
//     This is defense-in-depth against PID reuse between the supervisor's
//     state write and this read.
//
// PLATFORM POSTURE (explicit, never a silent downgrade):
//
//   - Windows (GA): all five checks run. An unresolvable image is a REFUSAL,
//     not a skip.
//   - Linux (beta): steps 1-4 run against the /proc socket tables; step 5 is
//     structurally unavailable (there is no image resolver on this target, so
//     imageIdentityProbeSupported is false) and is documented as such rather
//     than faked. The PID chain still binds the listener to the supervisor's
//     recorded child.
//   - macOS (preview) and other POSIX: step 4's resolver is a fail-closed stub,
//     so the gate REFUSES with that platform's own error. The cutover is not
//     available there, which is honest — the ownership proof it depends on is
//     not implementable on that target yet.
//
// RESIDUAL. supervisor-intent.json and supervisor-state.json are ordinary
// state files. On a host whose %LOCALAPPDATA% parent grants FILE_DELETE_CHILD
// to a co-resident principal, those files can be swapped (see CLAUDE.md,
// "Hardened state-file writes"), which is why the OS-level steps 4-5 exist:
// a swapped state file can only ever NAME a PID, it cannot make the kernel
// attribute the socket to it, nor make a foreign image pass step 5. Operators
// on such hosts set MCPHUB_REQUIRE_SINGLE_USER_HOME=1, the same mitigation
// every other state-file consumer documents.
package api

import (
	"errors"
	"fmt"

	"mcp-local-hub/internal/clients"
)

// ErrMCPFrontPortNotSupervisorOwned is the fail-closed sentinel every refusal
// below wraps. Callers must write NO client config when it fires: a client
// rewritten onto an unsupervised port is strictly worse than one left alone,
// because the operator loses the endpoint the moment the ad-hoc process exits.
var ErrMCPFrontPortNotSupervisorOwned = errors.New("mcp_front.port is not served by a live supervisor-managed `mcphub route` front daemon")

// mcpFrontOwnershipRemedy is the one operator instruction every refusal ends
// with. Single-owned so the twelve refusal paths below cannot drift apart.
const mcpFrontOwnershipRemedy = "start the supervisor (`mcphub supervise`, or enable autostart) and let it spawn the built-in route daemon, then re-run; a bare `mcphub route` answers the readiness probe but nothing restarts it, so the clients this command rewrites would lose their endpoint the moment it exits"

// AssertMCPFrontPortSupervisorOwned proves port is served by the supervisor's
// own built-in route child, per the five-step chain in this file's header.
//
// It is a pure read: no state file, client config, or process is mutated on
// any path. A nil return means every step passed; every other return wraps
// ErrMCPFrontPortNotSupervisorOwned and names the step that refused.
//
// Deliberately NOT best-effort. The sibling gate
// (assertMCPFrontPortNotForeignOwned) treats its own read errors as
// non-blocking because it is looking for a POSITIVE collision and an
// unreadable file is not one. This gate is the mirror image: it is looking for
// a POSITIVE ownership proof, so an unreadable file means the proof is absent
// and the run must refuse.
func AssertMCPFrontPortSupervisorOwned(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%w: port %d is out of range", ErrMCPFrontPortNotSupervisorOwned, port)
	}

	stateDir, err := DaemonStateDir()
	if err != nil {
		return fmt.Errorf("%w: resolve state dir: %v", ErrMCPFrontPortNotSupervisorOwned, err)
	}

	// (1) A live supervisor. Flock-authoritative — the same probe the
	//     liveness task uses, so "running" here means the same thing it means
	//     everywhere else in the fleet.
	running, supervisorPID, lerr := SupervisorRunningUnderStateDir(stateDir)
	if lerr != nil {
		return fmt.Errorf("%w: could not determine whether a supervisor is running: %v; %s", ErrMCPFrontPortNotSupervisorOwned, lerr, mcpFrontOwnershipRemedy)
	}
	if !running {
		return fmt.Errorf("%w: no supervisor holds %s; %s", ErrMCPFrontPortNotSupervisorOwned, joinStateFilePath(stateDir, "supervisor.lock"), mcpFrontOwnershipRemedy)
	}

	// (2) The canonical built-in route descriptor, at exactly this port.
	intentPath, ierr := DefaultSupervisorIntentPath()
	if ierr != nil {
		return fmt.Errorf("%w: resolve supervisor-intent path: %v", ErrMCPFrontPortNotSupervisorOwned, ierr)
	}
	intent, rerr := ReadSupervisorIntent(intentPath)
	if rerr != nil {
		return fmt.Errorf("%w: read %s: %v; %s", ErrMCPFrontPortNotSupervisorOwned, intentPath, rerr, mcpFrontOwnershipRemedy)
	}
	if intent == nil {
		return fmt.Errorf("%w: %s carries no supervisor intent; %s", ErrMCPFrontPortNotSupervisorOwned, intentPath, mcpFrontOwnershipRemedy)
	}
	descriptor := findBuiltinRouteDescriptor(intent)
	if descriptor == nil {
		return fmt.Errorf("%w: %s carries no built-in route daemon row (task %q, server %q, daemon %q); the supervisor is running but is not managing a route front daemon at all; %s",
			ErrMCPFrontPortNotSupervisorOwned, intentPath, BuiltinRouteTaskName, BuiltinRouteServer, BuiltinRouteDaemonName, mcpFrontOwnershipRemedy)
	}
	if descriptor.Port != port {
		return fmt.Errorf("%w: the built-in route daemon in %s is configured for port %d, not the mcp_front.port %d this run would write into client configs; the supervisor would keep %d alive and leave %d unowned. Align the setting with the managed descriptor (`mcphub settings set %s %d`) or restart the supervisor so it re-seeds the descriptor at %d, then re-run",
			ErrMCPFrontPortNotSupervisorOwned, intentPath, descriptor.Port, port, descriptor.Port, port, MCPFrontPortSettingKey, descriptor.Port, port)
	}

	// (3) A live supervisor-child PID for that descriptor.
	statePath := joinStateFilePath(stateDir, supervisorStateFileLeaf)
	state, serr := ReadSupervisorState(statePath)
	if serr != nil {
		return fmt.Errorf("%w: read %s: %v; %s", ErrMCPFrontPortNotSupervisorOwned, statePath, serr, mcpFrontOwnershipRemedy)
	}
	row, found := findBuiltinRouteState(state)
	if !found {
		return fmt.Errorf("%w: %s has no runtime row for %q, so the supervisor has not (yet) spawned the route daemon; %s", ErrMCPFrontPortNotSupervisorOwned, statePath, BuiltinRouteTaskName, mcpFrontOwnershipRemedy)
	}
	if row.State != supervisorDaemonStateRunning {
		return fmt.Errorf("%w: the supervisor records the route daemon as %q, not %q, in %s; %s", ErrMCPFrontPortNotSupervisorOwned, row.State, supervisorDaemonStateRunning, statePath, mcpFrontOwnershipRemedy)
	}
	if row.CurrentPID <= 0 {
		return fmt.Errorf("%w: the supervisor records the route daemon as running but with no current_pid in %s; %s", ErrMCPFrontPortNotSupervisorOwned, statePath, mcpFrontOwnershipRemedy)
	}
	alive, aerr := processAlive(row.CurrentPID)
	if aerr != nil {
		return fmt.Errorf("%w: check route-daemon PID %d liveness: %v", ErrMCPFrontPortNotSupervisorOwned, row.CurrentPID, aerr)
	}
	if !alive {
		return fmt.Errorf("%w: the route daemon PID %d recorded in %s is not alive (stale state; supervisor PID %d has not yet reconciled); %s", ErrMCPFrontPortNotSupervisorOwned, row.CurrentPID, statePath, supervisorPID, mcpFrontOwnershipRemedy)
	}

	// (4) The kernel's own answer to "who owns this socket". This is the step
	//     a standalone `mcphub route` cannot pass: it is not the PID the
	//     supervisor recorded, however perfectly it speaks the protocol.
	ownerPID, ok, oerr := loopbackPortOwnerFn(port)
	if oerr != nil {
		return fmt.Errorf("%w: resolve the OS owner of loopback port %d: %v", ErrMCPFrontPortNotSupervisorOwned, port, oerr)
	}
	if !ok || ownerPID <= 0 {
		return fmt.Errorf("%w: no process owns the loopback LISTENING socket on port %d", ErrMCPFrontPortNotSupervisorOwned, port)
	}
	if ownerPID != row.CurrentPID {
		return fmt.Errorf("%w: loopback port %d is owned by PID %d, but the supervisor's route child is PID %d (per %s). Something OTHER than the supervised front daemon is answering on this port — most commonly a standalone `mcphub route` started by hand, which passes the readiness probe but is restarted by nobody; %s",
			ErrMCPFrontPortNotSupervisorOwned, port, ownerPID, row.CurrentPID, statePath, mcpFrontOwnershipRemedy)
	}

	// (5) Image identity, where the platform can answer at all.
	image, iok := guiImageForPIDFn(ownerPID)
	if !iok {
		if imageIdentityProbeSupported {
			return fmt.Errorf("%w: could not resolve the image of port-%d owner PID %d; this platform supports image identity, so an unresolvable image fails closed rather than downgrading the proof", ErrMCPFrontPortNotSupervisorOwned, port, ownerPID)
		}
		// Documented tier: this target has no image resolver at all (see the
		// PLATFORM POSTURE block in this file's header). Steps 1-4 already
		// bound the listener to the supervisor's recorded child; the image
		// check would only add PID-reuse hardening on top of that.
		return nil
	}
	if !clients.IsMcphubBinary(image) {
		return fmt.Errorf("%w: loopback port %d owner PID %d image %q is not the mcphub binary", ErrMCPFrontPortNotSupervisorOwned, port, ownerPID, image)
	}
	return nil
}

// supervisorDaemonStateRunning is the durable persisted state a spawned daemon
// carries in supervisor-state.json. The only other durable value is the
// neutral "idle" (see SupervisorDaemonState's doc comment).
const supervisorDaemonStateRunning = "running"

// findBuiltinRouteDescriptor returns the intent row for the reserved built-in
// route daemon, or nil when the intent carries none.
//
// A row on the reserved task name whose Server/Daemon identity does NOT match
// the reserved pair is a FOREIGN row squatting on the name (the same case
// EnsureBuiltinRouteDaemon refuses with ErrBuiltinRouteTaskNameCollision) and
// is deliberately NOT treated as a match — a squatter must never be able to
// authorize a client rewrite by borrowing the reserved name.
func findBuiltinRouteDescriptor(intent *SupervisorIntentFile) *SupervisorDaemon {
	if intent == nil {
		return nil
	}
	for i := range intent.Daemons {
		d := &intent.Daemons[i]
		if canonicalIntentTaskKey(d.TaskName) != BuiltinRouteTaskName {
			continue
		}
		if d.Server != BuiltinRouteServer || d.Daemon != BuiltinRouteDaemonName {
			continue
		}
		return d
	}
	return nil
}

// findBuiltinRouteState returns the supervisor-state.json runtime row for the
// reserved built-in route task. Keys are compared through
// canonicalIntentTaskKey so a row written under either the leading-backslash
// or the bare spelling resolves identically.
func findBuiltinRouteState(state *SupervisorStateFile) (SupervisorDaemonState, bool) {
	if state == nil {
		return SupervisorDaemonState{}, false
	}
	for key, row := range state.Daemons {
		if canonicalIntentTaskKey(key) == BuiltinRouteTaskName {
			return row, true
		}
	}
	return SupervisorDaemonState{}, false
}
