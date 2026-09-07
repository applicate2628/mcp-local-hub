package vtune

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

const testMaxDurableTimeoutSec = 3600

// boundsDriver is deliberately process-free: its collect phase exposes the
// owner context and cannot settle until the test releases it.
type boundsDriver struct {
	started chan context.Context
	release chan struct{}
	once    sync.Once
	err     error
	out     *runOutput
}

func (d *boundsDriver) collect(ctx context.Context, _ vtuneRunRequest) (*runOutput, error) {
	d.started <- ctx
	if d.release != nil {
		<-d.release
	}
	if d.out != nil {
		return d.out, d.err
	}
	return &runOutput{ExitCode: phaseExitCode(d.err), ResultDir: "result", CommandLine: "collect"}, d.err
}

func (*boundsDriver) stop(context.Context, vtuneRunRequest) error      { return nil }
func (*boundsDriver) forceStop(context.Context, vtuneRunRequest) error { return nil }
func (*boundsDriver) finalize(context.Context, vtuneRunRequest) error  { return nil }
func (*boundsDriver) report(context.Context, vtuneRunRequest) (*runOutput, error) {
	return &runOutput{ExitCode: 0, ResultDir: "result", ReportCSV: "Function\tCPU Time\nmain\t1\n", Summary: "summary", ReportPath: "report.csv"}, nil
}

func (d *boundsDriver) releaseAll() {
	if d.release != nil {
		d.once.Do(func() { close(d.release) })
	}
}

func newBoundsOwner(t *testing.T, d *boundsDriver) *vtuneRunOwnerV1 {
	t.Helper()
	o, err := newVTuneRunOwnerV1(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.releaseAll()
		settled := make(chan struct{})
		go func() { o.wg.Wait(); close(settled) }()
		select {
		case <-settled:
		case <-time.After(5 * time.Second):
			t.Error("test worker did not settle")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := o.Close(ctx); err != nil {
			t.Errorf("close owner: %v", err)
		}
	})
	return o
}

func awaitBoundsCollect(t *testing.T, d *boundsDriver) context.Context {
	t.Helper()
	select {
	case ctx := <-d.started:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("collect was not called")
		return nil
	}
}

func TestProfileTool_StartRejectsNegativeDurableTimeout(t *testing.T) {
	d := &boundsDriver{started: make(chan context.Context, 1), release: make(chan struct{})}
	vs := &VTuneServer{findExe: func() (string, error) { return "vtune.exe", nil }, owner: newBoundsOwner(t, d)}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{"action": "start", "exe": "target.exe", "timeout_sec": -1}))
	if err != nil {
		t.Fatal(err)
	}
	var got profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &got); err != nil {
		t.Fatal(err)
	}
	if got.FailureID != "TIMEOUT_OUT_OF_RANGE" {
		t.Fatalf("negative timeout failure_id=%q, want TIMEOUT_OUT_OF_RANGE", got.FailureID)
	}
}

func TestProfileTool_StartRejectsFreshTooLongDurableTimeout(t *testing.T) {
	d := &boundsDriver{started: make(chan context.Context, 1), release: make(chan struct{})}
	vs := &VTuneServer{findExe: func() (string, error) { return "vtune.exe", nil }, owner: newBoundsOwner(t, d)}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{"action": "start", "exe": "target.exe", "timeout_sec": testMaxDurableTimeoutSec + 1}))
	if err != nil {
		t.Fatal(err)
	}
	var got profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &got); err != nil {
		t.Fatal(err)
	}
	if got.FailureID != "TIMEOUT_OUT_OF_RANGE" {
		t.Fatalf("too-long timeout failure_id=%q, want TIMEOUT_OUT_OF_RANGE", got.FailureID)
	}
}

func TestVTuneRunOwner_RejectsThirdUnsettledDurableRun(t *testing.T) {
	d := &boundsDriver{started: make(chan context.Context, 3), release: make(chan struct{})}
	o := newBoundsOwner(t, d)
	for i := 0; i < 2; i++ {
		if _, _, err := o.Start(vtuneRunRequest{RunID: fmt.Sprintf("active-%d", i), Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60}); err != nil {
			t.Fatal(err)
		}
		awaitBoundsCollect(t, d)
	}
	if _, _, err := o.Start(vtuneRunRequest{RunID: "third", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60}); err == nil {
		t.Fatal("third unsettled durable run was admitted")
	}
	d.releaseAll()
	for i := 0; i < 2; i++ {
		if !o.waitSettled(fmt.Sprintf("active-%d", i), 5*time.Second) {
			t.Fatal("settled worker did not release durable admission")
		}
	}
	if _, _, err := o.Start(vtuneRunRequest{RunID: "after-settlement", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60}); err != nil {
		t.Fatalf("settled workers did not free durable admission: %v", err)
	}
}

