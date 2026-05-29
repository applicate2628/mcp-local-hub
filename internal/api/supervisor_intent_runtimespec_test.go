package api

import (
	"reflect"
	"strconv"
	"testing"

	"mcp-local-hub/internal/config"
)

// serenaDynamicPoolManifest returns a workspace-scoped native-http serena
// manifest in the DESIGN-CORRECT shape for the descriptor-driven proxy:
// extra_args_template carries ONLY --project ${workspace.path} (NO --context
// token); DaemonTemplate.Context holds the single context value the
// materializer APPENDS as a trailing --context pair (design §5).
//
// A placeholder context value is used on purpose — the materializer + proxy
// are value-agnostic (O1 does NOT block this phase). The test asserts that
// WHATEVER DaemonTemplate.Context holds is appended verbatim.
func serenaDynamicPoolManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		BaseArgs:  []string{"--from", "git+https://example.invalid/serena", "serena", "start-mcp-server"},
		Env:       map[string]string{"PYTHONUNBUFFERED": "1"},
		DaemonTemplate: &config.DaemonTemplate{
			Context: "codex-placeholder",
			PortPool: &config.PortPool{
				Start: 9121,
				End:   9199,
			},
			// NO --context token here — design §5: only ${workspace.path}
			// is an expansion surface; --context is appended by the
			// materializer from DaemonTemplate.Context.
			ExtraArgsTemplate: []string{"--project", "${workspace.path}"},
		},
	}
}

// TestBuildSerenaDaemons_MaterializesRuntimeSpec_AppendsContextAndProject is
// the core materializer assertion (plan Phase 1; design §3/§5 + claim #5):
//   - RuntimeSpec.ChildArgs ENDS with `--context <DaemonTemplate.Context>`.
//   - RuntimeSpec.ChildArgs CONTAINS `--project <workspace>`.
//   - RuntimeSpec.ChildArgs does NOT include `--port` (the proxy appends it).
//   - RuntimeSpec.ChildCommand == m.Command.
func TestBuildSerenaDaemons_MaterializesRuntimeSpec_AppendsContextAndProject(t *testing.T) {
	m := serenaDynamicPoolManifest()
	const wsPath = "C:/work/alpha"
	ws := WorkspaceEntry{
		WorkspaceKey:  WorkspaceKey(wsPath),
		WorkspacePath: wsPath,
		Language:      SerenaLanguageSentinel,
		Port:          9121,
	}
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{ws}, "h", testMcphubBinary)
	if len(got) != 1 {
		t.Fatalf("got %d descriptors; want 1", len(got))
	}
	spec := got[0].RuntimeSpec
	if spec == nil {
		t.Fatalf("RuntimeSpec must be materialized; got nil")
	}

	if spec.ChildCommand != m.Command {
		t.Errorf("ChildCommand: got=%q want=%q", spec.ChildCommand, m.Command)
	}

	// Exact expected ChildArgs: base_args ++ [--project <ws>] ++ [--context <ctx>].
	wantChildArgs := []string{
		"--from", "git+https://example.invalid/serena", "serena", "start-mcp-server",
		"--project", wsPath,
		"--context", "codex-placeholder",
	}
	if !reflect.DeepEqual(spec.ChildArgs, wantChildArgs) {
		t.Fatalf("ChildArgs mismatch:\n got=%#v\nwant=%#v", spec.ChildArgs, wantChildArgs)
	}

	// Trailing pair must be the appended --context (claim #5).
	n := len(spec.ChildArgs)
	if n < 2 || spec.ChildArgs[n-2] != "--context" || spec.ChildArgs[n-1] != "codex-placeholder" {
		t.Errorf("ChildArgs must END with --context <value>; got tail=%v", spec.ChildArgs[max0(n-2):])
	}

	// --project <ws> must be present as an adjacent pair (token expanded).
	if !containsAdjacent(spec.ChildArgs, "--project", wsPath) {
		t.Errorf("ChildArgs must contain --project %q; got=%v", wsPath, spec.ChildArgs)
	}

	// No --port in ChildArgs — the proxy appends the internal port at spawn.
	for i, a := range spec.ChildArgs {
		if a == "--port" {
			t.Errorf("ChildArgs must NOT include --port (proxy appends it); found at index %d in %v", i, spec.ChildArgs)
		}
	}
}

