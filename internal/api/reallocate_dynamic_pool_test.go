package api

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// setPortAvailableForTest swaps the OS-bind probe seam so AllocatePort's port
// selection is deterministic. avail maps port→available; a port absent from the
// map defaults to `dflt`.
func setPortAvailableForTest(t *testing.T, avail map[int]bool, dflt bool) {
	t.Helper()
	prev := portAvailable
	portAvailable = func(port int) bool {
		if v, ok := avail[port]; ok {
			return v
		}
		return dflt
	}
	t.Cleanup(func() { portAvailable = prev })
}

func seedRegistrySerenaRow(t *testing.T, regPath, task string, port int) {
	t.Helper()
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  "wskey",
		WorkspacePath: `C:\proj`,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          port,
		TaskName:      task,
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
}

func seedRegistryLSPRow(t *testing.T, regPath, task string, port int) {
	t.Helper()
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "wskey",
		WorkspacePath: `C:\ws`,
		Language:      "go",
		Backend:       "mcp-language-server",
		Port:          port,
		TaskName:      task,
	})
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
}

func seedReallocIntent(t *testing.T, intentPath string, d SupervisorDaemon) {
	t.Helper()
	if err := MutateSupervisorIntentIfChanged(intentPath, func(f *SupervisorIntentFile) (bool, error) {
		f.Version = 1
		f.Daemons = []SupervisorDaemon{d}
		return true, nil
	}); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
}

func serenaDaemonWithRuntimeSpec(task string, port int) SupervisorDaemon {
	return SupervisorDaemon{
		TaskName: task,
		Server:   "serena",
		Daemon:   "wskey",
		Command:  `C:\mcphub.exe`,
		Args: []string{
			"daemon", "serena-proxy",
			"--server", "serena",
			"--workspace", `C:\proj`,
			"--port", itoa(port),
			"--task-name", task,
		},
		Port:      port,
		Workspace: `C:\proj`,
		RuntimeSpec: &DaemonRuntimeSpec{
			SpecVersion:   DaemonRuntimeSpecVersion,
			ChildCommand:  "uvx",
			ChildArgs:     []string{"--project", `C:\proj`, "--context", "codex"},
			ExternalPort:  port,
			UpstreamPort:  port + 10000,
			WorkspacePath: `C:\proj`,
		},
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func descriptorArgPortForTest(d SupervisorDaemon) int {
	for i := 2; i+1 < len(d.Args); i++ {
		if d.Args[i] == "--port" {
			if p, err := strconv.Atoi(d.Args[i+1]); err == nil {
				return p
			}
		}
	}
	return 0
}

func assertNoClientConfigFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	allowed := map[string]bool{
		"workspaces.yaml":             true,
		"workspaces.yaml.lock":        true,
		"supervisor-intent.json":      true,
		"supervisor-intent.json.lock": true,
	}
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("reallocation wrote an unexpected file %q; a self-heal must touch ZERO client configs (only workspaces.yaml + supervisor-intent.json)", e.Name())
		}
	}
}

