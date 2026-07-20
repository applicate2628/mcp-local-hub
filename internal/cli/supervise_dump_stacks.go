package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// `mcphub supervise dump-stacks` — the operator-facing manual trigger for the
// goroutine-stack capture in supervise_stall_dump.go.
//
// WHY A COMMAND EXISTS AT ALL. The capture mechanism's manual arm is a
// sentinel file the running supervisor polls. Nothing in the product created
// that file, so firing the arm meant hand-creating a file inside a hidden
// state directory — and the operator on the affected host cannot work that
// way. A backstop that only a file-system-fluent user can reach is not a
// backstop. This command is the affordance; the sentinel stays the transport.
//
// WHY IT DOES NOT TALK TO THE SUPERVISOR. This is the whole design
// constraint of the feature: IPC accept is the path that stalls, so any
// trigger routed through IPC cannot get in exactly when it is needed. This
// command therefore performs ONE file write and exits. It does not dial the
// pipe, does not need the supervisor to service a connection, and works
// while the accept path is fully wedged. The supervisor picks the sentinel
// up on its next poll (default 2 s) from a goroutine that shares nothing
// with the accept loop.
//
// Consequence, stated plainly for the operator in the command output: this
// command reports that the REQUEST was filed, never that a dump was taken.
// Confirming the capture means looking at the dump directory or the
// supervisor-stall-dump-captured event. Claiming success here would be a
// lie whenever no supervisor is running.
func newSuperviseDumpStacksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dump-stacks",
		Short: "Ask the running supervisor to write a goroutine stack dump (for diagnosing IPC/status stalls)",
		Long: `Request a goroutine stack dump from the running supervisor.

Use this when the GUI status panel is flapping red/green, or ` + "`mcphub status`" + `
is timing out, while MCP itself keeps working. Those are symptoms of the
supervisor's IPC accept path stalling, and diagnosing it needs a snapshot of
what every supervisor goroutine is doing WHILE the stall is happening.

This command does not contact the supervisor over IPC — deliberately, because
IPC is the thing that stalls. It drops a small marker file that the supervisor
polls for on an independent timer, so it still works when the accept path is
completely wedged.

The dump is written under <state-dir>/supervisor-stalls/ and a
supervisor-stall-dump-captured entry is added to supervisor-events.log.
Dumps contain goroutine stack traces only — no secrets, no config values.

Run it WHILE the problem is visible; a dump taken after things recover shows
a healthy process.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSuperviseDumpStacks(cmd)
		},
	}
}

// fileStallDumpRequest writes the sentinel the supervisor's watcher consumes
// and returns its path. Split from the cobra layer so the operator verb's
// actual effect is testable against a temp state dir without the env-tagged
// state-path override — and so the test can prove the verb creates exactly
// the leaf runStallDumpSentinelWatcher polls for, rather than a lookalike.
func fileStallDumpRequest(stateDir string) (string, error) {
	sentinel := filepath.Join(stateDir, stallDumpSentinelLeaf)

	// The sentinel's CONTENT is never read — presence is the whole signal.
	// A timestamp is written anyway so an operator who does open the file, or
	// finds a stale one the supervisor never consumed, can tell when it was
	// requested. Routed through the shared hardened state-file helper like
	// every other file in the state dir (owner-only DACL, atomic rename).
	body := []byte("mcphub supervise dump-stacks requested at " +
		time.Now().UTC().Format(time.RFC3339Nano) + "\n")
	if err := api.WriteStateFileBytesAtomic(sentinel, body); err != nil {
		return "", fmt.Errorf("write dump-stacks request %s: %w", sentinel, err)
	}
	return sentinel, nil
}

func runSuperviseDumpStacks(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("resolve state dir: %w", err)
	}
	sentinel, err := fileStallDumpRequest(stateDir)
	if err != nil {
		return err
	}

	dumpDir := filepath.Join(stateDir, stallDumpDirLeaf)
	fmt.Fprintf(out, "Stack-dump request filed: %s\n", sentinel)
	fmt.Fprintf(out, "\nIf a supervisor is running it will pick this up within a few seconds and\n")
	fmt.Fprintf(out, "write the dump to:\n  %s\n", dumpDir)

	// Honest reporting: a filed request is not a completed capture. Say so,
	// and give the operator the one check that settles it. Best-effort — a
	// stat failure here must not fail the command, since the request IS
	// filed either way.
	if _, statErr := os.Stat(sentinel); statErr == nil {
		fmt.Fprintf(out, "\nThis command only FILES the request; it does not wait for the dump.\n")
		fmt.Fprintf(out, "If the file above is still there after ~10 seconds, no supervisor is\n")
		fmt.Fprintf(out, "running (or it cannot delete the file) and no dump will be written.\n")
	}
	return nil
}
