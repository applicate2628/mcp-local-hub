package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReduceReadinessV1_ExternalMCPRequiresEveryCurrentGenerationStage(t *testing.T) {
	request := ReadinessRequest{
		Now: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		Observations: []DaemonReadinessObservationV1{
			readyExternalObservation("mcp-local-hub-serena-codex"),
		},
		Bindings: []BindingObservationV1{{
			Client: "codex-cli", Present: true, Enabled: true, Route: BindingRouteHub,
			ExactHubRoute: true, Requested: true, Readable: true,
		}},
	}

	snapshot := ReduceReadinessV1(request)
	if snapshot.Classification != ReadinessClassificationInstalledAndBound {
		t.Fatalf("classification = %q, want installed-and-bound", snapshot.Classification)
	}
	if snapshot.MaterializationState != MaterializationReady || snapshot.BindingState != BindingStateHub {
		t.Fatalf("axes = (%q, %q), want (ready, hub)", snapshot.MaterializationState, snapshot.BindingState)
	}
	if !snapshot.Settled || len(snapshot.Daemons) != 1 || snapshot.Daemons[0].ServiceState != ServiceStateRunning {
		t.Fatalf("healthy external observation did not settle to running: %#v", snapshot)
	}

	for _, missing := range []struct {
		name   string
		mutate func(*DaemonReadinessObservationV1)
		stage  ReadinessStageV1
	}{
		{"listener", func(o *DaemonReadinessObservationV1) { o.ListenerReady = false }, ReadinessStageUpstreamListener},
		{"initialize", func(o *DaemonReadinessObservationV1) { o.MCPInitializeReady = false }, ReadinessStageMCPInitialize},
		{"tools-list", func(o *DaemonReadinessObservationV1) { o.MCPToolsListReady = false }, ReadinessStageMCPToolsList},
	} {
		t.Run(missing.name, func(t *testing.T) {
			broken := readyExternalObservation("mcp-local-hub-serena-codex")
			missing.mutate(&broken)
			got := ReduceReadinessV1(ReadinessRequest{
				Now:          request.Now,
				Observations: []DaemonReadinessObservationV1{broken},
			})
			if got.Daemons[0].ServiceState == ServiceStateRunning {
				t.Fatalf("missing %s promoted service Running: %#v", missing.name, got.Daemons[0])
			}
			if got.Daemons[0].Stage != missing.stage {
				t.Fatalf("stage = %q, want %q", got.Daemons[0].Stage, missing.stage)
			}
		})
	}
}

func TestReduceReadinessV1_StaleGenerationNeverPromotesAndFailureOrderIsStable(t *testing.T) {
	stale := readyExternalObservation("mcp-local-hub-serena-codex")
	stale.ObservedPIDGeneration = 41
	stale.CurrentPIDGeneration = 42
	stale.Failures = []ReadinessFailureV1{
		{Stage: ReadinessStageMCPToolsList, FailureID: "mcp_tools_list_failed", TaskName: stale.TaskName},
		{Stage: ReadinessStageIntentAccess, FailureID: "intent_unreadable", TaskName: stale.TaskName, Detail: "private-marker\nsecond-line"},
	}

	got := ReduceReadinessV1(ReadinessRequest{Observations: []DaemonReadinessObservationV1{stale}})
	if got.Daemons[0].ServiceState == ServiceStateRunning {
		t.Fatalf("stale generation promoted Running: %#v", got.Daemons[0])
	}
	if got.PrimaryFailure == nil || got.PrimaryFailure.Stage != ReadinessStageIntentAccess {
		t.Fatalf("primary failure = %#v, want intent_access", got.PrimaryFailure)
	}
	if strings.Contains(got.PrimaryFailure.Detail, "private-marker") || strings.Contains(got.PrimaryFailure.Detail, "\n") {
		t.Fatalf("unsafe detail escaped reducer: %q", got.PrimaryFailure.Detail)
	}
}

