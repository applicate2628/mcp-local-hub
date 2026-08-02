// Package cli — tests for the `mcphub workspace ...` subcommand group
// (Phases B.2 + B.3 of the v0.5.x serena-supervisor unified plan).
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// ------------------------------------------------------------------
// Shared test helpers
// ------------------------------------------------------------------

// withSerenaManifest installs a test-only serena manifest loader that
// returns a dynamic-pool manifest with a daemon_template.port_pool block.
// Register resolves the pool through the shared dynamic-pool service
// (api.EffectiveSerenaPortPool), which prefers this embed's
// daemon_template over the built-in default — so these tests still exercise
// the injected [start,end] pool. The injected manifest carries a non-empty
// Context so it is a valid dynamic-pool shape under the post-PR-#246 Validate
// gate (which now rejects daemon_template manifests with an empty/duplicate
// --context). Restores the original loader on test cleanup.
func withSerenaManifest(t *testing.T, start, end int) {
	t.Helper()
	prev := loadSerenaManifestForCLI
	t.Cleanup(func() { loadSerenaManifestForCLI = prev })
	loadSerenaManifestForCLI = func() (*config.ServerManifest, error) {
		return &config.ServerManifest{
			Name:      "serena",
			Kind:      config.KindWorkspaceScoped,
			Transport: config.TransportNativeHTTP,
			Command:   "uvx",
			DaemonTemplate: &config.DaemonTemplate{
				Context:           "codex",
				PortPool:          &config.PortPool{Start: start, End: end},
				ExtraArgsTemplate: []string{"--project", "${workspace.path}"},
			},
		}, nil
	}
}

// withLegacyGlobalSerenaEmbed installs a test-only serena manifest loader that
// returns the CURRENT embedded shape: kind: global, native-http, NO
// daemon_template (the legacy 2-daemon claude/codex catalog). Before Phase 2
// this shape made `register` fail closed (serenaPortPool errored on the absent
// daemon_template.port_pool). After Phase 2 the shared dynamic-pool service
// supplies a built-in default pool, so register succeeds. Restores the original
// loader on test cleanup.
func withLegacyGlobalSerenaEmbed(t *testing.T) {
	t.Helper()
	prev := loadSerenaManifestForCLI
	t.Cleanup(func() { loadSerenaManifestForCLI = prev })
	loadSerenaManifestForCLI = func() (*config.ServerManifest, error) {
		return &config.ServerManifest{
			Name:      "serena",
			Kind:      config.KindGlobal,
			Transport: config.TransportNativeHTTP,
			Command:   "uvx",
			BaseArgs:  []string{"--from", "git+https://example/serena", "serena", "start-mcp-server", "--transport", "streamable-http"},
			Env:       map[string]string{"PYTHONUNBUFFERED": "1"},
			Daemons: []config.DaemonSpec{
				{Name: "claude", Context: "claude-code", Port: 9121, ExtraArgs: []string{"--context", "claude-code"}},
				{Name: "codex", Context: "codex", Port: 9122, ExtraArgs: []string{"--context", "codex"}},
			},
		}, nil
	}
}

// withStateDir redirects the registry path AND the per-user state dir
// (api.DaemonStateDir — where supervisor-intent.json lands) to a fresh temp dir.
//
// Two seams are required, NOT one:
//   - api.DefaultRegistryPath honors LOCALAPPDATA / XDG_STATE_HOME env vars, so
//     the registry redirect works via t.Setenv.
//   - api.DaemonStateDir does NOT: under -tags=test_state_path_env on Windows it
//     still prefers the real KnownFolder resolver over LOCALAPPDATA, so an env
//     var alone leaves supervisor-intent.json resolving to the REAL
//     %LOCALAPPDATA%\mcp-local-hub. A test that writes the supervisor intent or
//     drives the REAL (*api.API).Unregister (paired LSP teardown → reconcile)
//     without this override clobbers the developer's live MCP fleet. The
//     api.SetDaemonStateRootForTest seam is the sanctioned cross-package
//     redirect that takes precedence over the resolver in BOTH build variants.
func withStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	// Redirect api.DaemonStateDir() to the same temp dir so any
	// supervisor-intent.json / daemon-intent.json write (and the real Unregister
	// reconcile path) stays hermetic and can NEVER touch the live state dir.
	restoreStateRoot := api.SetDaemonStateRootForTest(dir)
	t.Cleanup(restoreStateRoot)
	// Verify the regPath actually lands inside dir (sanity check that the
	// env seam is wired the way these tests assume).
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	if !strings.HasPrefix(regPath, dir) {
		t.Fatalf("regPath %s not under tempDir %s — env seam broken", regPath, dir)
	}
	// Verify the state dir (where supervisor-intent.json lands) lands inside dir
	// too — the override is what neutralizes the live-state-wipe hazard.
	stateDir, serr := api.DaemonStateDir()
	if serr != nil {
		t.Fatalf("DaemonStateDir: %v", serr)
	}
	if !strings.HasPrefix(stateDir, dir) {
		t.Fatalf("stateDir %s not under tempDir %s — SetDaemonStateRootForTest seam broken", stateDir, dir)
	}

	// Default supervisor-materialization stubs. `mcphub workspace register`
	// (step 8) now nudges the supervisor to reconcile-apply and gates its
	// success message on the result — the fix for the P1 where register
	// printed an unqualified success without ever touching
	// supervisor-intent.json. Most existing register tests using this helper
	// are NOT exercising that materialization gate; they need register to
	// behave as if a live, healthy supervisor immediately reconciled and
	// reported the spec-bearing row, so they keep testing what they always
	// tested (allocation, bless, default-marker, idempotency, ...) without
	// also having to stub supervisor IPC by hand. Tests that specifically
	// exercise the new gate (TestWorkspaceRegisterSerena_*) override BOTH
	// seams again AFTER calling withStateDir and restore via their own
	// t.Cleanup — cleanup runs LIFO, so their override is in effect for
	// their own duration and this default resumes afterward.
	origReconcile := serenaRegisterReconcileFn
	serenaRegisterReconcileFn = func(context.Context, bool) (api.ReconcileResponse, error) {
		return api.ReconcileResponse{DriftCount: 1, AppliedCount: 1}, nil
	}
	t.Cleanup(func() { serenaRegisterReconcileFn = origReconcile })
	origIntentCheck := serenaRegisterSettledCheckFn
	serenaRegisterSettledCheckFn = func(expected api.WorkspaceEntry) (serenaRegisterSettledResult, error) {
		// Report the ACTUAL registry port when available (most of these tests
		// don't inspect it, but a couple of newer assertions do) instead of a
		// hardcoded placeholder.
		port := 0
		if regPath, err := api.DefaultRegistryPath(); err == nil {
			reg := api.NewRegistry(regPath)
			if lerr := reg.Load(); lerr == nil {
				if row, ok := reg.GetSerena(expected.WorkspaceKey); ok {
					port = row.Port
				}
			}
		}
		return serenaRegisterSettledResult{Settled: true, RegistryRowPresent: true, Port: port}, nil
	}
	t.Cleanup(func() { serenaRegisterSettledCheckFn = origIntentCheck })

	return dir
}

