package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCLITestBinaryRejectsMalformedHelperDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  []string
	}{
		{name: "exit sentinel without selector", args: []string{"-test.run=^$"}, env: []string{"MCPHUB_TEST_CHILD_EXIT_CODE=42"}},
		{name: "pid sentinel with wrong selector", args: []string{"-test.run=^TestDoesNotExist$"}, env: []string{"MCPHUB_PID_MATCH_HELPER=1"}},
		{name: "selector without sentinel", args: []string{"-test.run=^TestProductionTerminateFn_HelperSleep$"}},
		{name: "conflicting helpers", args: []string{"-test.run=^$"}, env: []string{"MCPHUB_PRODUCTION_TERMINATE_HELPER=1", "MCPHUB_PID_MATCH_HELPER=1"}},
		{name: "unknown overlay sentinel value", args: []string{"-test.run=^$"}, env: []string{overlayMarkerHelperSentinelEnv + "=2"}},
		{name: "raw route", args: []string{"route"}},
		{name: "incomplete route", args: []string{"route", "--port"}},
		{name: "raw supervise", args: []string{"supervise"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(withoutCLITestHelperEnvironment(os.Environ()), tc.env...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil {
				t.Fatalf("malformed helper dispatch exited 0; stderr=%q", stderr.String())
			}
		})
	}
}

func withoutCLITestHelperEnvironment(env []string) []string {
	keys := []string{
		"MCPHUB_TEST_CHILD_EXIT_CODE",
		staleExitHelperSentinelEnv,
		staleExitHelperReleaseEnv,
		"MCPHUB_PRODUCTION_TERMINATE_HELPER",
		"MCPHUB_PID_MATCH_HELPER",
		overlayMarkerHelperSentinelEnv,
		overlayMarkerHelperDumpPathEnv,
		forensicsSinkChildEnv,
		forensicsSinkChildModeEnv,
		cliProductionArgvHelperEnv,
	}
	result := make([]string, 0, len(env))
	for _, entry := range env {
		keep := true
		for _, key := range keys {
			if len(entry) > len(key) && entry[:len(key)+1] == key+"=" {
				keep = false
				break
			}
		}
		if keep {
			result = append(result, entry)
		}
	}
	return result
}

func cliTestStateRoots(t *testing.T) map[string]struct{} {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "mcphub-cli-test-state-*"))
	if err != nil {
		t.Fatalf("glob CLI test-state roots: %v", err)
	}
	roots := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		roots[path] = struct{}{}
	}
	return roots
}

func TestCLITestBinaryHelpers_BypassPackageStateRoot(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     []string
		kill    bool
		prepare func(*testing.T) []string
	}{
		{
			name: "exit-code",
			args: []string{"-test.run=^TestFormatChildExit_RealProcessShowsExitCode$"},
			env:  []string{"MCPHUB_TEST_CHILD_EXIT_CODE=42"},
		},
		{
			name: "stale-exit-release",
			args: []string{"-test.run=^TestStaleExitReleaseHelper$"},
			prepare: func(t *testing.T) []string {
				releasePath := filepath.Join(t.TempDir(), "release")
				if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
					t.Fatalf("create stale-exit release marker: %v", err)
				}
				return []string{
					staleExitHelperSentinelEnv + "=1",
					staleExitHelperReleaseEnv + "=" + releasePath,
				}
			},
		},
		{
			name: "production-terminate",
			args: []string{"-test.run=^TestProductionTerminateFn_HelperSleep$"},
			env:  []string{"MCPHUB_PRODUCTION_TERMINATE_HELPER=1"},
			kill: true,
		},
		{
			name: "pid-match",
			args: []string{"-test.run=^TestPidMatchesMcphub_HelperSleep$"},
			env:  []string{"MCPHUB_PID_MATCH_HELPER=1"},
			kill: true,
		},
		{
			name: "production-supervisor-argv",
			args: []string{"supervise"},
			env:  []string{cliProductionArgvHelperEnv + "=1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := cliTestStateRoots(t)
			env := append([]string{}, tc.env...)
			if tc.prepare != nil {
				env = append(env, tc.prepare(t)...)
			}

			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(os.Environ(), env...)
			if err := cmd.Start(); err != nil {
				t.Fatalf("start helper child: %v", err)
			}
			if tc.kill {
				time.Sleep(100 * time.Millisecond)
				if err := cmd.Process.Kill(); err != nil {
					t.Fatalf("kill helper child: %v", err)
				}
			}
			_ = cmd.Wait()

			for path := range cliTestStateRoots(t) {
				if _, existed := before[path]; !existed {
					t.Errorf("helper child left package test-state root %s", path)
				}
			}
		})
	}
}
