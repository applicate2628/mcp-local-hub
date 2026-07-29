package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
	"mcp-local-hub/internal/process"
)

// Test seams over the production surfaces `daemon recover` composes: the state
// dir, the intent/state readers, the port-owner and supervisor probes, and the
// force-respawn IPC. Tests swap these to drive the flow without a live
// supervisor or real kills. recoverHoldProcessFn supplies the held-generation
// kill primitive; recovery does not use the automatic sweep's
// squatterTerminatePIDFn seam.
var (
	recoverStateDirFn        = api.DaemonStateDir
	recoverReadIntentFn      = api.ReadSupervisorIntent
	recoverReadStateFn       = api.ReadSupervisorState
	recoverPortOwnerFn       = api.LoopbackPortOwnerPIDContext
	recoverSelfPIDFn         = os.Getpid
	recoverHoldProcessFn     = process.HoldPIDForTermination
	recoverProbeSupervisorFn = daemonrecovery.ProductionDependencies().ProbeSupervisor
	recoverRespawnFn         = func(ctx context.Context, task string, force bool) (api.RespawnResult, error) {
		return api.DialSupervisorIPCRespawn(ctx, task, force, 15000)
	}
	// recoverPortFreePollInterval / recoverPortFreeTimeout bound the post-kill
	// wait for the reaped squatter's port to become unbound. Package-level so a
	// test can shrink them.
	recoverPortFreePollInterval = 250 * time.Millisecond
	recoverPortFreeTimeout      = 10 * time.Second
)

// daemon-recover exit codes (command-scoped; propagated via forceExit →
// cmd/mcphub/main.go's ExitCode() branch):
//
//	2 — invalid/unknown task, or recovery-state read failure: state-directory
//	    resolution; unreadable/nil supervisor-intent.json or supervisor-state.json;
//	    or a missing target state row (the respawn-setup special case exits 5)
//	3 — FailureConfirmationRequired or refused: the operator declined/missed
//	    confirmation, the port owner is foreign / unverifiable (no kill), OR the
//	    final owner recheck timed out before any kill
//	4 — force respawn returned a non-success supervisor code, OR recovery reached
//	    an unclassified failure kind
//	5 — pre-kill supervisor probe or respawn call unavailable (no IPC owner / dial
//	    or local setup failed),
//	    or the request was canceled before completion
//	6 — refused before termination because the configured recovery budget could
//	    not preserve the mandatory detached respawn reservation
//	7 — process termination committed and respawn was attempted, but the audit
//	    record or its durable handoff could not be preserved
const (
	daemonRecoverExitUnknownTask        = 2
	daemonRecoverExitRefused            = 3
	daemonRecoverExitRespawnError       = 4
	daemonRecoverExitUnreachable        = 5
	daemonRecoverExitBudgetInsufficient = 6
	daemonRecoverExitAuditDurability    = 7
)

// newDaemonRecoverCmd builds `mcphub daemon recover <task> [--yes]`: reap a
// verified-own port squatter that is masking a stuck/quarantined daemon, then
// force a respawn through the supervisor. It composes the shared P2a classifier
// (identity-gated reap) with the existing force-respawn IPC — no new kill
// authority beyond decision D-A, no new IPC verb.
func newDaemonRecoverCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "recover <task-name>",
		Short: "Reap a verified-own port squatter for a stuck/quarantined daemon and force a respawn",
		Long: `Recover a daemon whose port is squatted by a forgotten own-child (the
supervisor "lost-child" class). recover:

  1. Resolves the daemon descriptor from supervisor-intent.json.
  2. Checks who owns the daemon's intended TCP port.
  3. If a VERIFIED-OWN squatter holds it (our binary, our argv for THIS task,
     start-time-proven), reaps it (with a confirmation prompt unless --yes).
     A foreign or unverifiable owner is REFUSED — never killed.
  4. Forces a respawn through the supervisor (force=true), which also resets
     the quarantine window.

Windows only in v1: identity verification needs a start-time-proof process
handle. On other platforms a bound foreign owner is refused (no kill).`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonRecover(cmd, args[0], yes)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt before killing a verified-own port squatter")
	return c
}