// makeWorkspaceDir creates an existing on-disk workspace directory and
// optionally seeds a `.serena/project.yml` with the given languages.
// Returns the canonical workspace path (after EvalSymlinks + drive-case
// normalization).
func makeWorkspaceDir(t *testing.T, root string, seedLanguages []string) string {
	t.Helper()
	// Use a deterministic name so the workspace_key in different tests
	// in the same temp dir parent does not collide.
	ws := filepath.Join(root, "workspace")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if seedLanguages != nil {
		if err := os.MkdirAll(filepath.Join(ws, ".serena"), 0o700); err != nil {
			t.Fatalf("mkdir .serena: %v", err)
		}
		// Hand-write the YAML so we don't depend on the implementation's
		// marshal shape.
		buf := &bytes.Buffer{}
		buf.WriteString("languages:\n")
		for _, l := range seedLanguages {
			buf.WriteString("  - ")
			buf.WriteString(l)
			buf.WriteString("\n")
		}
		buf.WriteString("read_only: false\n")
		if err := os.WriteFile(filepath.Join(ws, ".serena", "project.yml"), buf.Bytes(), 0o600); err != nil {
			t.Fatalf("write project.yml: %v", err)
		}
	}
	canon, err := api.CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", ws, err)
	}
	return canon
}

// runWorkspaceCmd executes the workspace cobra command with args and
// returns combined stdout+stderr plus any error.
func runWorkspaceCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := newWorkspaceCmd()
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// ------------------------------------------------------------------
// B.2: register
// ------------------------------------------------------------------

func TestWorkspaceRegister_AllocatesPortFromPool(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	stateDir := withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python", "typescript"})

	out, err := runWorkspaceCmd(t, "register", ws)
	if err != nil {
		t.Fatalf("register: %v\noutput: %s", err, out)
	}
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	got := reg.SerenaEntries()
	if len(got) != 1 {
		t.Fatalf("want 1 serena entry, got %d", len(got))
	}
	if got[0].Port != 9121 {
		t.Errorf("port = %d, want first-in-pool 9121", got[0].Port)
	}
	if got[0].Language != api.SerenaLanguageSentinel {
		t.Errorf("language = %q, want %q", got[0].Language, api.SerenaLanguageSentinel)
	}
	if got[0].Backend != "serena" {
		t.Errorf("backend = %q, want %q", got[0].Backend, "serena")
	}
	wantLangs := []string{"python", "typescript"}
	if !equalStrings(got[0].Languages, wantLangs) {
		t.Errorf("languages = %v, want %v", got[0].Languages, wantLangs)
	}
	if got[0].RegisteredVia != "manual" {
		t.Errorf("registered_via = %q, want %q", got[0].RegisteredVia, "manual")
	}
	if got[0].RegisteredAt.IsZero() {
		t.Errorf("registered_at unset")
	}

	// Ensure the second registration in this batch (different ws) would
	// pick the next free port — exercises AllocateSerenaPort iteration
	// over the registry's AllocatedPorts set.
	tmp2 := t.TempDir()
	ws2 := makeWorkspaceDir(t, tmp2, []string{"go"})
	if _, err := runWorkspaceCmd(t, "register", ws2); err != nil {
		t.Fatalf("register second: %v", err)
	}
	reg2 := api.NewRegistry(regPath)
	if err := reg2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entries := reg2.SerenaEntries()
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	seen := map[int]bool{}
	for _, e := range entries {
		seen[e.Port] = true
	}
	if !seen[9121] || !seen[9122] {
		t.Errorf("expected ports {9121, 9122}; got %v", seen)
	}
	_ = stateDir
}

func TestWorkspaceRegister_RejectsExistingPath(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})

	if _, err := runWorkspaceCmd(t, "register", ws); err != nil {
		t.Fatalf("first register: %v", err)
	}
	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("second register should fail; output: %s", out)
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error message should mention 'already registered'; got %q", err.Error())
	}
}

func TestWorkspaceRegister_MissingProjectYmlNoLanguages_ErrorsWithBootstrapHint(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	// Note: no seedLanguages → no .serena/project.yml on disk.
	ws := makeWorkspaceDir(t, tmp, nil)

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("expected error for missing project.yml; output: %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "bootstrap") {
		t.Errorf("error message should mention bootstrap; got %q", msg)
	}
	if !strings.Contains(msg, ".serena/project.yml") {
		t.Errorf("error message should name project.yml; got %q", msg)
	}
}

// readSerenaProjectLanguages now routes through the SAME hardened reader the
// auto-register path uses (api.ReadUntrustedSerenaProjectYML). This pins the
// sibling-call-site swap: an oversized `.serena/project.yml` (untrusted clone
// marker) is rejected by `workspace register` instead of being read unbounded
// via the old bare os.ReadFile. Guards against the sibling silently drifting
// back to an unhardened read.
func TestWorkspaceRegister_OversizedProjectYml_RejectedByHardenedReader(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	// Seed a marker so the .serena dir exists, then overwrite it with an
	// oversized body (past the 64 KiB cap the shared reader enforces).
	ws := makeWorkspaceDir(t, tmp, []string{"go"})
	marker := filepath.Join(ws, ".serena", "project.yml")
	oversized := []byte("languages:\n  - go\n# " + strings.Repeat("A", 64*1024) + "\n")
	if err := os.WriteFile(marker, oversized, 0o600); err != nil {
		t.Fatalf("write oversized marker: %v", err)
	}

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("expected an oversized-marker rejection from the hardened reader; output: %s", out)
	}
	// The hardened reader names the byte cap; the register wrapper prepends
	// "read .serena/project.yml:". Either substring proves the hardened path ran.
	msg := err.Error()
	if !strings.Contains(msg, "cap") {
		t.Errorf("error should name the byte cap (hardened reader); got %q", msg)
	}
}

func TestWorkspaceRegister_LanguagesFlagOverridesProjectYml(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})

	if _, err := runWorkspaceCmd(t, "register", ws, "--languages", "cpp,typescript,markdown"); err != nil {
		t.Fatalf("register: %v", err)
	}
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reg.SerenaEntries()
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	want := []string{"cpp", "markdown", "typescript"} // sorted on write
	if !equalStrings(got[0].Languages, want) {
		t.Errorf("languages = %v, want %v", got[0].Languages, want)
	}
}

// TestWorkspaceRegister_SucceedsAgainstLegacyGlobalEmbed is the Phase 2
// cycle-break regression guard (finding #3). It injects the CURRENT embedded
// `kind: global` serena manifest (no daemon_template) via the
// loadSerenaManifestForCLI seam and asserts that `register` now SUCCEEDS:
// it allocates a port from the shared dynamic-pool service's built-in default
// pool instead of failing closed on the absent daemon_template.port_pool.
// Scheduler-free (register only touches the registry).
func TestWorkspaceRegister_SucceedsAgainstLegacyGlobalEmbed(t *testing.T) {
	withLegacyGlobalSerenaEmbed(t)
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python", "typescript"})

	out, err := runWorkspaceCmd(t, "register", ws)
	if err != nil {
		t.Fatalf("register against legacy kind:global embed should SUCCEED (cycle broken); err=%v\noutput: %s", err, out)
	}

	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	entries := reg.SerenaEntries()
	if len(entries) != 1 {
		t.Fatalf("want 1 serena entry, got %d", len(entries))
	}
	// Port must come from the built-in default pool. Confirm against the same
	// service the register path uses (rather than hard-coding the range here),
	// so a future default-range change keeps this assertion correct.
	pool, perr := api.EffectiveSerenaPortPool(nil)
	if perr != nil {
		t.Fatalf("EffectiveSerenaPortPool: %v", perr)
	}
	if entries[0].Port < pool.Start || entries[0].Port > pool.End {
		t.Errorf("allocated port %d outside built-in default pool [%d,%d]",
			entries[0].Port, pool.Start, pool.End)
	}
	// On a fresh registry the first allocation is the bottom of the pool.
	if entries[0].Port != pool.Start {
		t.Errorf("first allocated port = %d, want bottom-of-pool %d", entries[0].Port, pool.Start)
	}
	if entries[0].Language != api.SerenaLanguageSentinel {
		t.Errorf("language = %q, want %q", entries[0].Language, api.SerenaLanguageSentinel)
	}
}

