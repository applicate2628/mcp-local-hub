package api

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// serenaLikeManifest returns a manifest resembling the Serena manifest:
// 3 daemons, weekly refresh, 4 client bindings (one shared daemon).
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
			{Client: "antigravity", Daemon: "antigravity", URLPath: "/mcp"},
			{Client: "gemini-cli", Daemon: "antigravity", URLPath: "/mcp"}, // shared daemon
		},
		WeeklyRefresh: true,
	}
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
	// 4 client bindings.
	if len(p.ClientUpdates) != 4 {
		t.Errorf("len(ClientUpdates) = %d, want 4", len(p.ClientUpdates))
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
	// antigravity daemon is referenced by TWO bindings: antigravity + gemini-cli.
	p, err := BuildPlan(m, "antigravity")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.SchedulerTasks) != 1 {
		t.Errorf("len(SchedulerTasks) = %d, want 1", len(p.SchedulerTasks))
	}
	if len(p.ClientUpdates) != 2 {
		t.Errorf("len(ClientUpdates) = %d, want 2 (antigravity + gemini-cli share the daemon)", len(p.ClientUpdates))
	}
	sawAG, sawGemini := false, false
	for _, u := range p.ClientUpdates {
		if u.Client == "antigravity" {
			sawAG = true
		}
		if u.Client == "gemini-cli" {
			sawGemini = true
		}
	}
	if !sawAG || !sawGemini {
		t.Errorf("expected both antigravity and gemini-cli bindings; got: %+v", p.ClientUpdates)
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

// TestPreflight_RespectsDaemonFilter ensures --daemon filter keeps Preflight
// from checking ports of unrelated daemons that may legitimately be occupied
// by a previous partial install.
//
// Setup: two daemons pointing at the SAME occupied port. With filter="second",
// the first daemon must be skipped and the error must reference only "second".
// With no filter, the first daemon is checked first and the error references
// "first".
func TestPreflight_RespectsDaemonFilter(t *testing.T) {
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
func TestInstallAllInstallsEverything(t *testing.T) {
	tmp := t.TempDir()
	makeFakeManifest(t, filepath.Join(tmp, "foo"), "foo", 9130)
	makeFakeManifest(t, filepath.Join(tmp, "bar"), "bar", 9131)

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

func makeFakeManifest(t *testing.T, dir, name string, port int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`name: %s
kind: global
transport: stdio-bridge
command: echo
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
