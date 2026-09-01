package vtune

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/process"
)

type scriptedVTuneDriver struct {
	collectStarted chan struct{}
	releaseCollect chan struct{}
	releaseOnStop  bool
	stopErr        error
	finalizeErr    error
	reportErr      error
	collectErr     error
	mu             sync.Mutex
	stops          int
	forceStops     int
	releaseOnce    sync.Once
}

// closeDeadlineDriver holds collect workers live until the test explicitly
// releases them, while native stop/force calls consume only their passed
// context. It makes shutdown-deadline accounting deterministic without a
// real VTune process.
type closeDeadlineDriver struct {
	collectStarted chan struct{}
	releaseCollect chan struct{}
}

func (d *closeDeadlineDriver) collect(context.Context, vtuneRunRequest) (*runOutput, error) {
	d.collectStarted <- struct{}{}
	<-d.releaseCollect
	return &runOutput{ExitCode: 0, ResultDir: "result", CommandLine: "collect"}, nil
}
func (d *closeDeadlineDriver) stop(ctx context.Context, _ vtuneRunRequest) error {
	<-ctx.Done()
	return ctx.Err()
}
func (d *closeDeadlineDriver) finalize(context.Context, vtuneRunRequest) error { return nil }
func (d *closeDeadlineDriver) report(context.Context, vtuneRunRequest) (*runOutput, error) {
	return &runOutput{ExitCode: 0, ResultDir: "result", ReportCSV: "Function\tCPU Time\nmain\t1.0\n", Summary: "summary", ReportPath: "report.csv"}, nil
}
func (d *closeDeadlineDriver) forceStop(ctx context.Context, _ vtuneRunRequest) error {
	<-ctx.Done()
	return ctx.Err()
}

func (d *scriptedVTuneDriver) collect(context.Context, vtuneRunRequest) (*runOutput, error) {
	close(d.collectStarted)
	<-d.releaseCollect
	return &runOutput{ExitCode: phaseExitCode(d.collectErr), ResultDir: "result", CommandLine: "collect"}, d.collectErr
}
func (d *scriptedVTuneDriver) stop(context.Context, vtuneRunRequest) error {
	d.mu.Lock()
	d.stops++
	d.mu.Unlock()
	if d.releaseOnStop {
		d.releaseOnce.Do(func() { close(d.releaseCollect) })
	}
	return d.stopErr
}
func (d *scriptedVTuneDriver) finalize(context.Context, vtuneRunRequest) error { return d.finalizeErr }
func (d *scriptedVTuneDriver) report(context.Context, vtuneRunRequest) (*runOutput, error) {
	if d.reportErr != nil {
		return nil, d.reportErr
	}
	return &runOutput{ExitCode: 0, ResultDir: "result", ReportCSV: "Function\tCPU Time\nmain\t1.0\n", Summary: "summary", ReportPath: "report.csv"}, nil
}
func (d *scriptedVTuneDriver) forceStop(context.Context, vtuneRunRequest) error {
	d.mu.Lock()
	d.forceStops++
	d.mu.Unlock()
	return nil
}

func newScriptedOwner(t *testing.T, d *scriptedVTuneDriver) *vtuneRunOwnerV1 {
	t.Helper()
	o, err := newVTuneRunOwnerV1(t.TempDir(), d)
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	t.Cleanup(func() { _ = o.Close(context.Background()) })
	return o
}

func TestVTuneRunOwner_BoundsDurableWork(t *testing.T) {
	d := &closeDeadlineDriver{collectStarted: make(chan struct{}, maxDurableVTuneRuns), releaseCollect: make(chan struct{})}
	o, err := newVTuneRunOwnerV1(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(d.releaseCollect)
		_ = o.Close(context.Background())
	})
	for i := 0; i < maxDurableVTuneRuns; i++ {
		if _, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		<-d.collectStarted
	}
	if _, _, err := o.Start(vtuneRunRequest{Target: "extra.exe", AnalysisType: "hotspots", TimeoutSec: 60}); !errors.Is(err, errVTuneAdmissionLimited) {
		t.Fatalf("extra start error = %v, want admission limit", err)
	}
	if _, _, err := o.Start(vtuneRunRequest{Target: "long.exe", AnalysisType: "hotspots", TimeoutSec: maxDurableTimeoutSec + 1}); !errors.Is(err, errVTuneTimeoutOutOfRange) {
		t.Fatalf("long start error = %v, want timeout range error", err)
	}
}