// ------------------------------------------------------------------
// B.2: unregister
// ------------------------------------------------------------------

// noopWorkspaceTestScheduler is a do-nothing api.TestSchedulerIface used by the
// REAL-seam unregister pairing test to keep the host's Task Scheduler untouched.
// Every method is an inert success so the legacy-task Delete in (*api.API).Unregister
// is a no-op rather than a real schtasks invocation.
type noopWorkspaceTestScheduler struct{}

func (noopWorkspaceTestScheduler) Create(scheduler.TaskSpec) error  { return nil }
func (noopWorkspaceTestScheduler) Delete(string) error              { return nil }
func (noopWorkspaceTestScheduler) Run(string) error                 { return nil }
func (noopWorkspaceTestScheduler) ExportXML(string) ([]byte, error) { return nil, nil }
func (noopWorkspaceTestScheduler) ImportXML(string, []byte) error   { return nil }

// seedTwoBackends registers one serena row + one LSP row under the same
// workspace_key for unregister-semantic tests.
func seedTwoBackends(t *testing.T) (regPath, wsCanon, wsKey string) {
	t.Helper()
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	wsCanon = makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey = api.WorkspaceKey(wsCanon)

	if _, err := runWorkspaceCmd(t, "register", wsCanon); err != nil {
		t.Fatalf("register serena: %v", err)
	}
	regPath, _ = api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Inject a fake LSP row via PutLSP — same path the production
	// registerOneLanguage uses (Phase 3 lazy-mode register).
	// Port:0 is deliberate. The default (LSP-only) unregister path uses the REAL
	// (*api.API).Unregister in TestWorkspaceUnregister_RemovesRegistryRowAndSupervisorIntentDescriptor,
	// whose teardown calls killByPortFn(entry.Port, 5s) → killDaemonByPort, which
	// has NO identity gate and TREE-KILLS whatever listens on entry.Port. A nonzero
	// port here (the old 9200 was the FIRST slot of the live LSP pool, per
	// servers/mcp-language-server/manifest.yaml `start: 9200`) would kill the
	// developer's live workspace-proxy. SetDaemonStateRootForTest isolates FILES,
	// not the network/process surface. With Port:0 the `entry.Port != 0` guard at
	// internal/api/register.go SKIPS the kill, and every assertion in the callers
	// is port-independent (they check row/descriptor presence, never the port).
	if err := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: wsCanon,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          0,
		TaskName:      "mcp-local-hub-lsp-" + wsKey + "-python",
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	return regPath, wsCanon, wsKey
}

// withStubbedLSPUnregister replaces the paired-teardown seam with a hermetic
// stub that performs ONLY the registry-row removal (no real scheduler, netstat,
// or supervisor IPC dial). It mirrors the production registry effect of
// (*api.API).Unregister so the existing registry-outcome assertions hold while
// keeping these CLI tests fast and isolated from the host's real Task Scheduler.
// The recorded languages let a caller assert the seam was invoked with the
// expected per-backend language set. The dedicated pairing test
// (TestWorkspaceUnregister_RemovesRegistryRowAndSupervisorIntentDescriptor) uses
// the REAL seam to prove the intent descriptor is dropped together with the row.
func withStubbedLSPUnregister(t *testing.T) *[]string {
	t.Helper()
	gotLangs := &[]string{}
	prev := unregisterLSPWorkspaceFn
	t.Cleanup(func() { unregisterLSPWorkspaceFn = prev })
	unregisterLSPWorkspaceFn = func(canonical string, languages []string) (*api.UnregisterReport, error) {
		*gotLangs = append(*gotLangs, languages...)
		wsKey := api.WorkspaceKey(canonical)
		regPath, err := api.DefaultRegistryPath()
		if err != nil {
			return nil, err
		}
		reg := api.NewRegistry(regPath)
		unlock, err := reg.Lock()
		if err != nil {
			return nil, err
		}
		defer unlock()
		if err := reg.Load(); err != nil {
			return nil, err
		}
		report := &api.UnregisterReport{Workspace: canonical, WorkspaceKey: wsKey}
		targets := languages
		if len(targets) == 0 {
			for _, e := range reg.ListByWorkspaceLSP(wsKey) {
				targets = append(targets, e.Language)
			}
		}
		for _, lang := range targets {
			reg.Remove(wsKey, lang)
			report.Removed = append(report.Removed, lang)
		}
		if err := reg.Save(); err != nil {
			return nil, err
		}
		return report, nil
	}
	return gotLangs
}

