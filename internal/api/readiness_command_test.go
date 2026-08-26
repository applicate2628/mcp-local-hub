package api

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

func TestInstallRelayExePath_FrozenOnceAcrossExpectationAndWriter(t *testing.T) {
	original := canonicalInstallRelayPathFn
	t.Cleanup(func() { canonicalInstallRelayPathFn = original })

	first := filepath.Join(t.TempDir(), "first-mcphub")
	second := filepath.Join(t.TempDir(), "second-mcphub")
	calls := 0
	canonicalInstallRelayPathFn = func() (string, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}

	plan := &Plan{ClientUpdates: []ClientUpdatePlan{{
		Client: "antigravity", EntryName: "memory", RelayURL: "http://127.0.0.1:9304/mcp",
	}}}
	if err := freezeInstallRelayExePath(plan); err != nil {
		t.Fatalf("first freeze: %v", err)
	}
	if err := freezeInstallRelayExePath(plan); err != nil {
		t.Fatalf("idempotent freeze: %v", err)
	}
	expectations, err := freezeInstallBindingExpectations(plan, "memory", plan.installRelayExePath)
	if err != nil {
		t.Fatalf("freeze expectations: %v", err)
	}
	writerEntry := installClientEntryV1("memory", plan.ClientUpdates[0], plan.installRelayExePath)
	if calls != 1 {
		t.Fatalf("canonical path resolutions = %d, want exactly 1", calls)
	}
	if plan.installRelayExePath != first || writerEntry.RelayExePath != first {
		t.Fatalf("frozen=%q writer=%q, want %q", plan.installRelayExePath, writerEntry.RelayExePath, first)
	}
	if len(expectations) != 1 || expectations[0].Expected.RelayExePath != first {
		t.Fatalf("expectations = %#v, want frozen relay path %q", expectations, first)
	}
}

func TestInstallRelayExePath_ResolutionFailurePrecedesMutation(t *testing.T) {
	originalResolver := canonicalInstallRelayPathFn
	originalSchedulerFactory := schedulerFactoryFn
	t.Cleanup(func() {
		canonicalInstallRelayPathFn = originalResolver
		schedulerFactoryFn = originalSchedulerFactory
	})

	sentinel := errors.New("synthetic canonical path resolution failure")
	canonicalInstallRelayPathFn = func() (string, error) { return "", sentinel }
	schedulerCalls := 0
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		schedulerCalls++
		return nil, errors.New("scheduler must not be reached")
	}
	plan := &Plan{
		Server:         "memory",
		SchedulerTasks: []ScheduledTaskPlan{{Name: "mcp-local-hub-memory-default"}},
		ClientUpdates:  []ClientUpdatePlan{{Client: "antigravity", EntryName: "memory"}},
	}
	var receipt InstallMutationReceiptV1
	err := NewAPI().installFrozenPlanCore(
		context.Background(),
		&config.ServerManifest{Name: "memory"},
		plan,
		InstallOpts{Server: "memory", Writer: io.Discard},
		CanonicalAtAdmission(),
		io.Discard,
		func(got InstallMutationReceiptV1) { receipt = got },
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("install error = %v, want %v", err, sentinel)
	}
	if schedulerCalls != 0 {
		t.Fatalf("scheduler factory calls = %d, want 0 before mutation", schedulerCalls)
	}
	if plan.installRelayExePath != "" || receipt.Committed || len(receipt.ClientConfigSettlements) != 0 {
		t.Fatalf("failure mutated plan/receipt: plan=%#v receipt=%#v", plan, receipt)
	}
}

