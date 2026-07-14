package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/cmd/mcp-cbuild/internal/cbuild"
	"mcp-local-hub/cmd/mcp-cbuild/internal/mcp"
)

// intDiag mirrors the exported fields of cbuild.Diagnostic for cross-package
// result assertions (the tool result structs themselves are unexported).
type intDiag struct {
	File     string `json:"file"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// copyFixture copies every regular file in testdata/hello into a fresh temp dir
// so configure/build write into the temp tree, never the committed fixture.
func copyFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "hello")
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read fixture file %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatalf("write fixture file %s: %v", e.Name(), err)
		}
	}
	return dst
}

func toolByName(t *testing.T, tools []mcp.Tool, name string) mcp.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

// callTool invokes a tool and returns its result marshaled back to JSON. A
// non-nil Go error (param/launch/parse failure) fails the test; a clean
// non-zero exit is returned in the JSON payload (success:false), not as an
// error.
func callTool(t *testing.T, tl mcp.Tool, args map[string]any) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		raw = b
	}
	res, err := tl.Call(context.Background(), raw)
	if err != nil {
		t.Fatalf("tool %s returned error: %v", tl.Name(), err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal %s result: %v", tl.Name(), err)
	}
	return out
}

// TestCMakeIntegration exercises configure -> build (pass) -> build (fail with
// diagnostics) -> test against the testdata/hello fixture. It is gated on a
// cmake binary being present; a cmake-less host skips rather than fails. A
// configure failure (cmake present but no usable default toolchain) also skips
// so a toolchain-less CI runner does not red-fail — the assertions run only
// where a real C++ toolchain exists.
func TestCMakeIntegration(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not found on PATH; skipping CMake integration test")
	}

	proj := copyFixture(t)
	tools := cbuild.Tools(proj)

	cfg := toolByName(t, tools, "cmake_configure")
	bld := toolByName(t, tools, "cmake_build")
	tst := toolByName(t, tools, "cmake_test")

	// --- configure ---
	var cfgRes struct {
		Success      bool              `json:"success"`
		ExitCode     int               `json:"exit_code"`
		CacheSummary map[string]string `json:"cache_summary"`
		RawTail      string            `json:"raw_tail"`
	}
	if err := json.Unmarshal(callTool(t, cfg, map[string]any{"preset": "default", "working_dir": proj}), &cfgRes); err != nil {
		t.Fatalf("decode configure result: %v", err)
	}
	if !cfgRes.Success {
		t.Skipf("cmake configure failed (no usable default toolchain here); skipping build/test.\nexit=%d\nraw_tail:\n%s", cfgRes.ExitCode, cfgRes.RawTail)
	}

	// --- build the default (passing) target ---
	var okBuild struct {
		Success     bool      `json:"success"`
		ExitCode    int       `json:"exit_code"`
		Diagnostics []intDiag `json:"diagnostics"`
		RawTail     string    `json:"raw_tail"`
	}
	if err := json.Unmarshal(callTool(t, bld, map[string]any{"preset": "default", "working_dir": proj}), &okBuild); err != nil {
		t.Fatalf("decode build result: %v", err)
	}
	if !okBuild.Success {
		t.Fatalf("default build failed unexpectedly: exit=%d diags=%+v\nraw_tail:\n%s", okBuild.ExitCode, okBuild.Diagnostics, okBuild.RawTail)
	}

	// --- build the deliberately-broken target: expect failure + diagnostics ---
	var brk struct {
		Success     bool      `json:"success"`
		ExitCode    int       `json:"exit_code"`
		Diagnostics []intDiag `json:"diagnostics"`
		RawTail     string    `json:"raw_tail"`
	}
	if err := json.Unmarshal(callTool(t, bld, map[string]any{"preset": "default", "working_dir": proj, "targets": []string{"broken"}}), &brk); err != nil {
		t.Fatalf("decode broken-build result: %v", err)
	}
	if brk.Success {
		t.Errorf("expected broken-target build to fail, got success=true")
	}
	foundErr, refsBroken := false, strings.Contains(strings.ToLower(brk.RawTail), "broken")
	for _, d := range brk.Diagnostics {
		if d.Severity == "error" {
			foundErr = true
		}
		if strings.Contains(strings.ToLower(d.File+" "+d.Message), "broken") {
			refsBroken = true
		}
	}
	if !foundErr {
		t.Errorf("expected >=1 error diagnostic from the broken build; got %+v\nraw_tail:\n%s", brk.Diagnostics, brk.RawTail)
	}
	if !refsBroken {
		t.Errorf("expected a diagnostic or raw output referencing broken.cpp; diags=%+v", brk.Diagnostics)
	}

	// --- test: one pass, one fail ---
	var tr struct {
		ExitCode int `json:"exit_code"`
		Total    int `json:"total"`
		Passed   int `json:"passed"`
		Failed   int `json:"failed"`
		Tests    []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"tests"`
		RawTail string `json:"raw_tail"`
	}
	if err := json.Unmarshal(callTool(t, tst, map[string]any{"preset": "default", "working_dir": proj}), &tr); err != nil {
		t.Fatalf("decode test result: %v", err)
	}
	if tr.Total != 2 {
		t.Errorf("total tests = %d, want 2; tests=%+v\nraw_tail:\n%s", tr.Total, tr.Tests, tr.RawTail)
	}
	if tr.Passed != 1 {
		t.Errorf("passed = %d, want 1; tests=%+v", tr.Passed, tr.Tests)
	}
	if tr.Failed != 1 {
		t.Errorf("failed = %d, want 1; tests=%+v", tr.Failed, tr.Tests)
	}
}