func TestVTuneRunOwner_ReplaysPreviouslyAcceptedLongRun(t *testing.T) {
	root := t.TempDir()
	legacy := vtuneRunRequest{RunID: "legacy", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: testMaxDurableTimeoutSec + 1, IdempotencyKey: "legacy-key"}
	record := vtuneRunRecord{SchemaVersion: vtuneRunSchemaV1, Generation: 1, RunID: legacy.RunID, State: vtuneRunCompleted, Phase: vtuneRunCompleted, Request: legacy, PhaseExitCodes: map[string]int{}}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "durable-runs", legacy.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00000001.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	d := &boundsDriver{started: make(chan context.Context, 1), release: make(chan struct{})}
	o, err := newVTuneRunOwnerV1(root, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close(context.Background()) })
	got, disposition, err := o.Start(legacy)
	if err != nil || disposition != "replayed" || got.RunID != legacy.RunID {
		t.Fatalf("legacy replay=(%+v,%q,%v), want persisted run replay", got, disposition, err)
	}
}

func TestVTuneRunOwner_TimeoutIsPersistedAndSurfaced(t *testing.T) {
	d := &boundsDriver{started: make(chan context.Context, 1), err: context.DeadlineExceeded, out: &runOutput{ExitCode: -1, ResultDir: "result", CommandLine: "collect"}}
	o := newBoundsOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{RunID: "deadline", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	awaitBoundsCollect(t, d)
	settled := waitVTuneRun(t, o, run.RunID)
	if settled.FailureID != "COLLECT_TIMEOUT" || settled.Output == nil || !settled.Output.TimedOut {
		t.Fatalf("settled timeout=%+v", settled)
	}
	if !profileResultFromRun(settled).TimedOut {
		t.Fatal("durable status omitted timed_out=true")
	}
}

func TestVTuneRunOwner_ContainedTimeoutWithExitCodeIsNotNormalNonzero(t *testing.T) {
	code := 124
	d := &boundsDriver{
		started: make(chan context.Context, 1),
		err:     &process.ContainedRunError{Stage: process.ContainedStageExit, Cause: context.DeadlineExceeded, ExitCode: &code},
		out:     &runOutput{ExitCode: code, ResultDir: "result", CommandLine: "collect", TimedOut: true},
	}
	o := newBoundsOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{RunID: "contained-timeout", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	awaitBoundsCollect(t, d)
	settled := waitVTuneRun(t, o, run.RunID)
	if settled.FailureID != "COLLECT_TIMEOUT" || settled.Output == nil || !settled.Output.TimedOut {
		t.Fatalf("contained timeout settled=%+v", settled)
	}
}

func TestVTuneRunOwner_CollectDeadlineLeavesVTuneDurationGrace(t *testing.T) {
	d := &boundsDriver{started: make(chan context.Context, 1), release: make(chan struct{})}
	o := newBoundsOwner(t, d)
	before := time.Now()
	if _, _, err := o.Start(vtuneRunRequest{RunID: "grace", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	ctx := awaitBoundsCollect(t, d)
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(60*time.Second+vtunePhaseTimeout)) || deadline.After(after.Add(60*time.Second+vtunePhaseTimeout)) {
		t.Fatalf("collect deadline=%v present=%v, want VTune duration plus settlement grace", deadline, ok)
	}
}

func TestVTuneRunOwner_WorkerExitCancelsCollectDeadline(t *testing.T) {
	d := &boundsDriver{started: make(chan context.Context, 1), release: make(chan struct{})}
	o := newBoundsOwner(t, d)
	run, _, err := o.Start(vtuneRunRequest{RunID: "early", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	ctx := awaitBoundsCollect(t, d)
	d.releaseAll()
	if !o.waitSettled(run.RunID, 5*time.Second) {
		t.Fatal("worker did not settle")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("collect deadline context survived worker exit: %v", ctx.Err())
	}
}