func TestStatusReadinessSnapshot_UsesOneIPCObservation(t *testing.T) {
	original := statusInternalDialFn
	t.Cleanup(func() { statusInternalDialFn = original })
	task := `\mcp-local-hub-memory-default`
	dials := 0
	statusInternalDialFn = func(context.Context) ([]DaemonStatus, error) {
		dials++
		return []DaemonStatus{{
			TaskName: task, Server: "memory", Daemon: "default", State: "Running", Port: 9304, PID: 4321,
			ReadinessObservation: &DaemonReadinessObservationV1{
				TaskName: task, Server: "memory", Daemon: "default", Port: 9304, PID: 4321,
				ProcessState: "Running", CurrentPIDGeneration: 1, ObservedPIDGeneration: 1,
				IntentPresent: true, IntentRunnable: true, WrapperStarted: true, ListenerReady: true,
				MCPInitializeReady: true, MCPToolsListReady: true, Policy: ReadinessPolicyMCPUpstream,
			},
		}}, nil
	}

	rows, snapshot, err := NewAPI().statusReadinessSnapshot(context.Background())
	if err != nil {
		t.Fatalf("statusReadinessSnapshot: %v", err)
	}
	if dials != 1 {
		t.Fatalf("IPC status dials = %d, want exactly 1", dials)
	}
	if len(rows) != 1 || rows[0].ServiceState != ServiceStateRunning {
		t.Fatalf("rows = %#v, want one running service", rows)
	}
	if len(snapshot.Daemons) != 1 || snapshot.Daemons[0].ServiceState != ServiceStateRunning {
		t.Fatalf("snapshot = %#v, want one running service", snapshot)
	}
}

func committedSettlementForCommandTest() ClientConfigSettlementV1 {
	return ClientConfigSettlementV1{
		SchemaVersion: "client-config-settlement-v1", Operation: "install", Phase: "settled",
		Client: "codex-cli", LogicalSource: "memory", TargetEntry: "memory-mcphub",
		WriteTarget: "codex_global", DesiredTransport: "http", CollisionReason: "cross_layer_opposite_transport",
		Action: "relocate", Outcome: "committed", Readback: "exact",
	}
}

func TestFrozenInstallMutationReceiptCarriesAppliedPlanReadinessFacts(t *testing.T) {
	plan := &Plan{
		Server: "memory", CanMigrate: true,
		SupervisorIntent: []SupervisorIntentEntry{
			{Name: `\\mcp-local-hub-memory-default`, StartupBindDeadlineSeconds: 15},
			{Name: `\\mcp-local-hub-memory-worker`, StartupBindDeadlineSeconds: 45},
		},
	}

	receipt := frozenInstallMutationReceipt(plan, InstallOpts{Server: "memory"})
	if !receipt.CanMigrate {
		t.Fatal("frozen receipt lost the caller-owned migration eligibility")
	}
	if got, want := receipt.StartupBindDeadline, 45*time.Second; got != want {
		t.Fatalf("StartupBindDeadline = %s, want max selected deadline %s", got, want)
	}
}

type commandReceiverFake struct {
	calls      int
	settlement CommandSettlementV1
	err        error
}

type commandMutationFake struct {
	installCalls int
	restartCalls int
	install      InstallMutationReceiptV1
	restart      RestartMutationReceiptV1
	err          error
}

func (f *commandMutationFake) Install(context.Context, InstallOpts) (InstallMutationReceiptV1, error) {
	f.installCalls++
	return f.install, f.err
}

func (f *commandMutationFake) Restart(context.Context, RestartCommandRequestV1) (RestartMutationReceiptV1, error) {
	f.restartCalls++
	return f.restart, f.err
}

func (f *commandReceiverFake) Await(_ context.Context, req ReceivingReadinessRequestV1) (CommandSettlementV1, error) {
	f.calls++
	f.settlement.Operation = req.Operation
	return f.settlement, f.err
}

