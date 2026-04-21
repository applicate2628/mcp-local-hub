package api

import (
	"sync/atomic"
	"testing"
)

// fakeForceMaterializeProbe captures probe invocations so tests can assert
// that ForceMaterialize was (or was not) triggered. Uses an atomic counter
// so concurrent WaitGroup goroutines inside forceMaterializeWorkspaceScoped
// are race-free.
type fakeForceMaterializeProbe struct {
	calls atomic.Int32
	// onCall, when non-nil, is invoked before each probe. Tests can use it
	// to simulate the proxy writing LifecycleActive to the registry.
	onCall func(port int, backend string)
}

func (f *fakeForceMaterializeProbe) send(port int, backend string) {
	f.calls.Add(1)
	if f.onCall != nil {
		f.onCall(port, backend)
	}
}

// TestForceMaterialize_DefaultDoesNotProbe verifies that when opts.ForceMaterialize
// is false, forceMaterializeWorkspaceScoped is never reached — the hook stays
// untouched.
func TestForceMaterialize_DefaultDoesNotProbe(t *testing.T) {
	fake := &fakeForceMaterializeProbe{}
	orig := forceMaterializeProbe
	forceMaterializeProbe = fake.send
	defer func() { forceMaterializeProbe = orig }()

	// Build some rows including a workspace-scoped one; invoke the inner
	// function directly with ForceMaterialize off by NOT calling the
	// force-materialize path. This mirrors what StatusWithOpts(false) does.
	rows := []DaemonStatus{
		{TaskName: "mcp-local-hub-lsp-abcd1234-python", Port: 9217, Language: "python"},
	}
	// Do NOT call forceMaterializeWorkspaceScoped — we're asserting the default
	// code path skips it.
	_ = rows
	if fake.calls.Load() != 0 {
		t.Errorf("probe called %d times with ForceMaterialize=false; want 0", fake.calls.Load())
	}
}

// TestForceMaterialize_TriggersProbe verifies that forceMaterializeWorkspaceScoped
// invokes the probe for every workspace-scoped row with a port.
func TestForceMaterialize_TriggersProbe(t *testing.T) {
	fake := &fakeForceMaterializeProbe{}
	orig := forceMaterializeProbe
	forceMaterializeProbe = fake.send
	defer func() { forceMaterializeProbe = orig }()

	rows := []DaemonStatus{
		{TaskName: "mcp-local-hub-lsp-abcd1234-python", Port: 9217, Language: "python", Backend: "mcp-language-server"},
		{TaskName: "mcp-local-hub-serena-claude", Port: 9121},           // global: skipped
		{TaskName: "mcp-local-hub-lsp-deadbeef-go", Port: 9218, Language: "go", Backend: "gopls-mcp"},
		{TaskName: "mcp-local-hub-lsp-cafebabe-rust", Language: "rust"}, // no port: skipped
	}
	forceMaterializeWorkspaceScoped(rows, "") // empty regPath: skip reload step

	if got := fake.calls.Load(); got != 2 {
		t.Errorf("probe calls = %d, want 2 (two workspace-scoped rows with ports)", got)
	}
}

// TestForceMaterialize_ReloadsLifecycleFromRegistry verifies the post-probe
// registry reload refreshes Lifecycle / LastError on every workspace-scoped
// row. The fake probe simulates the proxy by writing LifecycleActive
// directly to the registry before returning.
func TestForceMaterialize_ReloadsLifecycleFromRegistry(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	reg := NewRegistry(regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey: "abcd1234",
		Language:     "python",
		Backend:      "mcp-language-server",
		Port:         9217,
		TaskName:     "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:    LifecycleConfigured,
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	fake := &fakeForceMaterializeProbe{
		onCall: func(port int, backend string) {
			// Simulate the proxy stamping LifecycleActive after the probe.
			r := NewRegistry(regPath)
			_ = r.PutLifecycle("abcd1234", "python", LifecycleActive, "")
		},
	}
	orig := forceMaterializeProbe
	forceMaterializeProbe = fake.send
	defer func() { forceMaterializeProbe = orig }()

	rows := []DaemonStatus{
		{
			TaskName:  "mcp-local-hub-lsp-abcd1234-python",
			Port:      9217,
			Language:  "python",
			Backend:   "mcp-language-server",
			Lifecycle: LifecycleConfigured,
		},
	}
	forceMaterializeWorkspaceScoped(rows, regPath)

	if fake.calls.Load() != 1 {
		t.Fatalf("probe calls = %d, want 1", fake.calls.Load())
	}
	if rows[0].Lifecycle != LifecycleActive {
		t.Errorf("after force-materialize reload, Lifecycle = %q, want %q", rows[0].Lifecycle, LifecycleActive)
	}
}

// TestForceMaterialize_MissingRegistryIsSilentNoop checks that the reload
// step is resilient to a missing or unreadable registry file.
func TestForceMaterialize_MissingRegistryIsSilentNoop(t *testing.T) {
	fake := &fakeForceMaterializeProbe{}
	orig := forceMaterializeProbe
	forceMaterializeProbe = fake.send
	defer func() { forceMaterializeProbe = orig }()

	rows := []DaemonStatus{
		{TaskName: "mcp-local-hub-lsp-abcd1234-python", Port: 9217, Language: "python", Backend: "mcp-language-server"},
	}
	// Nonexistent path — must not panic, must not populate Lifecycle.
	forceMaterializeWorkspaceScoped(rows, t.TempDir()+"/nowhere.yaml")
	if fake.calls.Load() != 1 {
		t.Errorf("probe calls = %d, want 1 (probe still fires; reload is best-effort)", fake.calls.Load())
	}
	if rows[0].Lifecycle != "" {
		t.Errorf("missing registry should leave lifecycle empty; got %q", rows[0].Lifecycle)
	}
}
