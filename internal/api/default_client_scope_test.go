package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// clientsPlannedInDryRun parses the "Client configs to update" block of an
// install dry-run and returns the planned client ids. Parsing the rendered
// block (rather than substring-matching the whole output) keeps the assertion
// honest: a client id also appears inside its own config PATH on the same line,
// so a naive strings.Contains would pass for a client that was never planned.
func clientsPlannedInDryRun(t *testing.T, out string) []string {
	t.Helper()
	var got []string
	inBlock := false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Client configs to update (") {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if !strings.HasPrefix(trimmed, "• ") {
			// The block is a contiguous run of "• <client> (<path>)" lines each
			// followed by an indented detail line; the first line that is
			// neither ends it.
			if trimmed == "" || strings.HasPrefix(trimmed, "http") || strings.Contains(trimmed, "→") {
				continue
			}
			break
		}
		row := strings.TrimPrefix(trimmed, "• ")
		if idx := strings.Index(row, " ("); idx > 0 {
			row = row[:idx]
		}
		got = append(got, strings.TrimSpace(row))
	}
	return got
}

// TestInstallUsingEmbedFirst_HonorsPersistedDefaultClientsOverride pins the
// bulk-install lane to the operator's persisted default-install client set.
//
// installUsingEmbedFirst IS the per-server entry InstallAllWithOpts drives, so
// it owns the client scope for `mcphub install --all` and for the unfiltered
// GUI bulk install. It used to build its BuildPlanOpts by hand and omit
// DefaultClientsOverride, which meant those two paths silently ignored
// gui-preferences.yaml `clients.default_install` while every single-server
// install honored it. The divergence was masked for as long as the commonly
// selected clients were also compile-time defaults; it became reachable when
// cursor moved to opt-in (bot PR #583).
//
// The override here is deliberately BOTH additive and subtractive relative to
// the compile-time default set {claude-code, codex-cli}: it ADDS the opt-in
// cursor and DROPS codex-cli. Only a genuinely applied override produces that
// exact set, so the assertion cannot be satisfied by any fallback.
func TestInstallUsingEmbedFirst_HonorsPersistedDefaultClientsOverride(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpState)
	t.Setenv("XDG_DATA_HOME", tmpState)
	t.Setenv("XDG_STATE_HOME", tmpState)

	// Hermetic manifest: a synthetic four-binding server through the
	// MCPHUB_MANIFEST_DIR_OVERRIDE loader seam, so the assertion never depends
	// on which shipped daemons happen to be running on the host (and the test
	// never probes, needs, or disturbs a live daemon's port).
	manifestRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(manifestRoot, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestYAML := `name: demo
kind: global
transport: stdio-bridge
command: go
daemons:
  - name: default
    port: 51234
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
  - client: cursor
    daemon: default
    url_path: /mcp
  - client: vscode
    daemon: default
    url_path: /mcp
weekly_refresh: false
`
	if err := os.WriteFile(filepath.Join(manifestRoot, "demo", "manifest.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)

	// Never touch the host's real TCP state for an admission port check.
	prevPortInUse := preflightPortInUse
	preflightPortInUse = func(int) bool { return false }
	t.Cleanup(func() { preflightPortInUse = prevPortInUse })

	a := NewAPI()
	if err := a.SetDefaultInstallClientNames([]string{"claude-code", "cursor"}); err != nil {
		t.Fatalf("persist default-install override: %v", err)
	}

	var buf bytes.Buffer
	if err := a.installUsingEmbedFirst(InstallOpts{Server: "demo", DryRun: true, Writer: &buf}); err != nil {
		t.Fatalf("installUsingEmbedFirst dry-run: %v\noutput:\n%s", err, buf.String())
	}

	planned := map[string]bool{}
	for _, name := range clientsPlannedInDryRun(t, buf.String()) {
		planned[name] = true
	}
	if len(planned) == 0 {
		t.Fatalf("dry-run planned no client updates at all; output:\n%s", buf.String())
	}
	for _, want := range []string{"claude-code", "cursor"} {
		if !planned[want] {
			t.Errorf("bulk install dropped %q, which the operator's persisted clients.default_install selects "+
				"— `mcphub install --all` is ignoring the override that single-server installs honor "+
				"(bot PR #583); planned=%v\noutput:\n%s", want, planned, buf.String())
		}
	}
	for _, notWant := range []string{"codex-cli", "vscode"} {
		if planned[notWant] {
			t.Errorf("bulk install planned %q, which the operator's persisted clients.default_install "+
				"EXCLUDES — the override is not being applied; planned=%v\noutput:\n%s",
				notWant, planned, buf.String())
		}
	}
}

// TestRegisterBindings_HonorPersistedDefaultClientsOverride pins the REGISTER
// lane to the same operator-persisted default-install client set the INSTALL
// lane already honors.
//
// client_install_prefs.go declares DefaultInstallClientNamesEffective to be
// "the single owner of what is the default-install set once an operator
// override may exist". Register did not honor that contract: it snapshotted the
// COMPILE-TIME clients.DefaultInstallClientNames() into a package-level var at
// init, so the persisted `clients.default_install` was invisible to it forever.
//
// The operator-visible defect: tick Cursor in Settings -> Clients, and
// `mcphub install --server serena` writes cursor while `mcphub register
// D:\proj python` (and the GUI project-LSP toggle, which calls the same
// api.Register) writes claude-code + codex-cli only — silently, since
// registerOneLanguage prints only successes. There was no recovery lever:
// register has no --clients flag, RegisterOpts has no client field, and the
// only manifest register loads is the EMBEDDED mcp-language-server manifest,
// which declares no client_bindings (manifest_source.go deliberately ignores a
// disk copy of a shipped server's manifest).
//
// The override is deliberately BOTH additive and subtractive versus the
// compile-time set {claude-code, codex-cli}: it ADDS opt-in cursor and DROPS
// codex-cli. Only a genuinely applied override yields that exact set, so no
// fallback can satisfy the assertions.
func TestRegisterBindings_HonorPersistedDefaultClientsOverride(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpState)
	t.Setenv("XDG_DATA_HOME", tmpState)
	t.Setenv("XDG_STATE_HOME", tmpState)

	a := NewAPI()
	if err := a.SetDefaultInstallClientNames([]string{"claude-code", "cursor"}); err != nil {
		t.Fatalf("persist default-install override: %v", err)
	}

	// Guard the premise: the compile-time set really does differ from the
	// override, so "register agrees with install" cannot pass by coincidence.
	compile := map[string]bool{}
	for _, n := range clients.DefaultInstallClientNames() {
		compile[n] = true
	}
	if compile["cursor"] || !compile["codex-cli"] {
		t.Fatalf("test premise broken: compile-time default set must exclude cursor and include codex-cli; got %v", compile)
	}

	// REGISTER lane: the shipped mcp-language-server manifest declares no
	// client_bindings, so register falls through to the implicit default set.
	// That fallback is the code path under test.
	registerScope := map[string]bool{}
	for _, b := range effectiveClientBindings(&config.ServerManifest{Name: "mcp-language-server"}) {
		registerScope[b.Client] = true
	}

	for _, want := range []string{"claude-code", "cursor"} {
		if !registerScope[want] {
			t.Errorf("register dropped %q, which the operator's persisted clients.default_install selects — "+
				"`mcphub register` is ignoring the override that `mcphub install` honors, and there is no "+
				"--clients flag or editable manifest to recover with; registerScope=%v", want, registerScope)
		}
	}
	if registerScope["codex-cli"] {
		t.Errorf("register bound codex-cli, which the operator's persisted clients.default_install EXCLUDES — "+
			"the override is not being applied; registerScope=%v", registerScope)
	}

	// CROSS-LANE AGREEMENT: the whole point of the fix is that the two lanes
	// answer "which clients does this operator's default set name" identically.
	// Resolve the install lane through its OWN owner (the plan builder's
	// predicate inputs) and compare, so a future change that moves one lane off
	// the shared owner fails here rather than silently re-diverging.
	installScope := map[string]bool{}
	for _, n := range a.resolveDefaultClientsOverride(InstallOpts{}) {
		installScope[n] = true
	}
	if len(installScope) == 0 {
		t.Fatalf("test premise broken: the install lane resolved no default-clients override at all")
	}
	// Register additionally drops relay-stdio adapters (they cannot take a
	// URL-only workspace binding), so compare on the URL-capable subset.
	for name := range installScope {
		if clients.IsRelayStdio(name) {
			continue
		}
		if !registerScope[name] {
			t.Errorf("install would write client %q but register would not — the two lanes disagree about the "+
				"operator's default-install set; install=%v register=%v", name, installScope, registerScope)
		}
	}
	for name := range registerScope {
		if !installScope[name] {
			t.Errorf("register would write client %q but install would not — the two lanes disagree about the "+
				"operator's default-install set; install=%v register=%v", name, installScope, registerScope)
		}
	}
}

// manifestWithBrokenCursorBinding returns a manifest whose default-install
// bindings are valid and whose OPT-IN cursor binding is not: `url_path` without
// a leading slash is rejected by BuildPlanWithOpts' validateClientURLPath. A
// bare install never touches that binding; an install that explicitly targets
// cursor does.
func manifestWithBrokenCursorBinding() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: "stdio-bridge",
		Command:   "node",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 51234}},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "default", URLPath: "/mcp"},
			{Client: "codex-cli", Daemon: "default", URLPath: "/mcp"},
			{Client: "cursor", Daemon: "default", URLPath: "mcp-no-leading-slash"},
		},
	}
}

