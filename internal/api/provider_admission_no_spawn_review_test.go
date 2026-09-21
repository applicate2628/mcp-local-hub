package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

// Inject below admission's classification, then ask the real process owner to
// reject the command. No test manufactures a positive cleanup wrapper.
func TestProviderAdmissionNoSpawnReviewRecovery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         string
		priorStarted bool
		wantProof    bool
	}{
		{name: "empty_command", kind: "empty", wantProof: true},
		{name: "nil_command", kind: "nil", wantProof: true},
		{name: "prior_started", kind: "empty", priorStarted: true, wantProof: true},
		{name: "wrapped_owner_error", kind: "wrapped"},
		{name: "joined_cleanup_failure", kind: "joined"},
		{name: "wrapped_joined_cleanup_failure", kind: "wrapped_join"},
		{name: "unknown_start_error", kind: "launch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "provider-no-spawn-review"
			clientPath, dir, state, rec, provider, deps := setupPendingProviderAdmission(t, name)
			before := string(mustReadFileForAdoptTest(t, clientPath))
			if tc.priorStarted {
				if err := markProviderInstallStartedForTask(name, supervisorTaskNameForManifestDaemon(name, adoptDefaultDaemonName)); err != nil {
					t.Fatal(err)
				}
			}
			previousStart := startStdioBridgeProvisionalFn
			t.Cleanup(func() { startStdioBridgeProvisionalFn = previousStart })
			var ownerErr error
			uncertainty := errors.New("additional cleanup outcome is unknown")
			startCalls := 0
			startStdioBridgeProvisionalFn = func(cmd *exec.Cmd) (*process.ProvisionalProcess, error) {
				startCalls++
				if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseStarted {
					t.Fatalf("owner entered before durable started barrier: phase=%q err=%v", phase, err)
				}
				switch tc.kind {
				case "nil":
					cmd = nil
				case "launch":
					cmd = exec.Command(filepath.Join(t.TempDir(), "absent-executable"))
				default:
					cmd.Path = ""
				}
				child, err := process.StartProvisional(cmd)
				if child != nil || err == nil || (cmd != nil && cmd.Process != nil) {
					t.Fatalf("real process owner did not refuse: child=%v err=%v", child, err)
				}
				ownerErr = err
				switch tc.kind {
				case "wrapped":
					err = fmt.Errorf("unconfirmed outer operation: %w", err)
				case "joined":
					err = errors.Join(err, uncertainty)
				case "wrapped_join":
					err = fmt.Errorf("unconfirmed outer operation: %w", errors.Join(err, uncertainty))
				}
				return nil, err
			}
			frozenStdioBridgeAdmissionFn = func(ctx context.Context, requests []FrozenStdioBridgeAdmissionRequest) error {
				err := admitFrozenStdioBridgeRequests(ctx, requests)
				_, proof := err.(*stdioBridgeAdmissionReapedError)
				if proof != tc.wantProof {
					t.Errorf("admission cleanup proof=%v want=%v: %v", proof, tc.wantProof, err)
				}
				return err
			}
			err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
			if startCalls != 1 || ownerErr == nil || !errors.Is(err, ownerErr) {
				t.Fatalf("owner failure did not survive admission wrapping: starts=%d owner=%v err=%v", startCalls, ownerErr, err)
			}
			if !strings.Contains(err.Error(), "stdio bridge admission: start provisional "+name+"/") {
				t.Errorf("start failure lost admission context: %v", err)
			}
			if (tc.kind == "joined" || tc.kind == "wrapped_join") && !errors.Is(err, uncertainty) {
				t.Errorf("joined uncertainty was lost: %v", err)
			}
			if got := string(mustReadFileForAdoptTest(t, clientPath)); got != before {
				t.Fatal("failed admission changed client config")
			}
			if tc.wantProof && !tc.priorStarted {
				if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseNotStarted {
					t.Errorf("no-spawn owner stranded provider history: phase=%q err=%v", phase, err)
				}
				assertProviderAdmissionRecovery(t, name, dir, state, rec, provider, deps)
			} else {
				assertProviderAdmissionRetained(t, name, dir, provider)
				plan, planErr := NewAPI().BuildDeAdoptPlan(name)
				if planErr == nil {
					if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: deps}); err == nil {
						t.Error("provider recovery accepted unconfirmed or prior started history")
					}
				}
				assertProviderAdmissionRetained(t, name, dir, provider)
			}
		})
	}
}

func TestProviderAdmissionNoSpawnReviewInvalidDescriptor(t *testing.T) {
	previous := startStdioBridgeProvisionalFn
	t.Cleanup(func() { startStdioBridgeProvisionalFn = previous })
	startStdioBridgeProvisionalFn = func(*exec.Cmd) (*process.ProvisionalProcess, error) {
		t.Fatal("invalid descriptor reached process owner")
		return nil, nil
	}
	err := admitFrozenStdioBridgeRequest(context.Background(), FrozenStdioBridgeAdmissionRequest{})
	if err == nil {
		t.Fatal("empty descriptor was accepted")
	}
	if _, proof := err.(*stdioBridgeAdmissionReapedError); proof {
		t.Fatalf("invalid descriptor acquired owner cleanup proof: %v", err)
	}
}