func TestWorkspaceUnregister_RemovesLSPOnlyByDefault(t *testing.T) {
	regPath, ws, wsKey := seedTwoBackends(t)
	gotLangs := withStubbedLSPUnregister(t)

	if _, err := runWorkspaceCmd(t, "unregister", ws); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	// The paired teardown seam (not the bare RemoveByBackend) must own the
	// LSP-row removal so the supervisor-intent descriptor drops with the row.
	if len(*gotLangs) != 1 || (*gotLangs)[0] != "python" {
		t.Errorf("paired LSP teardown should fire for the python LSP row; got %v", *gotLangs)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Serena row must remain.
	if _, ok := reg.GetSerena(wsKey); !ok {
		t.Errorf("serena row was removed; default should be LSP-only")
	}
	// LSP rows must be gone.
	lsp := reg.ListByWorkspaceLSP(wsKey)
	if len(lsp) != 0 {
		t.Errorf("LSP rows survived; want 0, got %d", len(lsp))
	}
}

// TestClassifyWorkspaceUnregister_LegacyKeyFallback pins the pre-classification
// path used by `mcphub workspace unregister`: rows written before symlink-aware
// canonicalization may exist only under the legacy workspace key, and the CLI
// must find them before it decides "no registry rows match".
//
// Negative-control: classify only ListByWorkspace(canonicalKey) and this test
// returns no languages, so it fails before api.Unregister gets its own legacy
// fallback chance.
func TestClassifyWorkspaceUnregister_LegacyKeyFallback(t *testing.T) {
	withStateDir(t)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	const (
		canonicalKey = "newkey00"
		legacyKey    = "oldkey00"
	)
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey:  legacyKey,
		WorkspacePath: "/legacy/workspace",
		Language:      "python",
		Backend:       "mcp-language-server",
		TaskName:      "mcp-local-hub-lsp-" + legacyKey + "-python",
	}); err != nil {
		t.Fatalf("PutLSP legacy row: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	langs, removeSerena, err := classifyWorkspaceUnregister(regPath, canonicalKey, legacyKey, "")
	if err != nil {
		t.Fatalf("classifyWorkspaceUnregister: %v", err)
	}
	if removeSerena {
		t.Fatal("default unregister must not target serena rows")
	}
	if len(langs) != 1 || langs[0] != "python" {
		t.Fatalf("classified languages = %v, want [python] from legacy key", langs)
	}
}

func TestWorkspaceUnregister_BackendSerenaRemovesOnlySerena(t *testing.T) {
	regPath, ws, wsKey := seedTwoBackends(t)
	gotLangs := withStubbedLSPUnregister(t)

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	// --backend serena removes ONLY the sentinel row; the paired LSP teardown
	// seam must NOT fire (no LSP descriptor pairing involved).
	if len(*gotLangs) != 0 {
		t.Errorf("--backend serena must not invoke the LSP teardown seam; got %v", *gotLangs)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := reg.GetSerena(wsKey); ok {
		t.Errorf("serena row should be removed")
	}
	lsp := reg.ListByWorkspaceLSP(wsKey)
	if len(lsp) != 1 {
		t.Errorf("LSP rows want 1, got %d", len(lsp))
	}
}

func TestWorkspaceUnregister_BackendSerenaRemovesSupervisorIntentDescriptorAndStop(t *testing.T) {
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"go"})
	wsKey := api.WorkspaceKey(ws)
	taskName := api.SerenaTaskNameForWorkspace(ws)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: ws,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          0,
		TaskName:      taskName,
		Languages:     []string{"go"},
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}
	intentPath, err := api.DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName:  taskName,
			Server:    "serena",
			Daemon:    wsKey,
			Workspace: ws,
			Port:      0,
		}},
		Stops: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
			},
		},
	}); err != nil {
		t.Fatalf("WriteSupervisorIntent: %v", err)
	}

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister --backend serena: %v", err)
	}

	reg = api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if _, ok := reg.GetSerena(wsKey); ok {
		t.Fatalf("serena registry row survived unregister")
	}
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row != nil {
		t.Fatalf("serena supervisor-intent descriptor %q survived unregister: %+v", taskName, row)
	}
	if _, ok := intent.Stops[taskName]; ok {
		t.Fatalf("serena supervisor-intent stop %q survived unregister: %+v", taskName, intent.Stops)
	}
}

// stubSerenaSupervisorTeardown replaces the serena supervisor-descriptor
// teardown seam for the duration of the test. The returned counter records how
// many times it was invoked so callers can assert the teardown ran (or did not).
func stubSerenaSupervisorTeardown(t *testing.T, fn func(workspacePath string) (bool, error)) *int {
	t.Helper()
	prev := removeSerenaSupervisorIntentFn
	calls := 0
	removeSerenaSupervisorIntentFn = func(workspacePath string) (bool, error) {
		calls++
		return fn(workspacePath)
	}
	t.Cleanup(func() { removeSerenaSupervisorIntentFn = prev })
	return &calls
}

// TestWorkspaceUnregister_SerenaTeardownFailureKeepsRegistryRow is the FIX 2
// (bot r32 P2) falsifying regression. When the serena supervisor-descriptor
// teardown fails (a live-supervisor reconcile failure that RESTORES the
// descriptor and returns a retry-asking error), the serena registry row must
// STILL be present afterward — the row is the durable record that drives the
// next retry's paired teardown, so deleting it would orphan the restored
// descriptor.
//
// Pre-fix the row was deleted+saved BEFORE the teardown ran, so a teardown
// error left descriptor-restored + row-gone — the orphan this test guards.
func TestWorkspaceUnregister_SerenaTeardownFailureKeepsRegistryRow(t *testing.T) {
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"go"})
	wsKey := api.WorkspaceKey(ws)
	taskName := api.SerenaTaskNameForWorkspace(ws)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: ws,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          0,
		TaskName:      taskName,
		Languages:     []string{"go"},
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	teardownErr := errors.New("simulated live-supervisor reconcile failure; descriptor restored, retry")
	calls := stubSerenaSupervisorTeardown(t, func(string) (bool, error) {
		return false, teardownErr
	})

	out, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena")
	if err == nil {
		t.Fatalf("unregister must surface the teardown error; got nil (output: %s)", out)
	}
	if !strings.Contains(err.Error(), "paired serena supervisor teardown") {
		t.Errorf("error should name the paired teardown failure; got %v", err)
	}
	if *calls != 1 {
		t.Errorf("teardown seam invoked %d times, want 1", *calls)
	}

	// The serena registry row MUST survive the teardown failure.
	reg = api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if _, ok := reg.GetSerena(wsKey); !ok {
		t.Fatalf("serena registry row was deleted despite teardown failure — the orphan FIX 2 prevents (bot r32 P2)")
	}
}

// TestWorkspaceUnregister_SerenaTeardownSuccessRemovesRow is the FIX 2 negative
// control: when the teardown SUCCEEDS, the serena registry row IS removed.
func TestWorkspaceUnregister_SerenaTeardownSuccessRemovesRow(t *testing.T) {
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"go"})
	wsKey := api.WorkspaceKey(ws)
	taskName := api.SerenaTaskNameForWorkspace(ws)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: ws,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          0,
		TaskName:      taskName,
		Languages:     []string{"go"},
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	calls := stubSerenaSupervisorTeardown(t, func(string) (bool, error) {
		return true, nil
	})

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister --backend serena: %v", err)
	}
	if *calls != 1 {
		t.Errorf("teardown seam invoked %d times, want 1", *calls)
	}

	reg = api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if _, ok := reg.GetSerena(wsKey); ok {
		t.Fatalf("serena registry row survived a successful teardown — want removed")
	}
}

// TestWorkspaceUnregister_SerenaTeardownRunsBeforeRowDelete asserts the FIX 2
// ordering invariant directly: at the moment the teardown seam fires, the
// serena registry row is STILL on disk (the delete has not committed yet).
func TestWorkspaceUnregister_SerenaTeardownRunsBeforeRowDelete(t *testing.T) {
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"go"})
	wsKey := api.WorkspaceKey(ws)
	taskName := api.SerenaTaskNameForWorkspace(ws)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: ws,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          0,
		TaskName:      taskName,
		Languages:     []string{"go"},
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	var rowPresentAtTeardown bool
	stubSerenaSupervisorTeardown(t, func(string) (bool, error) {
		probe := api.NewRegistry(regPath)
		if err := probe.Load(); err != nil {
			t.Fatalf("probe load during teardown: %v", err)
		}
		_, rowPresentAtTeardown = probe.GetSerena(wsKey)
		return true, nil
	})

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister --backend serena: %v", err)
	}
	if !rowPresentAtTeardown {
		t.Fatalf("serena registry row was already deleted when the teardown seam fired — FIX 2 requires teardown-before-delete (bot r32 P2)")
	}
}