func TestReallocateDynamicPoolPort_Serena_MovesRegistryAndDescriptorTogether(t *testing.T) {
	// Old port 9151 is "stolen" (OS-unavailable); the rest of the serena pool
	// (9150-9199) is free. AllocatePort must skip 9151 and the registry entry.
	setPortAvailableForTest(t, map[int]bool{9151: false}, true)

	dir := hardenedTempDir(t)
	regPath := filepath.Join(dir, "workspaces.yaml")
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	task := `\mcp-local-hub-serena-b133f336`
	oldPort := 9151

	seedRegistrySerenaRow(t, regPath, task, oldPort)
	seedReallocIntent(t, intentPath, serenaDaemonWithRuntimeSpec(task, oldPort))

	newPort, err := ReallocateDynamicPoolPort(regPath, intentPath, serenaDaemonWithRuntimeSpec(task, oldPort))
	if err != nil {
		t.Fatalf("ReallocateDynamicPoolPort: %v", err)
	}
	if newPort == oldPort || newPort < 9150 || newPort > 9199 {
		t.Fatalf("newPort = %d, want a fresh serena-pool port (9150-9199) != %d", newPort, oldPort)
	}

	// Registry row updated.
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	row, ok := reg.GetSerena("wskey")
	if !ok {
		t.Fatal("serena registry row vanished")
	}
	if row.Port != newPort {
		t.Errorf("registry row port = %d, want %d", row.Port, newPort)
	}

	// Descriptor Port + --port argv + RuntimeSpec External/Upstream all agree.
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	dd := intent.FindSupervisorDaemonByTaskName(task)
	if dd == nil {
		t.Fatal("descriptor vanished from intent")
	}
	if dd.Port != newPort {
		t.Errorf("descriptor.Port = %d, want %d", dd.Port, newPort)
	}
	if got := descriptorArgPortForTest(*dd); got != newPort {
		t.Errorf("--port argv = %d, want %d (field↔argv disagreement)", got, newPort)
	}
	if dd.RuntimeSpec == nil {
		t.Fatal("RuntimeSpec dropped")
	}
	if dd.RuntimeSpec.ExternalPort != newPort {
		t.Errorf("RuntimeSpec.ExternalPort = %d, want %d", dd.RuntimeSpec.ExternalPort, newPort)
	}
	if want := newPort + 10000; dd.RuntimeSpec.UpstreamPort != want {
		t.Errorf("RuntimeSpec.UpstreamPort = %d, want %d (ExternalPort + offset invariant broken)", dd.RuntimeSpec.UpstreamPort, want)
	}
	// The field↔argv fail-closed guard never trips: EffectiveDaemonPort resolves
	// the new port (d.Port>0 and equal to argv).
	if p, ok := EffectiveDaemonPort(*dd); !ok || p != newPort {
		t.Errorf("EffectiveDaemonPort = (%d,%v), want (%d,true)", p, ok, newPort)
	}

	// ZERO client-config writes: the state dir holds only the two state files.
	assertNoClientConfigFiles(t, dir)
}

func TestReallocateDynamicPoolPort_LSP_UsesManifestPool(t *testing.T) {
	setPortAvailableForTest(t, map[int]bool{9401: false}, true)

	dir := hardenedTempDir(t)
	regPath := filepath.Join(dir, "workspaces.yaml")
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	task := `\mcp-local-hub-mcp-language-server-go-abc`
	oldPort := 9401

	seedRegistryLSPRow(t, regPath, task, oldPort)
	d := SupervisorDaemon{
		TaskName: task,
		Server:   "mcp-language-server",
		Daemon:   "go-abc",
		Command:  `C:\mcphub.exe`,
		Args:     []string{"daemon", "workspace-proxy", "--port", itoa(oldPort), "--workspace", `C:\ws`, "--language", "go"},
		Port:     oldPort,
	}
	seedReallocIntent(t, intentPath, d)

	newPort, err := ReallocateDynamicPoolPort(regPath, intentPath, d)
	if err != nil {
		t.Fatalf("ReallocateDynamicPoolPort: %v", err)
	}
	if newPort == oldPort || newPort < 9400 || newPort > 9599 {
		t.Fatalf("newPort = %d, want a fresh LSP-pool port (9400-9599) != %d", newPort, oldPort)
	}
	intent, _ := ReadSupervisorIntent(intentPath)
	dd := intent.FindSupervisorDaemonByTaskName(task)
	if dd.Port != newPort || descriptorArgPortForTest(*dd) != newPort {
		t.Errorf("LSP descriptor.Port=%d argv=%d, want both %d", dd.Port, descriptorArgPortForTest(*dd), newPort)
	}
	if dd.RuntimeSpec != nil {
		t.Error("LSP descriptor should carry no RuntimeSpec")
	}
	assertNoClientConfigFiles(t, dir)
}

