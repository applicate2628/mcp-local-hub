package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonTestHelperDispatch(t *testing.T) {
	root, err := filepath.Abs("daemon-helper-test-root")
	if err != nil {
		t.Fatal(err)
	}
	originalArgs := []string{"daemon.test", "-test.run=^TestStdioOriginalHelper$"}
	privateArgs := []string{"daemon.test", "-test.run=^TestOwnedIsolatedStdioFixture$"}
	directoryErr := errors.New("injected native directory refusal")
	type testCase struct {
		name              string
		args              []string
		env               map[string]string
		nativeUnavailable bool
		directoryError    error
		wantRole          daemonTestRole
		wantError         string
		wantChecks        int
	}
	tests := []testCase{
		{name: "ordinary suite", args: []string{"daemon.test", "-test.run=TestOrdinary", "-test.v=true"}},
		{name: "unmarked original selector", args: originalArgs},
		{name: "unmarked private selector", args: privateArgs},
		{name: "original missing mode", args: originalArgs, env: map[string]string{stdioOriginalHelperRoot: root}, wantError: "original stdio helper mode is invalid"},
		{name: "original missing root", args: originalArgs, env: map[string]string{stdioOriginalHelperMode: "echo"}, wantError: "absolute native directory"},
		{name: "original wrong selector", args: privateArgs, env: map[string]string{stdioOriginalHelperMode: "echo", stdioOriginalHelperRoot: root}, wantError: "original stdio helper requires its exact test selector"},
		{name: "original native unavailable", args: originalArgs, env: map[string]string{stdioOriginalHelperMode: "echo", stdioOriginalHelperRoot: root}, nativeUnavailable: true, wantError: "absolute native directory"},
		{name: "original native refusal", args: originalArgs, env: map[string]string{stdioOriginalHelperMode: "echo", stdioOriginalHelperRoot: root}, directoryError: directoryErr, wantError: directoryErr.Error(), wantChecks: 1},
		{name: "cooperative without gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "cooperative"}, wantRole: daemonTestPrivateHelper},
		{name: "ignore eof without gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "ignore-eof"}, wantRole: daemonTestPrivateHelper},
		{name: "grace wrapper gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "grace-wrapper", stdioPrivateHelperGate: root}, wantRole: daemonTestPrivateHelper, wantChecks: 1},
		{name: "grace member inherited gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "grace-member", stdioPrivateHelperGate: root}, wantRole: daemonTestPrivateHelper, wantChecks: 1},
		{name: "private native unavailable", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "cooperative"}, nativeUnavailable: true, wantError: "native Windows support"},
		{name: "private unknown mode", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "unknown"}, wantError: "private stdio helper mode is invalid"},
		{name: "private present empty mode", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: ""}, wantError: "private stdio helper mode is invalid"},
		{name: "private gate only", args: privateArgs, env: map[string]string{stdioPrivateHelperGate: root}, wantError: "private stdio helper mode is invalid"},
		{name: "private present empty gate only", args: privateArgs, env: map[string]string{stdioPrivateHelperGate: ""}, wantError: "private stdio helper mode is invalid"},
		{name: "grace missing gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "grace-wrapper"}, wantError: "absolute native gate directory"},
		{name: "grace present empty gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "grace-member", stdioPrivateHelperGate: ""}, wantError: "absolute native gate directory"},
		{name: "grace relative gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "grace-member", stdioPrivateHelperGate: "relative"}, wantError: "absolute native gate directory"},
		{name: "grace native refusal", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "grace-member", stdioPrivateHelperGate: root}, directoryError: directoryErr, wantError: directoryErr.Error(), wantChecks: 1},
		{name: "cooperative unexpected gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "cooperative", stdioPrivateHelperGate: root}, wantError: "does not accept a gate directory"},
		{name: "ignore eof present empty gate", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "ignore-eof", stdioPrivateHelperGate: ""}, wantError: "does not accept a gate directory"},
		{name: "private wrong selector", args: originalArgs, env: map[string]string{stdioPrivateHelperMode: "cooperative"}, wantError: "private stdio helper requires its exact test selector"},
		{name: "private missing selector", args: []string{"daemon.test"}, env: map[string]string{stdioPrivateHelperMode: "cooperative"}, wantError: "exact test selector"},
		{name: "private missing argv", env: map[string]string{stdioPrivateHelperMode: "cooperative"}, wantError: "exact test selector"},
		{name: "private broad selector", args: []string{"daemon.test", "-test.run=TestOwnedIsolatedStdioFixture"}, env: map[string]string{stdioPrivateHelperMode: "cooperative"}, wantError: "exact test selector"},
		{name: "private extra argument", args: []string{"daemon.test", privateArgs[1], "-test.v=true"}, env: map[string]string{stdioPrivateHelperMode: "cooperative"}, wantError: "exact test selector"},
		{name: "private duplicate selector", args: []string{"daemon.test", privateArgs[1], privateArgs[1]}, env: map[string]string{stdioPrivateHelperMode: "cooperative"}, wantError: "exact test selector"},
		{name: "original with private mode", args: originalArgs, env: map[string]string{stdioOriginalHelperMode: "echo", stdioOriginalHelperRoot: root, stdioPrivateHelperMode: "cooperative"}, wantError: "must not be mixed"},
		{name: "original with empty private marker", args: originalArgs, env: map[string]string{stdioOriginalHelperMode: "echo", stdioOriginalHelperRoot: root, stdioPrivateHelperMode: ""}, wantError: "must not be mixed"},
		{name: "original with private gate", args: originalArgs, env: map[string]string{stdioOriginalHelperMode: "echo", stdioOriginalHelperRoot: root, stdioPrivateHelperGate: root}, wantError: "must not be mixed"},
		{name: "private with partial original", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "cooperative", stdioOriginalHelperRoot: ""}, wantError: "must not be mixed"},
		{name: "mixed before native check", args: privateArgs, env: map[string]string{stdioPrivateHelperMode: "grace-member", stdioPrivateHelperGate: root, stdioOriginalHelperMode: "echo"}, nativeUnavailable: true, wantError: "must not be mixed"},
	}
	for _, mode := range []string{"echo", "final", "wrapper", "descendant", "sentinel"} {
		tests = append(tests, testCase{
			name: "original " + mode, args: originalArgs,
			env:      map[string]string{stdioOriginalHelperMode: mode, stdioOriginalHelperRoot: root},
			wantRole: daemonTestOriginalHelper, wantChecks: 1,
		})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			previous := stdioOriginalHelperDirectoryCheck
			t.Cleanup(func() { stdioOriginalHelperDirectoryCheck = previous })
			checks := 0
			stdioOriginalHelperDirectoryCheck = func(path string) error {
				checks++
				if path != root {
					t.Errorf("native check path=%q, want %q", path, root)
				}
				return tc.directoryError
			}
			if tc.nativeUnavailable {
				stdioOriginalHelperDirectoryCheck = nil
			}
			lookup := func(key string) (string, bool) {
				value, exists := tc.env[key]
				return value, exists
			}

			role, err := daemonTestHelperDispatch(tc.args, lookup)
			if tc.wantError == "" {
				if err != nil || role != tc.wantRole {
					t.Errorf("dispatch=(role=%d, err=%v), want role=%d without error", role, err, tc.wantRole)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("dispatch err=%v, want diagnostic containing %q", err, tc.wantError)
			}
			if tc.directoryError != nil && !errors.Is(err, tc.directoryError) {
				t.Errorf("native refusal not preserved: %v", err)
			}
			if checks != tc.wantChecks {
				t.Errorf("native directory checks=%d, want %d", checks, tc.wantChecks)
			}
		})
	}
}