func TestWorkspaceUnregister_BackendAllRemovesEverything(t *testing.T) {
	regPath, ws, wsKey := seedTwoBackends(t)
	gotLangs := withStubbedLSPUnregister(t)

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "all"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	// --backend all routes the LSP row through the paired teardown seam AND
	// removes the serena sentinel row directly.
	if len(*gotLangs) != 1 || (*gotLangs)[0] != "python" {
		t.Errorf("--backend all should route the python LSP row through the paired seam; got %v", *gotLangs)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.ListByWorkspace(wsKey); len(got) != 0 {
		t.Errorf("want 0 rows, got %d", len(got))
	}
}

func TestWorkspaceUnregister_BackendAllRemovesMixedCanonicalAndLegacyLSPRows(t *testing.T) {
	withStateDir(t)
	rawPath, canonical, legacy := mixedKeyWorkspaceAliasForUnregisterCmd(t)
	wsKey := api.WorkspaceKey(canonical)
	legacyWSKey := api.WorkspaceKey(legacy)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          0,
		TaskName:      "serena-" + wsKey,
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          0,
		TaskName:      api.LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
	}); err != nil {
		t.Fatalf("PutLSP canonical: %v", err)
	}
	if err := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey:  legacyWSKey,
		WorkspacePath: legacy,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          0,
		TaskName:      api.LSPTaskNameForWorkspaceLanguage(legacyWSKey, "python"),
	}); err != nil {
		t.Fatalf("PutLSP legacy: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	restoreHooks := api.InstallTestHooks(
		func() (api.TestSchedulerIface, error) { return noopWorkspaceTestScheduler{}, nil },
		func() map[string]api.TestClientIface { return map[string]api.TestClientIface{} },
		"",
	)
	t.Cleanup(restoreHooks)

	out, err := runWorkspaceCmd(t, "unregister", rawPath, "--backend", "all")
	if err != nil {
		t.Fatalf("workspace unregister --backend all: %v\n%s", err, out)
	}
	if strings.Contains(out, "warning: language python not registered") {
		t.Fatalf("legacy-only python was reported absent instead of removed:\n%s", out)
	}
	reg = api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if rows := reg.ListByWorkspace(wsKey); len(rows) != 0 {
		t.Fatalf("canonical rows survived --backend all: %+v", rows)
	}
	if rows := reg.ListByWorkspace(legacyWSKey); len(rows) != 0 {
		t.Fatalf("legacy rows survived --backend all: %+v", rows)
	}
}

func TestWorkspaceUnregister_RemovesEntryButLeavesDisk(t *testing.T) {
	_, ws, _ := seedTwoBackends(t)
	withStubbedLSPUnregister(t)

	// `.serena/project.yml` was created by makeWorkspaceDir; confirm
	// unregister --backend all does NOT touch it.
	projectYml := filepath.Join(ws, ".serena", "project.yml")
	if _, err := os.Stat(projectYml); err != nil {
		t.Fatalf("project.yml missing before unregister: %v", err)
	}
	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "all"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if _, err := os.Stat(projectYml); err != nil {
		t.Errorf("project.yml deleted by unregister: %v (must survive)", err)
	}
}

func TestWorkspaceUnregister_BackendByLanguageNameRemovesMatchingRows(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"go", "typescript"})
	wsKey := api.WorkspaceKey(ws)

	if _, err := runWorkspaceCmd(t, "register", ws); err != nil {
		t.Fatalf("register serena: %v", err)
	}
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	for i, lang := range []string{"go", "typescript"} {
		if err := reg.PutLSP(api.WorkspaceEntry{
			WorkspaceKey:  wsKey,
			WorkspacePath: ws,
			Language:      lang,
			Backend:       "mcp-language-server",
			Port:          9200 + i,
			TaskName:      "mcp-local-hub-lsp-" + wsKey + "-" + lang,
		}); err != nil {
			t.Fatalf("PutLSP %q: %v", lang, err)
		}
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	gotLangs := withStubbedLSPUnregister(t)
	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "go"); err != nil {
		t.Fatalf("unregister --backend go: %v", err)
	}
	// Only the matching `go` row routes through the paired teardown seam.
	if len(*gotLangs) != 1 || (*gotLangs)[0] != "go" {
		t.Errorf("--backend go should route ONLY the go row through the paired seam; got %v", *gotLangs)
	}
	reg = api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reg.Get(wsKey, "go"); ok {
		t.Error("go LSP row should be removed by --backend go")
	}
	if _, ok := reg.Get(wsKey, "typescript"); !ok {
		t.Error("typescript LSP row should survive --backend go")
	}
	if _, ok := reg.GetSerena(wsKey); !ok {
		t.Error("serena row should survive --backend go")
	}
}

// TestWorkspaceUnregister_RemovesRegistryRowAndSupervisorIntentDescriptor is the
// orphaned-LSP-daemon quarantine fix at the unregister-source layer. It uses the
// REAL paired-teardown seam ((*api.API).Unregister) to prove that
// `mcphub workspace unregister` of an LSP-bearing workspace drops the
// supervisor-intent descriptor TOGETHER WITH the registry row — so the
// reconciler never sees an unbacked LSP descriptor to spawn-and-quarantine.
// Before the fix, the bare reg.RemoveByBackend removed the registry row but left
// the supervisor-intent descriptor behind.
func TestWorkspaceUnregister_RemovesRegistryRowAndSupervisorIntentDescriptor(t *testing.T) {
	regPath, ws, wsKey := seedTwoBackends(t)

	// Stub the scheduler so the REAL Unregister's legacy-task Delete never
	// dials the host's real Task Scheduler (schtasks). seedTwoBackends already
	// seeds Port:0 for the row + descriptor, so the kill-by-port guard skips the
	// network kill; this hook closes the remaining real-OS surface (no real
	// schtasks /Delete runs against the host). Empty client set — the seeded LSP
	// row carries no ClientEntries, so no adapter writes are exercised. The
	// registry path stays the withStateDir-redirected temp default ("").
	restoreHooks := api.InstallTestHooks(
		func() (api.TestSchedulerIface, error) { return noopWorkspaceTestScheduler{}, nil },
		func() map[string]api.TestClientIface { return map[string]api.TestClientIface{} },
		"",
	)
	t.Cleanup(restoreHooks)

	// Seed the supervisor-intent.json LSP descriptor that pairs with the
	// seeded python LSP registry row — the exact shape register would write.
	// Port:0 mirrors the seeded registry row (see seedTwoBackends): the pairing
	// assertion keys on descriptor.TaskName, which BuildSupervisorDaemonForLSP
	// derives from (WorkspaceKey, Language) — independent of Port — so a zero
	// port preserves the assertion while keeping the real teardown's kill-by-port
	// path inert.
	intentPath, err := api.DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("intent path: %v", err)
	}
	descriptor := api.BuildSupervisorDaemonForLSP(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: ws,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          0,
	}, "mcphub")
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{descriptor},
	}); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	// Default unregister → LSP-only, through the REAL paired teardown.
	if _, err := runWorkspaceCmd(t, "unregister", ws); err != nil {
		t.Fatalf("unregister: %v", err)
	}

	// Registry: LSP row gone, serena row stays.
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if len(reg.ListByWorkspaceLSP(wsKey)) != 0 {
		t.Errorf("LSP registry row should be removed")
	}
	if _, ok := reg.GetSerena(wsKey); !ok {
		t.Errorf("serena row must survive a default (LSP-only) unregister")
	}

	// Supervisor-intent: the paired descriptor must be GONE — the whole point
	// of the fix. A surviving descriptor would be the orphan the reconciler
	// spawn-and-quarantines.
	got, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read supervisor-intent after unregister: %v", err)
	}
	if got != nil {
		for _, d := range got.Daemons {
			if d.TaskName == descriptor.TaskName {
				t.Fatalf("LSP supervisor-intent descriptor %q survived unregister — orphaned-daemon quarantine bug not fixed", descriptor.TaskName)
			}
		}
	}
}

