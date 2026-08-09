package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestActivateMCPFrontRoute_StagesDescriptorBeforePreflight(t *testing.T) {
	var order []string
	err := activateMCPFrontRoute(context.Background(), 9555, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error {
			order = append(order, "stage")
			return nil
		},
		preflight: func(context.Context, int) error {
			order = append(order, "preflight")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("activate route: %v", err)
	}
	if got := len(order); got != 2 || order[0] != "stage" || order[1] != "preflight" {
		t.Fatalf("activation order=%v, want [stage preflight]", order)
	}

	preflightCalled := false
	sentinel := errors.New("stage failed")
	err = activateMCPFrontRoute(context.Background(), 9555, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error { return sentinel },
		preflight: func(context.Context, int) error {
			preflightCalled = true
			return nil
		},
	})
	if !errors.Is(err, sentinel) || preflightCalled {
		t.Fatalf("stage failure err=%v preflightCalled=%v, want failure before probe", err, preflightCalled)
	}
}

func TestStageMCPFrontRouteDaemon_PersistsExactPortBeforeSupervisorReconcile(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	const port = 9555
	reconcileCalls := 0

	err := stageMCPFrontRouteDaemonWithOps(context.Background(), stateDir, port, mcpFrontRouteStageOps{
		commandPath: func() string { return "mcphub-test" },
		reconcile: func(context.Context, bool) (api.ReconcileResponse, error) {
			reconcileCalls++
			intent, err := api.ReadSupervisorIntent(intentPath)
			if err != nil {
				t.Fatalf("read staged intent: %v", err)
			}
			want := api.BuildBuiltinRouteDaemon("mcphub-test", port)
			if len(intent.Daemons) != 1 || intent.Daemons[0].TaskName != want.TaskName || intent.Daemons[0].Port != port {
				t.Fatalf("reconcile observed intent=%+v, want exact route descriptor at %d", intent.Daemons, port)
			}
			return api.ReconcileResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("stage route daemon: %v", err)
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls=%d, want 1", reconcileCalls)
	}
}
