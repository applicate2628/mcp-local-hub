package api

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
)

// serenaLikeManifest returns a manifest resembling the Serena manifest:
// 3 daemons, weekly refresh, and client bindings where Claude, Gemini,
// Antigravity, Cursor, VS Code, and Qwen share the claude-compatible daemon
// while Codex keeps its own daemon.
func serenaLikeManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		Daemons: []config.DaemonSpec{
			{Name: "claude", Port: 9121},
			{Name: "codex", Port: 9122},
			{Name: "antigravity", Port: 9123},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "claude", URLPath: "/mcp"},
			{Client: "codex-cli", Daemon: "codex", URLPath: "/mcp"},
			{Client: "antigravity", Daemon: "claude", URLPath: "/mcp"},
			{Client: "gemini-cli", Daemon: "claude", URLPath: "/mcp"},
			{Client: "cursor", Daemon: "claude", URLPath: "/mcp"},
			{Client: "vscode", Daemon: "claude", URLPath: "/mcp"},
			{Client: "qwen-cli", Daemon: "claude", URLPath: "/mcp"},
		},
		WeeklyRefresh: true,
	}
}

func preparePreflightBinaryChecks(t *testing.T) {
	t.Helper()
	origCanonical := testCanonicalMcphubPathOverride
	origShort := mcphubShortName
	t.Cleanup(func() {
		testCanonicalMcphubPathOverride = origCanonical
		mcphubShortName = origShort
	})
	binDir := t.TempDir()
	canonical := filepath.Join(binDir, "mcphub-test")
	if err := os.WriteFile(canonical, []byte(""), 0755); err != nil {
		t.Fatalf("write fake canonical mcphub: %v", err)
	}
	testCanonicalMcphubPathOverride = canonical
	mcphubShortName = "go"
}

func TestBuildPlan_NoFilter_FullInstall(t *testing.T) {
	m := serenaLikeManifest()
	p, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// 3 daemon tasks + 1 weekly refresh = 4 scheduler tasks.
	if len(p.SchedulerTasks) != 4 {
		t.Errorf("len(SchedulerTasks) = %d, want 4", len(p.SchedulerTasks))
	}
	// Default install targets only safe/default clients.
	if len(p.ClientUpdates) != 3 {
		t.Errorf("len(ClientUpdates) = %d, want 3", len(p.ClientUpdates))
	}
	gotClients := map[string]bool{}
	for _, u := range p.ClientUpdates {
		gotClients[u.Client] = true
	}
	for _, want := range []string{"claude-code", "codex-cli", "cursor"} {
		if !gotClients[want] {
			t.Errorf("default client %q missing from plan: %+v", want, p.ClientUpdates)
		}
	}
	for _, optIn := range []string{"gemini-cli", "antigravity", "qwen-cli", "vscode"} {
		if gotClients[optIn] {
			t.Errorf("opt-in client %q should not be in default plan: %+v", optIn, p.ClientUpdates)
		}
	}
	// Weekly refresh present.
	var sawWeekly bool
	for _, s := range p.SchedulerTasks {
		if strings.Contains(s.Name, "weekly-refresh") {
			sawWeekly = true
		}
	}
	if !sawWeekly {
		t.Error("weekly-refresh task missing in full install")
	}
}

func TestBuildPlan_AllClientsIncludesOptInClients(t *testing.T) {
	m := serenaLikeManifest()
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(p.ClientUpdates) != 7 {
		t.Fatalf("len(ClientUpdates) = %d, want 7: %+v", len(p.ClientUpdates), p.ClientUpdates)
	}
	got := map[string]bool{}
	for _, u := range p.ClientUpdates {
		got[u.Client] = true
	}
	for _, want := range []string{"claude-code", "codex-cli", "cursor", "vscode", "gemini-cli", "qwen-cli", "antigravity"} {
		if !got[want] {
			t.Errorf("client %q missing from all-clients plan", want)
		}
	}
}

