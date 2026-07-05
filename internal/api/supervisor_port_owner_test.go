package api

import "testing"

// stubOwnerResolver swaps resolveManifestPortAndDeadlineFn (the owner's manifest
// reader) for a hermetic table keyed "server/daemon" → (port, deadlineSecs).
func stubOwnerResolver(t *testing.T, table map[string][2]int) {
	t.Helper()
	prev := resolveManifestPortAndDeadlineFn
	t.Cleanup(func() { resolveManifestPortAndDeadlineFn = prev })
	resolveManifestPortAndDeadlineFn = func(server, daemon string) (int, int, bool) {
		v, ok := table[server+"/"+daemon]
		if !ok {
			return 0, 0, false
		}
		return v[0], v[1], true
	}
}

// --- EffectiveDaemonPort (AC1) ---

func TestEffectiveDaemonPort_PortShortCircuit(t *testing.T) {
	// A row with Port>0 resolves to that port WITHOUT consulting the manifest —
	// prove it by making the resolver panic if called.
	prev := resolveManifestPortAndDeadlineFn
	t.Cleanup(func() { resolveManifestPortAndDeadlineFn = prev })
	resolveManifestPortAndDeadlineFn = func(string, string) (int, int, bool) {
		t.Fatalf("manifest resolver must not be consulted for a Port>0 row")
		return 0, 0, false
	}
	d := SupervisorDaemon{Server: "memory", Daemon: "default", Port: 9123}
	got, ok := EffectiveDaemonPort(d)
	if !ok || got != 9123 {
		t.Fatalf("EffectiveDaemonPort(Port=9123) = (%d,%v), want (9123,true)", got, ok)
	}
}

func TestEffectiveDaemonPort_ManifestFallbackForPortZero(t *testing.T) {
	stubOwnerResolver(t, map[string][2]int{"memory/default": {9123, 0}})
	d := SupervisorDaemon{Server: "memory", Daemon: "default", Port: 0,
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"}}
	got, ok := EffectiveDaemonPort(d)
	if !ok || got != 9123 {
		t.Fatalf("Port=0 manifest fallback = (%d,%v), want (9123,true)", got, ok)
	}
}

func TestEffectiveDaemonPort_RenamedManifestNotOK(t *testing.T) {
	stubOwnerResolver(t, map[string][2]int{}) // resolver returns !ok for all
	d := SupervisorDaemon{Server: "ghost", Daemon: "default", Port: 0,
		Args: []string{"daemon", "--server", "ghost", "--daemon", "default"}}
	if got, ok := EffectiveDaemonPort(d); ok || got != 0 {
		t.Fatalf("renamed/removed manifest = (%d,%v), want (0,false)", got, ok)
	}
}

func TestEffectiveDaemonPort_TimerRowNotOK(t *testing.T) {
	stubOwnerResolver(t, map[string][2]int{})
	// A portless maintenance-timer row: no --server/--daemon args → identity !ok.
	d := SupervisorDaemon{TaskName: `\mcp-local-hub-workspace-weekly-refresh`,
		Args: []string{"workspace-weekly-refresh"}, Port: 0}
	if got, ok := EffectiveDaemonPort(d); ok || got != 0 {
		t.Fatalf("timer row = (%d,%v), want (0,false)", got, ok)
	}
}

// --- EffectiveStartupBindDeadlineSeconds (AC2 + §4b) ---

func TestEffectiveStartupBindDeadline_ExplicitWins(t *testing.T) {
	stubOwnerResolver(t, map[string][2]int{"memory/default": {9123, 45}})
	d := SupervisorDaemon{Server: "memory", Daemon: "default", StartupBindDeadlineSeconds: 300,
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"}}
	if got := EffectiveStartupBindDeadlineSeconds(d); got != 300 {
		t.Fatalf("explicit field = %d, want 300", got)
	}
}

func TestEffectiveStartupBindDeadline_ManifestOverDefault(t *testing.T) {
	stubOwnerResolver(t, map[string][2]int{"gdb/default": {9129, 45}})
	d := SupervisorDaemon{Server: "gdb", Daemon: "default",
		Args: []string{"daemon", "--server", "gdb", "--daemon", "default"}}
	if got := EffectiveStartupBindDeadlineSeconds(d); got != 45 {
		t.Fatalf("manifest deadline = %d, want 45", got)
	}
}

func TestEffectiveStartupBindDeadline_DefaultSixty(t *testing.T) {
	stubOwnerResolver(t, map[string][2]int{"memory/default": {9123, 0}}) // manifest declares no deadline
	d := SupervisorDaemon{Server: "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"}}
	if got := EffectiveStartupBindDeadlineSeconds(d); got != defaultStartupBindDeadlineSeconds {
		t.Fatalf("global default = %d, want %d", got, defaultStartupBindDeadlineSeconds)
	}
}

// §4b: serena by SERVER IDENTITY gets 120 for BOTH shapes — the legacy-unified
// `unified` daemon AND the dynamic-pool proxy whose daemon name is a workspace
// hash the manifest does not declare.
func TestEffectiveStartupBindDeadline_SerenaUnifiedIdentity120(t *testing.T) {
	stubOwnerResolver(t, map[string][2]int{"serena/unified": {9121, 0}}) // no manifest deadline
	d := SupervisorDaemon{Server: "serena", Daemon: "unified",
		Args: []string{"daemon", "--server", "serena", "--daemon", "unified"}}
	if got := EffectiveStartupBindDeadlineSeconds(d); got != serenaStartupBindDeadlineSeconds {
		t.Fatalf("legacy-unified serena = %d, want %d", got, serenaStartupBindDeadlineSeconds)
	}
}

func TestEffectiveStartupBindDeadline_SerenaWorkspaceHashIdentity120(t *testing.T) {
	// The workspace-hash daemon name MISSES the manifest (resolver !ok), so only
	// server-identity keying gives it 120 — the §4a-regression this fixes.
	stubOwnerResolver(t, map[string][2]int{}) // manifest lookup misses the hash
	d := SupervisorDaemon{Server: "serena", Daemon: "6935d24c",
		Args: []string{"daemon", "--server", "serena", "--daemon", "6935d24c"}}
	if got := EffectiveStartupBindDeadlineSeconds(d); got != serenaStartupBindDeadlineSeconds {
		t.Fatalf("workspace-hash serena = %d, want %d (server-identity, not manifest)", got, serenaStartupBindDeadlineSeconds)
	}
}

func TestEffectiveStartupBindDeadline_NonSerenaMissStaysSixty(t *testing.T) {
	stubOwnerResolver(t, map[string][2]int{}) // non-serena manifest miss
	d := SupervisorDaemon{Server: "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"}}
	if got := EffectiveStartupBindDeadlineSeconds(d); got != defaultStartupBindDeadlineSeconds {
		t.Fatalf("non-serena miss = %d, want %d (identity default is only for serena)", got, defaultStartupBindDeadlineSeconds)
	}
}