func TestVTuneRunOwner_DurableCollectHasDeadline(t *testing.T) {
	d := &deadlineCaptureDriver{deadline: make(chan time.Time, 1)}
	o, err := newVTuneRunOwnerV1(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close(context.Background()) })
	if _, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := <-d.deadline
	if remaining := time.Until(deadline); remaining <= 0 || remaining > time.Second {
		t.Fatalf("collect deadline remaining = %v", remaining)
	}
}

type deadlineCaptureDriver struct{ deadline chan time.Time }

func (d *deadlineCaptureDriver) collect(ctx context.Context, _ vtuneRunRequest) (*runOutput, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("collect context has no deadline")
	}
	d.deadline <- deadline
	return nil, errors.New("test complete")
}
func (*deadlineCaptureDriver) stop(context.Context, vtuneRunRequest) error     { return nil }
func (*deadlineCaptureDriver) finalize(context.Context, vtuneRunRequest) error { return nil }
func (*deadlineCaptureDriver) report(context.Context, vtuneRunRequest) (*runOutput, error) {
	return nil, nil
}
func (*deadlineCaptureDriver) forceStop(context.Context, vtuneRunRequest) error { return nil }

func TestVTuneRunOwner_StartStopFinalizesAndPublishesReceipt(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{}), releaseOnStop: true}
	o := newScriptedOwner(t, d)
	req := vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60, IdempotencyKey: "idem"}
	run, disposition, err := o.Start(req)
	if err != nil || disposition != "started" {
		t.Fatalf("Start = (%+v,%q,%v)", run, disposition, err)
	}
	<-d.collectStarted
	stopped, disposition, err := o.Stop(run.RunID, "stop-1", true)
	if err != nil || disposition != "stopped" || stopped.State != vtuneRunStopped {
		t.Fatalf("Stop = (%+v,%q,%v)", stopped, disposition, err)
	}
	settled := waitVTuneRun(t, o, run.RunID)
	if settled.State != vtuneRunStopped || !settled.Reportable || settled.ReceiptSHA256 == "" {
		t.Fatalf("settled = %+v, want reportable stopped receipt", settled)
	}
	if d.stops != 1 {
		t.Fatalf("native stops=%d, want 1", d.stops)
	}
	if _, _, err := o.Stop(run.RunID, "stop-1", true); err != nil {
		t.Fatalf("repeat stop: %v", err)
	}
	if d.stops != 1 {
		t.Fatalf("repeat operation executed native stop %d times", d.stops)
	}
}

func TestVTuneRunOwner_StopReplayReturnsSameSettledReceipt(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{}), releaseOnStop: true}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	first, firstDisposition, err := o.Stop(run.RunID, "replay-stop", true)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDisposition, err := o.Stop(run.RunID, "replay-stop", true)
	if err != nil {
		t.Fatal(err)
	}
	if firstDisposition != "stopped" || secondDisposition != firstDisposition || first.ReceiptSHA256 == "" || second.ReceiptSHA256 != first.ReceiptSHA256 || second.State != first.State {
		t.Fatalf("stop replay first=(%+v,%q) second=(%+v,%q)", first, firstDisposition, second, secondDisposition)
	}
}