func TestReduceReadinessV1_ClassificationPrecedenceMatrix(t *testing.T) {
	tests := []struct {
		name           string
		request        ReadinessRequest
		classification ReadinessClassificationV1
		binding        BindingStateV1
	}{
		{
			name: "unhealthy dominates disabled binding",
			request: ReadinessRequest{
				Observations: []DaemonReadinessObservationV1{{TaskName: "d", IntentPresent: true, IntentRunnable: true, CurrentPIDGeneration: 1, ObservedPIDGeneration: 1, WrapperStarted: true, DeadlineExceeded: true}},
				Bindings:     []BindingObservationV1{{Client: "codex-cli", Present: true, Disabled: true, Readable: true, Route: BindingRouteHub}},
			},
			classification: ReadinessClassificationUnhealthyDegraded,
			binding:        BindingStateDisabled,
		},
		{
			name: "ready with disabled client is unbound",
			request: ReadinessRequest{
				Observations: []DaemonReadinessObservationV1{readyExternalObservation("d")},
				Bindings:     []BindingObservationV1{{Client: "codex-cli", Present: true, Disabled: true, Readable: true, Route: BindingRouteHub}},
			},
			classification: ReadinessClassificationInstalledUnbound,
			binding:        BindingStateDisabled,
		},
		{
			name: "direct migratable",
			request: ReadinessRequest{
				CanMigrate: true,
				Bindings:   []BindingObservationV1{{Client: "codex-cli", Present: true, Enabled: true, Readable: true, Route: BindingRouteDirect}},
			},
			classification: ReadinessClassificationConfiguredDirectCanMigrate,
			binding:        BindingStateDirect,
		},
		{
			name:           "only disabled entries",
			request:        ReadinessRequest{Bindings: []BindingObservationV1{{Client: "codex-cli", Present: true, Disabled: true, Readable: true, Route: BindingRouteDirect}}},
			classification: ReadinessClassificationDisabled,
			binding:        BindingStateDisabled,
		},
		{
			name:           "manifest alone is not materialization",
			request:        ReadinessRequest{ManifestPresence: true},
			classification: ReadinessClassificationGenuinelyNotInstalled,
			binding:        BindingStateNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReduceReadinessV1(tt.request)
			if got.Classification != tt.classification || got.BindingState != tt.binding {
				t.Fatalf("got classification=%q binding=%q, want classification=%q binding=%q", got.Classification, got.BindingState, tt.classification, tt.binding)
			}
		})
	}
}

func TestAssessReadiness_UsesOnlyInjectedObservationsAndCancellationDoesNotPromote(t *testing.T) {
	api := NewAPI()
	request := ReadinessRequest{Observations: []DaemonReadinessObservationV1{readyExternalObservation("d")}}

	got, err := api.AssessReadiness(context.Background(), request)
	if err != nil || got.Daemons[0].ServiceState != ServiceStateRunning {
		t.Fatalf("injected healthy observation = (%#v, %v), want running without error", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = api.AssessReadiness(ctx, request)
	var readinessErr *ReadinessErrorV1
	if !errors.As(err, &readinessErr) || readinessErr.FailureID != "readiness_cancelled" {
		t.Fatalf("cancel error = %v, want typed readiness_cancelled", err)
	}
	if got.Classification != ReadinessClassificationUnhealthyDegraded || got.PrimaryFailure == nil || got.PrimaryFailure.FailureID != "readiness_cancelled" {
		t.Fatalf("cancellation promoted or lost failure: %#v", got)
	}
}

func readyExternalObservation(task string) DaemonReadinessObservationV1 {
	return DaemonReadinessObservationV1{
		TaskName: task, Server: "serena", Daemon: "codex", Port: 9304,
		IntentPresent: true, IntentRunnable: true,
		CurrentPIDGeneration: 7, ObservedPIDGeneration: 7, PID: 91,
		WrapperStarted: true, ListenerReady: true,
		Policy:             ReadinessPolicyMCPUpstream,
		MCPInitializeReady: true, MCPToolsListReady: true,
	}
}
