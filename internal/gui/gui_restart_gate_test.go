package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

type restartV3InertMatrixStarter struct {
	calls int
}

func (s *restartV3InertMatrixStarter) Start() (RestartCoordinatorStart, error) {
	s.calls++
	return RestartCoordinatorStart{
		HandoffID:  "inert-matrix-handoff",
		Generation: "inert-matrix-generation",
		Phase:      HandoffPhaseInProgress,
		SpawnedPID: 4242,
		OldPort:    9125,
		TargetPort: 9125,
	}, nil
}

// resetRestartV3ResolverForTest is a thin in-package alias for the exported
// ResetRestartV3ResolvedForTest (gui_restart_gate.go) — single owner of the
// reset logic (C1), kept here only so this file's existing call sites don't
// all need renaming.
func resetRestartV3ResolverForTest() {
	ResetRestartV3ResolvedForTest()
}

func TestRestartV3Enabled_DefaultOverrideAndProcessStability(t *testing.T) {
	t.Cleanup(resetRestartV3ResolverForTest)

	tests := []struct {
		name string
		env  string
		want bool
	}{
		{name: "default on", env: "", want: true},
		{name: "one forces on", env: "1", want: true},
		{name: "true forces on case insensitive", env: " TrUe ", want: true},
		{name: "zero forces off", env: "0", want: false},
		{name: "false forces off case insensitive", env: " FaLsE ", want: false},
		{name: "unknown uses default", env: "enable", want: true},
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

func TestRestartV3_FeatureGateInertMatrix(t *testing.T) {
	t.Cleanup(resetRestartV3ResolverForTest)

	t.Run("gate off is fully inert and returns manual guidance", func(t *testing.T) {
		resetRestartV3ResolverForTest()
		t.Setenv(restartV3Env, "0")
		stateDir := apitest.HardenedTempDir(t)
		t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

		exitCalls := 0
		originalExit := selfRestartExitFn
		selfRestartExitFn = func() { exitCalls++ }
		t.Cleanup(func() {
			selfRestartExitFn = originalExit
		})

		enabled := RestartV3Enabled()
		if enabled {
			t.Fatal("RestartV3Enabled() with explicit rollback override = true, want false")
		}
		coordinator := &restartV3InertMatrixStarter{}
		s := NewServer(Config{Port: 9125, RestartV3Enabled: enabled})
		s.restartCoordinator = coordinator
		req := httptest.NewRequest(http.MethodPost, "/api/gui/restart", nil)
		rr := httptest.NewRecorder()
		s.guiSelfRestartHandler(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("gate-off status = %d, want 503; body=%q", rr.Code, rr.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode gate-off response: %v", err)
		}
		if body["code"] != "GUI_RESTART_UNAVAILABLE" {
			t.Fatalf("gate-off code = %q, want GUI_RESTART_UNAVAILABLE", body["code"])
		}
		if !strings.Contains(strings.ToLower(body["error"]), "restart the gui manually") {
			t.Fatalf("gate-off guidance = %q, want explicit manual GUI restart guidance", body["error"])
		}
		if coordinator.calls != 0 || exitCalls != 0 {
			t.Fatalf("gate-off side effects: coordinator=%d exit=%d, want all zero", coordinator.calls, exitCalls)
		}
		for _, path := range []string{
			filepath.Join(stateDir, handoffMarkerFileLeaf),
			filepath.Join(stateDir, handoffMarkerFileLeaf) + ".lock",
		} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("gate-off request touched %s: stat error = %v", path, err)
			}
		}
	})

	t.Run("gate on activates the coordinator", func(t *testing.T) {
		resetRestartV3ResolverForTest()
		t.Setenv(restartV3Env, "1")
		enabled := RestartV3Enabled()
		if !enabled {
			t.Fatal("RestartV3Enabled() with explicit enable override = false, want true")
		}
		coordinator := &restartV3InertMatrixStarter{}
		s := NewServer(Config{Port: 9125, RestartV3Enabled: enabled})
		s.restartCoordinator = coordinator
		req := httptest.NewRequest(http.MethodPost, "/api/gui/restart", nil)
		rr := httptest.NewRecorder()
		s.guiSelfRestartHandler(rr, req)

		if rr.Code != http.StatusAccepted {
			t.Fatalf("gate-on status = %d, want 202; body=%q", rr.Code, rr.Body.String())
		}
		if coordinator.calls != 1 {
			t.Fatalf("gate-on coordinator calls = %d, want 1", coordinator.calls)
		}
	})
}