func runDaemonRecover(cmd *cobra.Command, taskArg string, yes bool) error {
	out := cmd.OutOrStdout()
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	deps := daemonrecovery.ProductionDependencies()
	deps.StateDir = recoverStateDirFn
	deps.ReadIntent = recoverReadIntentFn
	deps.ReadState = recoverReadStateFn
	deps.PortOwner = recoverPortOwnerFn
	deps.SelfPID = recoverSelfPIDFn
	deps.LookupIdentity = squatterLookupIdentityFn
	deps.ExecutableMatches = squatterExeMatchesFn
	deps.HoldProcess = recoverHoldProcessFn
	deps.ProbeSupervisor = recoverProbeSupervisorFn
	deps.Respawn = recoverRespawnFn
	deps.PortPollInterval = recoverPortFreePollInterval
	deps.PortWaitTimeout = recoverPortFreeTimeout

	result, err := daemonrecovery.ExecuteWithDependencies(ctx, taskArg, daemonrecovery.Options{
		Confirmed: yes,
		ConfirmReap: func(daemonrecovery.ReapCandidate) bool {
			return confirmRecoverReap(cmd)
		},
		Notify: func(notification daemonrecovery.Notification) {
			printRecoverNotification(cmd, notification)
		},
	}, deps)
	if err != nil {
		return printRecoverError(cmd, taskArg, err)
	}
	fmt.Fprintf(out, "recovered %s: forced respawn accepted by the supervisor.\n", result.TaskName)
	printRecoverAuditHandoffWarning(cmd, result)
	return nil
}

// printRecoverAuditHandoffWarning reports an unreleased cross-process event-log
// flock WITHOUT changing the exit code. The recovery succeeded and the audit row
// is durable; what could not be confirmed is the RELEASE of the lock, which this
// process may still hold.
//
// Both non-durable values are reported, because for a ONE-SHOT CLI they have the
// same remedy: this process is about to exit, and exiting releases the lock
// either way. The long-lived GUI is where they diverge — there a pending worker
// clears itself while a stranded flock never does — so the GUI reads the two
// values separately (internal/gui/frontend/src/screens/Dashboard.tsx). The
// wording here still distinguishes them so the stderr line is not misleading if
// an operator pastes it into a bug report.
func printRecoverAuditHandoffWarning(cmd *cobra.Command, result daemonrecovery.Result) {
	var detail string
	switch result.AuditHandoff {
	case daemonrecovery.AuditHandoffReleasePending:
		detail = "a background writer in this process still holds it"
	case daemonrecovery.AuditHandoffReleaseUnconfirmed:
		detail = "releasing it FAILED, so this process holds it until it exits"
	default:
		return
	}
	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "warning: the recovery audit row is durable, but the supervisor-events.log cross-process lock was not confirmed released: %s.\n", detail)
	fmt.Fprintln(errOut, "  While it is held, event-log writes from the supervisor and `mcphub install` are blocked.")
	fmt.Fprintln(errOut, "  Do NOT re-run recover: the termination and the respawn already committed. The lock is released when this process exits.")
}