func TestCommandCoordinator_SettlesThroughInjectedReceiver(t *testing.T) {
	fake := &commandReceiverFake{settlement: CommandSettlementV1{CommitState: "settled"}}
	got, err := (CommandCoordinator{Receiver: fake}).Settle(context.Background(), ReceivingReadinessRequestV1{Operation: CommandOperationRestart, ExpectedTasks: []string{"x"}})
	if err != nil || fake.calls != 1 || got.Operation != CommandOperationRestart {
		t.Fatalf("got=%#v err=%v calls=%d", got, err, fake.calls)
	}
}
func TestCommandCoordinator_ReceiverFailureStaysNonzero(t *testing.T) {
	fake := &commandReceiverFake{err: &ReadinessErrorV1{Stage: ReadinessStageMCPInitialize, FailureID: "mcp_initialize_failed"}}
	_, err := (CommandCoordinator{Receiver: fake}).Settle(context.Background(), ReceivingReadinessRequestV1{Operation: CommandOperationInstall, ExpectedTasks: []string{"x"}})
	var typed *ReadinessErrorV1
	if !errors.As(err, &typed) || typed.Stage != ReadinessStageMCPInitialize {
		t.Fatalf("err=%v", err)
	}
}

func TestCommandCoordinatorInstallReceivingErrorCarriesCommittedSettlementExactly(t *testing.T) {
	want := committedSettlementForCommandTest()
	receivingErr := &ReadinessErrorV1{Stage: ReadinessStageMCPInitialize, FailureID: "mcp_initialize_failed"}
	mutation := &commandMutationFake{install: InstallMutationReceiptV1{
		Committed:               true,
		ExpectedTasks:           []string{`\mcp-local-hub-memory-default`},
		ClientConfigSettlements: []ClientConfigSettlementV1{want},
	}}
	receiver := &commandReceiverFake{settlement: CommandSettlementV1{CommitState: "committed_unverified"}, err: receivingErr}
	settlement, err := (CommandCoordinator{Mutation: mutation, Receiver: receiver}).Install(context.Background(), InstallOpts{Server: "memory"})
	var typed *ReadinessErrorV1
	if !errors.As(err, &typed) || typed != receivingErr || typed.Stage != ReadinessStageMCPInitialize {
		t.Fatalf("Install error = %v, want unchanged typed receiving error", err)
	}
	if settlement.CommitState != "committed_unverified" || settlement.Operation != CommandOperationInstall {
		t.Fatalf("settlement polarity = %#v", settlement)
	}
	if !reflect.DeepEqual(settlement.ClientConfigSettlements, []ClientConfigSettlementV1{want}) {
		t.Fatalf("carried rows = %#v, want exact %#v", settlement.ClientConfigSettlements, []ClientConfigSettlementV1{want})
	}
	if mutation.installCalls != 1 || receiver.calls != 1 {
		t.Fatalf("mutation/receiver calls = %d/%d, want one each without retry", mutation.installCalls, receiver.calls)
	}
}

