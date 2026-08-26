package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"mcp-local-hub/internal/clients"
)

// AssessReadinessFromStatusRows is the one composition adapter for status,
// scan, install, restart, IPC, CLI, and GUI consumers. It only translates
// already-owned supervisor and client observations; ReduceReadinessV1 remains
// the sole state/classification reducer.
func (a *API) AssessReadinessFromStatusRows(ctx context.Context, rows []DaemonStatus, bindings []BindingObservationV1, requireBinding bool, now time.Time) (ReadinessSnapshotV1, error) {
	return a.AssessReadinessFromStatusRowsWithOptions(ctx, rows, bindings, ReadinessStatusRowsOptionsV1{
		RequireBinding: requireBinding,
		Now:            now,
	})
}

// ReadinessStatusRowsOptionsV1 carries caller-owned applicability facts into
// the status-row adapter without giving the reducer a second classifier.
type ReadinessStatusRowsOptionsV1 struct {
	Selectors      []string
	Mode           ReadinessModeV1
	RequireBinding bool
	CanMigrate     bool
	Now            time.Time
}

// AssessReadinessFromStatusRowsWithOptions is the canonical translation from
// externally observed rows and bindings to the sole readiness reducer.
func (a *API) AssessReadinessFromStatusRowsWithOptions(ctx context.Context, rows []DaemonStatus, bindings []BindingObservationV1, options ReadinessStatusRowsOptionsV1) (ReadinessSnapshotV1, error) {
	request := readinessRequestFromStatusRowsV1(rows, bindings, options)
	snapshot, err := a.AssessReadiness(ctx, request)
	applyReadinessSnapshotToDaemonStatuses(rows, snapshot)
	return snapshot, err
}

// readinessRequestFromStatusRowsV1 is the single translation from supervisor
// and client observations to reducer input. Scan and receiving settlement both
// supply caller-owned facts through ReadinessStatusRowsOptionsV1.
func readinessRequestFromStatusRowsV1(rows []DaemonStatus, bindings []BindingObservationV1, options ReadinessStatusRowsOptionsV1) ReadinessRequest {
	mode := options.Mode
	if mode == "" {
		mode = ReadinessModeSnapshot
	}
	request := ReadinessRequest{
		Selectors: append([]string(nil), options.Selectors...), Mode: mode,
		Bindings: bindings, RequireBinding: options.RequireBinding,
		CanMigrate: options.CanMigrate, Now: options.Now,
	}
	for i := range rows {
		request.Observations = append(request.Observations, readinessObservationFromStatusRow(rows[i]))
	}
	return request
}

func readinessObservationFromStatusRow(row DaemonStatus) DaemonReadinessObservationV1 {
	if row.ReadinessObservation != nil {
		out := *row.ReadinessObservation
		out.Failures = append([]ReadinessFailureV1(nil), row.ReadinessObservation.Failures...)
		if out.TaskName == "" {
			out.TaskName = row.TaskName
		}
		if out.Server == "" {
			out.Server = row.Server
		}
		if out.Daemon == "" {
			out.Daemon = row.Daemon
		}
		if out.Port == 0 {
			out.Port = row.Port
		}
		if out.PID == 0 {
			out.PID = row.PID
		}
		if out.ProcessState == "" {
			out.ProcessState = row.State
		}
		return out
	}
	// An old supervisor row remains observable as a process fact, but missing
	// readiness evidence cannot promote it to service Running.
	return DaemonReadinessObservationV1{
		TaskName: row.TaskName, Server: row.Server, Daemon: row.Daemon, Port: row.Port,
		PID: row.PID, ProcessState: row.State, IntentPresent: true, IntentRunnable: true,
		WrapperStarted: row.PID > 0,
		Policy:         ReadinessPolicyMCPUpstream,
	}
}

func applyReadinessSnapshotToDaemonStatuses(rows []DaemonStatus, snapshot ReadinessSnapshotV1) {
	byTask := make(map[string]DaemonReadinessV1, len(snapshot.Daemons))
	for _, daemon := range snapshot.Daemons {
		byTask[daemon.TaskName] = daemon
	}
	for i := range rows {
		daemon, ok := byTask[rows[i].TaskName]
		if !ok {
			continue
		}
		rows[i].ServiceState = daemon.ServiceState
		rows[i].ReadinessStage = daemon.Stage
		rows[i].ReadinessSettled = daemon.Settled
		rows[i].ReadinessFailure = daemon.Failure
	}
}

func statusReadinessError(err error) error {
	if err == nil {
		return nil
	}
	var readinessErr *ReadinessErrorV1
	if errors.As(err, &readinessErr) {
		return nil // Status returns the structured primary failure in its rows.
	}
	return err
}