func TestVTuneRunOwner_StopFallbackReceiptReplaysBeforeAndAfterTerminalSettlement(t *testing.T) {
	d := &scriptedVTuneDriver{
		collectStarted: make(chan struct{}),
		releaseCollect: make(chan struct{}),
		collectErr:     context.Canceled,
		stopErr:        errors.New("native stop unavailable"),
	}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted

	first, firstDisposition, err := o.Stop(run.RunID, "fallback-replay", true)
	if err != nil || firstDisposition != "stop_fallback_requested" || first.StopOperations["fallback-replay"].Disposition != firstDisposition {
		t.Fatalf("first fallback receipt=(%+v,%q,%v)", first, firstDisposition, err)
	}
	immediate, immediateDisposition, err := o.Stop(run.RunID, "fallback-replay", true)
	if err != nil || immediateDisposition != firstDisposition || immediate.StopOperations["fallback-replay"].Disposition != firstDisposition || immediate.Generation != first.Generation {
		t.Fatalf("immediate replay=(%+v,%q,%v), first=(%+v,%q)", immediate, immediateDisposition, err, first, firstDisposition)
	}

	close(d.releaseCollect)
	settled := waitVTuneRun(t, o, run.RunID)
	if settled.State != vtuneRunFailed || settled.FailureID != failureStopFailed || settled.StopOperations["fallback-replay"].Disposition != "failed" {
		t.Fatalf("terminal fallback settlement=%+v", settled)
	}
	afterFirst, afterFirstDisposition, err := o.Stop(run.RunID, "fallback-replay", true)
	if err != nil || afterFirstDisposition != "failed" || afterFirst.StopOperations["fallback-replay"].Disposition != afterFirstDisposition {
		t.Fatalf("first terminal replay=(%+v,%q,%v)", afterFirst, afterFirstDisposition, err)
	}
	afterSecond, afterSecondDisposition, err := o.Stop(run.RunID, "fallback-replay", true)
	if err != nil || afterSecondDisposition != afterFirstDisposition || afterSecond.StopOperations["fallback-replay"].Disposition != afterFirstDisposition || afterSecond.Generation != afterFirst.Generation {
		t.Fatalf("second terminal replay=(%+v,%q,%v), first=(%+v,%q)", afterSecond, afterSecondDisposition, err, afterFirst, afterFirstDisposition)
	}
}

func TestVTuneRunOwner_StopReturnsPersistedFailureAfterFinalizeOrReportFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finalize  error
		report    error
		failureID string
	}{
		{name: "finalize", finalize: errors.New("finalize failed"), failureID: failureFinalizeFailed},
		{name: "report", report: errors.New("report failed"), failureID: failureReportFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{}), releaseOnStop: true, finalizeErr: tc.finalize, reportErr: tc.report}
			o := newScriptedOwner(t, d)
			run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
			if err != nil {
				t.Fatal(err)
			}
			<-d.collectStarted
			operationID := "stop-" + tc.name + "-failure"
			got, disposition, err := o.Stop(run.RunID, operationID, true)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != vtuneRunFailed || got.FailureID != tc.failureID || disposition != "failed" || got.StopOperations[operationID].Disposition != disposition {
				t.Fatalf("Stop returned a non-persisted receipt: run=%+v disposition=%q", got, disposition)
			}
			replayed, replayDisposition, err := o.Stop(run.RunID, operationID, true)
			if err != nil || replayDisposition != disposition || replayed.StopOperations[operationID].Disposition != disposition {
				t.Fatalf("replay=(%+v,%q,%v)", replayed, replayDisposition, err)
			}
		})
	}
}

func TestVTuneRunOwner_StopPreparedPreventsCollectLaunch(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}
	o := newScriptedOwner(t, d)
	now := time.Now().UTC()
	r := &vtuneRunRecord{SchemaVersion: vtuneRunSchemaV1, Generation: 1, RunID: "prepared-stop", State: vtuneRunPrepared, Phase: vtuneRunPrepared, CreatedAt: now, UpdatedAt: now, PhaseExitCodes: map[string]int{}, StopOperations: map[string]vtuneStopOperation{}, Request: vtuneRunRequest{RunID: "prepared-stop", Target: "target.exe", AnalysisType: "hotspots"}}
	o.runs[r.RunID] = r
	o.durable[r.RunID] = cloneVTuneRunRecord(*r)
	o.done[r.RunID] = make(chan struct{})
	o.wg.Add(1)
	if _, disposition, err := o.Stop(r.RunID, "stop-before-worker", true); err != nil || disposition != "stopped_before_collect" {
		t.Fatalf("prepared stop = (%q,%v)", disposition, err)
	}
	o.worker(context.Background(), context.Background(), r.RunID)
	select {
	case <-d.collectStarted:
		t.Fatal("prepared stop still launched collect")
	default:
	}
}

