package cli

import (
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// TestLegacyUpgradeTaskLooksDaemon_ExcludesWorkspaceScopedTasks is the FIX 2
// (bot r33 P2, PR #288) falsifying regression. legacyUpgradeTaskLooksDaemon
// must NOT classify workspace-scoped LSP / serena per-workspace tasks as
// migratable global daemons.
//
// Those task names (`mcp-local-hub-lsp-<wsKey>-<lang>` and
// `mcp-local-hub-serena-<wsKey>`) match the generic "contains a hyphen after
// the prefix" shape, so pre-fix they returned true. On a host with ONLY
// workspace-scoped registrations, the per-server manifest match then found no
// global manifest → legacyTasks non-empty + servers empty → `install --upgrade`
// aborted with "none match a shipped manifest" instead of doing the normal
// binary copy/restart.
//
// Pre-fix: the LSP and serena assertions FAIL (return true). Real global
// daemons, plus the liveness/watchdog/supervisor/weekly-refresh exclusions,
// keep their pre-fix verdicts.
func TestLegacyUpgradeTaskLooksDaemon_ExcludesWorkspaceScopedTasks(t *testing.T) {
	// Derive the canonical workspace-scoped task-name shapes from the same
	// api builders the production code paths use, so the test tracks the real
	// shapes rather than hard-coded literals. The 8-hex key + language is the
	// LSP form; the 8-hex key alone is the serena form.
	wsKey := "abcd1234"                                         // 8 lowercase-hex chars — the WorkspaceKey shape
	lspBare := api.LSPTaskNameForWorkspaceLanguage(wsKey, "go") // mcp-local-hub-lsp-abcd1234-go
	// SerenaTaskNameForWorkspace returns the canonical leading-backslash form;
	// legacyUpgradeTaskLooksDaemon receives the bare (backslash-stripped) form
	// from its caller, so strip it here too.
	serenaCanonical := api.SerenaTaskNamePrefix + wsKey // \mcp-local-hub-serena-abcd1234
	serenaBare := "mcp-local-hub-serena-" + wsKey       // mcp-local-hub-serena-abcd1234

	tests := []struct {
		name string
		task string
		want bool
	}{
		// FIX 2 core: workspace-scoped tasks must NOT look like global daemons.
		{"lsp workspace proxy", lspBare, false},
		{"lsp another language", api.LSPTaskNameForWorkspaceLanguage("deadbeef", "python"), false},
		{"serena bare per-workspace", serenaBare, false},
		// The production caller strips the leading backslash, but be defensive:
		// even if a canonical-form name slipped through, it must not classify
		// as a global daemon either. IsSerenaTaskName accepts both forms.
		{"serena canonical per-workspace", serenaCanonical, false},

		// Real global daemon — must still classify as migratable (true).
		{"global memory daemon", "mcp-local-hub-memory-default", true},
		{"global serena legacy client daemon", "mcp-local-hub-godbolt-default", true},

		// Pre-existing exclusions must keep their verdicts (false).
		{"liveness", "mcp-local-hub-liveness", false},
		{"watchdog", "mcp-local-hub-watchdog", false},
		{"supervisor", "mcp-local-hub-supervisor", false},
		{"per-server weekly refresh", "mcp-local-hub-memory-weekly-refresh", false},
		{"serena weekly refresh", "mcp-local-hub-serena-weekly-refresh", false},

		// Non-mcphub names and the bare prefix are false (sanity).
		{"foreign task", "some-other-task", false},
		{"bare prefix no suffix", "mcp-local-hub-", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyUpgradeTaskLooksDaemon(tc.task)
			if got != tc.want {
				t.Fatalf("legacyUpgradeTaskLooksDaemon(%q) = %v, want %v", tc.task, got, tc.want)
			}
		})
	}
}