// ReceivingStateReaderV1 owns all receiving-side reads. Binding expectations
// are frozen before mutation and are never derived from a later client config.
type ReceivingStateReaderV1 interface {
	Read(context.Context, []bindingExpectationV1) ([]DaemonStatus, []BindingObservationV1, error)
}

type UTCClockV1 interface{ NowUTC() time.Time }

type CancellableWaiterV1 interface {
	Wait(context.Context, time.Duration) error
}

type SettlementDeadlineV1 interface {
	Begin(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

// ReceivingReadinessDepsV1 is deliberately complete: no package-global dial,
// wall clock, timer, or deadline is reachable from the receiving port.
type ReceivingReadinessDepsV1 struct {
	Reader   ReceivingStateReaderV1
	Clock    UTCClockV1
	Waiter   CancellableWaiterV1
	Deadline SettlementDeadlineV1
}

// ReceivingStateReadErrorV1 permits a reader that has already proved a
// repairable state-file cause and target to carry that fact without exposing a
// path in a readiness snapshot or settlement wire projection.
type ReceivingStateReadErrorV1 struct {
	Cause          error
	RepairPath     string
	RepairEligible bool
}

func (e *ReceivingStateReadErrorV1) Error() string {
	if e == nil || e.Cause == nil {
		return "receiving readiness read failed"
	}
	return e.Cause.Error()
}

func (e *ReceivingStateReadErrorV1) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type receivingReadinessPortV1 struct{ deps ReceivingReadinessDepsV1 }

func NewReceivingReadinessPort(deps ReceivingReadinessDepsV1) (ReceivingReadinessPort, error) {
	if deps.Reader == nil || deps.Clock == nil || deps.Waiter == nil || deps.Deadline == nil {
		return nil, fmt.Errorf("receiving readiness requires reader, clock, waiter, and deadline")
	}
	return receivingReadinessPortV1{deps: deps}, nil
}

func (p receivingReadinessPortV1) Await(parent context.Context, request ReceivingReadinessRequestV1) (CommandSettlementV1, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := p.deps.Deadline.Begin(parent, request.StartupBindDeadline)
	if ctx == nil || cancel == nil {
		return receivingReadinessFailureSettlement(request, ReadinessSnapshotV1{}, &ReadinessErrorV1{Stage: ReadinessStageSupervisorBootstrap, FailureID: "supervisor_bootstrap_failed"})
	}
	defer cancel()

	var last ReadinessSnapshotV1
	for {
		if err := ctx.Err(); err != nil {
			return receivingReadinessContextSettlement(request, last, err)
		}
		rows, bindings, err := p.deps.Reader.Read(ctx, request.BindingExpectations)
		if err != nil {
			return receivingReadinessFailureSettlement(request, last, receivingReadinessReadFailure(err))
		}
		readinessRequest := readinessRequestFromStatusRowsV1(rows, bindings, ReadinessStatusRowsOptionsV1{
			Selectors: request.ExpectedTasks, Mode: ReadinessModeAwaitSettled,
			RequireBinding: request.RequireBindings, CanMigrate: request.CanMigrate, Now: p.deps.Clock.NowUTC(),
		})
		last, err = NewAPI().AssessReadiness(ctx, readinessRequest)
		applyReadinessSnapshotToDaemonStatuses(rows, last)
		if err != nil {
			return receivingReadinessFailureSettlement(request, last, err)
		}
		if last.Settled {
			return commandSettlementV1(request.Operation, "settled", nil, last), nil
		}
		if err := p.deps.Waiter.Wait(ctx, 100*time.Millisecond); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				return receivingReadinessContextSettlement(request, last, err)
			}
			return receivingReadinessFailureSettlement(request, last, &ReadinessErrorV1{Stage: ReadinessStageSupervisorBootstrap, FailureID: "supervisor_bootstrap_failed"})
		}
	}
}

func receivingReadinessReadFailure(err error) *ReadinessErrorV1 {
	var typed *ReceivingStateReadErrorV1
	if errors.As(err, &typed) && typed != nil {
		if errors.Is(typed.Cause, ErrDaclOutsideAllowlist) || errors.Is(typed.Cause, ErrTooLoose) {
			failure := &ReadinessErrorV1{Stage: ReadinessStageIntentAccess, FailureID: "intent_dacl_refused"}
			if typed.RepairEligible && typed.RepairPath != "" && !errors.Is(typed.Cause, ErrWrongOwner) && !errors.Is(typed.Cause, ErrIrregularFile) {
				failure.Remediation = fmt.Sprintf("mcphub repair-state-dacl --path %q --yes", typed.RepairPath)
			}
			return failure
		}
	}
	var readinessErr *ReadinessErrorV1
	if errors.As(err, &readinessErr) {
		return readinessErr
	}
	if errors.Is(err, ErrWrongOwner) {
		return &ReadinessErrorV1{Stage: ReadinessStageIntentAccess, FailureID: "intent_wrong_owner"}
	}
	if errors.Is(err, ErrIrregularFile) {
		return &ReadinessErrorV1{Stage: ReadinessStageIntentAccess, FailureID: "intent_irregular_file"}
	}
	if errors.Is(err, ErrDaclOutsideAllowlist) || errors.Is(err, ErrTooLoose) {
		return &ReadinessErrorV1{Stage: ReadinessStageIntentAccess, FailureID: "intent_dacl_refused"}
	}
	if errors.Is(err, ErrStatusSetupFailure) {
		return &ReadinessErrorV1{Stage: ReadinessStageIntentAccess, FailureID: "intent_unreadable"}
	}
	return &ReadinessErrorV1{Stage: ReadinessStageSupervisorBootstrap, FailureID: "supervisor_bootstrap_failed"}
}

