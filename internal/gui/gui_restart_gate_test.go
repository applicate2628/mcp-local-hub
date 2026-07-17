package gui

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func resetRestartV3ResolverForTest() {
	restartV3Once = sync.Once{}
	restartV3Resolved = false
}

func TestRestartV3Enabled_DefaultOverrideAndProcessStability(t *testing.T) {
	t.Cleanup(resetRestartV3ResolverForTest)

	tests := []struct {
		name string
		env  string
		want bool
	}{
		{name: "default off", env: "", want: false},
		{name: "one forces on", env: "1", want: true},
		{name: "true forces on case insensitive", env: " TrUe ", want: true},
		{name: "zero forces off", env: "0", want: false},
		{name: "false forces off case insensitive", env: " FaLsE ", want: false},
		{name: "unknown uses default", env: "enable", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetRestartV3ResolverForTest()
			t.Setenv(restartV3Env, tc.env)

			if got := RestartV3Enabled(); got != tc.want {
				t.Fatalf("RestartV3Enabled() with %s=%q = %v, want %v", restartV3Env, tc.env, got, tc.want)
			}

			if tc.want {
				t.Setenv(restartV3Env, "0")
			} else {
				t.Setenv(restartV3Env, "1")
			}
			if got := RestartV3Enabled(); got != tc.want {
				t.Fatalf("RestartV3Enabled() changed after its first resolution: got %v, want stable %v", got, tc.want)
			}
		})
	}
}

func TestRestartV3Enabled_ThreadsIntoServer(t *testing.T) {
	s := NewServer(Config{RestartV3Enabled: true})
	if !s.cfg.RestartV3Enabled {
		t.Fatal("NewServer did not retain the composition-root-resolved RestartV3 gate")
	}
}

func TestRestartV3FeatureGate_DefaultOffCreatesNoMarkerFiles(t *testing.T) {
	resetRestartV3ResolverForTest()
	t.Cleanup(resetRestartV3ResolverForTest)
	t.Setenv(restartV3Env, "")

	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

	enabled := RestartV3Enabled()
	if enabled {
		t.Fatal("RestartV3Enabled() = true by default, want false")
	}
	_ = NewServer(Config{RestartV3Enabled: enabled})

	for _, path := range []string{
		filepath.Join(stateDir, handoffMarkerFileLeaf),
		filepath.Join(stateDir, handoffMarkerFileLeaf) + ".lock",
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("gate-off server construction touched %s: stat error = %v", path, err)
		}
	}
}