func TestReallocateDynamicPoolPort_PoolExhausted_NoChange(t *testing.T) {
	// Every serena-pool port is OS-unavailable → ErrPortPoolExhausted, and both
	// state files are left UNCHANGED.
	setPortAvailableForTest(t, nil, false)

	dir := hardenedTempDir(t)
	regPath := filepath.Join(dir, "workspaces.yaml")
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	task := `\mcp-local-hub-serena-b133f336`
	oldPort := 9151

	seedRegistrySerenaRow(t, regPath, task, oldPort)
	seedReallocIntent(t, intentPath, serenaDaemonWithRuntimeSpec(task, oldPort))

	_, err := ReallocateDynamicPoolPort(regPath, intentPath, serenaDaemonWithRuntimeSpec(task, oldPort))
	if !errors.Is(err, ErrPortPoolExhausted) {
		t.Fatalf("err = %v, want ErrPortPoolExhausted", err)
	}
	// Registry + descriptor still at the old port (no partial write).
	reg := NewRegistry(regPath)
	_ = reg.Load()
	if row, _ := reg.GetSerena("wskey"); row.Port != oldPort {
		t.Errorf("registry row moved to %d on pool-exhaustion; want unchanged %d", row.Port, oldPort)
	}
	intent, _ := ReadSupervisorIntent(intentPath)
	if dd := intent.FindSupervisorDaemonByTaskName(task); dd == nil || dd.Port != oldPort {
		t.Errorf("descriptor moved on pool-exhaustion; want unchanged %d", oldPort)
	}
}

func TestReallocateDynamicPoolPort_LSP_Step4Failure_CompensatesRegistry(t *testing.T) {
	// P1-2: an LSP proxy's fail-closed startup check (daemon_workspace.go:
	// entry.Port != --port → exit 1, a NORMAL crash the self-heal never re-drives)
	// would BRICK the daemon FOREVER if the two-store write left registry=newPort
	// while the descriptor still named oldPort. On a step-4 (intent) write failure
	// the reallocation MUST COMPENSATE by reverting the registry row to oldPort so
	// both stores stay consistent and the LSP daemon can still START on oldPort.
	//
	// NON-VACUITY: without the compensation the registry row stays at the allocated
	// newPort and the `row.Port == oldPort` assertion below FAILS.
	setPortAvailableForTest(t, map[int]bool{9401: false}, true)

	dir := hardenedTempDir(t)
	regPath := filepath.Join(dir, "workspaces.yaml")
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	task := `\mcp-local-hub-mcp-language-server-go-abc`
	oldPort := 9401

	seedRegistryLSPRow(t, regPath, task, oldPort)
	d := SupervisorDaemon{
		TaskName: task,
		Server:   "mcp-language-server",
		Daemon:   "go-abc",
		Command:  `C:\mcphub.exe`,
		Args:     []string{"daemon", "workspace-proxy", "--port", itoa(oldPort), "--workspace", `C:\ws`, "--language", "go"},
		Port:     oldPort,
	}
	seedReallocIntent(t, intentPath, d)

	// Inject a step-4 (intent) write failure (disk full / DACL / AV / crash).
	prev := reallocMutateIntentFn
	reallocMutateIntentFn = func(string, func(*SupervisorIntentFile) (bool, error)) error {
		return errors.New("injected intent write failure")
	}
	t.Cleanup(func() { reallocMutateIntentFn = prev })

	if _, err := ReallocateDynamicPoolPort(regPath, intentPath, d); err == nil {
		t.Fatal("expected an error on a step-4 intent write failure; got nil")
	}

	// Compensation: the registry row is reverted to oldPort.
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	row, ok := reg.Get("wskey", "go")
	if !ok {
		t.Fatal("LSP registry row vanished")
	}
	if row.Port != oldPort {
		t.Fatalf("registry row port = %d after a compensated step-4 failure, want %d (uncompensated → registry=newPort → LSP exits 1 → quarantine forever)", row.Port, oldPort)
	}

	// STARTABILITY: the (untouched) descriptor is still at oldPort, and the reverted
	// registry now AGREES — so the LSP fail-closed startup check (entry.Port ==
	// --port) passes and the daemon can START. This is the load-bearing property:
	// not merely that AllocatePort would skip a port, but that both stores are
	// consistent on a bindable port.
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	dd := intent.FindSupervisorDaemonByTaskName(task)
	if dd == nil {
		t.Fatal("descriptor vanished from intent")
	}
	if dd.Port != oldPort || descriptorArgPortForTest(*dd) != oldPort {
		t.Fatalf("descriptor Port=%d argv=%d after compensation, want both %d", dd.Port, descriptorArgPortForTest(*dd), oldPort)
	}
	if row.Port != descriptorArgPortForTest(*dd) {
		t.Fatalf("registry port %d != descriptor --port %d — the LSP fail-closed startup check would exit 1 (brick)", row.Port, descriptorArgPortForTest(*dd))
	}
}