func readinessHasInstallPlanBlocker(rep *ReadinessReport) bool {
	for _, r := range rep.Requirements {
		if r.Name == "install plan" && !r.OK {
			return true
		}
	}
	return false
}

// TestCheckServerReadinessWithScope_ValidatesExplicitlySelectedClients pins
// readiness to the client scope of the install that immediately follows it.
//
// Readiness runs a BuildPlanWithOpts dry-run precisely so it "never green-lights
// an install the planner rejects". It resolved that dry-run from the effective
// DEFAULT-install set only, so `mcphub install --server X --clients cursor` with
// a broken cursor binding reported Ready and was then rejected by the planner
// moments later. Cursor being a compile-time default hid this; its move to
// opt-in exposed it (bot PR #583).
//
// The default-scope case is asserted too, because widening readiness
// unconditionally would be the wrong fix: an untargeted opt-in binding must
// still NOT block a bare install (readiness.go, Codex #377 r7).
func TestCheckServerReadinessWithScope_ValidatesExplicitlySelectedClients(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpState)
	t.Setenv("XDG_DATA_HOME", tmpState)
	t.Setenv("XDG_STATE_HOME", tmpState)

	m := manifestWithBrokenCursorBinding()

	// Guard the premise: the broken binding really is broken, and really is
	// invisible to a default-scoped plan.
	if _, err := BuildPlanWithOpts(m, BuildPlanOpts{ClientsInclude: []string{"cursor"}}); err == nil {
		t.Fatal("test premise broken: the cursor binding must be rejected by the planner")
	}
	if _, err := BuildPlanWithOpts(m, BuildPlanOpts{}); err != nil {
		t.Fatalf("test premise broken: the default-scoped plan must succeed, got: %v", err)
	}

	t.Run("bare install does not block on an untargeted opt-in binding", func(t *testing.T) {
		if readinessHasInstallPlanBlocker(CheckServerReadinessWithScope(m, AdmissionScope{})) {
			t.Error("default-scoped readiness blocked on an opt-in binding a bare install never touches " +
				"— this is the false Ready=false the default scope exists to prevent")
		}
	})

	t.Run("--clients cursor blocks", func(t *testing.T) {
		rep := CheckServerReadinessWithScope(m, AdmissionScope{ClientsInclude: []string{"cursor"}})
		if !readinessHasInstallPlanBlocker(rep) {
			t.Errorf("readiness reported Ready for `--clients cursor` even though the planner rejects that " +
				"exact binding — readiness must validate the scope the install will apply, not the " +
				"default-install set (bot PR #583)")
		}
	})

	t.Run("--all-clients blocks", func(t *testing.T) {
		rep := CheckServerReadinessWithScope(m, AdmissionScope{IncludeAllClients: true})
		if !readinessHasInstallPlanBlocker(rep) {
			t.Errorf("readiness reported Ready for `--all-clients` even though the planner rejects the " +
				"cursor binding that flag explicitly selects")
		}
	})

	t.Run("explicitly selecting only healthy clients stays ready", func(t *testing.T) {
		rep := CheckServerReadinessWithScope(m, AdmissionScope{ClientsInclude: []string{"claude-code"}})
		if readinessHasInstallPlanBlocker(rep) {
			t.Error("readiness blocked a `--clients claude-code` install on a binding that selection " +
				"excludes — the explicit scope must NARROW validation, not widen it to every binding")
		}
	})
}