// ------------------------------------------------------------------
// B.2: list
// ------------------------------------------------------------------

// seedSerenaListFixture registers 2 workspaces and marks the first as
// default. Returns the canonical paths in registration order. Uses
// short distinguishable workspace dir names so the table-truncation
// helper does not collapse the two paths into a shared visible prefix.
func seedSerenaListFixture(t *testing.T) (string, string) {
	t.Helper()
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)

	tmpA := t.TempDir()
	wsA := makeWorkspaceDirNamed(t, tmpA, "alpha-ws", []string{"python"})
	if _, err := runWorkspaceCmd(t, "register", wsA, "--default"); err != nil {
		t.Fatalf("register A: %v", err)
	}
	tmpB := t.TempDir()
	wsB := makeWorkspaceDirNamed(t, tmpB, "bravo-ws", []string{"go", "typescript"})
	if _, err := runWorkspaceCmd(t, "register", wsB); err != nil {
		t.Fatalf("register B: %v", err)
	}
	return wsA, wsB
}

// makeWorkspaceDirNamed is makeWorkspaceDir with an explicit dirname so
// the trailing component differs between fixture workspaces even when
// their temp-dir parents share a long prefix (Go's testing temp dirs
// on Windows can be 60+ chars wide).
func makeWorkspaceDirNamed(t *testing.T, root, name string, seedLanguages []string) string {
	t.Helper()
	ws := filepath.Join(root, name)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if seedLanguages != nil {
		if err := os.MkdirAll(filepath.Join(ws, ".serena"), 0o700); err != nil {
			t.Fatalf("mkdir .serena: %v", err)
		}
		buf := &bytes.Buffer{}
		buf.WriteString("languages:\n")
		for _, l := range seedLanguages {
			buf.WriteString("  - ")
			buf.WriteString(l)
			buf.WriteString("\n")
		}
		buf.WriteString("read_only: false\n")
		if err := os.WriteFile(filepath.Join(ws, ".serena", "project.yml"), buf.Bytes(), 0o600); err != nil {
			t.Fatalf("write project.yml: %v", err)
		}
	}
	canon, err := api.CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", ws, err)
	}
	return canon
}

func mixedKeyWorkspaceAliasForUnregisterCmd(t *testing.T) (string, string, string) {
	t.Helper()
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real workspace: %v", err)
	}
	if runtime.GOOS == "windows" {
		var diagnostics []string
		for _, attempt := range []struct {
			label string
			flag  string
		}{
			{label: "directory symlink", flag: "/D"},
			{label: "junction", flag: "/J"},
		} {
			alias := filepath.Join(t.TempDir(), "alias")
			out, err := exec.Command("cmd", "/c", "mklink", attempt.flag, alias, realDir).CombinedOutput()
			if err != nil {
				diagnostics = append(diagnostics, attempt.label+" failed: "+err.Error()+" output="+strings.TrimSpace(string(out)))
				continue
			}
			canonical, legacy, distinct := mixedKeyWorkspacePathsForUnregisterCmd(t, alias)
			if distinct {
				return alias, canonical, legacy
			}
			diagnostics = append(diagnostics, attempt.label+" collapsed keys: canonical="+canonical+" legacy="+legacy)
		}
		t.Fatalf("mixed-key fixture did not produce distinct keys on Windows: %s", strings.Join(diagnostics, "; "))
	}

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatalf("create temp symlink: %v", err)
	}
	canonical, legacy, distinct := mixedKeyWorkspacePathsForUnregisterCmd(t, alias)
	if !distinct {
		t.Fatalf("mixed-key fixture did not produce distinct keys: canonical=%q legacy=%q", canonical, legacy)
	}
	return alias, canonical, legacy
}

func mixedKeyWorkspacePathsForUnregisterCmd(t *testing.T, alias string) (string, string, bool) {
	t.Helper()
	canonical, err := api.CanonicalWorkspacePathForCleanup(alias)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathForCleanup(alias): %v", err)
	}
	legacy, err := api.CanonicalWorkspacePathLegacyCompat(alias)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathLegacyCompat(alias): %v", err)
	}
	return canonical, legacy, api.WorkspaceKey(canonical) != api.WorkspaceKey(legacy)
}

func TestWorkspaceList_TabularOutput(t *testing.T) {
	wsA, wsB := seedSerenaListFixture(t)
	out, err := runWorkspaceCmd(t, "list")
	if err != nil {
		t.Fatalf("list: %v\noutput: %s", err, out)
	}
	for _, want := range []string{"WORKSPACE", "LANGUAGES", "DEFAULT", "PORT", "LAST_SPAWN"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing column %q; output:\n%s", want, out)
		}
	}
	// Paths may be truncated by the column-width helper; assert on the
	// observable prefix instead of the full path. CI temp dirs can be
	// 60+ chars wide on Windows.
	prefixA := workspacePrefixForTable(wsA)
	prefixB := workspacePrefixForTable(wsB)
	if !strings.Contains(out, prefixA) {
		t.Errorf("output missing workspace A prefix %q; out:\n%s", prefixA, out)
	}
	if !strings.Contains(out, prefixB) {
		t.Errorf("output missing workspace B prefix %q; out:\n%s", prefixB, out)
	}
	// Ordering: alphabetic by WorkspacePath. Find both lines and compare
	// their indexes.
	idxA := strings.Index(out, prefixA)
	idxB := strings.Index(out, prefixB)
	if idxA < 0 || idxB < 0 {
		t.Fatalf("paths not found; out:\n%s", out)
	}
	expectedFirstIdx := idxA
	expectedSecondIdx := idxB
	if wsB < wsA {
		expectedFirstIdx, expectedSecondIdx = idxB, idxA
	}
	if expectedFirstIdx > expectedSecondIdx {
		t.Errorf("ordering not alphabetic by path; idxA=%d idxB=%d", idxA, idxB)
	}
	// Default marker should appear on the wsA row (the one we set --default for).
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, prefixA) && !strings.Contains(line, "*") {
			t.Errorf("default marker '*' missing on wsA row: %q", line)
		}
	}
	// Languages render in the LANGUAGES column.
	for _, lang := range []string{"python", "go", "typescript"} {
		if !strings.Contains(out, lang) {
			t.Errorf("language %q missing from output", lang)
		}
	}
	// Ports render in the PORT column.
	for _, port := range []string{"9121", "9122"} {
		if !strings.Contains(out, port) {
			t.Errorf("port %q missing from output", port)
		}
	}
}

func TestWorkspaceList_TabularOutputPreservesDefaultMarkerForLongPath(t *testing.T) {
	const leaf = "alpha-default-leaf"
	longPath := longWorkspacePathForTable(t, leaf)
	buf := &bytes.Buffer{}
	if err := printWorkspaceTable(buf, []api.WorkspaceEntry{{
		WorkspacePath: longPath,
		Languages:     []string{"go"},
		Port:          9121,
	}}, longPath); err != nil {
		t.Fatalf("print table: %v", err)
	}
	out := buf.String()
	line := firstLineContaining(out, leaf)
	if line == "" {
		t.Fatalf("long default workspace row should preserve leaf %q; output:\n%s", leaf, out)
	}
	if !strings.Contains(line, "*") {
		t.Fatalf("default marker '*' should stay visible on long workspace row: %q", line)
	}
}

