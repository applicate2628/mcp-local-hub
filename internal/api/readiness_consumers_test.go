package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
)

// This is the receiving-side regression guard for the B/C/D wire: callers
// supply supervisor-owned observations, but API.AssessReadiness remains the
// only reducer that can turn them into service state or a scan classification.
func TestAssessReadinessFromStatusRows_UsesOnlyAPIReducer(t *testing.T) {
	rows := []DaemonStatus{{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
		Port:     9304,
		State:    "Running", // legacy process fact; not a service-success shortcut.
		ReadinessObservation: &DaemonReadinessObservationV1{
			TaskName:              `\mcp-local-hub-memory-default`,
			Server:                "memory",
			Daemon:                "default",
			Port:                  9304,
			PID:                   4321,
			ProcessState:          "Running",
			CurrentPIDGeneration:  7,
			ObservedPIDGeneration: 7,
			IntentPresent:         true,
			IntentRunnable:        true,
			WrapperStarted:        true,
			ListenerReady:         true,
			MCPInitializeReady:    true,
			MCPToolsListReady:     true,
			Policy:                ReadinessPolicyMCPUpstream,
		},
	}}
	bindings := []BindingObservationV1{{
		Client: "codex-cli", Present: true, Readable: true, Enabled: true,
		Route: BindingRouteHub, ExactHubRoute: true, Requested: true,
	}}

	snapshot, err := NewAPI().AssessReadinessFromStatusRows(context.Background(), rows, bindings, true, time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AssessReadinessFromStatusRows: %v", err)
	}
	if !snapshot.Settled || snapshot.Classification != ReadinessClassificationInstalledAndBound {
		t.Fatalf("snapshot = %#v, want settled installed-and-bound", snapshot)
	}
	if rows[0].State != "Running" {
		t.Fatalf("legacy process state mutated to %q", rows[0].State)
	}
	if rows[0].ServiceState != ServiceStateRunning || rows[0].ReadinessStage != ReadinessStageComplete || !rows[0].ReadinessSettled {
		t.Fatalf("row did not receive API-reduced readiness: %#v", rows[0])
	}
}

func TestAssessReadinessFromStatusRows_FailsClosedForProcessOnlyRunningRow(t *testing.T) {
	rows := []DaemonStatus{{
		TaskName: `\mcp-local-hub-memory-default`, State: "Running",
		ReadinessObservation: &DaemonReadinessObservationV1{
			TaskName: `\mcp-local-hub-memory-default`, ProcessState: "Running",
			CurrentPIDGeneration: 3, ObservedPIDGeneration: 3,
			IntentPresent: true, IntentRunnable: true, WrapperStarted: true,
			ListenerReady: true, Policy: ReadinessPolicyMCPUpstream,
		},
	}}

	snapshot, err := NewAPI().AssessReadinessFromStatusRows(context.Background(), rows, nil, false, time.Time{})
	if err != nil {
		t.Fatalf("unsettled observation is not a terminal error: %v", err)
	}
	if snapshot.Settled || rows[0].ServiceState == ServiceStateRunning {
		t.Fatalf("PID/listener-only row promoted to service running: snapshot=%#v row=%#v", snapshot, rows[0])
	}
}

func TestAssessReadinessFromStatusRowsWithOptionsCarriesCallerMigrationEligibility(t *testing.T) {
	bindings := []BindingObservationV1{{Client: "codex-cli", Requested: true, Present: true, Readable: true, Enabled: true, Route: BindingRouteDirect}}
	options := ReadinessStatusRowsOptionsV1{
		CanMigrate: true, Now: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
	}

	snapshot, err := NewAPI().AssessReadinessFromStatusRowsWithOptions(context.Background(), nil, bindings, options)
	if err != nil {
		t.Fatalf("AssessReadiness: %v", err)
	}
	if got, want := snapshot.Classification, ReadinessClassificationConfiguredDirectCanMigrate; got != want {
		t.Fatalf("classification = %q, want %q", got, want)
	}
}

type receivingReadinessReadResultFake struct {
	rows     []DaemonStatus
	bindings []BindingObservationV1
	err      error
}