func TestBuildPlan_ClientFilterOnlyIncludesRequestedClients(t *testing.T) {
	m := serenaLikeManifest()
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{ClientsInclude: []string{"qwen-cli", "vscode"}})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(p.ClientUpdates) != 2 {
		t.Fatalf("len(ClientUpdates) = %d, want 2: %+v", len(p.ClientUpdates), p.ClientUpdates)
	}
	got := map[string]bool{}
	for _, u := range p.ClientUpdates {
		got[u.Client] = true
		if u.DaemonName != "claude" {
			t.Errorf("client %s daemon = %q, want claude", u.Client, u.DaemonName)
		}
	}
	if !got["qwen-cli"] || !got["vscode"] {
		t.Fatalf("filtered clients missing: %+v", p.ClientUpdates)
	}
	if got["gemini-cli"] || got["antigravity"] || got["claude-code"] || got["cursor"] || got["codex-cli"] {
		t.Fatalf("unexpected client in filtered plan: %+v", p.ClientUpdates)
	}
}

func TestBuildPlan_SingleDaemonFilter_SkipsOthersAndWeeklyRefresh(t *testing.T) {
	m := serenaLikeManifest()
	p, err := BuildPlan(m, "codex")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// Only the codex scheduler task; weekly refresh is skipped for partial installs.
	if len(p.SchedulerTasks) != 1 {
		t.Errorf("len(SchedulerTasks) = %d, want 1 (got: %+v)", len(p.SchedulerTasks), p.SchedulerTasks)
	}
	if len(p.SchedulerTasks) >= 1 && !strings.HasSuffix(p.SchedulerTasks[0].Name, "-codex") {
		t.Errorf("task name %q, want suffix -codex", p.SchedulerTasks[0].Name)
	}
	// Only codex-cli binding (it's the only binding referencing daemon codex).
	if len(p.ClientUpdates) != 1 {
		t.Errorf("len(ClientUpdates) = %d, want 1 (got: %+v)", len(p.ClientUpdates), p.ClientUpdates)
	}
	if len(p.ClientUpdates) >= 1 && p.ClientUpdates[0].Client != "codex-cli" {
		t.Errorf("client = %q, want codex-cli", p.ClientUpdates[0].Client)
	}
	if len(p.ClientUpdates) >= 1 && !strings.Contains(p.ClientUpdates[0].URL, ":9122/") {
		t.Errorf("url = %q, want port 9122", p.ClientUpdates[0].URL)
	}
}

func TestBuildPlan_SharedDaemonFilter_IncludesAllReferencingBindings(t *testing.T) {
	m := serenaLikeManifest()
	// claude daemon is referenced by every non-Codex binding; all-clients mode
	// preserves that relationship when explicitly requested.
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{DaemonFilter: "claude", IncludeAllClients: true})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.SchedulerTasks) != 1 {
		t.Errorf("len(SchedulerTasks) = %d, want 1", len(p.SchedulerTasks))
	}
	if len(p.ClientUpdates) != 6 {
		t.Errorf("len(ClientUpdates) = %d, want 6 (all non-Codex clients share claude daemon)", len(p.ClientUpdates))
	}
	saw := map[string]bool{}
	for _, u := range p.ClientUpdates {
		saw[u.Client] = true
	}
	for _, want := range []string{"claude-code", "gemini-cli", "antigravity", "cursor", "vscode", "qwen-cli"} {
		if !saw[want] {
			t.Errorf("expected %s binding; got: %+v", want, p.ClientUpdates)
		}
	}
}

func TestBuildPlan_UnknownDaemonFilter_Errors(t *testing.T) {
	m := serenaLikeManifest()
	_, err := BuildPlan(m, "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown daemon filter, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should mention the unknown daemon name, got: %v", err)
	}
}

func TestBuildPlan_InvalidClientURLPath_Errors(t *testing.T) {
	m := serenaLikeManifest()
	m.ClientBindings[0].URLPath = "@evil.com/mcp"

	_, err := BuildPlan(m, "")
	if err == nil {
		t.Fatal("expected error for invalid url_path, got nil")
	}
	if !strings.Contains(err.Error(), "invalid url_path") {
		t.Fatalf("error = %v, want mention of invalid url_path", err)
	}
}