func receivingReadinessContextSettlement(request ReceivingReadinessRequestV1, snapshot ReadinessSnapshotV1, err error) (CommandSettlementV1, error) {
	if errors.Is(err, context.DeadlineExceeded) {
		return receivingReadinessTimeoutSettlement(request, snapshot)
	}
	return receivingReadinessFailureSettlement(request, snapshot, &ReadinessErrorV1{Stage: ReadinessStageSupervisorBootstrap, FailureID: "readiness_cancelled"})
}

var incompleteReadinessFailureIDsV1 = map[ReadinessStageV1]string{
	ReadinessStageTaskIntent:       "task_intent_missing",
	ReadinessStageWrapperStart:     "wrapper_start_timeout",
	ReadinessStageUpstreamListener: "upstream_listener_unbound",
	ReadinessStageMCPInitialize:    "mcp_initialize_failed",
	ReadinessStageMCPToolsList:     "mcp_tools_list_failed",
}

func receivingReadinessTimeoutSettlement(request ReceivingReadinessRequestV1, snapshot ReadinessSnapshotV1) (CommandSettlementV1, error) {
	failures := incompleteReadinessFailuresV1(snapshot)
	if len(failures) == 0 {
		return receivingReadinessFailureSettlement(request, snapshot, &ReadinessErrorV1{Stage: ReadinessStageSupervisorBootstrap, FailureID: "readiness_timeout"})
	}
	snapshot.Failures = append(snapshot.Failures, failures...)
	sortReadinessFailures(snapshot.Failures)
	primary := snapshot.Failures[0]
	snapshot.PrimaryFailure = &primary
	snapshot.Settled = false
	return commandSettlementV1(request.Operation, "committed_unverified", nil, snapshot), &ReadinessErrorV1{Stage: primary.Stage, FailureID: primary.FailureID}
}

func incompleteReadinessFailuresV1(snapshot ReadinessSnapshotV1) []ReadinessFailureV1 {
	failures := make([]ReadinessFailureV1, 0, len(snapshot.Bindings)+len(snapshot.Daemons))
	for _, binding := range snapshot.Bindings {
		if !binding.Requested {
			continue
		}
		if !binding.Present {
			failures = append(failures, ReadinessFailureV1{Stage: ReadinessStageClientBinding, FailureID: "client_binding_missing", Client: binding.Client})
			continue
		}
		if !binding.Readable {
			failures = append(failures, ReadinessFailureV1{Stage: ReadinessStageClientBinding, FailureID: "client_binding_unreadable", Client: binding.Client})
			continue
		}
		if !binding.Enabled || binding.Disabled || binding.Route != BindingRouteHub || !binding.ExactHubRoute {
			failures = append(failures, ReadinessFailureV1{Stage: ReadinessStageClientBinding, FailureID: "client_binding_mismatch", Client: binding.Client})
		}
	}
	for _, daemon := range snapshot.Daemons {
		if daemon.Settled {
			continue
		}
		if failureID, ok := incompleteReadinessFailureIDsV1[daemon.Stage]; ok {
			failures = append(failures, ReadinessFailureV1{Stage: daemon.Stage, FailureID: failureID, TaskName: daemon.TaskName, Port: daemon.Port})
		}
	}
	return failures
}

func receivingReadinessFailureSettlement(request ReceivingReadinessRequestV1, snapshot ReadinessSnapshotV1, err error) (CommandSettlementV1, error) {
	var typed *ReadinessErrorV1
	if !errors.As(err, &typed) {
		typed = &ReadinessErrorV1{Stage: ReadinessStageSupervisorBootstrap, FailureID: "supervisor_bootstrap_failed"}
		err = typed
	}
	failure := ReadinessFailureV1{Stage: typed.Stage, FailureID: typed.FailureID}
	if snapshot.SchemaVersion == "" {
		snapshot.SchemaVersion = "readiness-snapshot-v1"
	}
	snapshot.PrimaryFailure = &failure
	snapshot.Failures = append([]ReadinessFailureV1{failure}, snapshot.Failures...)
	snapshot.Settled = false
	return commandSettlementV1(request.Operation, "committed_unverified", nil, snapshot), err
}