func longWorkspacePathForTable(t *testing.T, leaf string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), strings.Repeat("shared-parent-", 8), leaf)
}

func firstLineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// workspacePrefixForTable returns the observable string for path p under
// the list-table helper.
func workspacePrefixForTable(p string) string {
	return truncateWorkspacePath(p, workspaceTablePathWidth)
}

func TestWorkspaceList_JSONOutput(t *testing.T) {
	wsA, wsB := seedSerenaListFixture(t)
	out, err := runWorkspaceCmd(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v\noutput: %s", err, out)
	}
	got := strings.TrimSpace(out)
	if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
		t.Fatalf("not a JSON array:\n%s", got)
	}
	var rows []workspaceListJSONRow
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("json decode: %v\nbody: %s", err, got)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// Build a map by path so order independence is explicit.
	byPath := map[string]workspaceListJSONRow{}
	for _, r := range rows {
		byPath[r.WorkspacePath] = r
	}
	rA, okA := byPath[wsA]
	rB, okB := byPath[wsB]
	if !okA || !okB {
		t.Fatalf("missing rows; got map keys %v", mapKeys(byPath))
	}
	if !rA.Default {
		t.Errorf("wsA should be default=true")
	}
	if rB.Default {
		t.Errorf("wsB should be default=false")
	}
	if rA.Language != api.SerenaLanguageSentinel || rB.Language != api.SerenaLanguageSentinel {
		t.Errorf("language sentinel missing: A=%q B=%q", rA.Language, rB.Language)
	}
	if rA.Backend != "serena" || rB.Backend != "serena" {
		t.Errorf("backend not serena: A=%q B=%q", rA.Backend, rB.Backend)
	}
	if rA.Port == 0 || rB.Port == 0 || rA.Port == rB.Port {
		t.Errorf("ports should be distinct non-zero; A=%d B=%d", rA.Port, rB.Port)
	}
}

func TestWorkspaceList_LockedDuringRead(t *testing.T) {
	seedSerenaListFixture(t)
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("lock registry: %v", err)
	}

	type listResult struct {
		out string
		err error
	}
	done := make(chan listResult, 1)
	go func() {
		out, err := runWorkspaceCmd(t, "list")
		done <- listResult{out: out, err: err}
	}()

	select {
	case result := <-done:
		unlock()
		t.Fatalf("workspace list returned while registry lock was held: err=%v output=%s", result.err, result.out)
	case <-time.After(150 * time.Millisecond):
	}

	unlock()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("workspace list after unlock: %v\noutput: %s", result.err, result.out)
		}
		if !strings.Contains(result.out, "WORKSPACE") {
			t.Fatalf("workspace list output missing table header after unlock:\n%s", result.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workspace list did not finish after registry lock was released")
	}
}

// ------------------------------------------------------------------
// B.2: set-default
// ------------------------------------------------------------------

func TestWorkspaceSetDefault_UpdatesRegistry(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmpA := t.TempDir()
	tmpB := t.TempDir()
	wsA := makeWorkspaceDir(t, tmpA, []string{"python"})
	wsB := makeWorkspaceDir(t, tmpB, []string{"go"})
	if _, err := runWorkspaceCmd(t, "register", wsA, "--default"); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if _, err := runWorkspaceCmd(t, "register", wsB); err != nil {
		t.Fatalf("register B: %v", err)
	}

	regPath, _ := api.DefaultRegistryPath()
	stateDir := filepath.Dir(regPath)
	got, err := readDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("read default: %v", err)
	}
	if got != wsA {
		t.Errorf("initial default = %q, want %q", got, wsA)
	}

	if _, err := runWorkspaceCmd(t, "set-default", wsB); err != nil {
		t.Fatalf("set-default B: %v", err)
	}
	got, err = readDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("read default: %v", err)
	}
	if got != wsB {
		t.Errorf("after set-default B: default = %q, want %q", got, wsB)
	}

	// set-default on an unregistered workspace must fail.
	tmpC := t.TempDir()
	wsC := makeWorkspaceDir(t, tmpC, []string{"rust"})
	if _, err := runWorkspaceCmd(t, "set-default", wsC); err == nil {
		t.Errorf("set-default on unregistered workspace should fail")
	}
}

// ------------------------------------------------------------------
// B.3: bootstrap
// ------------------------------------------------------------------

// touch creates an empty file at path, creating parent dirs as needed.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// readBootstrappedLanguages reads .serena/project.yml under root and
// returns the languages list (sorted).
func readBootstrappedLanguages(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".serena", "project.yml"))
	if err != nil {
		t.Fatalf("read project.yml: %v", err)
	}
	var doc serenaProjectYml
	if err := yamlUnmarshalForTest(data, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := append([]string(nil), doc.Languages...)
	sort.Strings(out)
	return out
}

// serenaLanguageEnumForTest is the external wire contract accepted by Serena's
// Language enum. It is intentionally duplicated at this process boundary so
// every mcphub-to-Serena mapping value is drift-gated by a test.
var serenaLanguageEnumForTest = func() map[string]struct{} {
	values := strings.Fields(`
		csharp python rust java kotlin typescript go ruby dart cpp cpp_ccls php r perl clojure
		elixir elm terraform swift bash crystal zig lua luau nix erlang ocaml al fsharp rego
		scala julia fortran haskell haxe lean4 groovy vue powershell pascal matlab msl
		typescript_vts python_jedi python_ty csharp_omnisharp ruby_solargraph php_phpactor
		markdown yaml toml hlsl systemverilog solidity ansible
	`)
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}()

func TestMCPhubToSerenaLanguageMap_ValuesAreValid(t *testing.T) {
	for mcphubLanguage, serenaLanguage := range mcphubToSerenaLanguage {
		if _, ok := serenaLanguageEnumForTest[serenaLanguage]; !ok {
			t.Errorf("mapping %q -> %q targets a value outside Serena's Language enum", mcphubLanguage, serenaLanguage)
		}
	}
	for mcphubLanguage, want := range map[string]string{
		"clangd":     "cpp",
		"javascript": "typescript",
	} {
		if got := mcphubToSerenaLanguage[mcphubLanguage]; got != want {
			t.Errorf("mapping %q = %q, want %q", mcphubLanguage, got, want)
		}
	}
	for _, unsupported := range []string{"vscode-css", "vscode-html"} {
		if got, ok := mcphubToSerenaLanguage[unsupported]; ok {
			t.Errorf("unsupported mcphub language %q unexpectedly maps to %q", unsupported, got)
		}
	}
}

func TestProjectSerenaLanguages_OmitsUnknownWithDiagnostic(t *testing.T) {
	var debug bytes.Buffer
	got := projectSerenaLanguages(
		[]string{"future-language", "go", "javascript", "typescript", "vscode-css"},
		&debug,
	)
	want := []string{"go", "typescript"}
	if !equalStrings(got, want) {
		t.Errorf("projected languages = %v, want %v", got, want)
	}
	for _, omitted := range []string{"future-language", "vscode-css"} {
		if !strings.Contains(debug.String(), omitted) {
			t.Errorf("debug output should name omitted language %q; output: %s", omitted, debug.String())
		}
	}
}