// TestCloneIntentWithReallocatedPort (fable P3): a direct unit test for the supervisor
// loop's FIX-3b targeted cache patch owner. It must (a) move ONLY the target descriptor's
// Port + --port argv + serena RuntimeSpec External/Upstream to newPort, (b) leave sibling
// descriptors untouched, (c) NOT mutate the SOURCE snapshot (a fresh Daemons backing array
// + deep-copied target args/RuntimeSpec, so off-loop cache readers holding the source are
// never touched), and (d) return (nil,false) on nil src / absent descriptor / no --port
// argv / newPort<=0.
func TestCloneIntentWithReallocatedPort(t *testing.T) {
	const oldPort, newPort, sibPort = 9151, 9170, 9160
	target := serenaDaemonWithRuntimeSpec(`\mcp-local-hub-serena-target`, oldPort)
	sibling := serenaDaemonWithRuntimeSpec(`\mcp-local-hub-serena-sibling`, sibPort)
	src := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{target, sibling}}

	got, ok := CloneIntentWithReallocatedPort(src, target.TaskName, newPort)
	if !ok || got == nil {
		t.Fatalf("CloneIntentWithReallocatedPort = (%v,%v), want (non-nil,true)", got, ok)
	}

	// (a) Target moved consistently: Port + --port argv + RuntimeSpec External/Upstream.
	gt := got.Daemons[0]
	if gt.Port != newPort {
		t.Errorf("clone target Port = %d, want %d", gt.Port, newPort)
	}
	if p := descriptorArgPortForTest(gt); p != newPort {
		t.Errorf("clone target --port argv = %d, want %d", p, newPort)
	}
	if gt.RuntimeSpec == nil {
		t.Fatal("clone target lost its RuntimeSpec")
	}
	if gt.RuntimeSpec.ExternalPort != newPort {
		t.Errorf("clone target RuntimeSpec.ExternalPort = %d, want %d", gt.RuntimeSpec.ExternalPort, newPort)
	}
	if want := newPort + 10000; gt.RuntimeSpec.UpstreamPort != want {
		t.Errorf("clone target RuntimeSpec.UpstreamPort = %d, want %d (External+offset)", gt.RuntimeSpec.UpstreamPort, want)
	}

	// (b) Sibling untouched in the clone.
	gs := got.Daemons[1]
	if gs.Port != sibPort || descriptorArgPortForTest(gs) != sibPort ||
		gs.RuntimeSpec == nil || gs.RuntimeSpec.ExternalPort != sibPort || gs.RuntimeSpec.UpstreamPort != sibPort+10000 {
		t.Errorf("clone sibling was disturbed: Port=%d argv=%d spec=%+v, want all at %d", gs.Port, descriptorArgPortForTest(gs), gs.RuntimeSpec, sibPort)
	}

	// (c) SOURCE snapshot NOT mutated — the clone deep-copied the target before the
	// in-place rewrite, so the original still names oldPort in Port + argv + RuntimeSpec.
	st := src.Daemons[0]
	if st.Port != oldPort || descriptorArgPortForTest(st) != oldPort ||
		st.RuntimeSpec.ExternalPort != oldPort || st.RuntimeSpec.UpstreamPort != oldPort+10000 {
		t.Errorf("source target mutated by the clone: Port=%d argv=%d spec=%+v, want all at %d", st.Port, descriptorArgPortForTest(st), st.RuntimeSpec, oldPort)
	}
	// Fresh Daemons backing array (the clone must not alias the source slice).
	if &got.Daemons[0] == &src.Daemons[0] {
		t.Error("clone aliases the SOURCE Daemons backing array; an off-loop reader holding the source would see the mutation")
	}
	// Target args + RuntimeSpec deep-copied (distinct backing / pointer from the source).
	if &gt.Args[0] == &st.Args[0] {
		t.Error("clone target shares the SOURCE args backing array; the in-place rewrite would corrupt the source")
	}
	if gt.RuntimeSpec == st.RuntimeSpec {
		t.Error("clone target shares the SOURCE RuntimeSpec pointer; the port move would corrupt the source")
	}

	// (d) Rejections → (nil,false).
	if g, ok := CloneIntentWithReallocatedPort(nil, target.TaskName, newPort); ok || g != nil {
		t.Errorf("nil src → (%v,%v), want (nil,false)", g, ok)
	}
	if g, ok := CloneIntentWithReallocatedPort(src, `\mcp-local-hub-serena-absent`, newPort); ok || g != nil {
		t.Errorf("absent descriptor → (%v,%v), want (nil,false)", g, ok)
	}
	if g, ok := CloneIntentWithReallocatedPort(src, target.TaskName, 0); ok || g != nil {
		t.Errorf("newPort=0 → (%v,%v), want (nil,false)", g, ok)
	}
	if g, ok := CloneIntentWithReallocatedPort(src, target.TaskName, -1); ok || g != nil {
		t.Errorf("newPort<0 → (%v,%v), want (nil,false)", g, ok)
	}
	// No --port argv → false (the field↔argv consistency cannot be met).
	noPort := SupervisorDaemon{
		TaskName: `\mcp-local-hub-serena-noport`,
		Server:   "serena",
		Args:     []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", `C:\proj`, "--task-name", `\mcp-local-hub-serena-noport`},
		Port:     oldPort,
	}
	srcNoPort := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{noPort}}
	if g, ok := CloneIntentWithReallocatedPort(srcNoPort, noPort.TaskName, newPort); ok || g != nil {
		t.Errorf("descriptor without a --port argv → (%v,%v), want (nil,false)", g, ok)
	}
}