// TestPreflight_RespectsDaemonFilter ensures --daemon filter keeps Preflight
// from checking ports of unrelated daemons that may legitimately be occupied
// by a previous partial install.
//
// Setup: two daemons pointing at the SAME occupied port. With filter="second",
// the first daemon must be skipped and the error must reference only "second".
// With no filter, the first daemon is checked first and the error references
// "first".
func TestPreflight_RespectsDaemonFilter(t *testing.T) {
	preparePreflightBinaryChecks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupiedPort := ln.Addr().(*net.TCPAddr).Port

	m := &config.ServerManifest{
		Name:      "testsrv",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go", // on PATH whenever `go test` runs
		Daemons: []config.DaemonSpec{
			{Name: "first", Port: occupiedPort},
			{Name: "second", Port: occupiedPort},
		},
	}

	// Filter="second" — "first" must be skipped; error should mention only "second".
	err = Preflight(m, "second")
	if err == nil {
		t.Fatal("Preflight(m, 'second') = nil, want error (port occupied)")
	}
	if !strings.Contains(err.Error(), "second") {
		t.Errorf("error should reference 'second' daemon: %v", err)
	}
	if strings.Contains(err.Error(), "first") {
		t.Errorf("error should NOT reference filtered-out 'first' daemon: %v", err)
	}

	// No filter — "first" is checked first, must be in the message.
	err = Preflight(m, "")
	if err == nil {
		t.Fatal("Preflight(m, '') = nil, want error")
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("unfiltered error should reference 'first' daemon (iteration order): %v", err)
	}
}

// TestPreflight_ChecksInternalPortForNativeHTTP verifies that a native-http
// manifest fails preflight when the internal port (external + offset) is
// already bound, even if the external port itself is free. Without this
// check, install would persist scheduler/client config and then crash at
// runtime when HTTPHost tries to spawn its upstream.
func TestPreflight_ChecksInternalPortForNativeHTTP(t *testing.T) {
	preparePreflightBinaryChecks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupiedInternal := ln.Addr().(*net.TCPAddr).Port
	// Pick an external port such that internal = external + offset hits
	// the occupied port. Working backward: external = occupied - offset.
	// We still need the external port itself to be free — allocate it
	// transiently and close before calling Preflight to confirm it's free.
	external := occupiedInternal - config.NativeHTTPInternalPortOffset
	if external < 1024 {
		t.Skipf("could not construct test ports from occupied=%d offset=%d", occupiedInternal, config.NativeHTTPInternalPortOffset)
	}

	m := &config.ServerManifest{
		Name:      "testsrv",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: external}},
	}

	err = Preflight(m, "")
	if err == nil {
		t.Fatal("expected preflight error when internal port is bound")
	}
	if !strings.Contains(err.Error(), "internal port") {
		t.Errorf("error should mention 'internal port': %v", err)
	}
}

// TestPreflight_StdioBridgeIgnoresInternalPort asserts that the internal-port
// check is scoped to native-http. stdio-bridge transports have no second
// port and must not be rejected for something outside their scope.
func TestPreflight_StdioBridgeIgnoresInternalPort(t *testing.T) {
	preparePreflightBinaryChecks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port
	external := occupied - config.NativeHTTPInternalPortOffset
	if external < 1024 {
		t.Skipf("could not construct test ports")
	}

	m := &config.ServerManifest{
		Name:      "testsrv",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: external}},
	}

	if err := Preflight(m, ""); err != nil {
		t.Errorf("stdio-bridge preflight should pass (internal-port check is native-http only): %v", err)
	}
}

// TestPreflight_MissingSecretFailsFast verifies that a manifest whose
// env declares a `secret:<key>` that is absent from the vault fails
// preflight — surfacing the missing secret BEFORE any side effect (task
// creation, client-config backup+rewrite, daemon spawn) is applied.
// The alternative path (deferred resolution at daemon launch) yielded
// a cryptic subprocess error several steps removed from the real cause.
func TestPreflight_MissingSecretFailsFast(t *testing.T) {
	preparePreflightBinaryChecks(t)
	// Point the secrets resolver at a non-existent vault location so
	// any secret: ref triggers the "vault unavailable" branch. Keeps
	// the test hermetic: we aren't exercising decryption, just the
	// gate that blocks install when a ref cannot resolve.
	t.Setenv("LOCALAPPDATA", t.TempDir())  // Windows path
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // Linux path

	m := &config.ServerManifest{
		Name:      "secretless-server",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Env:       map[string]string{"API_KEY": "secret:nonexistent_key"},
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 0}},
	}

	err := Preflight(m, "")
	if err == nil {
		t.Fatal("expected preflight to fail for missing secret ref")
	}
	if !strings.Contains(err.Error(), "nonexistent_key") {
		t.Errorf("error should name the missing key: %v", err)
	}
}

