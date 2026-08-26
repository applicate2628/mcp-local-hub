// internal/cli/install_reconcile_mcp_front_review_test.go
//
// Pre-submission adversarial-review coverage for
// `mcphub install --reconcile-mcp-front[/--rollback]`.
//
// Findings 1, 2, 5 and 7 are all the same question — WHEN IS THE RECOVERY
// RECORD TRUSTWORTHY? — so they are pinned here as four properties of one
// lifecycle rather than four unrelated regressions:
//
//	finding 1 [P1] rolling backup retention deleted the file the record pointed at
//	finding 2 [P1] client configs were mutated before their recovery row was durable
//	finding 5 [P2] a record missing a whole recovery section could still be consumed
//	finding 7 [P2] a rollback reported success after failing to retire the record
//
// Finding 3 [P1] (the forward cutover did not require the port to be
// supervisor-owned) is pinned here too, because "refuses to write any client
// config" is a property of the COMMAND, not of the gate function alone.
//
// SAFETY. These tests seed REAL client-config files and drive the REAL
// reconcile (whose serena leg calls api.RecordManagedEntry, a state-dir
// WRITE), so they must never reach the operator's live fleet. Every test goes
// through mcpFrontPR588Env (HOME/USERPROFILE/LOCALAPPDATA + the daemon state
// root all redirected to one throwaway t.TempDir()) and asserts that redirect
// held before doing anything.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// --- finding 1 -------------------------------------------------------------

// TestMCPFrontReview_RetentionCannotPruneTheRecordedRollbackInput is the
// finding-1 guard.
//
// The record is CUMULATIVE by design: it holds the FIRST generation's
// pre-reconcile backup for each client and never replaces it, because a later
// generation's "pre-state" is really the post-first-run state. But the backups
// it pointed at were the rolling `.bak-mcp-local-hub-<ts>` files, which
// BackupKeep prunes to backups.keep_n on every single run — and a single
// forward run takes TWO of them per client (the serena leg and the LSP leg).
// So the very retry the command recommends for per-client failures deleted the
// file the record depended on, and the rollback then failed before restoring
// either surface.
//
// The test drives the real command with keep_n=1 so the pruning is fast and
// unambiguous, VERIFIES that retention really did delete the first
// generation's rolling backup, and then requires the rollback to still restore
// the original URL.
func TestMCPFrontReview_RetentionCannotPruneTheRecordedRollbackInput(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	const originalSerenaURL = "http://127.0.0.1:9125/serena/mcp"
	cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": originalSerenaURL},
	})

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet mcp_front.port: %v", err)
	}
	// The default is 5; 1 reaches the same steady state in fewer runs and
	// makes the pruning impossible to mistake for a timing artifact.
	if err := a.SettingsSet("backups.keep_n", "1"); err != nil {
		t.Fatalf("SettingsSet backups.keep_n: %v", err)
	}

	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile #1: %v", err)
	}
	first := readPersistedMCPFrontReport(t, reportPath)
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, "claude-code", "", "serena")
	row := first.Rows[key]
	if row.Pin == nil || row.Pin.Origin == "" {
		t.Fatalf("forward #1 must record the rolling backup origin in its row-owned pin; row=%+v", row)
	}
	origin := row.Pin.Origin

	// The documented retry, twice. Each run takes a fresh rolling backup per
	// leg, so with keep_n=1 the first generation's is pruned.
	for i := 2; i <= 3; i++ {
		if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
			t.Fatalf("forward reconcile #%d: %v", i, err)
		}
	}

	if _, statErr := os.Stat(origin); !os.IsNotExist(statErr) {
		t.Fatalf("test precondition not reached: the first generation's rolling backup %s still exists (stat err = %v), so this run did not actually exercise retention pruning", origin, statErr)
	}

	if err := runReconcileMCPFront(newMCPFrontTestCmd(), true); err != nil {
		t.Fatalf("rollback must still restore the first generation's pre-reconcile state after rolling retention deleted the backup it was taken from: %v", err)
	}
	got, ok := claudeCodeEntryURL(t, cfgPath, "serena")
	if !ok {
		t.Fatalf("rollback must leave the serena entry present; it is gone from %s", cfgPath)
	}
	if got != originalSerenaURL {
		t.Fatalf("rollback restored %q, want the pre-reconcile %q", got, originalSerenaURL)
	}
}