type systemUTCClockV1 struct{}

func (systemUTCClockV1) NowUTC() time.Time { return time.Now().UTC() }

type timerWaiterV1 struct{}

func (timerWaiterV1) Wait(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type commandSettlementDeadlineV1 struct{}

func (commandSettlementDeadlineV1) Begin(parent context.Context, startupBindDeadline time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second+startupBindDeadline+3*time.Second)
}

type apiReceivingStateReaderV1 struct {
	readStatus func(context.Context) ([]DaemonStatus, error)
}

func (r apiReceivingStateReaderV1) Read(ctx context.Context, expectations []bindingExpectationV1) ([]DaemonStatus, []BindingObservationV1, error) {
	readStatus := r.readStatus
	if readStatus == nil {
		readStatus = readReceivingSupervisorStatusV1
	}
	rows, err := readStatus(ctx)
	if err != nil {
		return nil, nil, err
	}
	allClients := clients.AllClients()
	bindings := make([]BindingObservationV1, 0, len(expectations))
	for _, expectation := range expectations {
		client := allClients[expectation.Client]
		if client == nil {
			return rows, bindings, &ReadinessErrorV1{Stage: ReadinessStageClientBinding, FailureID: "client_binding_unreadable"}
		}
		binding, readErr := readBindingExpectationV1(expectation, client)
		if readErr != nil {
			return rows, bindings, readErr
		}
		bindings = append(bindings, binding)
	}
	return rows, bindings, nil
}

// readReceivingSupervisorStatusV1 obtains state-file failure evidence from the
// sidecar owner before entering the status transport. The typed wrapper carries
// only the already-owned cause, exact sidecar path, and repair eligibility; it
// never derives any of those facts from an error string.
func readReceivingSupervisorStatusV1(ctx context.Context) ([]DaemonStatus, error) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return DialSupervisorIPCStatus(ctx)
	}
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	if _, err := ReadSupervisorLockOwner(lockPath); err != nil && !os.IsNotExist(err) {
		return nil, receivingStateReadErrorFromOwnerV1(lockPath+".owner.json", err)
	}
	return dialSupervisorIPCStatusFromStateDir(ctx, stateDir)
}

func receivingStateReadErrorFromOwnerV1(path string, cause error) error {
	repairEligible := (errors.Is(cause, ErrDaclOutsideAllowlist) || errors.Is(cause, ErrTooLoose)) &&
		!errors.Is(cause, ErrWrongOwner) && !errors.Is(cause, ErrIrregularFile)
	return &ReceivingStateReadErrorV1{Cause: cause, RepairPath: path, RepairEligible: repairEligible}
}

func readBindingExpectationV1(expectation bindingExpectationV1, client clients.Client) (BindingObservationV1, error) {
	observation := BindingObservationV1{Client: expectation.Client, Requested: true}
	observed, err := client.GetEntry(expectation.Expected.Name)
	if err != nil {
		return observation, &ReadinessErrorV1{Stage: ReadinessStageClientBinding, FailureID: "client_binding_unreadable"}
	}
	if observed == nil {
		return observation, nil
	}
	observation.Present = true
	observation.Readable = true
	observation.Enabled = !observed.Disabled
	observation.Disabled = observed.Disabled
	actual := intendedEntryReadbackProjection(client, *observed)
	if reflect.DeepEqual(expectation.Expected, actual) && observation.Enabled {
		if (expectation.Expected.URL != "" && clients.IsHubHTTPURL(expectation.Expected.URL)) ||
			(expectation.Expected.RelayURL != "" && clients.IsHubHTTPURL(expectation.Expected.RelayURL)) {
			observation.Route = BindingRouteHub
			observation.ExactHubRoute = true
		} else {
			observation.Route = BindingRouteDirect
		}
	}
	return observation, nil
}

// awaitReadinessForTasks is a source-compatible bridge for the existing adopt
// verifier. It constructs the same injected production port as command
// composition; it does not restore the former ambient poll loop.
func (a *API) awaitReadinessForTasks(ctx context.Context, selectors []string, _ []BindingObservationV1, requireBinding bool) (ReadinessSnapshotV1, error) {
	port, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{
		Reader: apiReceivingStateReaderV1{}, Clock: systemUTCClockV1{}, Waiter: timerWaiterV1{}, Deadline: commandSettlementDeadlineV1{},
	})
	if err != nil {
		return ReadinessSnapshotV1{}, err
	}
	settlement, err := port.Await(ctx, ReceivingReadinessRequestV1{Operation: CommandOperationRestart, ExpectedTasks: selectors, RequireBindings: requireBinding})
	return settlement.Snapshot, err
}