func TestVTuneRunOwner_ContainedCollectNonzeroSettlesAsCompletedNonzero(t *testing.T) {
	code := 7
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{}), collectErr: &process.ContainedRunError{Stage: process.ContainedStageExit, Cause: errors.New("exit"), ExitCode: &code}}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	close(d.releaseCollect)
	settled := waitVTuneRun(t, o, run.RunID)
	if settled.State != vtuneRunCompletedNonzero || !settled.Reportable || settled.ExitCode != code || settled.PhaseExitCodes["collect"] != code || settled.ReceiptSHA256 == "" {
		t.Fatalf("contained nonzero settled=%+v", settled)
	}
	body, err := os.ReadFile(filepath.Join(o.root, "durable-runs", run.RunID, "report-receipt-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt vtuneReportReceiptV1
	if err := json.Unmarshal(body, &receipt); err != nil || receipt.ExitCode != code {
		t.Fatalf("receipt must preserve collect exit=%d; receipt=%+v err=%v", code, receipt, err)
	}
}

func TestVTuneRunOwner_ContainedCollectCleanupFailureIsNotReportable(t *testing.T) {
	code := 7
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{}), collectErr: &process.ContainedRunError{Stage: process.ContainedStageExit, Cause: errors.New("exit"), ExitCode: &code, CleanupCause: errors.New("reap failed")}}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	close(d.releaseCollect)
	settled := waitVTuneRun(t, o, run.RunID)
	if settled.Reportable || settled.State != vtuneRunFailed || settled.FailureID != failureCleanupFailed {
		t.Fatalf("cleanup-incomplete collect must fail closed: %+v", settled)
	}
}

func TestVTuneRunOwner_TerminalRecordCannotClaimLaterPhase(t *testing.T) {
	o := newScriptedOwner(t, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})})
	r := &vtuneRunRecord{RunID: "terminal-phase", State: vtuneRunFailed, Phase: vtuneRunFinalizing, Request: vtuneRunRequest{RunID: "terminal-phase"}}
	o.runs[r.RunID] = r
	if _, launched := o.claimPhase(r.RunID, vtuneRunFinalizing); launched {
		t.Fatal("terminal run claimed a later native phase")
	}
}

func TestVTuneRunOwner_InvalidReceiptPersistenceFailureIsFailClosedForCurrentStatus(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	close(d.releaseCollect)
	_ = waitVTuneRun(t, o, run.RunID)
	if err := os.Remove(filepath.Join(o.root, "durable-runs", run.RunID, "report-receipt-v1.json")); err != nil {
		t.Fatal(err)
	}
	o.snapshotWriter = func(string, []byte) error { return errors.New("injected invalidation persistence failure") }
	got, ok := o.Status(run.RunID)
	if !ok || got.Reportable || got.State != vtuneRunFailed || got.FailureID != failureResultNonReportable {
		t.Fatalf("invalid receipt must fail closed even when persistence fails: %+v", got)
	}
}

func TestVTuneRunOwner_CloseDeadlineRetainsLeaseUntilWorkerSettlement(t *testing.T) {
	root := t.TempDir()
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}
	o, err := newVTuneRunOwnerV1(root, d)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	deadline, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.Close(deadline); err == nil {
		t.Fatal("Close unexpectedly succeeded after cancelled shutdown context")
	}
	if next, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}); err == nil {
		_ = next.Close(context.Background())
		t.Fatal("second owner acquired durable store while original worker was still live")
	}
	close(d.releaseCollect)
	if !o.waitSettled(run.RunID, time.Second) {
		t.Fatal("worker did not settle after release")
	}
	if next, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}); err != nil {
		t.Fatalf("second owner remained refused after exact worker settlement: %v", err)
	} else {
		_ = next.Close(context.Background())
	}
}

