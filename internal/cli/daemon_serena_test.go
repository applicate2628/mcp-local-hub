package cli

import (
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// seedSerenaIntent writes a supervisor-intent.json into a test-redirected
// state dir containing exactly the supplied daemons, and returns the
// resolved intent path. The state dir is redirected via
// api.SetDaemonStateRootForTest so the proxy's DefaultSupervisorIntentPath()
// resolves to the same file — no env vars, no real %LOCALAPPDATA% touch.
func seedSerenaIntent(t *testing.T, daemons []api.SupervisorDaemon) string {
	t.Helper()
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	intentPath, err := api.DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
		Version: 1,
		Daemons: daemons,
	}); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	return intentPath
}

// serenaWorkspaceDir returns the CANONICAL form of a fresh real temp
// directory to use as a workspace path. CanonicalWorkspacePath requires the
// directory to exist (the proxy canonicalizes --workspace before use and the
// supervisor sets cmd.Dir to it), so test fixtures must use a real dir.
func serenaWorkspaceDir(t *testing.T) string {
	t.Helper()
	canonical, err := api.CanonicalWorkspacePath(t.TempDir())
	if err != nil {
		t.Fatalf("canonical workspace dir: %v", err)
	}
	return canonical
}

// serenaDescriptorFixture returns a well-formed serena descriptor whose
// task name + RuntimeSpec all key off the already-canonical canonicalWs path.
func serenaDescriptorFixture(t *testing.T, canonicalWs string, externalPort int) api.SupervisorDaemon {
	t.Helper()
	canonical := canonicalWs
	task := api.SerenaTaskNameForWorkspace(canonical)
	return api.SupervisorDaemon{
		TaskName:  task,
		Server:    "serena",
		Daemon:    api.WorkspaceKey(canonical),
		Command:   "mcphub",
		Args:      []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", canonical, "--port", itoaForTest(externalPort), "--task-name", task},
		Workspace: canonical,
		Port:      externalPort,
		RuntimeSpec: &api.DaemonRuntimeSpec{
			SpecVersion:   api.DaemonRuntimeSpecVersion,
			ChildCommand:  "uvx",
			ChildArgs:     []string{"--from", "git+https://example/serena", "serena", "start-mcp-server", "--project", canonical, "--context", "codex-placeholder"},
			EnvRefs:       map[string]string{"PYTHONUNBUFFERED": "1"},
			UpstreamPort:  externalPort + 10000,
			ExternalPort:  externalPort,
			WorkspacePath: canonical,
		},
	}
}

// TestSerenaProxy_LoadsRuntimeSpecByTaskName: the proxy resolves its own
// descriptor + RuntimeSpec from a seeded intent by --task-name, and the
// consistency contract passes for matching flags (design §4 step 3 + §3.2).
func TestSerenaProxy_LoadsRuntimeSpecByTaskName(t *testing.T) {
	canonical := serenaWorkspaceDir(t)
	d := serenaDescriptorFixture(t, canonical, 9121)
	seedSerenaIntent(t, []api.SupervisorDaemon{d})

	spec, err := loadSerenaProxyRuntimeSpec(d.TaskName, canonical, 9121)
	if err != nil {
		t.Fatalf("loadSerenaProxyRuntimeSpec: unexpected error: %v", err)
	}
	if spec == nil {
		t.Fatal("want non-nil RuntimeSpec")
	}
	if spec.ChildCommand != "uvx" {
		t.Errorf("ChildCommand: got=%q want=uvx", spec.ChildCommand)
	}
	if spec.WorkspacePath != canonical {
		t.Errorf("WorkspacePath: got=%q want=%q", spec.WorkspacePath, canonical)
	}
}

// TestSerenaProxy_BuildsChildArgsFromSpec_AppendsInternalPort: the final child
// argv is spec.ChildArgs ++ [--port, UpstreamPort], carrying --context and
// --project (design §4 step 6).
func TestSerenaProxy_BuildsChildArgsFromSpec_AppendsInternalPort(t *testing.T) {
	spec := &api.DaemonRuntimeSpec{
		ChildCommand:  "uvx",
		ChildArgs:     []string{"serena", "start-mcp-server", "--project", "C:/work/alpha", "--context", "codex-placeholder"},
		UpstreamPort:  19121,
		ExternalPort:  9121,
		WorkspacePath: "C:/work/alpha",
	}
	got := serenaProxyChildArgs(spec)
	want := []string{"serena", "start-mcp-server", "--project", "C:/work/alpha", "--context", "codex-placeholder", "--port", "19121"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("childArgs:\n got=%#v\nwant=%#v", got, want)
	}
	// The builder must NOT mutate spec.ChildArgs (append-on-clone).
	if len(spec.ChildArgs) != 6 {
		t.Errorf("spec.ChildArgs mutated; len=%d want=6", len(spec.ChildArgs))
	}
	// --context + --project must survive into the final argv.
	if !sliceContainsPair(got, "--context", "codex-placeholder") {
		t.Errorf("final child argv missing --context; got=%v", got)
	}
	if !sliceContainsPair(got, "--project", "C:/work/alpha") {
		t.Errorf("final child argv missing --project; got=%v", got)
	}
}

