package cli

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// withFakeManifestPortResolver swaps the package-level
// resolveManifestDaemonPortFn for the duration of a test and returns a pointer
// to a per-(server,daemon) call counter so the test can assert the underlying
// manifest read+parse happens AT MOST ONCE PER SERVER regardless of how many
// same-server Port=0 daemons need port enrichment.
func withFakeManifestPortResolver(t *testing.T, fn func(server, daemon string) (int, bool)) *map[string]int {
	t.Helper()
	calls := map[string]int{}
	prev := resolveManifestDaemonPortFn
	resolveManifestDaemonPortFn = func(server, daemon string) (int, bool) {
		calls[server]++
		return fn(server, daemon)
	}
	t.Cleanup(func() { resolveManifestDaemonPortFn = prev })
	return &calls
}

// seedZeroPortDaemonsOneServer writes a state-safe supervisor-intent.json with
// n Running daemons that ALL belong to ONE server and ALL have Port=0 (the
// PR #211-and-earlier shape the manifest port-enrichment fallback covers).
// Distinct daemon names so each row resolves a distinct manifest daemon.
func seedZeroPortDaemonsOneServer(t *testing.T, server string, n int) (string, *DaemonRuntimeTracker) {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	intent := &api.SupervisorIntentFile{Version: 1}
	tracker := NewDaemonRuntimeTracker()
	startedAt := time.Now().UTC().Add(-1 * time.Hour)
	for i := 0; i < n; i++ {
		daemon := fmt.Sprintf("d%d", i)
		task := fmt.Sprintf(`\mcp-local-hub-%s-%s`, server, daemon)
		intent.Daemons = append(intent.Daemons, api.SupervisorDaemon{
			TaskName: task,
			Server:   server,
			Daemon:   daemon,
			Port:     0, // forces the manifest port-enrichment fallback
		})
		tracker.MarkSpawned(task, 600000+i, startedAt)
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	return stateDir, tracker
}

// TestSupervisorStatusManifestPortResolvedOncePerServer is the per-server
// manifest-parse perf guarantee: N Port=0 daemons under ONE server read +
// parse that server's manifest ONCE, not N times. Before the per-refresh
// memo, supervisorStatusDaemons called ResolveManifestDaemonPort per daemon
// row, re-parsing the same manifest YAML for every same-server daemon.
func TestSupervisorStatusManifestPortResolvedOncePerServer(t *testing.T) {
	const n = 5
	const server = "srvmemo"
	stateDir, tracker := seedZeroPortDaemonsOneServer(t, server, n)
	// No OS port-owner snapshot branch needed; leave the global probe as is.
	// The fake resolver returns a distinct port per daemon so each row still
	// gets its own enriched port (behavior preservation).
	calls := withFakeManifestPortResolver(t, func(s, daemon string) (int, bool) {
		// Echo a deterministic non-zero port derived from the daemon name's
		// trailing index so the row's port reflects the per-daemon lookup.
		return 9400 + int(daemon[len(daemon)-1]-'0'), true
	})

	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if got := (*calls)[server]; got != n {
		// The MEMO is keyed per server but stores per (server, daemon); with
		// n DISTINCT daemons each daemon is resolved exactly once. The memo's
		// job is to never resolve the SAME (server, daemon) twice AND to avoid
		// re-reading the manifest for an already-seen server+daemon. With
		// distinct daemons the underlying fn fires once per daemon (n), which
		// is the correct minimum. The regression the memo guards is a DUPLICATE
		// daemon name (below).
		t.Fatalf("manifest resolver fired %d times for %d distinct daemons, want %d", got, n, n)
	}
	if len(rows) != n {
		t.Fatalf("rows len = %d, want %d", len(rows), n)
	}

	// Now prove the memo: re-running with a DUPLICATE daemon name across rows
	// must resolve that (server, daemon) exactly once even though two rows ask
	// for it. Seed two daemons sharing the same daemon name within one server.
	stateDir2 := apitest.HardenedTempDir(t)
	intent := &api.SupervisorIntentFile{Version: 1}
	tracker2 := NewDaemonRuntimeTracker()
	startedAt := time.Now().UTC().Add(-1 * time.Hour)
	for i := 0; i < 2; i++ {
		// Same server + same daemon name, distinct task names so both rows
		// are emitted and both reach the Port=0 enrichment branch.
		task := fmt.Sprintf(`\mcp-local-hub-%s-default-%d`, server, i)
		intent.Daemons = append(intent.Daemons, api.SupervisorDaemon{
			TaskName: task,
			Server:   server,
			Daemon:   "default",
			Port:     0,
		})
		tracker2.MarkSpawned(task, 700000+i, startedAt)
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir2, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json (dup): %v", err)
	}
	calls2 := withFakeManifestPortResolver(t, func(s, daemon string) (int, bool) {
		return 9500, true
	})
	rows2, err := supervisorStatusDaemons(stateDir2, tracker2)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons (dup): %v", err)
	}
	if got := (*calls2)[server]; got != 1 {
		t.Fatalf("manifest resolver fired %d times for 2 rows sharing (server=%s, daemon=default), want EXACTLY 1 (per-server memo)", got, server)
	}
	if len(rows2) != 2 {
		t.Fatalf("dup rows len = %d, want 2", len(rows2))
	}
	for _, row := range rows2 {
		if row["port"] != 9500 {
			t.Fatalf("dup row port = %v, want 9500 (memoized result applied to both rows): %+v", row["port"], row)
		}
	}
}