func TestWorkspaceBootstrap_ProjectsOnlySerenaLanguages(t *testing.T) {
	tmp := t.TempDir()
	// Seed a multi-language repo.
	touch(t, filepath.Join(tmp, "main.cpp"))
	touch(t, filepath.Join(tmp, "header.h"))
	touch(t, filepath.Join(tmp, "extra.hpp"))
	touch(t, filepath.Join(tmp, "tool.go"))
	touch(t, filepath.Join(tmp, "ui.tsx"))
	touch(t, filepath.Join(tmp, "util.ts"))
	touch(t, filepath.Join(tmp, "page.js"))
	touch(t, filepath.Join(tmp, "script.py"))
	touch(t, filepath.Join(tmp, "lib.rs"))
	touch(t, filepath.Join(tmp, "README.md"))
	touch(t, filepath.Join(tmp, "style.css"))
	touch(t, filepath.Join(tmp, "index.html"))
	touch(t, filepath.Join(tmp, "sim.f90"))

	out, err := runWorkspaceCmd(t, "bootstrap", tmp)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	got := readBootstrappedLanguages(t, tmp)
	want := []string{
		"cpp", "fortran", "go", "markdown", "python", "rust", "typescript",
	}
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Errorf("languages = %v, want %v", got, want)
	}
	for _, language := range got {
		if _, ok := serenaLanguageEnumForTest[language]; !ok {
			t.Errorf("bootstrap wrote Serena-invalid language %q", language)
		}
	}
	for _, invalid := range []string{"javascript", "vscode-css", "vscode-html"} {
		if slices.Contains(got, invalid) {
			t.Errorf("bootstrap leaked mcphub-only language %q into Serena config", invalid)
		}
	}
	count := 0
	for _, language := range got {
		if language == "typescript" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("typescript count = %d, want 1 after javascript mapping and deduplication", count)
	}
	for _, dropped := range []string{"vscode-css", "vscode-html"} {
		if !strings.Contains(out, dropped) {
			t.Errorf("bootstrap output should diagnose omitted language %q; output: %s", dropped, out)
		}
	}
}

func TestWorkspaceBootstrap_SkipsGitignored(t *testing.T) {
	tmp := t.TempDir()
	// Root-level .gitignore that should suppress "custom_ignored" subdir.
	if err := os.WriteFile(filepath.Join(tmp, ".gitignore"),
		[]byte("custom_ignored\n# comment\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(tmp, "main.go"))
	// File in the gitignored dir would otherwise flag rust as detected.
	touch(t, filepath.Join(tmp, "custom_ignored", "deep.rs"))

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if containsString(got, "rust") {
		t.Errorf("rust should have been gitignored; got %v", got)
	}
	if !containsString(got, "go") {
		t.Errorf("go should still be detected; got %v", got)
	}
}

func TestWorkspaceBootstrap_GitignoreTrailingSlash(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, ".gitignore"),
		[]byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(tmp, "main.go"))
	touch(t, filepath.Join(tmp, "generated", "deep.rs"))

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if containsString(got, "rust") {
		t.Errorf("rust under generated/ should have been gitignored; got %v", got)
	}
	if !containsString(got, "go") {
		t.Errorf("go should still be detected; got %v", got)
	}
}

func TestWorkspaceBootstrap_NestedGitignoreScopeLocal(t *testing.T) {
	tmp := t.TempDir()
	aDir := filepath.Join(tmp, "a")
	if err := os.MkdirAll(aDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aDir, ".gitignore"),
		[]byte("generated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(tmp, "a", "main.go"))
	touch(t, filepath.Join(tmp, "a", "generated", "deep.rs"))
	touch(t, filepath.Join(tmp, "b", "generated", "sibling.py"))

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if containsString(got, "rust") {
		t.Errorf("rust under a/generated should have been gitignored; got %v", got)
	}
	if !containsString(got, "python") {
		t.Errorf("python under sibling b/generated should still be detected; got %v", got)
	}
	if !containsString(got, "go") {
		t.Errorf("go should still be detected; got %v", got)
	}
}

func TestWorkspaceBootstrap_AlwaysSkipsNodeModulesEtc(t *testing.T) {
	tmp := t.TempDir()
	// NO .gitignore: rely on the hardcoded skip list only.
	touch(t, filepath.Join(tmp, "main.go"))
	for _, skipDir := range []string{"node_modules", "target", "dist", ".git"} {
		touch(t, filepath.Join(tmp, skipDir, "should_be_invisible.py"))
	}
	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if containsString(got, "python") {
		t.Errorf("python under hardcoded-skip dir should be invisible; got %v", got)
	}
	if !containsString(got, "go") {
		t.Errorf("go should still be detected; got %v", got)
	}
}

func TestWorkspaceBootstrap_RefusesOverwriteWithoutForce(t *testing.T) {
	tmp := t.TempDir()
	touch(t, filepath.Join(tmp, "main.go"))
	// Pre-create a project.yml.
	if err := os.MkdirAll(filepath.Join(tmp, ".serena"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".serena", "project.yml"),
		[]byte("preserved: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runWorkspaceCmd(t, "bootstrap", tmp)
	if err == nil {
		t.Fatalf("expected error; output: %s", out)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should mention --force; got %q", err.Error())
	}

	// And the file must NOT have been overwritten.
	data, _ := os.ReadFile(filepath.Join(tmp, ".serena", "project.yml"))
	if !strings.Contains(string(data), "preserved: true") {
		t.Errorf("project.yml was modified despite refusal: %s", data)
	}
}

func TestWorkspaceBootstrap_ForceOverwrites(t *testing.T) {
	tmp := t.TempDir()
	touch(t, filepath.Join(tmp, "main.go"))
	if err := os.MkdirAll(filepath.Join(tmp, ".serena"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".serena", "project.yml"),
		[]byte("preserved: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp, "--force"); err != nil {
		t.Fatalf("bootstrap --force: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if !containsString(got, "go") {
		t.Errorf("force-overwrite should write the survey result; got %v", got)
	}
}

func TestWorkspaceBootstrap_DepthBoundedAt5(t *testing.T) {
	tmp := t.TempDir()
	// Depth 1: tmp/lvl1 (file: tmp/lvl1/should_be_seen.go) — depth count from
	// root: the file is one separator deeper than tmp itself.
	touch(t, filepath.Join(tmp, "lvl1", "should_be_seen.go"))
	// Depth 6: tmp/a/b/c/d/e/f/deep.py — 6 directory levels deep.
	touch(t, filepath.Join(tmp, "a", "b", "c", "d", "e", "f", "deep.py"))

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if !containsString(got, "go") {
		t.Errorf("shallow .go must be detected; got %v", got)
	}
	if containsString(got, "python") {
		t.Errorf("python past depth-5 should not be detected; got %v", got)
	}
}

// ------------------------------------------------------------------
// Small helpers (test-only)
// ------------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func mapKeys(m map[string]workspaceListJSONRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// yamlUnmarshalForTest is a tiny indirection so we can swap implementations
// without dragging the yaml import into every test file.
func yamlUnmarshalForTest(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}