func TestCommandCoordinator_InstallMutatesOnceThenSettlesFrozenReceipt(t *testing.T) {
	mutation := &commandMutationFake{install: InstallMutationReceiptV1{
		Committed:           true,
		ExpectedTasks:       []string{"\\mcp-local-hub-memory-default"},
		bindingExpectations: []bindingExpectationV1{{Client: "codex-cli", Expected: clients.MCPEntry{Name: "memory", URL: "http://127.0.0.1:9123/mcp"}}},
		RequireBindings:     true,
	}}
	receiver := &commandReceiverFake{settlement: CommandSettlementV1{CommitState: "settled"}}
	coordinator := CommandCoordinator{Mutation: mutation, Receiver: receiver}

	settlement, err := coordinator.Install(context.Background(), InstallOpts{Server: "memory"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if mutation.installCalls != 1 || receiver.calls != 1 {
		t.Fatalf("mutation/receiver calls = %d/%d, want 1/1", mutation.installCalls, receiver.calls)
	}
	if settlement.CommitState != "settled" || settlement.Operation != CommandOperationInstall {
		t.Fatalf("settlement = %#v", settlement)
	}
}

func TestCommandCoordinator_DryRunDoesNotDialReceiver(t *testing.T) {
	mutation := &commandMutationFake{install: InstallMutationReceiptV1{DryRun: true}}
	receiver := &commandReceiverFake{settlement: CommandSettlementV1{CommitState: "settled"}}
	settlement, err := (CommandCoordinator{Mutation: mutation, Receiver: receiver}).Install(context.Background(), InstallOpts{Server: "memory", DryRun: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if mutation.installCalls != 1 || receiver.calls != 0 {
		t.Fatalf("mutation/receiver calls = %d/%d, want 1/0", mutation.installCalls, receiver.calls)
	}
	if settlement.CommitState != "dry_run" {
		t.Fatalf("commit state = %q, want dry_run", settlement.CommitState)
	}
}

func TestCommandCoordinatorInstallCarriesSettlementOnMutationError(t *testing.T) {
	want := committedSettlementForCommandTest()
	sentinel := errors.New("injected mutation error")
	mutation := &commandMutationFake{install: InstallMutationReceiptV1{ClientConfigSettlements: []ClientConfigSettlementV1{want}}, err: sentinel}
	settlement, err := (CommandCoordinator{Mutation: mutation}).Install(context.Background(), InstallOpts{Server: "memory"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want preserved mutation error", err)
	}
	if settlement.CommitState != "not_committed" {
		t.Fatalf("commit state = %q, want not_committed", settlement.CommitState)
	}
	if !reflect.DeepEqual(settlement.ClientConfigSettlements, []ClientConfigSettlementV1{want}) {
		t.Fatalf("carried rows = %#v, want %#v", settlement.ClientConfigSettlements, []ClientConfigSettlementV1{want})
	}
}

func TestCommandCoordinatorInstallCarriesSettlementOnMissingTaskReceiptError(t *testing.T) {
	want := committedSettlementForCommandTest()
	mutation := &commandMutationFake{install: InstallMutationReceiptV1{Committed: true, ClientConfigSettlements: []ClientConfigSettlementV1{want}}}
	settlement, err := (CommandCoordinator{Mutation: mutation}).Install(context.Background(), InstallOpts{Server: "memory"})
	if err == nil || err.Error() != "install committed without a frozen task receipt" {
		t.Fatalf("error = %v, want unchanged missing-task receipt error", err)
	}
	if settlement.CommitState != "committed_unverified" {
		t.Fatalf("commit state = %q, want committed_unverified", settlement.CommitState)
	}
	if !reflect.DeepEqual(settlement.ClientConfigSettlements, []ClientConfigSettlementV1{want}) {
		t.Fatalf("carried rows = %#v, want %#v", settlement.ClientConfigSettlements, []ClientConfigSettlementV1{want})
	}
}

func TestCommandCoordinatorInstallBatchCarriesSettlementOnPerServerError(t *testing.T) {
	want := committedSettlementForCommandTest()
	sentinel := errors.New("injected bulk mutation error")
	mutation := &commandMutationFake{install: InstallMutationReceiptV1{ClientConfigSettlements: []ClientConfigSettlementV1{want}}, err: sentinel}
	results := (CommandCoordinator{Mutation: mutation}).InstallBatch(context.Background(), []string{"memory"}, InstallAllOpts{})
	if len(results) != 1 || !errors.Is(results[0].Err, sentinel) {
		t.Fatalf("results = %#v, want one preserved per-server error", results)
	}
	if !reflect.DeepEqual(results[0].ClientConfigSettlements, []ClientConfigSettlementV1{want}) {
		t.Fatalf("bulk carried rows = %#v, want %#v", results[0].ClientConfigSettlements, []ClientConfigSettlementV1{want})
	}
}

func TestCommandCoordinatorDryRunDoesNotCarryClientConfigSettlement(t *testing.T) {
	mutation := &commandMutationFake{install: InstallMutationReceiptV1{DryRun: true, ClientConfigSettlements: []ClientConfigSettlementV1{committedSettlementForCommandTest()}}}
	settlement, err := (CommandCoordinator{Mutation: mutation}).Install(context.Background(), InstallOpts{Server: "memory", DryRun: true})
	if err != nil || settlement.CommitState != "dry_run" || len(settlement.ClientConfigSettlements) != 0 {
		t.Fatalf("dry-run settlement = %#v, err = %v", settlement, err)
	}
}

func TestCommandCoordinator_InstallBatchMutatesAndSettlesEveryServer(t *testing.T) {
	mutation := &commandMutationFake{install: InstallMutationReceiptV1{
		Committed:     true,
		ExpectedTasks: []string{"\\mcp-local-hub-memory-default"},
	}}
	receiver := &commandReceiverFake{settlement: CommandSettlementV1{CommitState: "settled"}}
	results := (CommandCoordinator{Mutation: mutation, Receiver: receiver}).InstallBatch(context.Background(), []string{"memory", "time"}, InstallAllOpts{})
	if len(results) != 2 || results[0].Err != nil || results[1].Err != nil {
		t.Fatalf("batch results = %#v", results)
	}
	if mutation.installCalls != 2 || receiver.calls != 2 {
		t.Fatalf("mutation/receiver calls = %d/%d, want 2/2", mutation.installCalls, receiver.calls)
	}
}

func TestCommandCoordinator_PreservesCommittedMutationWhenBindingReadbackFails(t *testing.T) {
	mutation := &commandMutationFake{
		install: InstallMutationReceiptV1{Committed: true, ExpectedTasks: []string{"\\mcp-local-hub-memory-default"}},
		err:     &ReadinessErrorV1{Stage: ReadinessStageClientBinding, FailureID: "client_binding_missing"},
	}
	settlement, err := (CommandCoordinator{Mutation: mutation, Receiver: &commandReceiverFake{}}).Install(context.Background(), InstallOpts{Server: "memory"})
	var typed *ReadinessErrorV1
	if !errors.As(err, &typed) || typed.Stage != ReadinessStageClientBinding {
		t.Fatalf("error = %v, want typed client-binding failure", err)
	}
	if settlement.CommitState != "committed_unverified" {
		t.Fatalf("commit state = %q, want committed_unverified after an applied mutation", settlement.CommitState)
	}
}

func TestCommandSettlement_PreservesMutationRowsAndAllFailures(t *testing.T) {
	first := ReadinessFailureV1{TaskName: `\mcp-local-hub-memory-default`, Stage: ReadinessStageMCPInitialize, FailureID: "mcp_initialize_failed"}
	second := ReadinessFailureV1{TaskName: `\mcp-local-hub-time-default`, Stage: ReadinessStageMCPToolsList, FailureID: "mcp_tools_list_failed"}
	settlement := CommandSettlementV1{
		SchemaVersion: "command-settlement-v1",
		Operation:     CommandOperationRestart,
		CommitState:   "committed_unverified",
		MutationRows: []CommandMutationRowV1{
			{TaskName: first.TaskName, FailureID: "restart_dispatch_failed"},
			{TaskName: second.TaskName},
		},
		Snapshot:       ReadinessSnapshotV1{Failures: []ReadinessFailureV1{first, second}},
		PrimaryFailure: &first,
		Failures:       []ReadinessFailureV1{first, second},
		ClientConfigSettlements: []ClientConfigSettlementV1{{
			SchemaVersion: "client-config-settlement-v1", Operation: "install", Phase: "settled",
			Client: "codex-cli", LogicalSource: "memory", TargetEntry: "memory-mcphub",
			WriteTarget: "codex_global", DesiredTransport: "http", Action: "relocate",
			Outcome: "committed", Readback: "exact",
		}},
	}

	if len(settlement.MutationRows) != 2 || settlement.MutationRows[0].FailureID != "restart_dispatch_failed" {
		t.Fatalf("mutation rows = %#v", settlement.MutationRows)
	}
	if len(settlement.Failures) != 2 || settlement.Failures[0] != first || settlement.Failures[1] != second {
		t.Fatalf("failures = %#v", settlement.Failures)
	}
	if len(settlement.ClientConfigSettlements) != 1 || settlement.ClientConfigSettlements[0].Readback != "exact" {
		t.Fatalf("client config settlements = %#v", settlement.ClientConfigSettlements)
	}
}