// TestSerenaProxy_NilRuntimeSpec_FailsLoud: a descriptor with nil RuntimeSpec
// makes the loader fail loud with an operator-actionable "reinstall" message —
// NO manifest fallback (design §4 boundary defense).
func TestSerenaProxy_NilRuntimeSpec_FailsLoud(t *testing.T) {
	canonical := serenaWorkspaceDir(t)
	d := serenaDescriptorFixture(t, canonical, 9121)
	d.RuntimeSpec = nil // pre-RuntimeSpec / stale row
	seedSerenaIntent(t, []api.SupervisorDaemon{d})

	_, err := loadSerenaProxyRuntimeSpec(d.TaskName, canonical, 9121)
	if err == nil {
		t.Fatal("nil RuntimeSpec must fail loud, got nil error")
	}
	if !strings.Contains(err.Error(), "reinstall") {
		t.Errorf("error must be operator-actionable (mention reinstall); got %q", err.Error())
	}
}

// TestSerenaProxy_DescriptorNotFound_FailsLoud: --task-name resolving to no
// descriptor fails loud (design §4 step 3).
func TestSerenaProxy_DescriptorNotFound_FailsLoud(t *testing.T) {
	canonical := serenaWorkspaceDir(t)
	// Seed a DIFFERENT task so the lookup misses.
	other := serenaDescriptorFixture(t, serenaWorkspaceDir(t), 9122)
	seedSerenaIntent(t, []api.SupervisorDaemon{other})

	missing := api.SerenaTaskNameForWorkspace(canonical)
	_, err := loadSerenaProxyRuntimeSpec(missing, canonical, 9121)
	if err == nil {
		t.Fatal("missing descriptor must fail loud, got nil error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error must name the missing task; got %q", err.Error())
	}
}

// TestSerenaProxy_UnsupportedSpecVersion_FailsLoud: a SpecVersion the binary
// does not support fails loud (no manifest fallback) — design §4 step 4.
func TestSerenaProxy_UnsupportedSpecVersion_FailsLoud(t *testing.T) {
	canonical := serenaWorkspaceDir(t)
	d := serenaDescriptorFixture(t, canonical, 9121)
	d.RuntimeSpec.SpecVersion = api.DaemonRuntimeSpecVersion + 99 // future, unsupported
	seedSerenaIntent(t, []api.SupervisorDaemon{d})

	_, err := loadSerenaProxyRuntimeSpec(d.TaskName, canonical, 9121)
	if err == nil {
		t.Fatal("unsupported SpecVersion must fail loud, got nil error")
	}
	if !strings.Contains(err.Error(), "spec_version") && !strings.Contains(err.Error(), "spec version") {
		t.Errorf("error must name the spec version; got %q", err.Error())
	}
}

// TestSerenaProxy_ArgvSpecMismatch_FailsLoud covers the §3.2 consistency
// contract: when the --port argv disagrees with RuntimeSpec.ExternalPort, OR
// the --workspace argv disagrees with RuntimeSpec.WorkspacePath, the proxy
// fails loud — no silent reconcile to one side.
func TestSerenaProxy_ArgvSpecMismatch_FailsLoud(t *testing.T) {
	canonical := serenaWorkspaceDir(t)
	d := serenaDescriptorFixture(t, canonical, 9121)
	seedSerenaIntent(t, []api.SupervisorDaemon{d})

	t.Run("port_mismatch", func(t *testing.T) {
		_, err := loadSerenaProxyRuntimeSpec(d.TaskName, canonical, 9999) // flag port != ExternalPort 9121
		if err == nil {
			t.Fatal("port mismatch must fail loud, got nil error")
		}
		if !strings.Contains(err.Error(), "port") {
			t.Errorf("error must name the disagreeing port; got %q", err.Error())
		}
	})

	t.Run("workspace_mismatch", func(t *testing.T) {
		otherCanonical := serenaWorkspaceDir(t) // a different real dir != RuntimeSpec.WorkspacePath
		_, err := loadSerenaProxyRuntimeSpec(d.TaskName, otherCanonical, 9121)
		if err == nil {
			t.Fatal("workspace mismatch must fail loud, got nil error")
		}
		if !strings.Contains(err.Error(), "workspace") {
			t.Errorf("error must name the disagreeing workspace; got %q", err.Error())
		}
	})
}

// TestSerenaProxy_NoManifestRead_OnNilSpec is the airtight-fail-loud guard
// (design §4, plan deep-sec (b)): with MCPHUB_MANIFEST_DIR_OVERRIDE pointing
// at an empty dir (so ANY manifest read would error), a nil-RuntimeSpec
// descriptor still fails with the descriptor-level "reinstall" message — NOT
// a manifest-read error. This proves the proxy never falls back to a manifest
// read on a missing/inconsistent spec.
func TestSerenaProxy_NoManifestRead_OnNilSpec(t *testing.T) {
	emptyDir := t.TempDir() // contains no serena manifest; a ManifestGet here would fail loudly
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", emptyDir)

	canonical := serenaWorkspaceDir(t)
	d := serenaDescriptorFixture(t, canonical, 9121)
	d.RuntimeSpec = nil
	seedSerenaIntent(t, []api.SupervisorDaemon{d})

	_, err := loadSerenaProxyRuntimeSpec(d.TaskName, canonical, 9121)
	if err == nil {
		t.Fatal("nil spec must fail loud even with manifest override set")
	}
	// The error must be the descriptor-level fail-loud, NOT a manifest read
	// error — proving NO fallback read happened.
	if !strings.Contains(err.Error(), "reinstall") {
		t.Errorf("error must be the descriptor-level reinstall message (no manifest fallback); got %q", err.Error())
	}
	if strings.Contains(err.Error(), "manifest") && strings.Contains(strings.ToLower(err.Error()), "load") {
		t.Errorf("error looks like a manifest-load fallback — the proxy must NOT read the manifest; got %q", err.Error())
	}
}

// --- tiny test-local helpers (avoid strconv/fmt in test body) ---

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func sliceContainsPair(args []string, a, c string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == c {
			return true
		}
	}
	return false
}
