package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newCleanupAggressiveCmdReal builds `mcphub cleanup aggressive` — the
// Phase H.1 operator-confirmed override that kills the live-rooted
// MCP-stdio processes the default safe sweep (`mcphub cleanup`)
// correctly refuses to touch.
//
// Motivation (serena-unified plan §H, 2026-05-20 operational evidence):
// a single live client (e.g. codex) spawns N internal subagents that
// each spawn their own stdio MCP children which are never reaped on
// subagent finish. They are NOT orphans (the ancestor — the live
// client — is alive), so the default sweep spares them; they
// accumulate (the 1×N×M fleet-multiplier). This subcommand lets the
// operator reclaim that accumulation under one explicit scope.
//
// Safety contract (spec H.1):
//   - REQUIRES exactly one scope: --client <name> OR --root-pid <pid>.
//   - Dry-run preview is MANDATORY: the first invocation (no token)
//     prints the candidate list with per-PID match-source and a
//     confirmation token, and kills NOTHING.
//   - The kill invocation passes --confirm-aggressive-token <token>;
//     the token is recomputed from a FRESH snapshot and must still
//     match (the candidate set is bound to the token). A stale token
//     (set changed since preview) is rejected and a new preview prints.
//   - Dangerous classes (cmd/conhost/pwsh/powershell/chrome) are
//     excluded by default; --include-class <name> opts one back in with
//     a stderr warning.
//   - mcphub.exe daemon descendants are ALWAYS spared (no bypass).
func newCleanupAggressiveCmdReal() *cobra.Command {
	var client string
	var rootPID int
	var minAge int64
	var token string
	var includeClasses []string
	c := &cobra.Command{
		Use:   "aggressive",
		Short: "Kill live-rooted MCP-stdio processes under one explicit scope (operator-confirmed override)",
		Long: `Kill the live-rooted MCP-stdio processes that the default safe sweep
(mcphub cleanup) correctly refuses to touch — the per-subagent stdio
fan-out where a live client spawns N subagents that each leak their own
stdio MCP children.

REQUIRES exactly one scope:
  --client <name>     descendants of a live client launcher
                      (claude / codex / gemini / qwen / cursor / code /
                      cascade / antigravity)
  --root-pid <pid>    descendants of an explicit process id

Mandatory two-step confirmation (no single-shot kill):
  1. Preview (no token): prints the candidate list with per-PID
     match-source and a confirmation token. Kills NOTHING.
  2. Kill: re-run with --confirm-aggressive-token <token>. The token is
     recomputed from a fresh snapshot and must still match the previewed
     candidate set; if the set changed, the token is rejected and a new
     preview prints.

Default-excluded dangerous classes (operator terminals + Playwright):
  cmd · conhost · pwsh · powershell · chrome
Opt one back in with --include-class <name> (prints a stderr warning).

Always spared (no aggressive bypass): mcphub.exe daemon descendants and
mcphub's own binaries.

Examples:
  mcphub cleanup aggressive --client codex            # preview + token
  mcphub cleanup aggressive --client codex --confirm-aggressive-token <token>
  mcphub cleanup aggressive --root-pid 12345
  mcphub cleanup aggressive --client codex --include-class chrome   # also sweep Playwright chrome

See also: cleanup (safe sweep), stop, status.`,
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

			computedToken := aggressiveConfirmToken(candidates)

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

			// Token matches — execute the kill.
			opts.DryRun = false
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
		"confirmation token from a prior preview run; required to actually kill")
	c.Flags().StringArrayVar(&includeClasses, "include-class", nil,
		"opt a default-excluded dangerous class (cmd/conhost/pwsh/powershell/chrome) back into the kill set; repeatable")
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

// aggressiveConfirmToken derives a deterministic confirmation token
// bound to the candidate snapshot. The token is the first 16 hex chars
// of SHA-256 over the SORTED (PID, exe-basename, match-source) tuples.
// Two preview runs over the same candidate set produce the same token;
// any add/remove/identity-change produces a different token, so a stale
// --confirm-aggressive-token is rejected by the recompute-and-compare
// in the kill path.
func aggressiveConfirmToken(candidates []api.OrphanProcess) string {
	lines := make([]string, 0, len(candidates))
	for _, o := range candidates {
		lines = append(lines, fmt.Sprintf("%d|%s|%s", o.PID, o.CmdlineDisplay, o.MatchSource))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

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