// TestPreflight_NoSecretsNeeded confirms manifests without any secret:
// references preflight cleanly even when no vault exists (fresh
// machine, user has not run `mcphub secrets init`).
func TestPreflight_NoSecretsNeeded(t *testing.T) {
	preparePreflightBinaryChecks(t)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // must be free for preflight

	m := &config.ServerManifest{
		Name:      "plain-server",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Env:       map[string]string{"PORT": "literal", "OTHER": "$MY_ENV_VAR_UNSET"},
		Daemons:   []config.DaemonSpec{{Name: "default", Port: port}},
	}

	// Preflight should succeed despite the $VAR ref because it is only
	// the secret: refs that are gated here (the $VAR check happens at
	// daemon launch where the contract is different).
	if err := Preflight(m, ""); err != nil {
		t.Errorf("preflight unexpectedly failed with no secret refs: %v", err)
	}
}

// TestPreflight_UnknownCommand ensures the command check runs regardless of filter.
func TestPreflight_UnknownCommand(t *testing.T) {
	m := &config.ServerManifest{
		Name:    "testsrv",
		Command: "this-binary-definitely-does-not-exist-mcp-local-hub",
		Daemons: []config.DaemonSpec{{Name: "x", Port: 1}},
	}
	if err := Preflight(m, "x"); err == nil {
		t.Error("expected error for missing command")
	}
}

// TestInstallAllInstallsEverything spawns a tempdir with two fake manifests
// and asserts Install is invoked for each (dry-run mode so no scheduler/
// client writes). Verifies InstallAllFrom returns one result per manifest.
//
// Ports must be OS-allocated (`net.Listen(":0")` via pickFreeLocalPort)
// rather than literal 9130/9131: dev workstations frequently have those
// ports in TIME_WAIT from prior test runs (or held by an installed
// daemon), and the install preflight rejects manifests whose port is
// already in use. Using pickFreeLocalPort matches the sibling test
// TestInstallAllFrom_PortConflictFailsThatServer below.
func TestInstallAllInstallsEverything(t *testing.T) {
	tmp := t.TempDir()
	fooPort := pickFreeLocalPort(t)
	barPort := pickFreeLocalPort(t)
	makeFakeManifest(t, filepath.Join(tmp, "foo"), "foo", fooPort)
	makeFakeManifest(t, filepath.Join(tmp, "bar"), "bar", barPort)
	preparePreflightBinaryChecks(t)

	a := NewAPI()
	var buf bytes.Buffer
	results := a.InstallAllFrom(InstallAllOpts{
		ManifestDir: tmp,
		DryRun:      true,
		Writer:      &buf,
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("server %s: unexpected error %v", r.Server, r.Err)
		}
	}
}