// --- finding 2 -------------------------------------------------------------

// TestMCPFrontReview_ClientIsNotMutatedWhenItsRecoveryRowCannotBeDurable is
// the finding-2 guard.
//
// The pre-fix order was: rewrite every serena client, THEN persist the record.
// On a fresh host (no prior record to fall back on) a state-file write failure
// after successful rewrites left every client pointing at the front port while
// `--rollback` refused, because no record existed. The window was small but
// its cost was total: the host was migrated with no way back.
//
// The record is now written AHEAD of each mutation, so a record that cannot be
// made durable PREVENTS the mutation instead of orphaning it. This test makes
// the write impossible (the report's parent path is a regular file, so the
// state-file pipeline's MkdirAll cannot succeed) and asserts the client config
// is byte-unchanged.
func TestMCPFrontReview_ClientIsNotMutatedWhenItsRecoveryRowCannotBeDurable(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)

	const originalSerenaURL = "http://127.0.0.1:9125/serena/mcp"
	cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": originalSerenaURL},
	})
	before, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		t.Fatalf("read seeded config: %v", rerr)
	}

	// A regular file where the report's parent DIRECTORY must be: every write
	// through the hardened state-file pipeline fails at its MkdirAll, and the
	// read of a not-yet-existing report under it reports "does not exist"
	// (so the run proceeds past the prior-record gate and reaches the
	// write-ahead hand-off, which is the window under test).
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	reportPath := filepath.Join(blocker, "mcp-front-reconcile-serena-report.json")
	orig := mcpFrontReconcileSerenaReportPathFn
	mcpFrontReconcileSerenaReportPathFn = func() (string, error) { return reportPath, nil }
	t.Cleanup(func() { mcpFrontReconcileSerenaReportPathFn = orig })

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	err := runReconcileMCPFront(newMCPFrontTestCmd(), false)
	if err == nil {
		t.Fatalf("the run must report failure when the recovery record cannot be made durable; it returned nil")
	}

	after, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		t.Fatalf("read config after the run: %v", rerr)
	}
	if string(after) != string(before) {
		t.Fatalf("the client config was MUTATED even though its recovery row could not be made durable — this is the window where the host is migrated with no way back.\nbefore: %s\nafter:  %s", string(before), string(after))
	}
	if got, _ := claudeCodeEntryURL(t, cfgPath, "serena"); got != originalSerenaURL {
		t.Fatalf("serena entry = %q, want the untouched %q", got, originalSerenaURL)
	}
}

// --- finding 3 -------------------------------------------------------------

