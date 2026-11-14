//go:build windows

package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"mcp-local-hub/internal/gui"
)

// These operation tests cover the continuation after native classification.
// They do not substitute for an executable Product/native-owner replay test.
func TestCanonicalGUIRuntimeRecoveryUsesExistingIndependentSupervisor(t *testing.T) {
	for _, state := range []guiOwnedPriorSupervisorState{guiOwnedPriorSupervisorRecoveredIndependent, guiOwnedPriorSupervisorSame, guiOwnedPriorSupervisorAbsent} {
		t.Run(string(state), func(t *testing.T) {
			recorded := windowsProductSupervisorGenerationV1{PID: 203, StartedAt: "2026-09-29T00:00:00Z"}
			journal := windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{
				GUIOwner:   windowsProductGUIOwnerConfirmedPresent,
				Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled, Generation: &recorded},
			}}
			originalGeneration := recorded
			ops, lease, launch := guiPriorDiagnosticOpsFixture(t)
			var order []string
			launch.order = &order
			guiStarted := false
			ops.Readback = func(windowsProductRecoveryJournalV3) error { order = append(order, "readback"); return nil }
			ops.ObserveHealthy = func(context.Context, windowsProductRecoveryJournalV3) error {
				order = append(order, "health")
				if !guiStarted {
					return errors.New("GUI absent after previous supervisor restart")
				}
				return nil
			}
			ops.Reservation = func(context.Context) (gui.OwnedSingleInstanceLease, error) {
				order = append(order, "reserve")
				return lease, nil
			}
			lease.onRelease = func() { order = append(order, "release") }
			ops.SupervisorState = func(context.Context, windowsProductRecoveryJournalV3) (guiOwnedPriorSupervisorState, error) {
				order = append(order, "classify")
				return state, nil
			}
			ops.SettleSame = func(context.Context, windowsProductRecoveryJournalV3) error {
				order = append(order, "reap")
				return nil
			}
			ops.ProveAbsent = func(context.Context, windowsProductRecoveryJournalV3) error {
				order = append(order, "absence")
				return nil
			}
			ops.StartSupervisor = func() (SupervisorLaunch, error) { order = append(order, "start-supervisor"); return launch, nil }
			ops.WaitSupervisorReady = func(context.Context) error { order = append(order, "creator-readiness"); return nil }
			ops.ProveIndependent = func(context.Context, windowsProductRecoveryJournalV3) error {
				order = append(order, "fleet")
				return nil
			}
			ops.StartGUI = func() error {
				if lease.released != 1 {
					t.Fatal("GUI started before its reservation was released")
				}
				order = append(order, "start-gui")
				guiStarted = true
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := recoverGUIOwnedPriorSettlementWithOps(ctx, journal, ops); err != nil {
				t.Fatal(err)
			}
			want := []string{"readback", "health", "reserve", "classify"}
			if state == guiOwnedPriorSupervisorSame {
				want = append(want, "reap")
			}
			if state != guiOwnedPriorSupervisorRecoveredIndependent {
				want = append(want, "absence", "start-supervisor", "bind-launch", "creator-readiness")
			}
			want = append(want, "fleet", "release", "start-gui", "readback", "health")
			if state != guiOwnedPriorSupervisorRecoveredIndependent {
				want = append(want, "handoff-launch")
			}
			if !reflect.DeepEqual(order, want) || lease.released != 1 || launch.aborts != 0 {
				t.Fatalf("prior recovery effects differ: order=%v want=%v lease=%+v launch=%+v", order, want, lease, launch)
			}
			if state == guiOwnedPriorSupervisorRecoveredIndependent && (launch.binds != 0 || launch.handoffs != 0) {
				t.Fatal("replay acquired creator authority over an observed supervisor")
			}
			if recorded != originalGeneration {
				t.Fatal("replay rewrote the journal's original generation")
			}
		})
	}
}

func TestCanonicalGUIRuntimeRecoveryReplayFailuresPreserveSupervisor(t *testing.T) {
	for _, failAt := range []upgradeGUIPriorOperation{upgradeGUIPriorIndependentFleet, upgradeGUIPriorGUIStart} {
		t.Run(string(failAt), func(t *testing.T) {
			journal := windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}}
			ops, lease, launch := guiPriorDiagnosticOpsFixture(t)
			initial := errors.New("previous recovery has no GUI yet")
			failure := errors.New("injected replay continuation failure")
			ops.ObserveHealthy = func(context.Context, windowsProductRecoveryJournalV3) error { return initial }
			ops.SupervisorState = func(context.Context, windowsProductRecoveryJournalV3) (guiOwnedPriorSupervisorState, error) {
				return guiOwnedPriorSupervisorRecoveredIndependent, nil
			}
			forbiddenCalls, guiStarts := 0, 0
			ops.SettleSame = func(context.Context, windowsProductRecoveryJournalV3) error { forbiddenCalls++; return nil }
			ops.ProveAbsent = func(context.Context, windowsProductRecoveryJournalV3) error { forbiddenCalls++; return nil }
			ops.StartSupervisor = func() (SupervisorLaunch, error) { forbiddenCalls++; return launch, nil }
			ops.WaitSupervisorReady = func(context.Context) error { forbiddenCalls++; return nil }
			ops.ProveIndependent = func(context.Context, windowsProductRecoveryJournalV3) error {
				if failAt == upgradeGUIPriorIndependentFleet {
					return failure
				}
				return nil
			}
			ops.StartGUI = func() error { guiStarts++; return failure }
			err := recoverGUIOwnedPriorSettlementWithOps(context.Background(), journal, ops)
			var diagnostic *upgradeGUIPriorSettlementError
			if !errors.Is(err, failure) || !errors.Is(err, initial) || !errors.Is(err, ErrWindowsProductRecoveryPending) || !errors.As(err, &diagnostic) || diagnostic.operation != failAt {
				t.Fatalf("replay failure lost its operation/cause: %v", err)
			}
			wantGUIStarts := 0
			if failAt == upgradeGUIPriorGUIStart {
				wantGUIStarts = 1
			}
			if forbiddenCalls != 0 || guiStarts != wantGUIStarts || lease.released != 1 || launch.binds != 0 || launch.aborts != 0 || launch.handoffs != 0 {
				t.Fatalf("failed replay disturbed the observed supervisor: forbidden=%d GUI=%d lease=%+v launch=%+v", forbiddenCalls, guiStarts, lease, launch)
			}
		})
	}
}