func TestInstallAllFrom_PortConflictFailsThatServer(t *testing.T) {
	tmp := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port
	makeFakeManifest(t, filepath.Join(tmp, "busy"), "busy", occupied)
	makeFakeManifest(t, filepath.Join(tmp, "free"), "free", occupied+1)
	preparePreflightBinaryChecks(t)

	a := NewAPI()
	results := a.InstallAllFrom(InstallAllOpts{
		ManifestDir: tmp,
		DryRun:      true,
		Writer:      &bytes.Buffer{},
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	byServer := map[string]error{}
	for _, r := range results {
		byServer[r.Server] = r.Err
	}
	if byServer["busy"] == nil {
		t.Fatalf("expected busy server to fail preflight for occupied port")
	}
	if !strings.Contains(byServer["busy"].Error(), "already in use") {
		t.Fatalf("busy error should mention occupied port, got: %v", byServer["busy"])
	}
	if byServer["free"] != nil {
		t.Fatalf("expected free server to pass, got: %v", byServer["free"])
	}
}

func TestInstallFromManifestDirRejectsYAMLNameMismatch(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "demo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := `name: other
kind: global
transport: stdio-bridge
command: go
daemons:
  - name: default
    port: 0
client_bindings: []
weekly_refresh: false
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	err := NewAPI().installFromManifestDir(InstallOpts{
		Server: "demo",
		DryRun: true,
		Writer: &bytes.Buffer{},
	}, tmp)
	if err == nil {
		t.Fatal("expected YAML name mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), `manifest yaml name "other" must match requested server "demo"`) {
		t.Fatalf("error = %v, want YAML/requested name mismatch", err)
	}
}

func makeFakeManifest(t *testing.T, dir, name string, port int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// 'go' is guaranteed to be on PATH in every Go test environment.
	// Previously the fixture used 'echo', which works under Unix shells
	// but not on Windows where echo is a cmd.exe builtin, not a PE file
	// — exec.LookPath fails and install preflight rejects the manifest.
	body := fmt.Sprintf(`name: %s
kind: global
transport: stdio-bridge
command: go
daemons:
  - name: default
    port: %d
client_bindings: []
weekly_refresh: false
`, name, port)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// pickFreeLocalPort returns a 127.0.0.1 port that net.Listen succeeded
// on (and immediately closed). The kernel is unlikely to reuse the
// exact port within a few microseconds for a different listener, so
// the freshly-closed port is a reasonable "free" probe target. Tests
// that need the port held open should re-Listen before probing.
func pickFreeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// TestWaitForPortFree_FreePortReturnsImmediately asserts the DM-3
// helper returns nil on the first probe when nothing is listening —
// no spurious sleep delay in the common Restart path.
func TestWaitForPortFree_FreePortReturnsImmediately(t *testing.T) {
	port := pickFreeLocalPort(t)
	start := time.Now()
	if err := waitForPortFree(port, 3*time.Second); err != nil {
		t.Fatalf("expected nil on free port, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("free-port path was slow: elapsed=%v (must succeed on first probe)", elapsed)
	}
}

// TestWaitForPortFree_HeldPortTimesOut asserts that when the port
// stays bound, waitForPortFree returns an error after roughly the
// configured timeout. A daemon that fails to release would otherwise
// trigger a new daemon's bind to fail too — surfacing the wait error
// to the operator is more informative than dropping straight into
// `schtasks /Run` and letting it record last_result=1.
func TestWaitForPortFree_HeldPortTimesOut(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	start := time.Now()
	err = waitForPortFree(port, 300*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error on held port, got nil")
	}
	if !strings.Contains(err.Error(), "still in use") {
		t.Errorf("error must mention 'still in use'; got: %v", err)
	}
	// Lower bound: the loop must wait at least one full timeout window.
	if elapsed < 250*time.Millisecond {
		t.Errorf("timed out too soon: elapsed=%v, want >=250ms", elapsed)
	}
	// Upper bound: a generous tolerance for slow CI; primary assertion
	// is that we don't block forever.
	if elapsed > 3*time.Second {
		t.Errorf("timed out too late: elapsed=%v, want <3s", elapsed)
	}
}

// TestWaitForPortFree_PortReleasedDuringWait simulates the realistic
// TIME_WAIT race: the helper starts probing while the port is still
// held, the listener releases mid-wait, and the helper succeeds before
// the timeout. This is the entire reason DM-3 added the wait — the
// new daemon's bind would otherwise race the kernel's socket cleanup
// and lose.
func TestWaitForPortFree_PortReleasedDuringWait(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port

	// Release the port asynchronously after a short hold.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = l.Close()
	}()

	start := time.Now()
	if err := waitForPortFree(port, 3*time.Second); err != nil {
		t.Fatalf("expected port to free during wait, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned suspiciously fast: elapsed=%v (port was held %v)",
			elapsed, 150*time.Millisecond)
	}
}
