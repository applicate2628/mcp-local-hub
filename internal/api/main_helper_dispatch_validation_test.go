package api

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyAPITestHelperDispatch(t *testing.T) {
	root, err := filepath.Abs("api-helper-fixture")
	if err != nil {
		t.Fatal(err)
	}
	type testCase struct {
		name     string
		selector string
		env      map[string]string
		want     apiTestHelperDispatch
	}
	tests := []testCase{
		{name: "ordinary suite"},
		{name: "oracle parent", selector: "^TestIntentAudit_CallerStartTimeAgainstIndependentOracle$"},
		{name: "fence parent", selector: "^TestUpgradeFenceReacquiresAfterHolderProcessDeath$"},
		{name: "canonical manifest without child marker", selector: "^TestCanonicalManifestDirHelperProcess$"},
		{
			name: "canonical manifest child", selector: "^TestCanonicalManifestDirHelperProcess$",
			env:  map[string]string{canonicalManifestDirHelperEnv: filepath.Join(root, "canonical-bin", "mcphub.exe")},
			want: apiTestHelperDispatch{runSelectedTest: true},
		},
		{
			name: "oracle child", selector: "^TestIntentAudit_CallerStartTimeAgainstIndependentOracle$",
			env:  map[string]string{callerStartTimeChildStateDirEnv: root, callerStartTimeChildDelayEnv: "3000"},
			want: apiTestHelperDispatch{callerStartTimeStateDir: root},
		},
		{
			name: "vault child", selector: "^TestVaultFlockHelperProcess$",
			env: map[string]string{
				"MCPHUB_TEST_HOLD_VAULT_LOCK": "1", "MCPHUB_TEST_VAULT_PATH": filepath.Join(root, "vault"),
				"MCPHUB_TEST_READY_PATH": filepath.Join(root, "ready"), "MCPHUB_TEST_RELEASE_PATH": filepath.Join(root, "release"),
			},
			want: apiTestHelperDispatch{runSelectedTest: true, mustExit: true},
		},
		{
			name: "fence child", selector: "^TestUpgradeFenceReacquiresAfterHolderProcessDeath$",
			env:  map[string]string{"MCPHUB_TEST_UPGRADE_FENCE_HOLDER": "1", "MCPHUB_TEST_UPGRADE_FENCE_STATE_DIR": root},
			want: apiTestHelperDispatch{runSelectedTest: true, mustExit: true},
		},
	}
	for _, role := range []string{"A-settle-window", "wait-try", "wait-try-hold", "try", "crash"} {
		tests = append(tests, testCase{
			name: "adopt " + role, selector: "^TestAdoptLeaseRaceChild$",
			env:  map[string]string{"MCPHUB_A2_ROLE": role, "MCPHUB_A2_STATE": root, "MCPHUB_A2_ENTRY": "owned-entry"},
			want: apiTestHelperDispatch{runSelectedTest: true, mustExit: role == "crash"},
		})
	}
	for _, mode := range []string{"ready", "legacy", "hang"} {
		tests = append(tests, testCase{
			name: "stdio " + mode, selector: "^TestFrozenStdioBridgeAdmissionHelper$",
			env:  map[string]string{stdioBridgeAdmissionHelperEnv: mode, stdioBridgeAdmissionHelperPortEnv: "12345"},
			want: apiTestHelperDispatch{runSelectedTest: true, mustExit: true},
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"api.test"}
			if tt.selector != "" {
				args = append(args, "-test.run="+tt.selector)
			}
			got := classifyAPITestHelperDispatch(args, func(key string) string { return tt.env[key] })
			if got != tt.want {
				t.Fatalf("dispatch = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestClassifyAPITestHelperDispatchRejectsUnsafeInputs(t *testing.T) {
	root, err := filepath.Abs("api-helper-fixture")
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		name     string
		selector string
		env      map[string]string
	}{
		{"oracle", "^TestIntentAudit_CallerStartTimeAgainstIndependentOracle$", map[string]string{callerStartTimeChildStateDirEnv: root, callerStartTimeChildDelayEnv: "3000"}},
		{"vault", "^TestVaultFlockHelperProcess$", map[string]string{"MCPHUB_TEST_HOLD_VAULT_LOCK": "1", "MCPHUB_TEST_VAULT_PATH": filepath.Join(root, "vault"), "MCPHUB_TEST_READY_PATH": filepath.Join(root, "ready"), "MCPHUB_TEST_RELEASE_PATH": filepath.Join(root, "release")}},
		{"fence", "^TestUpgradeFenceReacquiresAfterHolderProcessDeath$", map[string]string{"MCPHUB_TEST_UPGRADE_FENCE_HOLDER": "1", "MCPHUB_TEST_UPGRADE_FENCE_STATE_DIR": root}},
		{"adopt", "^TestAdoptLeaseRaceChild$", map[string]string{"MCPHUB_A2_ROLE": "try", "MCPHUB_A2_STATE": root, "MCPHUB_A2_ENTRY": "owned-entry"}},
		{"stdio", "^TestFrozenStdioBridgeAdmissionHelper$", map[string]string{stdioBridgeAdmissionHelperEnv: "ready", stdioBridgeAdmissionHelperPortEnv: "12345"}},
		{"canonical manifest", "^TestCanonicalManifestDirHelperProcess$", map[string]string{canonicalManifestDirHelperEnv: filepath.Join(root, "canonical-bin", "mcphub.exe")}},
	}
	reject := func(t *testing.T, args []string, env map[string]string) {
		t.Helper()
		if got := classifyAPITestHelperDispatch(args, func(key string) string { return env[key] }); got != (apiTestHelperDispatch{invalid: true}) {
			t.Fatalf("dispatch = %#v, want invalid without any admitted helper", got)
		}
	}
	for i, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			for _, args := range [][]string{
				{"api.test"},
				{"api.test", "-test.run=.*"},
				{"api.test", "-test.run=^$"},
				{"api.test", "-test.run=" + strings.Trim(fixture.selector, "^$")},
				{"api.test", "-test.run", fixture.selector},
				{"api.test", fixture.selector},
				{"api.test", "-test.run=" + fixture.selector, "-test.run=" + fixture.selector},
				{"api.test", "-test.run=" + fixture.selector, "-test.bench=.*"},
				{"api.test", "-test.run=" + fixture.selector, "-test.list=.*"},
				{"api.test", "-test.run=" + fixture.selector, "-test.count=0"},
			} {
				reject(t, args, fixture.env)
			}
			for j := i + 1; j < len(fixtures); j++ {
				mixed := make(map[string]string)
				for key, value := range fixture.env {
					mixed[key] = value
				}
				for key, value := range fixtures[j].env {
					mixed[key] = value
				}
				reject(t, []string{"api.test", "-test.run=" + fixture.selector}, mixed)
			}
		})
	}
	for _, tt := range []struct {
		fixture int
		key     string
		values  []string
	}{
		{0, callerStartTimeChildStateDirEnv, []string{"relative", root + string(filepath.Separator) + ".."}},
		{0, callerStartTimeChildDelayEnv, []string{"", "-1", "not-an-integer", "999999999999999999999999999"}},
		{1, "MCPHUB_TEST_HOLD_VAULT_LOCK", []string{"2"}},
		{1, "MCPHUB_TEST_VAULT_PATH", []string{"", "relative", root + "\x00"}},
		{1, "MCPHUB_TEST_READY_PATH", []string{"", "relative"}},
		{1, "MCPHUB_TEST_RELEASE_PATH", []string{"", "relative"}},
		{2, "MCPHUB_TEST_UPGRADE_FENCE_HOLDER", []string{"2"}},
		{2, "MCPHUB_TEST_UPGRADE_FENCE_STATE_DIR", []string{"", "relative"}},
		{3, "MCPHUB_A2_ROLE", []string{"unknown", "wait-try-unowned"}},
		{3, "MCPHUB_A2_STATE", []string{"", "relative"}},
		{3, "MCPHUB_A2_ENTRY", []string{"", "../escape", "owned.lease", "NUL"}},
		{4, stdioBridgeAdmissionHelperEnv, []string{"unknown"}},
		{4, stdioBridgeAdmissionHelperPortEnv, []string{"", "0", "-1", "65536", "not-an-integer"}},
		{5, canonicalManifestDirHelperEnv, []string{"relative", root + "\x00", root + string(filepath.Separator) + ".."}},
	} {
		fixture := fixtures[tt.fixture]
		for _, value := range tt.values {
			env := make(map[string]string)
			for key, original := range fixture.env {
				env[key] = original
			}
			env[tt.key] = value
			reject(t, []string{"api.test", "-test.run=" + fixture.selector}, env)
		}
	}
	for _, selector := range []string{"^TestVaultFlockHelperProcess$", "^TestAdoptLeaseRaceChild$", "^TestFrozenStdioBridgeAdmissionHelper$"} {
		reject(t, []string{"api.test", "-test.run=" + selector}, nil)
	}
}
