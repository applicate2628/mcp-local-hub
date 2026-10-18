package cli

// Tests for the install readiness surfacing (install-and-it-works Area 2,
// Phase 1): renderReadinessReport + the `mcphub install --check` flag.
//
// STATE SAFETY: renderReadinessReport is a pure formatter over an in-memory
// report. The --check command test seeds a hermetic manifest catalog via
// MCPHUB_MANIFEST_DIR_OVERRIDE and exercises ONLY the read-only --check
// short-circuit, which returns before api.Install — no scheduler, no client
// config, no supervisor, no binary copy.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestRenderReadinessReport_BlockerStopsAndShowsFix verifies a non-optional
// unmet requirement is rendered with its guided Fix and reports blocked=true
// (the caller hard-stops the install).
func TestRenderReadinessReport_BlockerStopsAndShowsFix(t *testing.T) {
	rep := &api.ReadinessReport{
		Server: "wolfram",
		Ready:  false,
		Requirements: []api.ReadinessRequirement{
			{Name: "launcher: uv (Python tool runner)", OK: false, Optional: false,
				Reason: `"uvx" not found on PATH`,
				Fix:    "Install uv — Windows: `winget install astral-sh.uv`."},
		},
	}
	var out bytes.Buffer
	blocked := renderReadinessReport(&out, rep)
	if !blocked {
		t.Fatal("renderReadinessReport returned blocked=false for a non-optional unmet requirement")
	}
	s := out.String()
	if !strings.Contains(s, "not ready to install") {
		t.Errorf("output missing not-ready header:\n%s", s)
	}
	if !strings.Contains(s, "winget install astral-sh.uv") {
		t.Errorf("output missing the guided Fix:\n%s", s)
	}
}

// TestRenderReadinessReport_OptionalUnsetIsAdvisoryNonBlocking verifies the
// SECRETS-OPTIONAL invariant: an unmet OPTIONAL requirement renders as a
// non-blocking advisory (blocked=false → install PROCEEDS), and the render
// never flips it into a blocker.
func TestRenderReadinessReport_OptionalUnsetIsAdvisoryNonBlocking(t *testing.T) {
	rep := &api.ReadinessReport{
		Server: "wolfram",
		Ready:  true,
		Requirements: []api.ReadinessRequirement{
			{Name: "launcher: uv (Python tool runner)", OK: true},
			{Name: "secret: wolfram_app_id", OK: false, Optional: true,
				Reason: "could not be resolved from the vault (optional — the server otherwise runs without it)",
				Fix:    "Enter wolfram_app_id at install, or `mcphub secrets set wolfram_app_id`."},
		},
	}
	var out bytes.Buffer
	blocked := renderReadinessReport(&out, rep)
	if blocked {
		t.Fatal("renderReadinessReport returned blocked=true for an OPTIONAL unmet requirement; the secrets-optional invariant requires non-blocking")
	}
	s := out.String()
	if !strings.Contains(s, "secret: wolfram_app_id") {
		t.Errorf("advisory did not name the secret key:\n%s", s)
	}
	if !strings.Contains(s, "proceed") {
		t.Errorf("advisory did not signal the install will proceed:\n%s", s)
	}
	// Security: the KEY name is rendered, never a value (there is none here, but
	// assert the verbatim Reason/Fix passed through unmodified).
	if !strings.Contains(s, "mcphub secrets set wolfram_app_id") {
		t.Errorf("advisory Fix not rendered verbatim:\n%s", s)
	}
}