// bindingReaderFake embeds the production client interface while exposing only
// the existing GetEntry owner used by the canonical receiving reader.
type bindingReaderFake struct {
	clients.Client
	entry *clients.MCPEntry
	err   error
	relay bool
}

func (f bindingReaderFake) GetEntry(string) (*clients.MCPEntry, error) { return f.entry, f.err }
func (f bindingReaderFake) IsRelayStdio() bool                         { return f.relay }

type receivingReadinessReaderFake struct {
	results      []receivingReadinessReadResultFake
	calls        int
	expectations [][]bindingExpectationV1
}

func (f *receivingReadinessReaderFake) Read(_ context.Context, expectations []bindingExpectationV1) ([]DaemonStatus, []BindingObservationV1, error) {
	f.expectations = append(f.expectations, append([]bindingExpectationV1(nil), expectations...))
	result := f.results[f.calls]
	f.calls++
	return result.rows, result.bindings, result.err
}

type receivingReadinessClockFake struct{ now time.Time }

func (f receivingReadinessClockFake) NowUTC() time.Time { return f.now }

type receivingReadinessWaiterFake struct {
	calls int
	wait  func(context.Context) error
}

func (f *receivingReadinessWaiterFake) Wait(ctx context.Context, _ time.Duration) error {
	f.calls++
	if f.wait == nil {
		return nil
	}
	return f.wait(ctx)
}

type receivingReadinessDeadlineFake struct {
	begin func(context.Context) (context.Context, context.CancelFunc)
}

func (f receivingReadinessDeadlineFake) Begin(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return f.begin(ctx)
}

func readyReceivingRow(task string) DaemonStatus {
	observation := readyExternalObservation(task)
	return DaemonStatus{TaskName: task, ReadinessObservation: &observation}
}

func TestNewReceivingReadinessPort_RequiresReaderClockWaiterDeadline(t *testing.T) {
	reader := &receivingReadinessReaderFake{}
	clock := receivingReadinessClockFake{}
	waiter := &receivingReadinessWaiterFake{}
	deadline := receivingReadinessDeadlineFake{begin: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }}
	for _, test := range []struct {
		name string
		deps ReceivingReadinessDepsV1
	}{
		{"reader", ReceivingReadinessDepsV1{Clock: clock, Waiter: waiter, Deadline: deadline}},
		{"clock", ReceivingReadinessDepsV1{Reader: reader, Waiter: waiter, Deadline: deadline}},
		{"waiter", ReceivingReadinessDepsV1{Reader: reader, Clock: clock, Deadline: deadline}},
		{"deadline", ReceivingReadinessDepsV1{Reader: reader, Clock: clock, Waiter: waiter}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewReceivingReadinessPort(test.deps); err == nil {
				t.Fatal("constructor accepted a missing receiving dependency")
			}
			if reader.calls != 0 || waiter.calls != 0 {
				t.Fatalf("missing-dependency constructor read/waited: reads=%d waits=%d", reader.calls, waiter.calls)
			}
		})
	}
}

func TestReceivingReadiness_RereadsBindingsAgainstFrozenPlanIdentity(t *testing.T) {
	const task = `\mcp-local-hub-memory-default`
	expectation := bindingExpectationV1{
		Client:   "codex-cli",
		Expected: clients.MCPEntry{Name: "memory", URL: "http://127.0.0.1:9123/mcp"},
	}
	reader := &receivingReadinessReaderFake{results: []receivingReadinessReadResultFake{
		{rows: []DaemonStatus{readyReceivingRow(task)}, bindings: []BindingObservationV1{{Client: "codex-cli", Requested: true}}},
		{rows: []DaemonStatus{readyReceivingRow(task)}, bindings: []BindingObservationV1{{Client: "codex-cli", Requested: true, Present: true, Readable: true, Enabled: true, Route: BindingRouteHub, ExactHubRoute: true}}},
	}}
	waiter := &receivingReadinessWaiterFake{}
	port, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{
		Reader:   reader,
		Clock:    receivingReadinessClockFake{now: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)},
		Waiter:   waiter,
		Deadline: receivingReadinessDeadlineFake{begin: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }},
	})
	if err != nil {
		t.Fatalf("NewReceivingReadinessPort: %v", err)
	}
	settlement, err := port.Await(context.Background(), ReceivingReadinessRequestV1{
		Operation: CommandOperationInstall, ExpectedTasks: []string{task}, BindingExpectations: []bindingExpectationV1{expectation}, RequireBindings: true,
	})
	if err != nil || !settlement.Snapshot.Settled || reader.calls != 2 || waiter.calls != 1 {
		t.Fatalf("settlement=%#v err=%v reads=%d waits=%d", settlement, err, reader.calls, waiter.calls)
	}
	for _, frozen := range reader.expectations {
		if len(frozen) != 1 || frozen[0].Expected.URL != expectation.Expected.URL {
			t.Fatalf("receiver accepted config drift instead of frozen plan identity: %#v", frozen)
		}
	}
}