// confirmRecoverReap prompts for an interactive y/N confirmation. A non-tty /
// EOF / non-affirmative answer returns false (do not kill) — fail safe.
func confirmRecoverReap(cmd *cobra.Command) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "Kill this process and force a respawn? [y/N]: ")
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func printRecoverNotification(cmd *cobra.Command, notification daemonrecovery.Notification) {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	switch notification.Kind {
	case daemonrecovery.NotificationPortUnresolvable:
		fmt.Fprintf(errOut, "warning: descriptor for %s has no resolvable port (its server manifest is missing/renamed, or it is not a manifest-backed daemon); the port-squatter check was SKIPPED (a lost-child squatter, if present, was NOT reaped). Verify the server is still installed (`mcphub install`), or remove this stale supervisor-intent row.\n", notification.TaskName)
	case daemonrecovery.NotificationProbeUnavailable:
		fmt.Fprintf(out, "note: could not determine the owner of port %d (%v); proceeding to force respawn without a reap.\n", notification.Port, notification.Cause)
	case daemonrecovery.NotificationTrackedChild:
		fmt.Fprintf(out, "note: port %d is owned by this task's own tracked child (pid %d); skipping reap.\n", notification.Port, notification.PID)
	case daemonrecovery.NotificationReapCandidate:
		if notification.Candidate != nil {
			printRecoverCandidate(out, *notification.Candidate)
		}
	case daemonrecovery.NotificationReaped:
		fmt.Fprintf(out, "reaped pid %d.\n", notification.PID)
	case daemonrecovery.NotificationTerminationUnconfirmed:
		fmt.Fprintf(errOut, "warning: termination was committed for pid %d, but exit was not confirmed (%v); the mandatory force-respawn request was still dispatched.\n", notification.PID, notification.Cause)
	case daemonrecovery.NotificationAlreadyExited:
		fmt.Fprintf(out, "pid %d had already exited before the reap; proceeding to force respawn.\n", notification.PID)
	case daemonrecovery.NotificationPortWaitTimeout:
		if notification.Duration <= 0 {
			fmt.Fprintf(errOut, "note: port %d release wait was skipped to preserve the mandatory respawn reservation; the post-termination port state was not observed, and the respawn is being forced anyway.\n", notification.Port)
		} else {
			fmt.Fprintf(errOut, "note: port %d still appears bound after %s; forcing the respawn anyway.\n", notification.Port, notification.Duration)
		}
	}
}

func printRecoverCandidate(out interface{ Write([]byte) (int, error) }, candidate daemonrecovery.ReapCandidate) {
	identity := candidate.Identity
	fmt.Fprintf(out, "port %d is squatted by a verified-own disowned child of %s:\n", candidate.Port, candidate.TaskName)
	fmt.Fprintf(out, "  pid:        %d\n", candidate.PID)
	fmt.Fprintf(out, "  executable: %s\n", boundSquatterField(stripTerminalControls(identity.ExecutablePath)))
	fmt.Fprintf(out, "  command line: %s\n", boundSquatterField(stripTerminalControls(identity.CommandLine)))
	if started := squatterStartedAt(identity); started != "" {
		fmt.Fprintf(out, "  started_at: %s\n", started)
	}
}