func TestVTuneRunOwner_CloseSharesCancelledBudgetAcrossRunsAndRetainsLease(t *testing.T) {
	root := t.TempDir()
	d := &closeDeadlineDriver{collectStarted: make(chan struct{}, 2), releaseCollect: make(chan struct{})}
	o, err := newVTuneRunOwnerV1(root, d)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60}); err != nil {
			t.Fatal(err)
		}
		<-d.collectStarted
	}
	shutdownCtx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := o.Close(shutdownCtx); err == nil {
		t.Fatal("Close unexpectedly succeeded with an already-cancelled deadline")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Close consumed fresh per-run phase budgets: %s", elapsed)
	}
	if _, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}); err == nil {
		t.Fatal("successor acquired the store before the cancelled runs settled")
	}
	close(d.releaseCollect)
	for _, r := range o.runs {
		if !o.waitSettled(r.RunID, time.Second) {
			t.Fatalf("run %q did not complete the lease-proven handoff", r.RunID)
		}
	}
	next, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})})
	if err != nil {
		t.Fatalf("successor remained blocked after every worker settled: %v", err)
	}
	if err := next.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVTuneRunOwner_UnlockFailureBlocksHandoffUntilRecoveryRetry(t *testing.T) {
	root := t.TempDir()
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{}), releaseOnStop: true}
	o, err := newVTuneRunOwnerV1(root, d)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	unlockErr := errors.New("injected unlock failure")
	attempts := 0
	o.unlockStoreLease = func(lease *flock.Flock) error {
		attempts++
		if attempts == 1 {
			return unlockErr
		}
		return lease.Unlock()
	}
	if err := o.Close(context.Background()); !errors.Is(err, unlockErr) {
		t.Fatalf("Close error=%v, want injected unlock failure", err)
	}
	if o.storeLease == nil {
		t.Fatal("unlock failure discarded the live lease handle")
	}
	select {
	case <-o.done[run.RunID]:
		t.Fatal("unlock failure published a settled worker handoff")
	default:
	}
	if _, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}); err == nil {
		t.Fatal("successor acquired the store despite unproven unlock")
	}
	if err := o.Close(context.Background()); err != nil {
		t.Fatalf("recovery retry did not release the lease: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("unlock attempts=%d, want retry", attempts)
	}
	select {
	case <-o.done[run.RunID]:
	default:
		t.Fatal("proven unlock did not publish the settled worker handoff")
	}
	next, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})})
	if err != nil {
		t.Fatalf("successor remained blocked after proven unlock: %v", err)
	}
	if err := next.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVTuneRunOwner_RetainedReceiptRequiresLiveArtifacts(t *testing.T) {
	o := newScriptedOwner(t, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})})
	runID := "retained-artifacts"
	rawRoot := filepath.Join(o.root, "runs", runID)
	resultDir := filepath.Join(rawRoot, "result")
	reportPath := filepath.Join(rawRoot, "reports", "report.csv")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultDir, "raw-data"), []byte("raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte("csv"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &vtuneRunRecord{SchemaVersion: vtuneRunSchemaV1, RunID: runID, State: vtuneRunCompleted, Phase: vtuneRunCompleted, Reportable: true, ExitCode: 0, Request: vtuneRunRequest{RunID: runID, KeepResult: true, ResultDir: resultDir}, Output: &runOutput{ResultDir: resultDir, ReportPath: reportPath, ReportCSV: "csv", Summary: "summary"}}
	hash, err := o.writeReceiptLocked(r)
	if err != nil {
		t.Fatal(err)
	}
	r.ReceiptSHA256 = hash
	if !o.validReceiptLocked(r) {
		t.Fatal("retained receipt rejected while result and report artifacts exist")
	}
	if err := os.Remove(reportPath); err != nil {
		t.Fatal(err)
	}
	if o.validReceiptLocked(r) {
		t.Fatal("retained receipt accepted after its report artifact was removed")
	}
}

func TestVTuneRunOwner_LoadIgnoresReceiptAsSnapshot(t *testing.T) {
	root := t.TempDir()
	first, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := vtuneRunRecord{SchemaVersion: vtuneRunSchemaV1, Generation: 2, RunID: "load-snapshot", State: vtuneRunFailed, Phase: vtuneRunFailed, CreatedAt: now, UpdatedAt: now, ExitCode: -1, Request: vtuneRunRequest{RunID: "load-snapshot"}}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "durable-runs", r.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00000002.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report-receipt-v1.json"), []byte("not a snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close(context.Background())
	got, ok := loaded.Status(r.RunID)
	if !ok || got.Generation != r.Generation || got.State != vtuneRunFailed {
		t.Fatalf("load selected a non-generation JSON file: %+v", got)
	}
}

func TestVTuneRunOwner_FailureRemovesUnretainedRawDirectory(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{}), collectErr: errors.New("collect launch failed")}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{RunID: "remove-unretained", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	raw := filepath.Join(o.root, "runs", run.RunID)
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	close(d.releaseCollect)
	_ = waitVTuneRun(t, o, run.RunID)
	if _, err := os.Stat(raw); !os.IsNotExist(err) {
		t.Fatalf("unretained failure left raw run directory: %v", err)
	}
}