// TestInstallCheckFlag_MissingLauncherExitsZeroNoMutation drives the real
// `mcphub install --server <name> --check` command against a hermetic
// missing-launcher manifest. It asserts: the guided Fix is printed, the command
// exits 0 (no error), and NOTHING is mutated (the seeded catalog dir is the
// only on-disk surface and stays a single manifest file — no install plan
// applied because --check returns before api.Install).
func TestInstallCheckFlag_MissingLauncherExitsZeroNoMutation(t *testing.T) {
	// Redirect every state surface CheckServerReadiness reads (registry path,
	// settings, vault, canonical-binary lookup) to a temp HOME so the probe
	// touches no live host state. The readiness path is read-only by design, but
	// the redirect makes the test hermetic regardless.
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "Local"))
	t.Setenv("APPDATA", filepath.Join(home, "Roaming"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	name := "needs-uvx-zzz"
	srvDir := filepath.Join(dir, name)
	if err := os.MkdirAll(srvDir, 0o700); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	// Launcher is a name guaranteed absent from PATH so the launcher admission
	// finding is a BLOCKER with a guided Fix.
	body := "name: " + name + "\nkind: global\ntransport: stdio-bridge\ncommand: this-launcher-definitely-not-on-path-zzz\ndaemons:\n  - name: default\n    port: 51234\n"
	if err := os.WriteFile(filepath.Join(srvDir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cmd := newInstallCmdReal()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", name, "--check"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("`install --check` returned a non-nil error (must exit 0): %v\noutput:\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "not ready to install") {
		t.Errorf("--check output missing the blocker header:\n%s", s)
	}
	// The launcher Fix comes from LauncherGuidance (single owner); assert the
	// guided fix is surfaced rather than only the cryptic Reason.
	if !strings.Contains(s, "Fix:") {
		t.Errorf("--check output missing the guided Fix line:\n%s", s)
	}

	// No mutation: the only on-disk entry under the seeded catalog dir is the
	// manifest we wrote. A real install would create supervisor-intent /
	// client-config / scheduler artifacts; --check creates none. Assert the
	// catalog dir still contains exactly the one seeded server.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read seeded dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		t.Errorf("--check mutated the catalog dir; entries = %v, want only %q", dirEntryNames(entries), name)
	}
}

// TestInstallCheckFlag_RequiresServer verifies --check refuses without --server
// (a read-only probe needs a target) rather than silently no-op'ing.
func TestInstallCheckFlag_RequiresServer(t *testing.T) {
	cmd := newInstallCmdReal()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--check"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("`install --check` without --server must error")
	}
	if !strings.Contains(err.Error(), "--check requires --server") {
		t.Errorf("unexpected error: %v", err)
	}
}

func dirEntryNames(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// A project apply delegates to the existing API read/admission owner even when
// bare command discovery fails. The missing manifest makes that boundary inert:
// no backend, configuration, scheduler or bootstrap operation can be reached.
func TestProjectInstallReachesAdmissionWithoutBareRuntimePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		loose bool
	}{
		{name: "missing bare command"},
		{name: "relative current directory image", loose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			project := t.TempDir()
			catalog := t.TempDir()
			cwd := t.TempDir()
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOME", home)
			t.Setenv("LOCALAPPDATA", filepath.Join(home, "Local"))
			t.Setenv("APPDATA", filepath.Join(home, "Roaming"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", catalog)
			t.Setenv("GODEBUG", "execerrdot=1")
			t.Setenv("PATH", "")
			t.Chdir(cwd)

			loosePath := filepath.Join(cwd, mcphubShortName)
			marker := []byte("inert fixture; never execute\n")
			wantLookup := exec.ErrNotFound
			if tc.loose {
				if err := os.WriteFile(loosePath, marker, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", ".")
				wantLookup = exec.ErrDot
			}
			if _, err := exec.LookPath(mcphubShortName); !errors.Is(err, wantLookup) {
				t.Fatalf("bare lookup precondition = %v, want %v", err, wantLookup)
			}

			const server = "project-runtime-admission-fixture"
			cmd := newInstallCmdReal()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{
				"--server", server, "--daemon", "default", "--clients", "codex-cli",
				"--project-client-root", project, "--project-client-entry", "fixture",
				"--project-client-policy-source", "native-defaults",
				"--create-project-client", "--project-client-plan-sha256", strings.Repeat("0", 64),
			})
			err := cmd.Execute()
			var pathErr *os.PathError
			wantPath := filepath.Join(catalog, server, "manifest.yaml")
			if !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathErr) || pathErr.Path != wantPath {
				t.Fatalf("project apply must retain the API manifest read refusal, got %v; output: %s", err, out.String())
			}

			for _, dir := range []string{home, project, catalog} {
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("project refusal mutated %s: entries=%v error=%v", dir, entries, err)
				}
			}
			entries, err := os.ReadDir(cwd)
			wantEntries := 0
			if tc.loose {
				wantEntries = 1
				got, readErr := os.ReadFile(loosePath)
				if readErr != nil || !bytes.Equal(got, marker) {
					t.Fatalf("unrelated cwd image changed: bytes=%q error=%v", got, readErr)
				}
			}
			if err != nil || len(entries) != wantEntries {
				t.Fatalf("project refusal mutated cwd: entries=%v error=%v", entries, err)
			}
		})
	}
}