// TestBuildSerenaDaemons_AppendsTaskNameToWrapperArgs asserts the wrapper
// Args (the supervisor execs verbatim) END with --task-name
// <SerenaTaskNameForWorkspace> so the proxy can find its own descriptor
// (plan Phase 1; design §2.2).
func TestBuildSerenaDaemons_AppendsTaskNameToWrapperArgs(t *testing.T) {
	m := serenaDynamicPoolManifest()
	const wsPath = "C:/work/alpha"
	ws := WorkspaceEntry{WorkspacePath: wsPath, Language: SerenaLanguageSentinel, Port: 9121}
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{ws}, "", testMcphubBinary)
	if len(got) != 1 {
		t.Fatalf("got %d descriptors; want 1", len(got))
	}
	wantTask := SerenaTaskNameForWorkspace(wsPath)
	wantArgs := []string{
		"daemon", "serena-proxy",
		"--server", "serena",
		"--workspace", wsPath,
		"--port", strconv.Itoa(ws.Port),
		"--task-name", wantTask,
	}
	if !reflect.DeepEqual(got[0].Args, wantArgs) {
		t.Fatalf("wrapper Args mismatch:\n got=%#v\nwant=%#v", got[0].Args, wantArgs)
	}
	// The --task-name value must equal the descriptor's own TaskName so the
	// proxy's --task-name lookup resolves to itself.
	if got[0].TaskName != wantTask {
		t.Errorf("descriptor TaskName=%q must equal the --task-name argv value %q", got[0].TaskName, wantTask)
	}
}

// TestBuildSerenaDaemons_RuntimeSpecEnvRefsUnresolved is the deep-sec guard:
// secret:KEY values are carried VERBATIM in RuntimeSpec.EnvRefs (resolved in
// the proxy, never persisted in cleartext) and NEVER leak into ChildArgs
// (design §3 / claim #10).
func TestBuildSerenaDaemons_RuntimeSpecEnvRefsUnresolved(t *testing.T) {
	m := serenaDynamicPoolManifest()
	m.Env = map[string]string{
		"PYTHONUNBUFFERED": "1",
		"SERENA_TOKEN":     "secret:SERENA_TOKEN",
	}
	ws := WorkspaceEntry{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121}
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{ws}, "", testMcphubBinary)
	if len(got) != 1 {
		t.Fatalf("got %d descriptors; want 1", len(got))
	}
	spec := got[0].RuntimeSpec
	if spec == nil {
		t.Fatal("RuntimeSpec nil")
	}
	if spec.EnvRefs["SERENA_TOKEN"] != "secret:SERENA_TOKEN" {
		t.Errorf("EnvRefs must carry secret:KEY verbatim; got=%q", spec.EnvRefs["SERENA_TOKEN"])
	}
	// EnvRefs must be a CLONE, not the manifest map aliased.
	spec.EnvRefs["INJECTED"] = "x"
	if _, leaked := m.Env["INJECTED"]; leaked {
		t.Error("EnvRefs mutation leaked into manifest.Env (must be a clone)")
	}
	// The secret token must NOT appear anywhere in ChildArgs.
	for _, a := range spec.ChildArgs {
		if a == "secret:SERENA_TOKEN" || a == "SERENA_TOKEN" {
			t.Errorf("secret ref/key leaked into ChildArgs: %q in %v", a, spec.ChildArgs)
		}
	}
}

// TestBuildSerenaDaemons_RuntimeSpecPortMath asserts the port relationship:
// UpstreamPort == ExternalPort + config.NativeHTTPInternalPortOffset, and
// ExternalPort == ws.Port (design §3 acceptance).
func TestBuildSerenaDaemons_RuntimeSpecPortMath(t *testing.T) {
	m := serenaDynamicPoolManifest()
	ws := WorkspaceEntry{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9137}
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{ws}, "", testMcphubBinary)
	if len(got) != 1 {
		t.Fatalf("got %d descriptors; want 1", len(got))
	}
	spec := got[0].RuntimeSpec
	if spec == nil {
		t.Fatal("RuntimeSpec nil")
	}
	if spec.ExternalPort != ws.Port {
		t.Errorf("ExternalPort: got=%d want=%d", spec.ExternalPort, ws.Port)
	}
	if want := ws.Port + config.NativeHTTPInternalPortOffset; spec.UpstreamPort != want {
		t.Errorf("UpstreamPort: got=%d want=%d (ExternalPort + %d)", spec.UpstreamPort, want, config.NativeHTTPInternalPortOffset)
	}
	if spec.WorkspacePath != ws.WorkspacePath {
		t.Errorf("WorkspacePath: got=%q want=%q", spec.WorkspacePath, ws.WorkspacePath)
	}
	if spec.SpecVersion != DaemonRuntimeSpecVersion {
		t.Errorf("SpecVersion: got=%d want=%d", spec.SpecVersion, DaemonRuntimeSpecVersion)
	}
}

// TestBuildSerenaDaemons_RuntimeSpecMirrorsTopLevelFields is the build-time
// invariant (design §3): RuntimeSpec.ExternalPort == SupervisorDaemon.Port and
// RuntimeSpec.WorkspacePath == SupervisorDaemon.Workspace for every row.
func TestBuildSerenaDaemons_RuntimeSpecMirrorsTopLevelFields(t *testing.T) {
	m := serenaDynamicPoolManifest()
	workspaces := []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
		{WorkspacePath: "C:/work/beta", Language: SerenaLanguageSentinel, Port: 9122},
	}
	got := BuildSupervisorDaemonsForSerena(m, workspaces, "", testMcphubBinary)
	if len(got) != 2 {
		t.Fatalf("got %d descriptors; want 2", len(got))
	}
	for i, d := range got {
		if d.RuntimeSpec == nil {
			t.Fatalf("[%d] RuntimeSpec nil", i)
		}
		if d.RuntimeSpec.ExternalPort != d.Port {
			t.Errorf("[%d] RuntimeSpec.ExternalPort=%d != SupervisorDaemon.Port=%d", i, d.RuntimeSpec.ExternalPort, d.Port)
		}
		if d.RuntimeSpec.WorkspacePath != d.Workspace {
			t.Errorf("[%d] RuntimeSpec.WorkspacePath=%q != SupervisorDaemon.Workspace=%q", i, d.RuntimeSpec.WorkspacePath, d.Workspace)
		}
	}
}