func TestVTuneRunOwner_NativeStopWaitsThenFallsBackWhenCollectDoesNotSettle(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}
	o := newScriptedOwner(t, d)
	o.settlementTimeout = 10 * time.Millisecond
	run, _, err := o.Start(vtuneRunRequest{Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	got, disposition, err := o.Stop(run.RunID, "stop-timeout", true)
	if err != nil || disposition != "failed" {
		t.Fatalf("Stop=(%+v,%q,%v)", got, disposition, err)
	}
	if got.State != vtuneRunFailed || got.FailureID != failureStopSettlementTimeout || got.Reportable {
		t.Fatalf("timeout result=%+v", got)
	}
	if d.forceStops != 1 {
		t.Fatalf("force stops=%d, want one fallback", d.forceStops)
	}
	close(d.releaseCollect)
	if !o.waitSettled(run.RunID, time.Second) {
		t.Fatal("worker remained live after test release")
	}
}

func TestVTuneRunOwner_IdempotencyConflictDoesNotStartSecondWorker(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}
	o := newScriptedOwner(t, d)
	first, _, err := o.Start(vtuneRunRequest{Target: "a.exe", AnalysisType: "hotspots", TimeoutSec: 60, IdempotencyKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	again, disposition, err := o.Start(vtuneRunRequest{Target: "a.exe", AnalysisType: "hotspots", TimeoutSec: 60, IdempotencyKey: "same"})
	if err != nil || disposition != "replayed" || again.RunID != first.RunID {
		t.Fatalf("same start=(%+v,%q,%v)", again, disposition, err)
	}
	_, _, err = o.Start(vtuneRunRequest{Target: "different.exe", AnalysisType: "hotspots", TimeoutSec: 60, IdempotencyKey: "same"})
	if !errors.Is(err, errVTuneIdempotencyConflict) {
		t.Fatalf("different key err=%v", err)
	}
	close(d.releaseCollect)
	_ = waitVTuneRun(t, o, first.RunID)
}

func TestVTuneRunOwner_ReceiptRemovalMakesTerminalResultNonreportable(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{Target: "a.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	close(d.releaseCollect)
	completed := waitVTuneRun(t, o, run.RunID)
	if !completed.Reportable || completed.ReceiptSHA256 == "" {
		t.Fatalf("completed=%+v", completed)
	}
	if err := os.Remove(filepath.Join(o.root, "durable-runs", run.RunID, "report-receipt-v1.json")); err != nil {
		t.Fatal(err)
	}
	got, ok := o.Status(run.RunID)
	if !ok || got.Reportable || got.FailureID != failureResultNonReportable || profileResultFromRun(got).ResultDir != "" {
		t.Fatalf("receipt removal status=%+v", got)
	}
}

func TestVTuneRunOwner_RejectsSecondStoreOwner(t *testing.T) {
	root := t.TempDir()
	first, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(context.Background())
	if _, err := newVTuneRunOwnerV1(root, &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}); err == nil {
		t.Fatal("second owner unexpectedly acquired the durable run store")
	}
}

func TestVTuneRunOwner_TerminalPersistenceFailureNeverPublishesResult(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{Target: "a.exe", AnalysisType: "hotspots", TimeoutSec: 60, KeepResult: true})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	o.snapshotWriter = func(string, []byte) error { return errors.New("injected snapshot write failure") }
	close(d.releaseCollect)
	if !o.waitSettled(run.RunID, time.Second) {
		t.Fatal("worker did not return after durable write failure")
	}
	got, ok := o.Status(run.RunID)
	if !ok || got.Reportable || got.FailureID != failureCleanupFailed || got.ExitCode == 0 {
		t.Fatalf("persistence failure status=%+v", got)
	}
}

func TestVTuneRunOwner_ReportFailureIsNonReportableAndNonzero(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{}), reportErr: errors.New("report failed")}
	o := newScriptedOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{Target: "a.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.collectStarted
	close(d.releaseCollect)
	settled := waitVTuneRun(t, o, run.RunID)
	if settled.State != vtuneRunFailed || settled.Reportable || settled.FailureID != failureReportFailed || settled.ExitCode == 0 {
		t.Fatalf("settled=%+v", settled)
	}
}

func waitVTuneRun(t *testing.T, o *vtuneRunOwnerV1, id string) vtuneRunRecord {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		run, ok := o.Status(id)
		if ok && run.Terminal() {
			return run
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("run %q did not settle", id)
	return vtuneRunRecord{}
}
