package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClassifyCLITestHelperDispatch(t *testing.T) {
	root := filepath.Join(os.TempDir(), "cli-dispatch-fixture")
	vaultPath := filepath.Join(root, "secrets.age")
	keyPath := filepath.Join(root, ".age-key")
	markerPath := filepath.Join(root, "mapped-image-ready")
	exitSelector := "-test.run=^TestFormatChildExit_RealProcessShowsExitCode$"
	exitEnv := map[string]string{"MCPHUB_TEST_CHILD_EXIT_CODE": "42"}
	rootExitSelector := "-test.run=^TestDaemonRootExitChildProcess$"
	rootExitEnv := map[string]string{"MCPHUB_DAEMON_ROOT_EXIT_CHILD_CODE": "0"}
	staleEnv := map[string]string{staleExitHelperSentinelEnv: "1", staleExitHelperReleaseEnv: filepath.Join(root, "release")}
	overlayEnv := map[string]string{overlayMarkerHelperSentinelEnv: "1", overlayMarkerHelperDumpPathEnv: filepath.Join(root, "dump"), cliProductionArgvHelperEnv: "1"}
	secretArgs := []string{"test", "-test.run=^TestSecretsConcurrentSetHelperProcess$"}
	secretEnv := map[string]string{
		"MCPHUB_TEST_CONCURRENT_SET": "fixture-key",
		"MCPHUB_TEST_CONCURRENT_VAL": "",
		"MCPHUB_TEST_VAULT_PATH":     vaultPath,
		"MCPHUB_TEST_KEY_PATH":       keyPath,
	}
	withSecret := func(key, value string, remove bool) map[string]string {
		env := make(map[string]string, len(secretEnv))
		for key, value := range secretEnv {
			env[key] = value
		}
		if remove {
			delete(env, key)
		} else {
			env[key] = value
		}
		return env
	}
	invalid := cliTestHelperDispatch{invalid: true}
	legacy := cliTestHelperDispatch{runSelectedTest: true}
	mustExit := cliTestHelperDispatch{runSelectedTest: true, mustExit: true}
	secret := cliTestHelperDispatch{runSelectedTest: true, mustExit: true, stateRoot: root}
	windowsRole := mustExit
	windowsChecks := 2
	if runtime.GOOS != "windows" {
		windowsRole = invalid
		windowsChecks = 0
	}
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		want       cliTestHelperDispatch
		wantChecks int
		failCheck  int
		nilCheck   bool
	}{
		{name: "ordinary suite", args: []string{"test"}},
		{name: "ordinary empty selector", args: []string{"test", "-test.run=^$"}},
		{name: "ordinary split selector stays ordinary", args: []string{"test", "-test.run", "^TestOrdinary$"}},
		{name: "ordinary flag alias stays ordinary", args: []string{"test", "--test.run=^TestOrdinary$"}},
		{name: "ordinary repeated selector stays ordinary", args: []string{"test", "-test.run=^TestOrdinary$", "-test.run=.*"}},
		{name: "empty argv", want: invalid},
		{name: "legacy exit code", args: []string{"test", "-test.run=^TestFormatChildExit_RealProcessShowsExitCode$"}, env: map[string]string{"MCPHUB_TEST_CHILD_EXIT_CODE": "42"}, want: legacy},
		{name: "legacy release", args: []string{"test", "-test.run=^TestStaleExitReleaseHelper$"}, env: map[string]string{staleExitHelperSentinelEnv: "1", staleExitHelperReleaseEnv: filepath.Join(root, "release")}, want: legacy},
		{name: "legacy terminate", args: []string{"test", "-test.run=TestProductionTerminateFn_HelperSleep"}, env: map[string]string{"MCPHUB_PRODUCTION_TERMINATE_HELPER": "1"}, want: legacy},
		{name: "legacy PID", args: []string{"test", "-test.run=^TestPidMatchesMcphub_HelperSleep$"}, env: map[string]string{"MCPHUB_PID_MATCH_HELPER": "1"}, want: legacy},
		{name: "legacy overlay", args: []string{"test", "-test.run=^TestSpawnEnvDumpHelper$"}, env: map[string]string{overlayMarkerHelperSentinelEnv: "1", overlayMarkerHelperDumpPathEnv: filepath.Join(root, "dump")}, want: cliTestHelperDispatch{overlayDump: true}},
		{name: "legacy forensics", args: []string{"test", "-test.run=TestSupervisorStderrSink_CapturesRuntimePanic"}, env: map[string]string{forensicsSinkChildEnv: root}, want: cliTestHelperDispatch{forensicsStateDir: root}},
		{name: "legacy production argv", args: []string{"test", "supervise"}, env: map[string]string{cliProductionArgvHelperEnv: "1"}, want: cliTestHelperDispatch{productionArgv: true}},
		{name: "legacy large exit code", args: []string{"test", "-test.run=^TestFormatChildExit_LargeExitCodeShowsHex$"}, env: exitEnv, want: legacy},
		{name: "legacy release exact custody argv", args: []string{"test", "-test.run=^TestStaleExitReleaseHelper$", "-test.count=1", "-test.timeout=15s"}, env: staleEnv, want: legacy},
		{name: "legacy terminate anchored caller", args: []string{"test", "-test.run=^TestProductionTerminateFn_HelperSleep$"}, env: map[string]string{"MCPHUB_PRODUCTION_TERMINATE_HELPER": "1"}, want: legacy},
		{name: "legacy PID unanchored caller", args: []string{"test", "-test.run=TestPidMatchesMcphub_HelperSleep"}, env: map[string]string{"MCPHUB_PID_MATCH_HELPER": "1"}, want: legacy},
		{name: "legacy overlay raw production argv", args: []string{"test", "daemon", "serena-proxy", "--server", "serena", "--workspace", root, "--port", "9121", "--task-name", "fixture"}, env: overlayEnv, want: cliTestHelperDispatch{overlayDump: true}},
		{name: "legacy main-goroutine forensics", args: []string{"test", "-test.run=TestSupervisorStderrSink_CapturesMainGoroutinePanic"}, env: map[string]string{forensicsSinkChildEnv: root, forensicsSinkChildModeEnv: "main"}, want: cliTestHelperDispatch{forensicsStateDir: root}},
		{name: "legacy selector after double dash", args: []string{"test", "--", exitSelector}, env: exitEnv, want: invalid},
		{name: "legacy selector after positional", args: []string{"test", "positional", exitSelector}, env: exitEnv, want: invalid},
		{name: "legacy selector after single dash", args: []string{"test", "-", exitSelector}, env: exitEnv, want: invalid},
		{name: "legacy selector after empty positional", args: []string{"test", "", exitSelector}, env: exitEnv, want: invalid},
		{name: "legacy split selector", args: []string{"test", "-test.run", "^TestFormatChildExit_RealProcessShowsExitCode$"}, env: exitEnv, want: invalid},
		{name: "legacy double-dash selector", args: []string{"test", "--test.run=^TestFormatChildExit_RealProcessShowsExitCode$"}, env: exitEnv, want: invalid},
		{name: "legacy split double-dash selector", args: []string{"test", "--test.run", "^TestFormatChildExit_RealProcessShowsExitCode$"}, env: exitEnv, want: invalid},
		{name: "legacy later canonical selector overrides", args: []string{"test", exitSelector, "-test.run=.*"}, env: exitEnv, want: invalid},
		{name: "legacy later alias selector overrides", args: []string{"test", exitSelector, "--test.run=.*"}, env: exitEnv, want: invalid},
		{name: "legacy later split selector overrides", args: []string{"test", exitSelector, "-test.run", ".*"}, env: exitEnv, want: invalid},
		{name: "legacy later split alias selector overrides", args: []string{"test", exitSelector, "--test.run", ".*"}, env: exitEnv, want: invalid},
		{name: "legacy list suppresses execution", args: []string{"test", exitSelector, "-test.list=.*"}, env: exitEnv, want: invalid},
		{name: "legacy bench adds execution", args: []string{"test", exitSelector, "-test.bench=.*"}, env: exitEnv, want: invalid},
		{name: "legacy skip suppresses execution", args: []string{"test", exitSelector, "-test.skip=.*"}, env: exitEnv, want: invalid},
		{name: "legacy terminal delimiter unsupported", args: []string{"test", exitSelector, "--"}, env: exitEnv, want: invalid},
		{name: "legacy leading flag unsupported", args: []string{"test", "-test.v", exitSelector}, env: exitEnv, want: invalid},
		{name: "legacy trailing flag unsupported", args: []string{"test", exitSelector, "-test.v"}, env: exitEnv, want: invalid},
		{name: "legacy release split count unsupported", args: []string{"test", "-test.run=^TestStaleExitReleaseHelper$", "-test.count", "1", "-test.timeout=15s"}, env: staleEnv, want: invalid},
		{name: "legacy release count cannot skip helper", args: []string{"test", "-test.run=^TestStaleExitReleaseHelper$", "-test.count=0", "-test.timeout=15s"}, env: staleEnv, want: invalid},
		{name: "legacy release extra selector", args: []string{"test", "-test.run=^TestStaleExitReleaseHelper$", "-test.count=1", "-test.timeout=15s", "--test.run=.*"}, env: staleEnv, want: invalid},
		{name: "legacy overlay extra flag", args: []string{"test", "-test.run=^TestSpawnEnvDumpHelper$", "-test.list=.*"}, env: overlayEnv, want: invalid},
		{name: "legacy forensics extra flag", args: []string{"test", "-test.run=TestSupervisorStderrSink_CapturesRuntimePanic", "-test.skip=.*"}, env: map[string]string{forensicsSinkChildEnv: root}, want: invalid},
		{name: "CodeGraph exact caller argv", args: []string{"test", "-test.run=^TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper$", "--"}, env: map[string]string{"MCPHUB_CODEGRAPH_COMPAT_HELPER": "1"}, want: mustExit},
		{name: "CodeGraph nil path checker unused", args: []string{"test", "-test.run=^TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper$", "--"}, env: map[string]string{"MCPHUB_CODEGRAPH_COMPAT_HELPER": "1"}, want: mustExit, nilCheck: true},
		{name: "CodeGraph old unanchored caller", args: []string{"test", "-test.run=TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper", "--"}, env: map[string]string{"MCPHUB_CODEGRAPH_COMPAT_HELPER": "1"}, want: invalid},
		{name: "CodeGraph missing delimiter", args: []string{"test", "-test.run=^TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper$"}, env: map[string]string{"MCPHUB_CODEGRAPH_COMPAT_HELPER": "1"}, want: invalid},
		{name: "CodeGraph extra argv", args: []string{"test", "-test.run=^TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper$", "--", "extra"}, env: map[string]string{"MCPHUB_CODEGRAPH_COMPAT_HELPER": "1"}, want: invalid},
		{name: "CodeGraph missing marker", args: []string{"test", "-test.run=^TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper$", "--"}, want: invalid},
		{name: "CodeGraph empty marker", args: []string{"test"}, env: map[string]string{"MCPHUB_CODEGRAPH_COMPAT_HELPER": ""}, want: invalid},
		{name: "CodeGraph unsupported marker", args: []string{"test", "-test.run=^TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper$", "--"}, env: map[string]string{"MCPHUB_CODEGRAPH_COMPAT_HELPER": "2"}, want: invalid},
		{name: "secrets empty value is valid", args: secretArgs, env: secretEnv, want: secret, wantChecks: 4},
		{name: "secrets nonempty value", args: secretArgs, env: withSecret("MCPHUB_TEST_CONCURRENT_VAL", "fixture-value", false), want: secret, wantChecks: 4},
		{name: "secrets missing value", args: secretArgs, env: withSecret("MCPHUB_TEST_CONCURRENT_VAL", "", true), want: invalid},
		{name: "secrets empty key", args: secretArgs, env: withSecret("MCPHUB_TEST_CONCURRENT_SET", "", false), want: invalid},
		{name: "secrets orphan value", args: secretArgs, env: map[string]string{"MCPHUB_TEST_CONCURRENT_VAL": ""}, want: invalid},
		{name: "secrets relative vault", args: secretArgs, env: withSecret("MCPHUB_TEST_VAULT_PATH", "secrets.age", false), want: invalid},
		{name: "secrets wrong vault leaf", args: secretArgs, env: withSecret("MCPHUB_TEST_VAULT_PATH", filepath.Join(root, "other.age"), false), want: invalid},
		{name: "secrets wrong key leaf", args: secretArgs, env: withSecret("MCPHUB_TEST_KEY_PATH", filepath.Join(root, "other-key"), false), want: invalid},
		{name: "secrets different roots", args: secretArgs, env: withSecret("MCPHUB_TEST_KEY_PATH", filepath.Join(root, "other", ".age-key"), false), want: invalid},
		{name: "secrets broad selector", args: []string{"test", "-test.run=TestSecrets"}, env: secretEnv, want: invalid},
		{name: "secrets duplicate selector", args: []string{"test", secretArgs[1], secretArgs[1]}, env: secretEnv, want: invalid},
		{name: "secrets extra flag", args: []string{"test", secretArgs[1], "-test.v"}, env: secretEnv, want: invalid},
		{name: "secrets missing marker", args: secretArgs, want: invalid},
		{name: "secrets missing checker", args: secretArgs, env: secretEnv, want: invalid, nilCheck: true},
		{name: "secrets rejected root", args: secretArgs, env: secretEnv, want: invalid, failCheck: 1, wantChecks: 1},
		{name: "secrets rejected vault", args: secretArgs, env: secretEnv, want: invalid, failCheck: 2, wantChecks: 2},
		{name: "secrets rejected key", args: secretArgs, env: secretEnv, want: invalid, failCheck: 3, wantChecks: 3},
		{name: "secrets rejected lock leaf", args: secretArgs, env: secretEnv, want: invalid, failCheck: 4, wantChecks: 4},
		{name: "mixed new roles before native checks", args: secretArgs, env: withSecret("MCPHUB_CODEGRAPH_COMPAT_HELPER", "1", false), want: invalid},
		{name: "mixed legacy and new before native checks", args: secretArgs, env: withSecret("MCPHUB_PID_MATCH_HELPER", "1", false), want: invalid},
		{name: "mapped exact child", args: []string{"test", "-test.run=^TestWindowsMappedImageRenameRequiresExactGenerationReceipt$"}, env: map[string]string{"MCPHUB_MAPPED_IMAGE_RENAME_HELPER": markerPath}, want: windowsRole, wantChecks: windowsChecks},
		{name: "mapped parent remains ordinary", args: []string{"test", "-test.run=^TestWindowsMappedImageRenameRequiresExactGenerationReceipt$"}},
		{name: "mapped empty marker", args: []string{"test"}, env: map[string]string{"MCPHUB_MAPPED_IMAGE_RENAME_HELPER": ""}, want: invalid},
		{name: "mapped relative marker", args: []string{"test", "-test.run=^TestWindowsMappedImageRenameRequiresExactGenerationReceipt$"}, env: map[string]string{"MCPHUB_MAPPED_IMAGE_RENAME_HELPER": "relative"}, want: invalid},
		{name: "mapped missing checker", args: []string{"test", "-test.run=^TestWindowsMappedImageRenameRequiresExactGenerationReceipt$"}, env: map[string]string{"MCPHUB_MAPPED_IMAGE_RENAME_HELPER": markerPath}, want: invalid, nilCheck: true},
		{name: "precontrol exact child", args: []string{"test", "-test.run=^TestSupervisorLaunchChildFixture$"}, env: map[string]string{"MCPHUB_TEST_OWNED_PRECONTROL_CHILD": "1"}, want: windowsRole},
		{name: "precontrol unsupported marker", args: []string{"test", "-test.run=^TestSupervisorLaunchChildFixture$"}, env: map[string]string{"MCPHUB_TEST_OWNED_PRECONTROL_CHILD": "2"}, want: invalid},
		{name: "precontrol missing marker", args: []string{"test", "-test.run=^TestSupervisorLaunchChildFixture$"}, want: invalid},
		{name: "precontrol broad selector", args: []string{"test", "-test.run=TestSupervisorLaunch"}, env: map[string]string{"MCPHUB_TEST_OWNED_PRECONTROL_CHILD": "1"}, want: invalid},
		{name: "daemon root exit zero", args: []string{"test", rootExitSelector}, env: rootExitEnv, want: mustExit, nilCheck: true},
		{name: "daemon root exit seven", args: []string{"test", rootExitSelector}, env: map[string]string{"MCPHUB_DAEMON_ROOT_EXIT_CHILD_CODE": "7"}, want: mustExit},
		{name: "daemon root exit missing marker", args: []string{"test", rootExitSelector}, want: invalid},
		{name: "daemon root exit empty marker", args: []string{"test", rootExitSelector}, env: map[string]string{"MCPHUB_DAEMON_ROOT_EXIT_CHILD_CODE": ""}, want: invalid},
		{name: "daemon root exit malformed code", args: []string{"test", rootExitSelector}, env: map[string]string{"MCPHUB_DAEMON_ROOT_EXIT_CHILD_CODE": "invalid"}, want: invalid},
		{name: "daemon root exit unsupported code", args: []string{"test", rootExitSelector}, env: map[string]string{"MCPHUB_DAEMON_ROOT_EXIT_CHILD_CODE": "42"}, want: invalid},
		{name: "daemon root exit noncanonical code", args: []string{"test", rootExitSelector}, env: map[string]string{"MCPHUB_DAEMON_ROOT_EXIT_CHILD_CODE": "07"}, want: invalid},
		{name: "daemon root exit missing selector", args: []string{"test"}, env: rootExitEnv, want: invalid},
		{name: "daemon root exit wrong selector", args: []string{"test", "-test.run=^TestOrdinary$"}, env: rootExitEnv, want: invalid},
		{name: "daemon root exit broad selector", args: []string{"test", "-test.run=TestDaemonRootExitChildProcess"}, env: rootExitEnv, want: invalid},
		{name: "daemon root exit split selector", args: []string{"test", "-test.run", "^TestDaemonRootExitChildProcess$"}, env: rootExitEnv, want: invalid},
		{name: "daemon root exit selector alias", args: []string{"test", "--test.run=^TestDaemonRootExitChildProcess$"}, env: rootExitEnv, want: invalid},
		{name: "daemon root exit duplicate selector", args: []string{"test", rootExitSelector, rootExitSelector}, env: rootExitEnv, want: invalid},
		{name: "daemon root exit extra flag", args: []string{"test", rootExitSelector, "-test.v"}, env: rootExitEnv, want: invalid},
		{name: "daemon root exit mixed legacy role", args: []string{"test", rootExitSelector}, env: map[string]string{"MCPHUB_DAEMON_ROOT_EXIT_CHILD_CODE": "0", "MCPHUB_TEST_CHILD_EXIT_CODE": "42"}, want: invalid},
		{name: "daemon root exit mixed new role", args: []string{"test", rootExitSelector}, env: map[string]string{"MCPHUB_DAEMON_ROOT_EXIT_CHILD_CODE": "7", "MCPHUB_CODEGRAPH_COMPAT_HELPER": "1"}, want: invalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			type check struct {
				path string
				kind cliTestHelperPathKind
			}
			var checks []check
			checker := func(path string, kind cliTestHelperPathKind) error {
				checks = append(checks, check{path, kind})
				if tc.failCheck == len(checks) {
					return errors.New("synthetic native refusal")
				}
				return nil
			}
			if tc.nilCheck {
				checker = nil
			}
			got := classifyCLITestHelperDispatch(tc.args, func(key string) (string, bool) {
				value, present := tc.env[key]
				return value, present
			}, checker)
			if got != tc.want {
				t.Fatalf("dispatch = %+v, want %+v", got, tc.want)
			}
			if len(checks) != tc.wantChecks {
				t.Fatalf("path checks = %d, want %d", len(checks), tc.wantChecks)
			}
			if got.stateRoot != "" {
				want := []check{{root, cliTestHelperDirectory}, {vaultPath, cliTestHelperExistingFile}, {keyPath, cliTestHelperExistingFile}, {vaultPath + ".lock", cliTestHelperOptionalFile}}
				for i := range want {
					if checks[i] != want[i] {
						t.Fatalf("path check %d = %+v, want %+v", i, checks[i], want[i])
					}
				}
			}
		})
	}
}