func TestReceivingReadiness_PreservesCanMigrateIntoSoleReducer(t *testing.T) {
	reader := &receivingReadinessReaderFake{results: []receivingReadinessReadResultFake{{
		bindings: []BindingObservationV1{{Client: "codex-cli", Requested: true, Present: true, Readable: true, Enabled: true, Route: BindingRouteDirect}},
	}}}
	port, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{
		Reader: reader, Clock: receivingReadinessClockFake{}, Waiter: &receivingReadinessWaiterFake{},
		Deadline: receivingReadinessDeadlineFake{begin: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }},
	})
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := port.Await(context.Background(), ReceivingReadinessRequestV1{Operation: CommandOperationInstall, CanMigrate: true})
	if err != nil || settlement.Snapshot.Classification != ReadinessClassificationConfiguredDirectCanMigrate {
		t.Fatalf("settlement=%#v err=%v", settlement, err)
	}
}

func TestReceivingReadiness_OnlyProvenDACLGetsEligibleExactCLIAdvice(t *testing.T) {
	deadline := receivingReadinessDeadlineFake{begin: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }}
	for _, test := range []struct {
		name      string
		err       error
		failureID string
		advice    bool
	}{
		{"eligible-dacl", &ReceivingStateReadErrorV1{Cause: ErrDaclOutsideAllowlist, RepairPath: `C:\state\supervisor.lock.owner.json`, RepairEligible: true}, "intent_dacl_refused", true},
		{"generic-setup", fmt.Errorf("corrupt owner: %w", ErrStatusSetupFailure), "intent_unreadable", false},
		{"wrong-owner", ErrWrongOwner, "intent_wrong_owner", false},
		{"irregular", ErrIrregularFile, "intent_irregular_file", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			port, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{
				Reader: &receivingReadinessReaderFake{results: []receivingReadinessReadResultFake{{err: test.err}}}, Clock: receivingReadinessClockFake{}, Waiter: &receivingReadinessWaiterFake{}, Deadline: deadline,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = port.Await(context.Background(), ReceivingReadinessRequestV1{Operation: CommandOperationInstall})
			var readinessErr *ReadinessErrorV1
			if !errors.As(err, &readinessErr) || readinessErr.FailureID != test.failureID {
				t.Fatalf("error=%v, want %s", err, test.failureID)
			}
			if (readinessErr.Remediation != "") != test.advice {
				t.Fatalf("remediation=%q, advice=%v", readinessErr.Remediation, test.advice)
			}
		})
	}
}

func TestReceivingStateReadErrorFromOwnerV1_UsesTypedOwnerEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		cause    error
		eligible bool
	}{
		{name: "dacl", cause: ErrDaclOutsideAllowlist, eligible: true},
		{name: "irregular", cause: ErrIrregularFile},
		{name: "generic-setup", cause: ErrStatusSetupFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := receivingStateReadErrorFromOwnerV1(`C:\state\supervisor.lock.owner.json`, test.cause)
			var typed *ReceivingStateReadErrorV1
			if !errors.As(err, &typed) || typed.Cause != test.cause || typed.RepairPath != `C:\state\supervisor.lock.owner.json` || typed.RepairEligible != test.eligible {
				t.Fatalf("typed owner evidence = %#v", typed)
			}
		})
	}
}