func printRecoverError(cmd *cobra.Command, taskArg string, err error) error {
	errOut := cmd.ErrOrStderr()
	var operationErr *daemonrecovery.OperationError
	if !errors.As(err, &operationErr) {
		fmt.Fprintf(errOut, "error: daemon recovery: %v\n", err)
		return forceExit(daemonRecoverExitRespawnError)
	}
	if operationErr.Kind == daemonrecovery.FailureStateRead && errors.Is(operationErr.Cause, api.ErrRespawnSetupFailure) {
		fmt.Fprintf(errOut, "error: force respawn call could not be prepared: %v\n", operationErr)
		return forceExit(daemonRecoverExitUnreachable)
	}
	switch operationErr.Kind {
	case daemonrecovery.FailureAuditDurability:
		if operationErr.Respawn.Success {
			fmt.Fprintln(errOut, "error: process termination was committed and forced respawn was accepted, but the recovery audit record or durable handoff could not be preserved.")
		} else {
			fmt.Fprintln(errOut, "error: process termination was committed; forced respawn was attempted but not accepted, and the recovery audit record or durable handoff could not be preserved.")
			if operationErr.Respawn.Code != "" || operationErr.Respawn.Message != "" {
				fmt.Fprintf(errOut, "respawn result [%s]: %s\n", operationErr.Respawn.Code, operationErr.Respawn.Message)
			}
		}
		if operationErr.Cause != nil {
			fmt.Fprintf(errOut, "details: %v\n", operationErr.Cause)
		}
		return forceExit(daemonRecoverExitAuditDurability)
	case daemonrecovery.FailureInvalidArgs, daemonrecovery.FailureStateRead, daemonrecovery.FailureUnknownTask:
		if operationErr.Kind == daemonrecovery.FailureUnknownTask {
			fmt.Fprintf(errOut, "error: task %q not found in supervisor-intent.json\n", taskArg)
			if len(operationErr.KnownTasks) > 0 {
				fmt.Fprintln(errOut, "known tasks:")
				for _, taskName := range operationErr.KnownTasks {
					fmt.Fprintf(errOut, "  %s\n", taskName)
				}
			}
		} else {
			fmt.Fprintf(errOut, "error: recover state: %v\n", operationErr)
		}
		return forceExit(daemonRecoverExitUnknownTask)
	case daemonrecovery.FailureConfirmationRequired, daemonrecovery.FailureRefusedPortOwner:
		candidate := operationErr.Candidate
		if candidate != nil && errors.Is(operationErr.Cause, daemonrecovery.ErrSupervisorTrackedChild) {
			fmt.Fprintf(errOut, "refused: pid %d is now a supervisor-tracked child of %s; no process was killed and no respawn was forced.\n", candidate.PID, candidate.TaskName)
		} else if candidate != nil && candidate.Verdict != daemonrecovery.VerdictOwnTask {
			identity := candidate.Identity
			fmt.Fprintf(errOut, "refused: port %d is held by pid %d, which is NOT a verified disowned child of %s (%s).\n", candidate.Port, candidate.PID, candidate.TaskName, candidate.Verdict)
			if identity.ExecutablePath != "" {
				fmt.Fprintf(errOut, "  executable: %s\n", boundSquatterField(stripTerminalControls(identity.ExecutablePath)))
			}
			if identity.CommandLine != "" {
				fmt.Fprintf(errOut, "  command line: %s\n", boundSquatterField(stripTerminalControls(identity.CommandLine)))
			}
			fmt.Fprintln(errOut, "  This tool will not kill a foreign or unverifiable process. Investigate and stop it yourself, then retry.")
		} else if operationErr.Cause != nil && candidate != nil {
			fmt.Fprintf(errOut, "error: reap pid %d failed: %v\n", candidate.PID, operationErr.Cause)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "aborted: no process was killed and no respawn was forced.")
		}
		return forceExit(daemonRecoverExitRefused)
	case daemonrecovery.FailureSupervisorUnavailable:
		message := operationErr.Error()
		if operationErr.Respawn.Message != "" {
			message = operationErr.Respawn.Message
		}
		fmt.Fprintf(errOut, "error: supervisor not reachable: %s\n", message)
		fmt.Fprintln(errOut, "hint: start it with `mcphub supervise` (or ensure the autostart task is enabled), then retry.")
		return forceExit(daemonRecoverExitUnreachable)
	case daemonrecovery.FailureRequestCanceled:
		fmt.Fprintln(errOut, "error: recovery was canceled before any process termination was committed.")
		return forceExit(daemonRecoverExitUnreachable)
	case daemonrecovery.FailureBoundaryProbeTimeout:
		fmt.Fprintln(errOut, "refused: recovery timed out while rechecking the port owner; no process was killed and no respawn was forced.")
		return forceExit(daemonRecoverExitRefused)
	case daemonrecovery.FailureRespawnBudgetInsufficient:
		fmt.Fprintln(errOut, "refused: recovery could not reserve the mandatory post-termination respawn time; no process was killed and no respawn was forced.")
		fmt.Fprintln(errOut, "hint: retry when the local system is less busy.")
		return forceExit(daemonRecoverExitBudgetInsufficient)
	case daemonrecovery.FailureRespawnFailed:
		fmt.Fprintf(errOut, "error: force respawn refused [%s]: %s\n", operationErr.Respawn.Code, operationErr.Respawn.Message)
		return forceExit(daemonRecoverExitRespawnError)
	default:
		fmt.Fprintln(errOut, "error: daemon recovery failed: unclassified failure.")
		return forceExit(daemonRecoverExitRespawnError)
	}
}