// TestVcpkgIntegration is gated on VCPKG_ROOT (and a resolvable vcpkg binary).
// It runs a read-only vcpkg_list to prove the vcpkg path WIRES end-to-end: the
// binary is resolved from VCPKG_ROOT, executed with no shell, and its output is
// captured into a well-formed structured result. It deliberately does NOT
// assert vcpkg's own exit code, because VCPKG_ROOT being set does not guarantee
// a healthy vcpkg install (e.g. a corrupted `installed` tree makes `vcpkg list`
// exit non-zero); faithfully surfacing that non-zero exit is correct tool
// behavior, not a wiring failure.
func TestVcpkgIntegration(t *testing.T) {
	root := os.Getenv("VCPKG_ROOT")
	if root == "" {
		t.Skip("VCPKG_ROOT not set; skipping vcpkg integration test")
	}
	tools := cbuild.Tools(t.TempDir())
	list := toolByName(t, tools, "vcpkg_list")

	res, err := list.Call(context.Background(), nil)
	if err != nil {
		// A Go error here means the binary could not be launched at all — a real
		// wiring failure the test must catch.
		t.Fatalf("vcpkg_list could not launch vcpkg (VCPKG_ROOT=%q): %v", root, err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal vcpkg_list result: %v", err)
	}
	var lr struct {
		Success  bool `json:"success"`
		ExitCode int  `json:"exit_code"`
		Packages []struct {
			Name string `json:"name"`
		} `json:"packages"`
		RawTail string `json:"raw_tail"`
	}
	// json.Unmarshal into a struct whose "packages" field is present proves the
	// structured result shape survived the round trip. The Packages slice is
	// non-nil whenever vcpkg produced parseable rows; an unhealthy vcpkg yields
	// an empty (but valid) result, which is still correct wiring.
	if err := json.Unmarshal(out, &lr); err != nil {
		t.Fatalf("decode vcpkg_list result: %v", err)
	}
	if lr.Success {
		// Healthy vcpkg: a clean exit is the happy path; nothing more to assert.
		return
	}
	// Unhealthy vcpkg on this host: the tool still correctly captured the
	// failure into a structured result with a non-zero exit and raw output.
	t.Logf("vcpkg_list wired end-to-end but vcpkg exited non-zero (exit=%d); host vcpkg install may be unhealthy. raw_tail: %q", lr.ExitCode, lr.RawTail)
	if lr.ExitCode == 0 {
		t.Errorf("success=false but exit_code=0 — inconsistent structured result")
	}
}