func TestCLITestHelperPathChecks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "existing")
	if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	for _, tc := range []struct {
		name string
		path string
		kind cliTestHelperPathKind
		ok   bool
	}{
		{"directory", root, cliTestHelperDirectory, true},
		{"file as directory", file, cliTestHelperDirectory, false},
		{"missing directory", missing, cliTestHelperDirectory, false},
		{"existing file", file, cliTestHelperExistingFile, true},
		{"directory as file", root, cliTestHelperExistingFile, false},
		{"missing required file", missing, cliTestHelperExistingFile, false},
		{"optional absent file", missing, cliTestHelperOptionalFile, true},
		{"optional existing file", file, cliTestHelperOptionalFile, true},
		{"optional directory", root, cliTestHelperOptionalFile, false},
		{"fresh output", missing, cliTestHelperAbsentFile, true},
		{"existing output refused", file, cliTestHelperAbsentFile, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkCLITestHelperPath(tc.path, tc.kind); (err == nil) != tc.ok {
				t.Fatalf("path check error = %v, want success %v", err, tc.ok)
			}
		})
	}
	t.Run("symlink leaf refused", func(t *testing.T) {
		link := filepath.Join(root, "link")
		if err := os.Symlink(file, link); err != nil {
			t.Skipf("symlink fixture unavailable: %v", err)
		}
		if err := checkCLITestHelperPath(link, cliTestHelperExistingFile); err == nil {
			t.Fatal("symlink leaf accepted")
		}
	})
}