func TestReallocateDynamicPoolPort_NotDynamicProxy_Refused(t *testing.T) {
	dir := hardenedTempDir(t)
	regPath := filepath.Join(dir, "workspaces.yaml")
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	global := SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
		Args:     []string{"daemon", "--server", "memory", "--daemon", "default"},
		Port:     9123,
	}
	if _, err := ReallocateDynamicPoolPort(regPath, intentPath, global); err == nil {
		t.Fatal("expected refusal for a fixed-port global daemon; got nil error")
	}
}

func TestReallocateDynamicPoolPort_CrashConsistency_SkipsReservedPort(t *testing.T) {
	// Old port 9151 stolen; simulate a crash BETWEEN the registry write and the
	// intent write: the registry row already holds the first new port, but the
	// descriptor still names the old port. A re-heal must SKIP the reserved first
	// new port (no cross-daemon double-alloc) AND the still-stolen old port.
	setPortAvailableForTest(t, map[int]bool{9151: false}, true)

	dir := hardenedTempDir(t)
	regPath := filepath.Join(dir, "workspaces.yaml")
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	task := `\mcp-local-hub-serena-b133f336`
	oldPort := 9151

	seedRegistrySerenaRow(t, regPath, task, oldPort)
	seedReallocIntent(t, intentPath, serenaDaemonWithRuntimeSpec(task, oldPort))

	firstNew, err := ReallocateDynamicPoolPort(regPath, intentPath, serenaDaemonWithRuntimeSpec(task, oldPort))
	if err != nil {
		t.Fatalf("first realloc: %v", err)
	}
	// Simulate the crash: revert ONLY the descriptor back to the old port,
	// leaving the registry reserving firstNew (the crash-between-3-and-4 state).
	if err := MutateSupervisorIntentIfChanged(intentPath, func(f *SupervisorIntentFile) (bool, error) {
		// Mutate by index (FindSupervisorDaemonByTaskName returns a copy).
		f.Daemons[0].Port = oldPort
		f.Daemons[0].RuntimeSpec.ExternalPort = oldPort
		for i := range f.Daemons[0].Args {
			if f.Daemons[0].Args[i] == "--port" && i+1 < len(f.Daemons[0].Args) {
				f.Daemons[0].Args[i+1] = itoa(oldPort)
			}
		}
		return true, nil
	}); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	secondNew, err := ReallocateDynamicPoolPort(regPath, intentPath, serenaDaemonWithRuntimeSpec(task, oldPort))
	if err != nil {
		t.Fatalf("re-heal: %v", err)
	}
	if secondNew == firstNew {
		t.Fatalf("re-heal re-allocated the reserved port %d (cross-daemon double-alloc)", firstNew)
	}
	if secondNew == oldPort {
		t.Fatalf("re-heal re-allocated the still-stolen old port %d", oldPort)
	}
}