// TestBuildSerenaDaemons_NonNativeHTTP_ReturnsNil is the build-time transport
// gate (design §3.1): a workspace-scoped daemon_template manifest whose
// transport is NOT native-http must yield NO descriptors from the materializer.
// Note: such a manifest (stdio-bridge + daemon_template + kind:workspace-scoped)
// passes config.ServerManifest.Validate today, so this gate is load-bearing.
func TestBuildSerenaDaemons_NonNativeHTTP_ReturnsNil(t *testing.T) {
	m := serenaDynamicPoolManifest()
	m.Transport = config.TransportStdioBridge // passes Validate, must be rejected by the materializer
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
	}, "", testMcphubBinary)
	if got != nil {
		t.Fatalf("non-native-http daemon_template manifest must yield nil from the materializer; got=%#v", got)
	}
}

// TestBuildSerenaDaemons_EmptyContext_ReturnsNil is the build-time empty-context
// gate (bot PR #246 P2): a workspace-scoped daemon_template manifest whose
// DaemonTemplate.Context is empty (absent or "") must yield NO descriptors from
// the materializer. Without this gate, buildSerenaChildArgs would append
// `--context ""` into every RuntimeSpec.ChildArgs and the serena child would
// launch with an invalid empty context. Note: such a manifest passes
// config.ServerManifest.Validate today (Validate checks port_pool +
// extra_args_template, NOT Context), so this build-time gate plus the
// InstallParsedManifest contract gate are the only enforcers.
func TestBuildSerenaDaemons_EmptyContext_ReturnsNil(t *testing.T) {
	for _, ctx := range []string{"", "   "} {
		m := serenaDynamicPoolManifest()
		m.DaemonTemplate.Context = ctx
		got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{
			{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
		}, "", testMcphubBinary)
		if got != nil {
			t.Fatalf("empty/blank daemon_template.context (%q) must yield nil from the materializer; got=%#v", ctx, got)
		}
	}
}

// TestBuildSerenaDaemons_NonEmptyContext_Materializes is the positive control
// for the empty-context gate: a non-empty Context still materializes a
// descriptor whose RuntimeSpec.ChildArgs ends with `--context <value>`.
func TestBuildSerenaDaemons_NonEmptyContext_Materializes(t *testing.T) {
	m := serenaDynamicPoolManifest() // Context = "codex-placeholder" (non-empty)
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
	}, "", testMcphubBinary)
	if len(got) != 1 || got[0].RuntimeSpec == nil {
		t.Fatalf("non-empty context must materialize 1 descriptor with RuntimeSpec; got=%#v", got)
	}
	ca := got[0].RuntimeSpec.ChildArgs
	if n := len(ca); n < 2 || ca[n-2] != "--context" || ca[n-1] != "codex-placeholder" {
		t.Errorf("ChildArgs must end with --context codex-placeholder; got=%v", ca)
	}
}

// TestBuildSerenaDaemons_ChildArgsTokenExpansion asserts ${workspace.path}
// inside extra_args_template is substituted with the registry path (not left
// literal) in the materialized ChildArgs.
func TestBuildSerenaDaemons_ChildArgsTokenExpansion(t *testing.T) {
	m := serenaDynamicPoolManifest()
	const wsPath = `C:\Users\dev\repos\alpha`
	ws := WorkspaceEntry{WorkspacePath: wsPath, Language: SerenaLanguageSentinel, Port: 9121}
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{ws}, "", testMcphubBinary)
	if len(got) != 1 || got[0].RuntimeSpec == nil {
		t.Fatalf("expected 1 descriptor with RuntimeSpec; got=%#v", got)
	}
	for _, a := range got[0].RuntimeSpec.ChildArgs {
		if a == config.WorkspacePathToken {
			t.Fatalf("literal %q must be expanded in ChildArgs; got=%v", config.WorkspacePathToken, got[0].RuntimeSpec.ChildArgs)
		}
	}
	if !containsAdjacent(got[0].RuntimeSpec.ChildArgs, "--project", wsPath) {
		t.Errorf("--project must carry the expanded backslash path %q; got=%v", wsPath, got[0].RuntimeSpec.ChildArgs)
	}
}

// containsAdjacent reports whether args contains a..b as adjacent tokens.
func containsAdjacent(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
