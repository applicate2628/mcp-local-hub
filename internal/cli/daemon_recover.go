package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/process"
)

// Test seams over the production surfaces `daemon recover` composes: the state
// dir, the intent/state readers, the port-owner probe, and the force-respawn
// IPC. Tests swap these to drive the flow without a live supervisor or real
// kills. squatterTerminatePIDFn (shared with the sweep) is the kill primitive.
var (
	recoverStateDirFn   = api.DaemonStateDir
	recoverReadIntentFn = api.ReadSupervisorIntent
	recoverReadStateFn  = api.ReadSupervisorState
	recoverPortOwnerFn  = api.LoopbackPortOwnerPID
	recoverSelfPIDFn    = os.Getpid
	recoverRespawnFn    = func(ctx context.Context, task string, force bool) (api.RespawnResult, error) {
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
//	2 — unknown task (not in supervisor-intent.json) OR intent unreadable
//	3 — refused: the port owner is a foreign / unverifiable process (no kill),
//	    OR the operator declined the confirmation prompt
//	4 — force respawn returned a non-success supervisor code
//	5 — supervisor unreachable (no IPC owner / dial failed)
const (
	daemonRecoverExitUnknownTask  = 2
	daemonRecoverExitRefused      = 3
	daemonRecoverExitRespawnError = 4
	daemonRecoverExitUnreachable  = 5
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
	errOut := cmd.ErrOrStderr()

	stateDir, err := recoverStateDirFn()
	if err != nil {
		fmt.Fprintf(errOut, "error: resolve state dir: %v\n", err)
		return forceExit(daemonRecoverExitUnknownTask)
	}

	norm := daemon_env_overlay.NormalizeOverlayKey(taskArg)
	intent, err := recoverReadIntentFn(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil || intent == nil {
		fmt.Fprintf(errOut, "error: read supervisor-intent.json: %v\n", err)
		return forceExit(daemonRecoverExitUnknownTask)
	}
	var desc *api.SupervisorDaemon
	for i := range intent.Daemons {
		if daemon_env_overlay.NormalizeOverlayKey(intent.Daemons[i].TaskName) == norm {
			desc = &intent.Daemons[i]
			break
		}
	}
	if desc == nil {
		fmt.Fprintf(errOut, "error: task %q not found in supervisor-intent.json\n", taskArg)
		if known := knownRecoverTaskNames(intent); len(known) > 0 {
			fmt.Fprintf(errOut, "known tasks:\n")
			for _, k := range known {
				fmt.Fprintf(errOut, "  %s\n", k)
			}
		}
		return forceExit(daemonRecoverExitUnknownTask)
	}

	// Port disposition: identity-gate and reap a verified-own squatter before
	// the respawn. Skip when the port is unbound, unprobeable, or owned by this
	// task's own live child.
	if reapErr := recoverReapPortSquatter(cmd, *desc, norm, stateDir, yes); reapErr != nil {
		return reapErr
	}

	// Force respawn through the supervisor (never a direct spawn — ownership
	// stays with the controller). force=true bypasses the quarantine refusal and
	// resets the failure window.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	res, respErr := recoverRespawnFn(ctx, norm, true)
	if respErr != nil {
		fmt.Fprintf(errOut, "error: force respawn: %v\n", respErr)
		return forceExit(daemonRecoverExitUnreachable)
	}
	if res.Code == "SUPERVISOR_UNAVAILABLE" {
		fmt.Fprintf(errOut, "error: supervisor not reachable: %s\n", res.Message)
		fmt.Fprintf(errOut, "hint: start it with `mcphub supervise` (or ensure the autostart task is enabled), then retry.\n")
		return forceExit(daemonRecoverExitUnreachable)
	}
	if !res.Success {
		fmt.Fprintf(errOut, "error: force respawn refused [%s]: %s\n", res.Code, res.Message)
		return forceExit(daemonRecoverExitRespawnError)
	}
	fmt.Fprintf(out, "recovered %s: forced respawn accepted by the supervisor.\n", norm)
	return nil
}

// recoverReapPortSquatter classifies the current owner of the daemon's port and
// reaps ONLY a verified-own squatter. Returns nil to continue to the respawn
// (nothing to reap, or reap succeeded), or a forceExit error to abort (foreign/
// unverifiable owner refused, or operator declined). Never kills on ambiguity.
func recoverReapPortSquatter(cmd *cobra.Command, desc api.SupervisorDaemon, norm, stateDir string, yes bool) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	if desc.Port <= 0 {
		return nil // no port to fight over
	}
	ownerPID, ok, err := recoverPortOwnerFn(desc.Port)
	if err != nil {
		fmt.Fprintf(out, "note: could not determine the owner of port %d (%v); proceeding to force respawn without a reap.\n", desc.Port, err)
		return nil
	}
	if !ok || ownerPID <= 0 {
		return nil // port unbound — nothing squatting it
	}

	tracked := recoverTrackedEntries(stateDir)
	if entry, ok := tracked[canonicalSupervisorTaskName(norm)]; ok && ownerPID == entry.CurrentPID {
		fmt.Fprintf(out, "note: port %d is owned by this task's own tracked child (pid %d); skipping reap.\n", desc.Port, ownerPID)
		return nil
	}

	verdict, id := classifyPortSquatter(desc, ownerPID, recoverSelfPIDFn(), tracked)
	switch verdict {
	case squatterForeign, squatterUnverified:
		fmt.Fprintf(errOut, "refused: port %d is held by pid %d, which is NOT a verified disowned child of %s (%s).\n", desc.Port, ownerPID, norm, verdict)
		if id.ExecutablePath != "" {
			fmt.Fprintf(errOut, "  executable: %s\n", boundSquatterField(id.ExecutablePath))
		}
		if id.CommandLine != "" {
			fmt.Fprintf(errOut, "  command line: %s\n", boundSquatterField(id.CommandLine))
		}
		fmt.Fprintf(errOut, "  This tool will not kill a foreign or unverifiable process. Investigate and stop it yourself, then retry.\n")
		return forceExit(daemonRecoverExitRefused)
	case squatterOwnTask:
		fmt.Fprintf(out, "port %d is squatted by a verified-own disowned child of %s:\n", desc.Port, norm)
		fmt.Fprintf(out, "  pid:        %d\n", ownerPID)
		fmt.Fprintf(out, "  executable: %s\n", boundSquatterField(id.ExecutablePath))
		fmt.Fprintf(out, "  command line: %s\n", boundSquatterField(id.CommandLine))
		if started := squatterStartedAt(id); started != "" {
			fmt.Fprintf(out, "  started_at: %s\n", started)
		}
		if !yes && !confirmRecoverReap(cmd) {
			fmt.Fprintf(out, "aborted: no process was killed and no respawn was forced.\n")
			return forceExit(daemonRecoverExitRefused)
		}
		proof := squatterKillProof(id)
		if killErr := squatterTerminatePIDFn(proof); killErr != nil && !errors.Is(killErr, process.ErrProcessAlreadyExited) {
			fmt.Fprintf(errOut, "error: reap pid %d failed: %v\n", ownerPID, killErr)
			return forceExit(daemonRecoverExitRefused)
		}
		fmt.Fprintf(out, "reaped pid %d.\n", ownerPID)
		waitRecoverPortFree(cmd, desc.Port)
		return nil
	}
	return nil
}

// recoverTrackedEntries reads supervisor-state.json into the minimal
// per-task PID map classifyPortSquatter needs for its own-child + tracked-
// sibling gates. A missing/unreadable state file yields an empty map (the
// classifier still applies its exe + argv gates).
func recoverTrackedEntries(stateDir string) map[string]DaemonRuntimeEntry {
	out := map[string]DaemonRuntimeEntry{}
	st, err := recoverReadStateFn(filepath.Join(stateDir, "supervisor-state.json"))
	if err != nil || st == nil {
		return out
	}
	for task, ds := range st.Daemons {
		out[canonicalSupervisorTaskName(task)] = DaemonRuntimeEntry{
			CurrentPID: ds.CurrentPID,
			OrphanPID:  ds.OrphanPID,
		}
	}
	return out
}

func knownRecoverTaskNames(intent *api.SupervisorIntentFile) []string {
	names := make([]string, 0, len(intent.Daemons))
	for _, d := range intent.Daemons {
		names = append(names, canonicalSupervisorTaskName(d.TaskName))
	}
	sort.Strings(names)
	return names
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

// waitRecoverPortFree polls until the reaped squatter's port is unbound
// (bounded), so the subsequent force respawn does not race a still-closing
// socket. Best-effort — a still-bound port is reported but not fatal.
func waitRecoverPortFree(cmd *cobra.Command, port int) {
	deadline := time.Now().Add(recoverPortFreeTimeout)
	for time.Now().Before(deadline) {
		_, ok, err := recoverPortOwnerFn(port)
		if err == nil && !ok {
			return
		}
		time.Sleep(recoverPortFreePollInterval)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "note: port %d still appears bound after %s; forcing the respawn anyway.\n", port, recoverPortFreeTimeout)
}