func TestAPIReceivingStateReaderV1_PreservesTypedStateOwnerFailure(t *testing.T) {
	ownerErr := receivingStateReadErrorFromOwnerV1(`C:\state\supervisor.lock.owner.json`, ErrDaclOutsideAllowlist)
	reader := apiReceivingStateReaderV1{readStatus: func(context.Context) ([]DaemonStatus, error) {
		return nil, ownerErr
	}}
	_, _, err := reader.Read(context.Background(), nil)
	var typed *ReceivingStateReadErrorV1
	if !errors.As(err, &typed) || typed.RepairPath != `C:\state\supervisor.lock.owner.json` || !typed.RepairEligible {
		t.Fatalf("production reader lost owner evidence: %v", err)
	}
}

func TestReadBindingExpectationV1RejectsUnreadableMissingAndMismatchedState(t *testing.T) {
	expectation := bindingExpectationV1{Client: "codex-cli", Expected: clients.MCPEntry{Name: "memory", URL: "http://127.0.0.1:9123/mcp"}}
	for _, test := range []struct {
		name     string
		client   bindingReaderFake
		accepted bool
	}{
		{name: "unreadable", client: bindingReaderFake{err: errors.New("synthetic read failure")}},
		{name: "missing", client: bindingReaderFake{}},
		{name: "mismatched", client: bindingReaderFake{entry: &clients.MCPEntry{Name: "memory", URL: "http://127.0.0.1:9999/mcp"}}},
		{name: "exact", client: bindingReaderFake{entry: &expectation.Expected}, accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding, err := readBindingExpectationV1(expectation, test.client)
			if test.name == "unreadable" {
				if err == nil {
					t.Fatal("canonical reader accepted an unreadable client binding")
				}
				return
			}
			if err != nil || (binding.Present && binding.Readable && binding.Enabled && binding.ExactHubRoute) != test.accepted {
				t.Fatalf("binding=%#v err=%v accepted=%v", binding, err, test.accepted)
			}
		})
	}
}