func TestEffectiveStartupBindDeadline_IndependentOfPort(t *testing.T) {
	// A port-stamped (Port>0) but deadline-zero row still resolves the manifest
	// deadline — the port short-circuit must not gate the deadline.
	stubOwnerResolver(t, map[string][2]int{"gdb/default": {9129, 45}})
	d := SupervisorDaemon{Server: "gdb", Daemon: "default", Port: 9129,
		Args: []string{"daemon", "--server", "gdb", "--daemon", "default"}}
	if got := EffectiveStartupBindDeadlineSeconds(d); got != 45 {
		t.Fatalf("deadline with Port>0 = %d, want 45 (independent of port)", got)
	}
}

// --- DaemonPortResolver memo (AC3) ---

func TestDaemonPortResolver_ParsesManifestOncePerServer(t *testing.T) {
	calls := map[string]int{}
	prev := resolveManifestPortAndDeadlineFn
	t.Cleanup(func() { resolveManifestPortAndDeadlineFn = prev })
	resolveManifestPortAndDeadlineFn = func(server, daemon string) (int, int, bool) {
		calls[server+"/"+daemon]++
		return 9123, 0, true
	}
	r := NewDaemonPortResolver()
	d := SupervisorDaemon{Server: "memory", Daemon: "default", Port: 0,
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"}}
	// Resolve twice: EffectiveDaemonPort + EffectiveStartupBindDeadline both hit
	// the memo, and a second Resolve of the same descriptor must not re-read.
	r.Resolve(d)
	r.Resolve(d)
	if calls["memory/default"] != 1 {
		t.Fatalf("manifest read %d times, want exactly 1 (memoized)", calls["memory/default"])
	}
	// And the memoized values match the pure calls.
	port, deadline, ok := r.Resolve(d)
	if !ok || port != 9123 || deadline != defaultStartupBindDeadlineSeconds {
		t.Fatalf("memoized Resolve = (%d,%d,%v), want (9123,%d,true)", port, deadline, ok, defaultStartupBindDeadlineSeconds)
	}
}

// --- DescriptorServerDaemon identity + fail-closed mismatch (AC4) ---

func TestDescriptorServerDaemon_AgreeingFields(t *testing.T) {
	d := SupervisorDaemon{Server: "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"}}
	s, dm, ok := DescriptorServerDaemon(d)
	if !ok || s != "memory" || dm != "default" {
		t.Fatalf("agreeing fields = (%q,%q,%v), want (memory,default,true)", s, dm, ok)
	}
}

func TestDescriptorServerDaemon_RecoversBlankFieldsFromArgs(t *testing.T) {
	d := SupervisorDaemon{Args: []string{"daemon", "--server", "paper-search-mcp", "--daemon", "default"}}
	s, dm, ok := DescriptorServerDaemon(d)
	if !ok || s != "paper-search-mcp" || dm != "default" {
		t.Fatalf("blank-field recovery = (%q,%q,%v), want (paper-search-mcp,default,true)", s, dm, ok)
	}
}

func TestDescriptorServerDaemon_RejectsFieldArgvMismatch(t *testing.T) {
	// Server field disagrees with the --server argv token → fail-closed ok=false,
	// so no port-decision stamps a port the process (which launches from args)
	// never binds.
	d := SupervisorDaemon{Server: "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "time", "--daemon", "default"}}
	if s, dm, ok := DescriptorServerDaemon(d); ok {
		t.Fatalf("field/argv mismatch = (%q,%q,%v), want ok=false", s, dm, ok)
	}
	// Daemon field disagrees.
	d2 := SupervisorDaemon{Server: "memory", Daemon: "alpha",
		Args: []string{"daemon", "--server", "memory", "--daemon", "beta"}}
	if _, _, ok := DescriptorServerDaemon(d2); ok {
		t.Fatalf("daemon field/argv mismatch must be ok=false")
	}
}
