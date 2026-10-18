package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newCleanupAggressiveCmdReal builds the explicit-scope aggressive command.
// On Windows the API refuses both preview and apply because creator retirement
// authority is not established. Confirmation and class opt-ins cannot enable it.
func newCleanupAggressiveCmdReal() *cobra.Command {
	var client string
	var rootPID int
	var minAge int64
	var token string
	var includeClasses []string
	c := &cobra.Command{
		Use:   "aggressive",
		Short: "Aggressive cleanup is unavailable on Windows (creator retirement authority required)",
		Long: `Aggressive cleanup is unavailable on Windows: creator retirement authority
is not established, so preview and apply are disabled. A confirmation token,
process identity, age or class opt-in cannot enable this operation.

REQUIRES exactly one scope:
  --client <name>     client launcher (claude / codex / gemini / qwen /
                      cursor / code / cascade / antigravity)
  --root-pid <pid>    explicit process id

Scope and confirmation flags retain their syntax. Valid non-Windows calls
retain the existing empty-result behavior.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			killMode := strings.TrimSpace(token) != ""

			opts := api.CleanupOpts{
				Aggressive:     true,
				Client:         client,
				RootPID:        rootPID,
				MinAgeSec:      minAge,
				IncludeClasses: includeClasses,
				DryRun:         true, // always resolve candidates dry first
			}

			a := api.NewAPI()
			candidates, err := a.AggressiveCleanup(opts)
			if err != nil {
				return err
			}

			// Surface a stderr warning for every dangerous class the
			// operator opted back in (spec H.1). ErrOrStderr, not
			// OutOrStderr — the latter returns stdout when SetOut is set,
			// which would mis-route the warning off the error stream.
			for _, inc := range includeClasses {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: --include-class %s opts a dangerous class back into the kill set (operator terminals / Playwright sessions may be terminated)\n",
					strings.TrimSpace(inc))
			}

			if len(candidates) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No aggressive candidates found.")
				return nil
			}

			computedToken := api.AggressiveConfirmToken(candidates)

			if !killMode {
				printAggressiveCandidates(cmd, candidates)
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nConfirmation token: %s\n"+
						"Re-run with --confirm-aggressive-token %s to kill the %d candidate(s) above.\n"+
						"(The token is bound to this exact candidate set; if the set changes, re-run for a fresh token.)\n",
					computedToken, computedToken, len(candidates))
				return nil
			}

			// Kill mode: the supplied token must match the freshly
			// recomputed token (candidate set unchanged since preview).
			if strings.TrimSpace(token) != computedToken {
				printAggressiveCandidates(cmd, candidates)
				return fmt.Errorf(
					"confirm token mismatch: candidate set changed since the preview (supplied %q, current %q); re-run without --confirm-aggressive-token to get a fresh token",
					strings.TrimSpace(token), computedToken)
			}

			// Token matches — execute the kill, BOUND to the validated
			// {PID, StartedAt} identities so a process spawned since the
			// preview, or a PID recycled onto a different process in the
			// validate→kill window, cannot be killed unacknowledged (the CLI
			// aggressive kill was previously UNBOUND; bug 2026-07-08).
			opts.DryRun = false
			opts.Expect = api.IdentitiesOf(candidates)
			killed, err := a.AggressiveCleanup(opts)
			if err != nil {
				return err
			}
			killedN, skippedN := 0, 0
			for _, o := range killed {
				if o.KillErr != "" {
					skippedN++
				} else {
					killedN++
				}
			}
			printAggressiveCandidates(cmd, killed)
			fmt.Fprintf(cmd.OutOrStdout(), "\nkilled: %d · skipped: %d · total: %d\n",
				killedN, skippedN, len(killed))
			for _, o := range killed {
				if o.KillErr != "" {
					fmt.Fprintf(cmd.OutOrStderr(), "  x PID %d: %s\n", o.PID, o.KillErr)
				}
			}

			emitAggressiveCleanupAuditEvent(cmd, opts, len(killed), killedN, includeClasses, computedToken)
			return nil
		},
	}
	c.Flags().StringVar(&client, "client", "",
		"scope to descendants of this live client launcher (claude / codex / gemini / qwen / cursor / code / cascade / antigravity)")
	c.Flags().IntVar(&rootPID, "root-pid", 0, "scope to descendants of this process id")
	c.Flags().Int64Var(&minAge, "min-age-sec", 60, "ignore processes younger than this (seconds)")
	c.Flags().StringVar(&token, "confirm-aggressive-token", "",
		"confirmation token syntax retained; cannot enable unavailable cleanup")
	c.Flags().StringArrayVar(&includeClasses, "include-class", nil,
		"class opt-in syntax retained; cannot enable unavailable cleanup; repeatable")
	return c
}

// printAggressiveCandidates renders one line per candidate with the
// per-PID match-source (spec H.1: "which ancestor walked the gate, why
// included" must appear in output). Only the redacted basename
// (CmdlineDisplay) is printed — never the full cmdline — so workspace
// paths / tokens in args do not reach the terminal.
func printAggressiveCandidates(cmd *cobra.Command, candidates []api.OrphanProcess) {
	totalRAM := uint64(0)
	for _, o := range candidates {
		totalRAM += o.RAMBytes
		fmt.Fprintf(cmd.OutOrStdout(),
			"  PID %-6d  exe=%-24s  RAM=%-6.0f MB  age=%-6ds  match=%s\n",
			o.PID, o.CmdlineDisplay, float64(o.RAMBytes)/(1024*1024), o.AgeSec, o.MatchSource)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\n%d candidate(s) · %.0f MB total\n",
		len(candidates), float64(totalRAM)/(1024*1024))
}

// The confirmation-token derivation lives in the api package
// (api.AggressiveConfirmToken) as the SINGLE OWNER of the preview-token
// contract shared by the CLI and the GUI apply path. The local copy was
// removed so the two surfaces can never drift.

// emitAggressiveCleanupAuditEvent writes the `aggressive-cleanup-executed`
// info event to supervisor-events.log (best-effort; a nil log or emit
// failure is silently non-fatal — mirrors the migrate-serena audit
// helper). It uses TryEmit so a live supervisor holding the flock never
// blocks the CLI. The body carries the operator scope + outcome
// counters; no full cmdlines (wire-safety).
func emitAggressiveCleanupAuditEvent(_ *cobra.Command, opts api.CleanupOpts, candidateCount, killedCount int, includeClasses []string, token string) {
	stateDir, err := stateDirFunc()
	if err != nil {
		return
	}
	events, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		return
	}
	scope := ""
	if strings.TrimSpace(opts.Client) != "" {
		scope = "client=" + strings.TrimSpace(opts.Client)
	} else {
		scope = "root-pid=" + strconv.Itoa(opts.RootPID)
	}
	_ = events.TryEmit(api.SupervisorEvent{
		SchemaVersion: api.SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      api.SupervisorEventSeverityInfo,
		Source:        api.SupervisorEventSourceLifecycle,
		Event:         "aggressive-cleanup-executed",
		Body: map[string]any{
			"scope":           scope,
			"candidate_count": candidateCount,
			"killed_count":    killedCount,
			"skipped_classes": aggressiveSkippedClasses(includeClasses),
			"token_used":      token,
		},
	})
}

// aggressiveSkippedClasses returns the dangerous classes that REMAINED
// excluded (deny-list minus operator opt-ins) for the audit body.
func aggressiveSkippedClasses(includeClasses []string) []string {
	included := map[string]bool{}
	for _, inc := range includeClasses {
		included[strings.ToLower(strings.TrimSpace(inc))] = true
	}
	var out []string
	for _, c := range api.AggressiveDenyClasses() {
		if !included[c] {
			out = append(out, c)
		}
	}
	return out
}