func TestReceivingReadiness_TimeoutPreservesPersistentBindingMismatch(t *testing.T) {
	const task = `\mcp-local-hub-memory-default`
	reader := &receivingReadinessReaderFake{results: []receivingReadinessReadResultFake{{
		rows:     []DaemonStatus{readyReceivingRow(task)},
		bindings: []BindingObservationV1{{Client: "codex-cli", Requested: true, Present: true, Readable: true, Enabled: true, Route: BindingRouteDirect}},
	}}}
	port, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{
		Reader: reader, Clock: receivingReadinessClockFake{},
		Waiter:   &receivingReadinessWaiterFake{wait: func(context.Context) error { return context.DeadlineExceeded }},
		Deadline: receivingReadinessDeadlineFake{begin: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = port.Await(context.Background(), ReceivingReadinessRequestV1{Operation: CommandOperationInstall, ExpectedTasks: []string{task}, RequireBindings: true})
	var readinessErr *ReadinessErrorV1
	if !errors.As(err, &readinessErr) || readinessErr.Stage != ReadinessStageClientBinding || readinessErr.FailureID != "client_binding_mismatch" {
		t.Fatalf("timeout masked the last binding mismatch: %v", err)
	}
}

func TestReceivingReadiness_TimeoutPreservesEveryIncompleteCanonicalStage(t *testing.T) {
	rows := []DaemonStatus{
		{TaskName: "intent", ReadinessObservation: &DaemonReadinessObservationV1{TaskName: "intent", IntentPresent: true}},
		{TaskName: "wrapper", ReadinessObservation: &DaemonReadinessObservationV1{TaskName: "wrapper", IntentPresent: true, IntentRunnable: true}},
		{TaskName: "listener", ReadinessObservation: &DaemonReadinessObservationV1{TaskName: "listener", IntentPresent: true, IntentRunnable: true, CurrentPIDGeneration: 1, ObservedPIDGeneration: 1, WrapperStarted: true, Policy: ReadinessPolicyMCPUpstream}},
		{TaskName: "initialize", ReadinessObservation: &DaemonReadinessObservationV1{TaskName: "initialize", IntentPresent: true, IntentRunnable: true, CurrentPIDGeneration: 1, ObservedPIDGeneration: 1, WrapperStarted: true, ListenerReady: true, Policy: ReadinessPolicyMCPUpstream}},
		{TaskName: "tools", ReadinessObservation: &DaemonReadinessObservationV1{TaskName: "tools", IntentPresent: true, IntentRunnable: true, CurrentPIDGeneration: 1, ObservedPIDGeneration: 1, WrapperStarted: true, ListenerReady: true, MCPInitializeReady: true, Policy: ReadinessPolicyMCPUpstream}},
	}
	port, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{
		Reader:   &receivingReadinessReaderFake{results: []receivingReadinessReadResultFake{{rows: rows}}},
		Clock:    receivingReadinessClockFake{},
		Waiter:   &receivingReadinessWaiterFake{wait: func(context.Context) error { return context.DeadlineExceeded }},
		Deadline: receivingReadinessDeadlineFake{begin: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }},
	})
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := port.Await(context.Background(), ReceivingReadinessRequestV1{Operation: CommandOperationInstall, ExpectedTasks: []string{"intent", "wrapper", "listener", "initialize", "tools"}})
	if err == nil {
		t.Fatal("Await unexpectedly settled")
	}
	got := make(map[ReadinessStageV1]string)
	for _, failure := range settlement.Failures {
		got[failure.Stage] = failure.FailureID
	}
	for stage, want := range map[ReadinessStageV1]string{
		ReadinessStageTaskIntent: "task_intent_missing", ReadinessStageWrapperStart: "wrapper_start_timeout",
		ReadinessStageUpstreamListener: "upstream_listener_unbound", ReadinessStageMCPInitialize: "mcp_initialize_failed",
		ReadinessStageMCPToolsList: "mcp_tools_list_failed",
	} {
		if got[stage] != want {
			t.Errorf("timeout failure for %s = %q, want %q; all=%#v", stage, got[stage], want, settlement.Failures)
		}
	}
}

func TestReceivingReadiness_DeadlineCancellationReleasesWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := &receivingReadinessWaiterFake{wait: func(context.Context) error { cancel(); return context.Canceled }}
	stalled := readyExternalObservation("d")
	stalled.ListenerReady = false
	port, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{
		Reader: &receivingReadinessReaderFake{results: []receivingReadinessReadResultFake{{rows: []DaemonStatus{{TaskName: "d", ReadinessObservation: &stalled}}}}},
		Clock:  receivingReadinessClockFake{}, Waiter: waiter,
		Deadline: receivingReadinessDeadlineFake{begin: func(context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = port.Await(context.Background(), ReceivingReadinessRequestV1{Operation: CommandOperationRestart, ExpectedTasks: []string{"d"}})
	var readinessErr *ReadinessErrorV1
	if !errors.As(err, &readinessErr) || readinessErr.FailureID != "readiness_cancelled" || waiter.calls != 1 {
		t.Fatalf("error=%v waits=%d", err, waiter.calls)
	}
}

func TestReceivingReadiness_DeadlineTimesOutWithoutReadOrWait(t *testing.T) {
	deadline := receivingReadinessDeadlineFake{begin: func(context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		return ctx, cancel
	}}
	reader := &receivingReadinessReaderFake{}
	waiter := &receivingReadinessWaiterFake{}
	port, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{Reader: reader, Clock: receivingReadinessClockFake{}, Waiter: waiter, Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	_, err = port.Await(context.Background(), ReceivingReadinessRequestV1{Operation: CommandOperationRestart})
	var readinessErr *ReadinessErrorV1
	if !errors.As(err, &readinessErr) || readinessErr.FailureID != "readiness_timeout" || reader.calls != 0 || waiter.calls != 0 {
		t.Fatalf("error=%v reads=%d waits=%d", err, reader.calls, waiter.calls)
	}
}