// TestMCPFrontOwnership_ForwardRefusesUnsupervisedRouteListener is the
// finding-3 guard, and the assertion is deliberately about OWNERSHIP rather
// than protocol shape.
//
// Every subtest below runs against a REAL route listener serving the genuine
// production handler, so the liveness probe is fully satisfied and the port
// collides with no other known owner. The ONLY thing that differs from a
// legitimate cutover is who owns the port — which is exactly the distinction
// the pre-fix command could not make. A bare `mcphub route --port N` looks
// identical to the supervised daemon from the outside; nothing restarts it,
// so every client the cutover rewrites loses its endpoint when that process
// exits.
func TestMCPFrontOwnership_ForwardRefusesUnsupervisedRouteListener(t *testing.T) {
	cases := []struct {
		name string
		// mutate turns the fully-supervised seed into one specific
		// not-actually-supervised situation.
		mutate func(t *testing.T, stateDir string, port int)
		want   string
	}{
		{
			name: "standalone route: no supervisor is running at all",
			mutate: func(t *testing.T, stateDir string, port int) {
				// Release the lock the seed holds — the flock probe is the
				// authority, so this is exactly "no supervisor process".
				releaseSeededSupervisorLock(t)
			},
			want: "no supervisor holds",
		},
		{
			name: "supervisor running but managing no route daemon",
			mutate: func(t *testing.T, stateDir string, port int) {
				intentPath, err := api.DefaultSupervisorIntentPath()
				if err != nil {
					t.Fatalf("intent path: %v", err)
				}
				if werr := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{Version: 1}); werr != nil {
					t.Fatalf("rewrite intent without the route row: %v", werr)
				}
			},
			want: "no built-in route daemon row",
		},
		{
			name: "supervisor manages a route daemon on a DIFFERENT port",
			mutate: func(t *testing.T, stateDir string, port int) {
				intentPath, err := api.DefaultSupervisorIntentPath()
				if err != nil {
					t.Fatalf("intent path: %v", err)
				}
				if werr := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
					Version: 1,
					Daemons: []api.SupervisorDaemon{api.BuildBuiltinRouteDaemon("mcphub.exe", port+1)},
				}); werr != nil {
					t.Fatalf("rewrite intent with a mismatched port: %v", werr)
				}
			},
			want: "configured for port",
		},
		{
			name: "the port is held by a process the supervisor did not spawn",
			mutate: func(t *testing.T, stateDir string, port int) {
				// Everything on disk says "supervised". The kernel says the
				// socket belongs to somebody else — the standalone-listener
				// signature.
				t.Cleanup(api.SetPortOwnerIdentityProbesForTest(
					func(int) (int, bool, error) { return os.Getpid() + 1, true, nil },
					func(int) (string, bool) { return "mcphub.exe", true },
				))
			},
			want: "is owned by PID",
		},
		{
			name: "the supervisor has not spawned its route child yet",
			mutate: func(t *testing.T, stateDir string, port int) {
				if werr := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
					Version: 1,
					Daemons: map[string]api.SupervisorDaemonState{
						api.BuiltinRouteTaskName: {State: "idle"},
					},
				}); werr != nil {
					t.Fatalf("rewrite supervisor-state as idle: %v", werr)
				}
			},
			want: `records the route daemon as "idle"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := mcpFrontPR588Env(t)
			assertRedirectedStateDir(t, tmp)
			reportPath := withMCPFrontReportPathSeam(t)

			const originalSerenaURL = "http://127.0.0.1:9125/serena/mcp"
			cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
				"serena": map[string]any{"url": originalSerenaURL},
			})

			port, cleanup := startTestRouteServer(t)
			defer cleanup()
			seedSupervisorOwnedRoutePort(t, port)

			stateDir, derr := api.DaemonStateDir()
			if derr != nil {
				t.Fatalf("state dir: %v", derr)
			}
			tc.mutate(t, stateDir, port)

			a := api.NewAPI()
			if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
				t.Fatalf("SettingsSet: %v", err)
			}

			err := runReconcileMCPFront(newMCPFrontTestCmd(), false)
			if err == nil {
				t.Fatalf("forward reconcile must REFUSE: a real route-shaped listener is answering on the port, but it is not a live supervisor's own child, so nothing will restart it and every rewritten client would lose its endpoint when it exits")
			}
			if !errors.Is(err, api.ErrMCPFrontPortNotSupervisorOwned) {
				t.Fatalf("the refusal must carry the ownership sentinel so callers can distinguish it from a liveness failure; got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal must name what is wrong (%q) so the operator can fix it; got %v", tc.want, err)
			}
			if got, _ := claudeCodeEntryURL(t, cfgPath, "serena"); got != originalSerenaURL {
				t.Fatalf("a refused run must write NOTHING; the serena entry changed to %q", got)
			}
			if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
				t.Fatalf("a refused run must not persist a reconcile report; stat err = %v", statErr)
			}
		})
	}
}

// TestMCPFrontOwnership_ForwardAcceptsASupervisedRouteDaemon is the
// finding-3 positive control. Without it, "refuse an unsupervised listener"
// could be satisfied by refusing everything.
func TestMCPFrontOwnership_ForwardAcceptsASupervisedRouteDaemon(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("a port served by the supervisor's own built-in route child must be ACCEPTED: %v", err)
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("the accepted run must persist its recovery record: %v", statErr)
	}
}

// --- finding 5 -------------------------------------------------------------

// TestMCPFrontReview_RollbackRefusesAnIncompleteRecord is the finding-5 guard.
//
// The pre-fix rollback validated only the artifact version and the presence of
// a serena section. A record with a valid version, real serena data and no
// second recovery section therefore restored one surface, performed a zero-row
// "restore" of the other, and then DELETED itself — while every client on that
// second surface was still on the front port, with the only record of their
// pre-state now gone.
//
// VERSION-3 MIGRATION (this file's fixtures used to be version-2 bodies). The
// version-2 record had two value-typed sections, `serena` and `lsp`, and the
// nil-versus-empty distinction was the crux: Go marshals a nil slice to `null`
// and an empty one to `[]`, so "no rows" and "no section" were
// indistinguishable. Version 3 replaced both sections with ONE `rows` map plus
// an `active_plan` that names every row it expects (I2: rows are the only
// authority), so incompleteness now has exactly two shapes — and the
// nil-versus-empty crux SURVIVES unchanged in the first two cases, because a
// missing `rows` key and an explicit `"rows": null` both decode to a nil map:
//
//   - the row map is absent or null, so no row can authorise anything;
//   - the active plan names a row the map does not carry, which is the direct
//     analogue of "the record says it captured this surface and did not".
//
// Every subtest also asserts that NO restoration write happened and the record
// survived, which is the property the whole test exists for.
func TestMCPFrontReview_RollbackRefusesAnIncompleteRecord(t *testing.T) {
	frontSerenaURL := "http://127.0.0.1:9137/serena/mcp"

	cases := []struct {
		name string
		// perturb receives the valid version-3 record's own JSON encoding and
		// breaks exactly one thing.
		perturb func(map[string]any)
		want    string
	}{
		{
			name:    "no row map at all",
			perturb: func(m map[string]any) { delete(m, "rows") },
			want:    "carries no version-3 row map",
		},
		{
			name:    "explicitly null row map",
			perturb: func(m map[string]any) { m["rows"] = nil },
			want:    "carries no version-3 row map",
		},
		{
			name:    "not marked snapshot-complete",
			perturb: func(m map[string]any) { delete(m, "snapshot_complete") },
			want:    "not marked snapshot-complete",
		},
		{
			name: "active plan references a row the map does not carry",
			perturb: func(m map[string]any) {
				// The plan still names the serena row; the map no longer has
				// it. An EMPTY map (not a nil one) is the point: it decodes to
				// a non-nil map, so it clears the "no row map" gate above and
				// must be caught by the plan/row agreement check instead.
				m["rows"] = map[string]any{}
			},
			want: "references missing row",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := mcpFrontPR588Env(t)
			assertRedirectedStateDir(t, tmp)
			reportPath := withMCPFrontReportPathSeam(t)

			// A host mid-cutover: the client IS on the front port, so a
			// wrongly-consumed record is unrecoverable.
			cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
				"serena":                     map[string]any{"url": frontSerenaURL},
				api.LSPRouterEntryName("go"): map[string]any{"url": api.LSPRouterURL(9137, "go")},
			})
			before, rerr := os.ReadFile(cfgPath)
			if rerr != nil {
				t.Fatalf("read seeded config: %v", rerr)
			}

			record := v3RecordAsMap(t, newV3RollbackRecord(t, reportPath, 9137, "claude-code"))
			tc.perturb(record)
			if werr := api.WriteStateFileAtomic(reportPath, record); werr != nil {
				t.Fatalf("seed record: %v", werr)
			}

			err := runReconcileMCPFront(newMCPFrontTestCmd(), true)
			if err == nil {
				t.Fatalf("rollback must refuse a record that is missing a recovery section; consuming it restores part of the host and deletes the evidence for the rest")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal must name the missing section (%q); got %v", tc.want, err)
			}
			after, rerr := os.ReadFile(cfgPath)
			if rerr != nil {
				t.Fatalf("read config after the refusal: %v", rerr)
			}
			if string(after) != string(before) {
				t.Fatalf("validation must complete BEFORE any restoration write; the client config changed.\nbefore: %s\nafter:  %s", string(before), string(after))
			}
			if _, statErr := os.Stat(reportPath); statErr != nil {
				t.Fatalf("a refused rollback must KEEP the record: %v", statErr)
			}
		})
	}
}

// TestMCPFrontReview_RollbackRefusesARecordWhosePinnedInputIsGone pins the
// other half of "validate everything before writing anything": a record whose
// pinned backup has been removed or altered must stop the rollback up front,
// not halfway through the serena leg.
//
// VERSION-3 MIGRATION. The pin moved from a top-level `pins` array to the
// serena ROW that owns it, and verifyMCPFrontSerenaPins now requires the pin to
// live inside the report's own pin directory. The fixture therefore points at a
// path INSIDE that directory and simply never creates the file: that is the
// state a pruned or hand-deleted backup actually leaves behind, and it reaches
// the digest read — which is the step this test means. A path outside the pin
// root would be refused one step earlier, for escaping containment, and would
// no longer exercise the unreadable-input refusal at all.
func TestMCPFrontReview_RollbackRefusesARecordWhosePinnedInputIsGone(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	const frontSerenaURL = "http://127.0.0.1:9137/serena/mcp"
	cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": frontSerenaURL},
	})
	before, _ := os.ReadFile(cfgPath)

	missingPin := filepath.Join(mcpFrontReconcilePinDir(reportPath), "claude-code", "gone.json")
	record := newV3RollbackRecordWithPin(t, 9137, "claude-code", mcpFrontSerenaPin{
		Client: "claude-code", Origin: "rolling", Path: missingPin, SHA256: strings.Repeat("0", 64),
	})
	if werr := api.WriteStateFileAtomic(reportPath, record); werr != nil {
		t.Fatalf("seed record: %v", werr)
	}

	err := runReconcileMCPFront(newMCPFrontTestCmd(), true)
	if err == nil {
		t.Fatalf("rollback must refuse when a pinned pre-reconcile backup is unreadable")
	}
	if !strings.Contains(err.Error(), "serena-pin-open-unsafe") {
		t.Fatalf("the refusal must name the typed unsafe-open diagnostic; got %v", err)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(after) != string(before) {
		t.Fatalf("no client may be touched when a pinned input cannot be verified")
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("a refused rollback must KEEP the record: %v", statErr)
	}
}

// --- finding 4, at the consumption boundary --------------------------------

// TestMCPFrontReview_RollbackKeepsTheRecordWhileAnyRowIsPending is the
// caller-side half of finding 4: the api-level restore now REPORTS an
// unreachable client, and this pins that the command acts on that report
// instead of retiring the record anyway.
func TestMCPFrontReview_RollbackKeepsTheRecordWhileAnyRowIsPending(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	// No client config is seeded at all, so the recorded client is present in
	// the record and unreachable on disk.
	entryName := api.LSPRouterEntryName("go")
	baseline := api.LSPRouterEntrySnapshot{
		Client: "claude-code", Language: "go", EntryName: entryName,
		Present: true, URL: api.LSPRouterURL(9125, "go"),
	}
	applied := api.LSPRouterEntrySnapshot{
		Client: "claude-code", Language: "go", EntryName: entryName,
		Present: true, URL: api.LSPRouterURL(9137, "go"),
	}
	rowKey := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", entryName)
	row := mcpFrontReconcileRow{
		Surface: mcpFrontSurfaceLSP, Client: "claude-code", Language: "go", EntryName: entryName,
		Baseline: mcpFrontLSPState(baseline), BaselineSet: true,
		Applied: &mcpFrontAppliedReceipt{
			Generation: 1, Port: 9137, PostState: mcpFrontLSPState(applied),
		},
	}
	if werr := api.WriteStateFileAtomic(reportPath, mcpFrontReconcileReport{
		Version: mcpFrontReconcileReportVersion, SnapshotComplete: true, Generation: 1,
		Rows: map[string]mcpFrontReconcileRow{
			rowKey: row,
		},
		ActivePlan: &mcpFrontReconcilePlan{
			Generation: 1, Port: 9137, Rows: []string{rowKey},
			Operations: []mcpFrontReconcilePlanOp{
				mcpFrontPlanOperationForRow(rowKey, row, "add", mcpFrontLSPState(baseline), mcpFrontLSPState(applied)),
			},
		},
	}); werr != nil {
		t.Fatalf("seed record: %v", werr)
	}

	err := runReconcileMCPFront(newMCPFrontTestCmd(), true)
	if err == nil {
		t.Fatalf("rollback must not report success while a recorded entry still owes a restore")
	}
	if !strings.Contains(err.Error(), "not reachable right now") {
		t.Fatalf("the error must explain that the row is pending, not failed; got %v", err)
	}
	if !strings.Contains(err.Error(), "claude-code/go") {
		t.Fatalf("the error must name the pending row so the operator knows what is outstanding; got %v", err)
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("the record must be KEPT so a later `--rollback` can finish the job once the client is back: %v", statErr)
	}
}

// --- finding 7 -------------------------------------------------------------

// TestMCPFrontReview_RollbackFailsWhenTheRecordCannotBeRetired is the
// finding-7 guard.
//
// The pre-fix rollback printed "warning: could not remove persisted report"
// and returned SUCCESS. The consequence is not cosmetic: the consumed
// generation stayed ACTIVE, so the next forward run merged into it and — under
// the never-overwrite-a-recorded-row rule that finding-1's fix depends on —
// kept the ALREADY-RESTORED generation's rows. That later run's own rollback
// would then restore the pre-PREVIOUS state.
func TestMCPFrontReview_RollbackFailsWhenTheRecordCannotBeRetired(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	entryName := api.LSPRouterEntryName("go")
	baseline := api.LSPRouterEntrySnapshot{
		Client: "claude-code", Language: "go", EntryName: entryName,
	}
	intended := api.LSPRouterEntrySnapshot{
		Client: "claude-code", Language: "go", EntryName: entryName,
		Present: true, URL: api.LSPRouterURL(9137, "go"),
	}
	rowKey := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", entryName)
	row := mcpFrontReconcileRow{
		Surface: mcpFrontSurfaceLSP, Client: "claude-code", Language: "go", EntryName: entryName,
		Baseline: mcpFrontLSPState(baseline), BaselineSet: true,
		Disposition: &mcpFrontRollbackDisposition{
			State: mcpFrontDispositionBaselineOnly, Reason: "no-effective-applied-receipt",
		},
	}
	if werr := api.WriteStateFileAtomic(reportPath, mcpFrontReconcileReport{
		Version: mcpFrontReconcileReportVersion, SnapshotComplete: true, Generation: 1,
		Rows: map[string]mcpFrontReconcileRow{
			rowKey: row,
		},
		ActivePlan: &mcpFrontReconcilePlan{
			Generation: 1, Port: 9137, Rows: []string{rowKey},
			Operations: []mcpFrontReconcilePlanOp{
				mcpFrontPlanOperationForRow(rowKey, row, "add", mcpFrontLSPState(baseline), mcpFrontLSPState(intended)),
			},
		},
	}); werr != nil {
		t.Fatalf("seed record: %v", werr)
	}

	origRetire := mcpFrontRetireReportFn
	mcpFrontRetireReportFn = func(string) (string, error) {
		return "", fmt.Errorf("simulated: the OS denied the rename")
	}
	t.Cleanup(func() { mcpFrontRetireReportFn = origRetire })

	err := runReconcileMCPFront(newMCPFrontTestCmd(), true)
	if err == nil {
		t.Fatalf("a rollback that could not retire its record must FAIL: leaving a consumed generation active makes the NEXT forward run merge into it, and that run's rollback then restores the pre-previous state")
	}
	if !strings.Contains(err.Error(), "still ACTIVE") {
		t.Fatalf("the error must tell the operator the record is still active and must be moved aside; got %v", err)
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("the record itself must still be on disk (the failure is that it could not be MOVED): %v", statErr)
	}
}

// TestMCPFrontReview_RetirementClearsTheActiveNamespace covers the retirement
// mechanism itself: the active name must stop resolving, and the content must
// survive under the retired name (so a failure to delete afterwards is
// harmless).
func TestMCPFrontReview_RetirementClearsTheActiveNamespace(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "record.json")
	body := []byte(`{"version":2}`)
	if err := os.WriteFile(reportPath, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	retired, err := retireMCPFrontReconcileReport(reportPath)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("the ACTIVE name must stop resolving after retirement; stat err = %v", statErr)
	}
	got, rerr := os.ReadFile(retired)
	if rerr != nil || string(got) != string(body) {
		t.Fatalf("the retired copy must carry the record verbatim: err=%v body=%q", rerr, string(got))
	}
	if !strings.Contains(filepath.Base(retired), ".retired-") {
		t.Fatalf("the retired name must be recognizable as such; got %s", retired)
	}
}

// TestMCPFrontReview_ForwardRefusesToMergeIntoARetiredGeneration is the
// end-to-end statement of what finding 7 protects: once a generation has been
// rolled back, a fresh forward run must start a NEW generation rather than
// inherit the consumed one's rows.
func TestMCPFrontReview_ForwardRefusesToMergeIntoARetiredGeneration(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	const originalSerenaURL = "http://127.0.0.1:9125/serena/mcp"
	cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": originalSerenaURL},
	})

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	// Generation 1: forward, then a complete rollback.
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward #1: %v", err)
	}
	gen1 := readPersistedMCPFrontReport(t, reportPath)
	gen1Backup := serenaBackupFor(t, gen1, "claude-code")
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), true); err != nil {
		t.Fatalf("rollback #1: %v", err)
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("a complete rollback must clear the ACTIVE record; stat err = %v", statErr)
	}

	// An operator edit BETWEEN generations. Generation 2's rollback must
	// restore THIS, not generation 1's pre-state.
	const betweenURL = "http://127.0.0.1:9200/serena/mcp"
	seedClaudeCodeConfig(t, tmp, map[string]any{"serena": map[string]any{"url": betweenURL}})

	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward #2: %v", err)
	}
	gen2 := readPersistedMCPFrontReport(t, reportPath)
	if got := serenaBackupFor(t, gen2, "claude-code"); got == gen1Backup {
		t.Fatalf("generation 2 inherited generation 1's already-consumed row (%q); the retired record must not be merged into", got)
	}
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), true); err != nil {
		t.Fatalf("rollback #2: %v", err)
	}
	got, ok := claudeCodeEntryURL(t, cfgPath, "serena")
	if !ok || got != betweenURL {
		t.Fatalf("generation 2's rollback restored %q (present=%v), want the state it actually replaced, %q — restoring %q would mean it merged into the consumed generation", got, ok, betweenURL, originalSerenaURL)
	}
}

// serenaBackupFor returns the recorded restore input for one client.
func serenaBackupFor(t *testing.T, record mcpFrontReconcileReport, client string) string {
	t.Helper()
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, client, "", "serena")
	row, ok := record.Rows[key]
	if ok && row.Pin != nil {
		return row.Pin.Path
	}
	t.Fatalf("record carries no row-owned serena pin for %s: %+v", client, row)
	return ""
}

// --- record shape ----------------------------------------------------------

// TestMCPFrontReview_PersistedRecordIsSelfDescribing pins the on-disk shape
// the four lifecycle properties depend on, independently of any restore path:
// the completeness marker is present, the LSP section is an explicit array
// (never absent), and every serena row's restore input is a pinned, checksummed
// copy rather than the rolling backup it was taken from.
func TestMCPFrontReview_PersistedRecordIsSelfDescribing(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile: %v", err)
	}

	raw, rerr := os.ReadFile(reportPath)
	if rerr != nil {
		t.Fatalf("read record: %v", rerr)
	}
	var loose map[string]json.RawMessage
	if uerr := json.Unmarshal(raw, &loose); uerr != nil {
		t.Fatalf("parse record: %v", uerr)
	}
	if _, ok := loose["snapshot_complete"]; !ok {
		t.Fatalf("the record must carry the completeness marker; keys = %v", keysOf(loose))
	}
	for _, forbidden := range []string{"serena", "lsp", "pins", "applied", "port"} {
		if _, ok := loose[forbidden]; ok {
			t.Fatalf("version-3 record carries superseded top-level projection %q", forbidden)
		}
	}

	record := readPersistedMCPFrontReport(t, reportPath)
	pinCount := 0
	for _, row := range record.Rows {
		if row.Pin == nil {
			continue
		}
		pinCount++
		pin := *row.Pin
		if pin.Path == pin.Origin {
			t.Fatalf("the pin for %s points at the rolling backup itself (%s); that file is governed by backups.keep_n, which does not know this record exists", pin.Client, pin.Path)
		}
		if _, statErr := os.Stat(pin.Path); statErr != nil {
			t.Fatalf("the pinned copy for %s must exist at %s: %v", pin.Client, pin.Path, statErr)
		}
		if pin.SHA256 == "" {
			t.Fatalf("the pin for %s must carry a checksum so a changed backup fails closed instead of driving a client write", pin.Client)
		}
	}
	if pinCount == 0 {
		t.Fatal("the record must carry a row-owned Serena pin")
	}
	if got := serenaBackupFor(t, record, "claude-code"); !strings.Contains(got, mcpFrontReconcilePinDirLeaf) {
		t.Fatalf("the serena row's restore input must be the PINNED copy, not the rolling backup; got %s", got)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