// TestUpgradeInstallServer_PassesNoClientWriteOpts is the FIX 3 (bot r33 P2,
// PR #288) CALL-SITE falsifying regression. It drives the PRODUCTION
// upgradeInstallServer body (upgradeInstallServerFn nil) and captures the
// api.InstallOpts it constructs via the narrow upgradeServerInstallFn seam.
// The legacy-scheduler upgrade migration uses upgradeInstallServer ONLY to
// absorb matched legacy daemons into supervisor intent AFTER the binary copy;
// it must NOT rewrite client configs.
//
// Pre-fix the body passed api.InstallOpts{Server, Writer} with NO
// ClientsInclude → api.installClientPredicate fell back to
// clients.DefaultInstallClientNames() (claude-code/codex-cli/cursor) and the
// install ADDED/OVERWROTE those clients' entries. This test asserts the
// captured ClientsInclude selects ZERO supported clients (via the same api
// predicate), so it FAILS pre-fix (the captured opts would carry no/nil
// ClientsInclude → predicate selects the default 3).
func TestUpgradeInstallServer_PassesNoClientWriteOpts(t *testing.T) {
	resetUpgradeSeams(t)

	origServerInstall := upgradeServerInstallFn
	t.Cleanup(func() { upgradeServerInstallFn = origServerInstall })

	var captured api.InstallOpts
	var called bool
	upgradeServerInstallFn = func(opts api.InstallOpts) error {
		captured = opts
		called = true
		return nil
	}

	if err := upgradeInstallServer("memory", nil); err != nil {
		t.Fatalf("upgradeInstallServer: %v", err)
	}
	if !called {
		t.Fatal("production upgradeInstallServer did not reach the api.Install call site")
	}
	if captured.Server != "memory" {
		t.Fatalf("captured Server = %q, want memory", captured.Server)
	}

	// Resolve the captured ClientsInclude through the SAME predicate the real
	// install uses, against a manifest with all three default-client bindings.
	// Zero ClientUpdates proves the call site suppressed default-client writes.
	m := defaultClientManifestFixture()
	plan, err := api.BuildPlanWithOpts(m, api.BuildPlanOpts{
		ClientsInclude: captured.ClientsInclude,
	})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(captured ClientsInclude): %v", err)
	}
	if len(plan.ClientUpdates) != 0 {
		t.Fatalf("captured ClientsInclude=%v selected %d client(s) → %d ClientUpdates; "+
			"legacy upgrade must materialize supervisor intent WITHOUT touching client configs",
			captured.ClientsInclude, len(plan.ClientUpdates), len(plan.ClientUpdates))
	}
	if len(plan.SupervisorIntent) == 0 {
		t.Fatal("captured opts suppressed supervisor-intent materialization; must only skip client writes")
	}
}

// TestUpgradeNoClientWriteSentinel_SelectsZeroClients pins the api contract the
// FIX 3 sentinel relies on (bot r33 P2, PR #288): a non-empty ClientsInclude
// whose entries all trim to "" selects zero clients (the empty entry is
// dropped before the unknown-client check), so the plan carries zero
// ClientUpdates while supervisor-intent / scheduler rows still materialize. If
// that empty-entry tolerance ever changes, this test fails loud so the
// sentinel is revisited.
func TestUpgradeNoClientWriteSentinel_SelectsZeroClients(t *testing.T) {
	m := defaultClientManifestFixture()

	// Negative control: the DEFAULT (nil ClientsInclude) — what the buggy
	// pre-fix upgradeInstallServer effectively passed — DOES write clients.
	defaultPlan, err := api.BuildPlanWithOpts(m, api.BuildPlanOpts{})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(default): %v", err)
	}
	if len(defaultPlan.ClientUpdates) == 0 {
		t.Fatal("negative control failed: default install plan emitted zero ClientUpdates; " +
			"the manifest fixture must have client bindings for this regression to be meaningful")
	}

	// FIX 3: the no-client-write sentinel upgradeInstallServer passes must
	// select zero clients → zero ClientUpdates, while still being a valid
	// (error-free) plan that materializes the supervisor-intent / scheduler
	// rows.
	sentinelPlan, err := api.BuildPlanWithOpts(m, api.BuildPlanOpts{
		ClientsInclude: upgradeNoClientWriteSentinel,
	})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(no-client-write sentinel): %v", err)
	}
	if len(sentinelPlan.ClientUpdates) != 0 {
		t.Fatalf("no-client-write sentinel emitted %d ClientUpdates, want 0 (legacy upgrade must not touch client configs)",
			len(sentinelPlan.ClientUpdates))
	}
	// Supervisor-intent rows must still materialize — the sentinel only
	// suppresses client writes, not daemon/intent materialization.
	if len(sentinelPlan.SupervisorIntent) == 0 {
		t.Fatal("no-client-write sentinel suppressed supervisor-intent materialization; " +
			"it must only skip client-config writes")
	}
}

// defaultClientManifestFixture returns a minimal global manifest carrying one
// daemon and the three default-install client bindings (claude-code, codex-cli,
// cursor). A non-empty ClientBindings set is load-bearing for the FIX 3
// regression: it is what makes the default-client install plan emit
// ClientUpdates, so the zero-ClientUpdates assertion is meaningful.
func defaultClientManifestFixture() *config.ServerManifest {
	return &config.ServerManifest{
		Name: "memory",
		Kind: config.KindGlobal,
		Daemons: []config.DaemonSpec{
			{Name: "default", Port: 9128},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "default"},
			{Client: "codex-cli", Daemon: "default"},
			{Client: "cursor", Daemon: "default"},
		},
	}
}
