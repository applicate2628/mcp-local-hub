package oneapirun

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type asyncTestStore struct {
	mu   sync.Mutex
	raw  []byte
	fail bool
}

func (s *asyncTestStore) Read(string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.raw == nil {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), s.raw...), nil
}

func (s *asyncTestStore) Write(_ string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("injected write failure")
	}
	s.raw = append(s.raw[:0], raw...)
	return nil
}

func (s *asyncTestStore) WriteJSON(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.Write("", raw)
}

func newAsyncTestOwner(t *testing.T, store *asyncTestStore, execute func(context.Context, oneAPIRunRequest) asyncExecution) *asyncRunOwner {
	t.Helper()
	if store == nil {
		store = &asyncTestStore{}
	}
	var next atomic.Int64
	owner, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: filepath.Join(t.TempDir(), "runs-v1.json"),
		Execute:   execute,
		Read:      store.Read,
		Write:     store.Write,
		Now:       func() time.Time { return time.Unix(100, next.Add(1)).UTC() },
		NextID:    func() string { return "run-" + time.Unix(0, next.Add(1)).UTC().Format("150405.000000000") },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	return owner
}

func asyncRequest(key string, timeout time.Duration) oneAPIRunRequest {
	return oneAPIRunRequest{Command: "fixture", Args: []string{"input"}, Cwd: "work", Timeout: timeout, IdempotencyKey: key}
}

func waitForAsyncTerminal(t *testing.T, owner *asyncRunOwner, runID string) asyncResultReceipt {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		result, err := owner.Result(runID)
		if err == nil && result.Terminal {
			return result
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("run %q did not settle", runID)
	return asyncResultReceipt{}
}

// TestAsyncRun_CallerLossDoesNotCancelAcceptedWorker catches an owner that
// accidentally derives a durable worker from the initiating request context.
func TestAsyncRun_CallerLossDoesNotCancelAcceptedWorker(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var launches atomic.Int32
	owner := newAsyncTestOwner(t, nil, func(ctx context.Context, _ oneAPIRunRequest) asyncExecution {
		launches.Add(1)
		close(started)
		select {
		case <-release:
			return asyncExecution{Result: runResult{ExitCode: 0, Stdout: "durable"}}
		case <-ctx.Done():
			return asyncExecution{Result: runResult{ExitCode: -1}, Err: ctx.Err()}
		}
	})

	caller, cancelCaller := context.WithCancel(context.Background())
	start, err := owner.Start(asyncRequest("caller-loss", time.Second))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancelCaller()
	<-started
	if err := caller.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller context = %v, want canceled", err)
	}
	replay, err := owner.Start(asyncRequest("caller-loss", time.Second))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed || replay.RunID != start.RunID || launches.Load() != 1 {
		t.Fatalf("replay=%#v launches=%d, want same receipt and one execution", replay, launches.Load())
	}
	conflict := asyncRequest("caller-loss", time.Second)
	conflict.Args = []string{"different-input"}
	if _, err := owner.Start(conflict); !errors.Is(err, errAsyncIdempotencyConflict) {
		t.Fatalf("conflicting replay error=%v, want idempotency conflict", err)
	}
	close(release)
	result := waitForAsyncTerminal(t, owner, start.RunID)
	if !result.Available || result.Result == nil || result.Result.Stdout != "durable" {
		t.Fatalf("result=%#v, want durable terminal output", result)
	}
}

// TestAsyncRun_CancelJoinsBeforeReply catches a cancel path that only signals
// a worker and exposes a receipt before the contained execution has settled.
func TestAsyncRun_CancelJoinsBeforeReply(t *testing.T) {
	started := make(chan struct{})
	owner := newAsyncTestOwner(t, nil, func(ctx context.Context, _ oneAPIRunRequest) asyncExecution {
		close(started)
		<-ctx.Done()
		return asyncExecution{Result: runResult{ExitCode: -1}, Err: ctx.Err()}
	})
	start, err := owner.Start(asyncRequest("cancel", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	status, err := owner.Cancel(start.RunID, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Action != "cancel" || !status.Terminal || status.State != asyncStateCancelled || status.FailureID != "oneapi_run_cancelled" {
		t.Fatalf("cancel status=%#v, want joined cancelled receipt", status)
	}
	replay, err := owner.Cancel(start.RunID, true)
	if err != nil || replay != status {
		t.Fatalf("cancel replay status=%#v err=%v, want same terminal receipt", replay, err)
	}
}

// TestAsyncRun_TimeoutSettlesWithTimeoutState catches a worker that reports a
// deadline as a generic failure or leaves a running receipt after cleanup.
func TestAsyncRun_TimeoutSettlesWithTimeoutState(t *testing.T) {
	started := make(chan struct{})
	owner := newAsyncTestOwner(t, nil, func(ctx context.Context, _ oneAPIRunRequest) asyncExecution {
		close(started)
		<-ctx.Done()
		return asyncExecution{Result: runResult{ExitCode: -1, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}, Err: ctx.Err()}
	})
	start, err := owner.Start(asyncRequest("timeout", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	result := waitForAsyncTerminal(t, owner, start.RunID)
	if result.State != asyncStateTimedOut || !result.Terminal || !result.Available || result.Result == nil || !result.Result.TimedOut {
		t.Fatalf("timeout result=%#v, want durable timed_out receipt", result)
	}
}

// TestAsyncRun_ShutdownJoinsAndPersistsServerShutdown catches Run returning
// while an accepted worker remains live or loses its shutdown discriminator.
func TestAsyncRun_ShutdownJoinsAndPersistsServerShutdown(t *testing.T) {
	started := make(chan struct{})
	owner := newAsyncTestOwner(t, nil, func(ctx context.Context, _ oneAPIRunRequest) asyncExecution {
		close(started)
		<-ctx.Done()
		return asyncExecution{Result: runResult{ExitCode: -1}, Err: ctx.Err()}
	})
	start, err := owner.Start(asyncRequest("shutdown", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := owner.Status(start.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Terminal || status.State != asyncStateCancelled || status.FailureID != "oneapi_run_server_shutdown" {
		t.Fatalf("shutdown status=%#v, want settled shutdown receipt", status)
	}
}

// TestAsyncRun_RestartRetainsUnsettledNonterminalWithoutExecuting proves a
// reloaded owner never invents settlement or reopens admission without a
// durable receipt that proves the prior process tree is gone.
func TestAsyncRun_RestartRetainsUnsettledNonterminalWithoutExecuting(t *testing.T) {
	const recoveryFailureID = "oneapi_run_recovery_unsettled"

	for _, tc := range []struct {
		name      string
		state     string
		startedAt *time.Time
	}{
		{name: "accepted", state: asyncStateAccepted},
		{name: "running", state: asyncStateRunning, startedAt: timePtr(time.Unix(101, 0).UTC())},
		{name: "cancel requested", state: asyncStateCancelRequested, startedAt: timePtr(time.Unix(101, 0).UTC())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &asyncTestStore{}
			request := asyncRequest("restart-"+tc.state, time.Minute)
			acceptedAt := time.Unix(100, 0).UTC()
			seed := asyncRunSnapshot{Version: asyncRunSnapshotVersion, Runs: []asyncRunRecord{{
				RunID: "run-restart-" + tc.state, IdempotencyKey: request.IdempotencyKey, RequestDigest: mustAsyncRequestDigest(t, request),
				State: tc.state, AcceptedAt: acceptedAt, StartedAt: tc.startedAt,
			}}}
			if err := store.WriteJSON(seed); err != nil {
				t.Fatal(err)
			}
			before, err := store.Read("")
			if err != nil {
				t.Fatal(err)
			}

			var launches, writes atomic.Int32
			lease := &asyncTestLease{}
			options := asyncOwnerOptions{
				StatePath: filepath.Join(t.TempDir(), "runs-v1.json"),
				Execute: func(context.Context, oneAPIRunRequest) asyncExecution {
					launches.Add(1)
					return asyncExecution{}
				},
				Read: store.Read,
				Write: func(string, []byte) error {
					writes.Add(1)
					return errors.New("recovery must not write")
				},
				NewLease: func(string) asyncRunLease { return lease },
			}
			owner, err := newAsyncRunOwner(t.Context(), options)
			if err != nil {
				t.Fatalf("load recovered %s run: %v", tc.state, err)
			}

			status, err := owner.Status(seed.Runs[0].RunID)
			startedAtPreserved := (status.StartedAt == nil && tc.startedAt == nil) || (status.StartedAt != nil && tc.startedAt != nil && status.StartedAt.Equal(*tc.startedAt))
			if err != nil || status.State != tc.state || status.Terminal || status.FailureID != recoveryFailureID || !status.AcceptedAt.Equal(acceptedAt) || !startedAtPreserved || status.FinishedAt != nil {
				t.Fatalf("recovered status=(%#v,%v), want preserved nonterminal recovery receipt", status, err)
			}
			result, err := owner.Result(seed.Runs[0].RunID)
			if err != nil || result.State != tc.state || result.Terminal || result.Available || result.Result != nil {
				t.Fatalf("recovered result=(%#v,%v), want unavailable nonterminal receipt", result, err)
			}
			replay, err := owner.Start(request)
			if err != nil || !replay.Replayed || replay.RunID != seed.Runs[0].RunID || replay.State != tc.state || replay.Terminal {
				t.Fatalf("recovered replay=(%#v,%v), want preserved nonterminal run", replay, err)
			}
			conflict := request
			conflict.Args = []string{"different"}
			if _, err := owner.Start(conflict); !errors.Is(err, errAsyncIdempotencyConflict) {
				t.Fatalf("recovered conflict error=%v, want idempotency conflict", err)
			}
			if _, err := owner.Start(asyncRequest("new-"+tc.state, time.Minute)); !errors.Is(err, errAsyncBusy) {
				t.Fatalf("recovered new key error=%v, want busy", err)
			}
			if _, err := owner.Cancel(seed.Runs[0].RunID, false); !errors.Is(err, errAsyncInvalidRequest) {
				t.Fatalf("unconfirmed recovered cancel error=%v, want invalid request", err)
			}
			if _, err := owner.Cancel(seed.Runs[0].RunID, true); err == nil || err.Error() != recoveryFailureID {
				t.Fatalf("confirmed recovered cancel error=%v, want recovery unsettled", err)
			}
			if err := owner.Close(); err == nil || err.Error() != recoveryFailureID || lease.unlockCalls != 1 {
				t.Fatalf("recovered close=(%v, unlocks=%d), want recovery unsettled and one lease release", err, lease.unlockCalls)
			}

			secondLease := &asyncTestLease{}
			options.NewLease = func(string) asyncRunLease { return secondLease }
			second, err := newAsyncRunOwner(t.Context(), options)
			if err != nil {
				t.Fatalf("second recovered load: %v", err)
			}
			if _, err := second.Start(asyncRequest("second-new-"+tc.state, time.Minute)); !errors.Is(err, errAsyncBusy) {
				t.Fatalf("second recovered new key error=%v, want busy", err)
			}
			if err := second.Close(); err == nil || err.Error() != recoveryFailureID || secondLease.unlockCalls != 1 {
				t.Fatalf("second recovered close=(%v, unlocks=%d), want recovery unsettled and one lease release", err, secondLease.unlockCalls)
			}
			after, err := store.Read("")
			if err != nil || !bytes.Equal(after, before) || launches.Load() != 0 || writes.Load() != 0 {
				t.Fatalf("recovery changed persistence or executed: raw same=%t launches=%d writes=%d err=%v", bytes.Equal(after, before), launches.Load(), writes.Load(), err)
			}
		})
	}
}

// TestAsyncRun_RetainsNewest64TerminalReceipts catches an eviction policy that
// drops active work, retains 65 records, or loses a retained idempotency key.
func TestAsyncRun_RetainsNewest64TerminalReceipts(t *testing.T) {
	owner := newAsyncTestOwner(t, nil, func(_ context.Context, request oneAPIRunRequest) asyncExecution {
		return asyncExecution{Result: runResult{ExitCode: 0, Stdout: request.IdempotencyKey}}
	})
	ids := make([]string, 65)
	for i := range ids {
		key := "retain-" + time.Unix(0, int64(i)).UTC().Format("150405.000000000")
		start, err := owner.Start(asyncRequest(key, time.Second))
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		_ = waitForAsyncTerminal(t, owner, start.RunID)
		ids[i] = start.RunID
	}
	if _, err := owner.Status(ids[0]); !errors.Is(err, errAsyncNotFound) {
		t.Fatalf("oldest status error=%v, want not found", err)
	}
	for i := 1; i < len(ids); i++ {
		result, err := owner.Result(ids[i])
		if err != nil || !result.Available {
			t.Fatalf("retained %d result=%#v err=%v", i, result, err)
		}
	}
}

// TestAsyncRun_TerminalWriteFailureIsVisibleAndCloseReturns catches a success
// mask or a cancel/shutdown deadlock after a terminal receipt cannot persist.
func TestAsyncRun_TerminalWriteFailureIsVisibleAndCloseReturns(t *testing.T) {
	store := &asyncTestStore{}
	started := make(chan struct{})
	release := make(chan struct{})
	owner := newAsyncTestOwner(t, store, func(context.Context, oneAPIRunRequest) asyncExecution {
		close(started)
		<-release
		return asyncExecution{Result: runResult{ExitCode: 0, Stdout: "must-not-publish"}}
	})
	start, err := owner.Start(asyncRequest("persist-fail", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	close(release)
	owner.mu.Lock()
	workerDone := owner.runs[start.RunID].workerDone
	owner.mu.Unlock()
	<-workerDone
	if _, err := owner.Result(start.RunID); !errors.Is(err, errAsyncTerminalPersist) {
		t.Fatalf("result error=%v, want terminal persistence failure", err)
	}
	if err := owner.Close(); !errors.Is(err, errAsyncTerminalPersist) {
		t.Fatalf("Close error=%v, want terminal persistence failure without deadlock", err)
	}
}

// TestAsyncRun_RejectsOversizedPersistedOutput catches a corrupt snapshot that
// exceeds even the migration-only legacy-expanded receipt ceiling after restart.
func TestAsyncRun_RejectsOversizedPersistedOutput(t *testing.T) {
	store := &asyncTestStore{}
	seed := asyncRunSnapshot{Version: asyncRunSnapshotVersion, Runs: []asyncRunRecord{{
		RunID: "run-overflow", IdempotencyKey: "overflow", RequestDigest: mustAsyncRequestDigest(t, oneAPIRunRequest{Command: "fixture", Timeout: time.Minute, IdempotencyKey: "overflow"}),
		State: asyncStateCompleted, AcceptedAt: time.Unix(100, 0).UTC(), FinishedAt: timePtr(time.Unix(101, 0).UTC()), FinishSeq: 1,
		Result: &runResult{Stdout: strings.Repeat("x", maxLegacyExpandedStreamBytes+1)},
	}}}
	if err := store.WriteJSON(seed); err != nil {
		t.Fatal(err)
	}
	_, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{StatePath: filepath.Join(t.TempDir(), "runs-v1.json"), Execute: func(context.Context, oneAPIRunRequest) asyncExecution { return asyncExecution{} }, Read: store.Read, Write: store.Write})
	if err == nil {
		t.Fatal("oversized persisted output accepted")
	}
}

func TestAsyncRunOwner_SameRootLeaseRefusesThenReopens(t *testing.T) {
	store := &asyncTestStore{}
	statePath := filepath.Join(t.TempDir(), "oneapi-run", "runs-v1.json")
	var launches atomic.Int32
	options := asyncOwnerOptions{StatePath: statePath, Execute: func(context.Context, oneAPIRunRequest) asyncExecution { launches.Add(1); return asyncExecution{} }, Read: store.Read, Write: store.Write}
	first, err := newAsyncRunOwner(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newAsyncRunOwner(t.Context(), options); err == nil || !strings.Contains(err.Error(), "oneapi_run_owner_busy") {
		t.Fatalf("second owner error=%v, want owner busy", err)
	}
	if launches.Load() != 0 {
		t.Fatalf("competing constructor executed driver %d times", launches.Load())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := newAsyncRunOwner(t.Context(), options)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncRunOwner_ConstructorErrorReleasesLease(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "oneapi-run", "runs-v1.json")
	readErr := errors.New("injected state read failure")
	bad := asyncOwnerOptions{StatePath: statePath, Execute: func(context.Context, oneAPIRunRequest) asyncExecution { return asyncExecution{} }, Read: func(string) ([]byte, error) { return nil, readErr }, Write: (&asyncTestStore{}).Write}
	if _, err := newAsyncRunOwner(t.Context(), bad); !errors.Is(err, readErr) {
		t.Fatalf("constructor error=%v, want injected read failure", err)
	}
	goodStore := &asyncTestStore{}
	good, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{StatePath: statePath, Execute: func(context.Context, oneAPIRunRequest) asyncExecution { return asyncExecution{} }, Read: goodStore.Read, Write: goodStore.Write})
	if err != nil {
		t.Fatalf("lease remained held after constructor error: %v", err)
	}
	if err := good.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncRunOwner_ConstructorJoinsLoadAndUnlockFailures(t *testing.T) {
	loadErr := errors.New("injected load failure")
	unlockErr := errors.New("injected unlock failure")
	lease := &asyncTestLease{unlockErrs: []error{unlockErr}}
	_, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: filepath.Join(t.TempDir(), "oneapi-run", "runs-v1.json"),
		Execute:   func(context.Context, oneAPIRunRequest) asyncExecution { return asyncExecution{} },
		Read:      func(string) ([]byte, error) { return nil, loadErr },
		Write:     (&asyncTestStore{}).Write,
		NewLease:  func(string) asyncRunLease { return lease },
	})
	if !errors.Is(err, loadErr) || !errors.Is(err, unlockErr) {
		t.Fatalf("constructor error=%v, want joined load and unlock failures", err)
	}
	if lease.unlockCalls != 1 {
		t.Fatalf("constructor unlock calls=%d, want one", lease.unlockCalls)
	}
}

func TestAsyncRun_PersistsDigestWithoutExecutionInputAndReplaysAfterReload(t *testing.T) {
	store := &asyncTestStore{}
	statePath := filepath.Join(t.TempDir(), "oneapi-run", "runs-v1.json")
	request := oneAPIRunRequest{
		Command:        "private-command-canary",
		Args:           []string{"private-arg-canary"},
		Cwd:            "private-cwd-canary",
		Timeout:        time.Second,
		IdempotencyKey: "digest-replay",
	}
	var firstLaunches atomic.Int32
	first, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: statePath,
		Execute: func(context.Context, oneAPIRunRequest) asyncExecution {
			firstLaunches.Add(1)
			return asyncExecution{Result: runResult{ExitCode: 0, Stdout: "terminal"}}
		},
		Read: store.Read, Write: store.Write, NextID: func() string { return "run-digest" },
	})
	if err != nil {
		t.Fatal(err)
	}
	start, err := first.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = waitForAsyncTerminal(t, first, start.RunID)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := store.Read(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-command-canary", "private-arg-canary", "private-cwd-canary"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("persisted snapshot leaked runtime input %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(string(raw), `"request_digest"`) {
		t.Fatalf("persisted snapshot lacks request digest: %s", raw)
	}
	if firstLaunches.Load() != 1 {
		t.Fatalf("initial launches=%d, want one", firstLaunches.Load())
	}
	var replayLaunches atomic.Int32
	second, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: statePath,
		Execute: func(context.Context, oneAPIRunRequest) asyncExecution {
			replayLaunches.Add(1)
			return asyncExecution{}
		},
		Read: store.Read, Write: store.Write, NextID: func() string { return "unexpected" },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	replay, err := second.Start(request)
	if err != nil {
		t.Fatalf("replay after reload: %v", err)
	}
	if !replay.Replayed || replay.RunID != start.RunID || replayLaunches.Load() != 0 {
		t.Fatalf("replay=%#v launches=%d, want persisted receipt and no execution", replay, replayLaunches.Load())
	}
}

// TestAsyncRun_PersistsAndReloadsCappedNonUTF8Output catches a persistence
// path that lets JSON replace arbitrary captured bytes instead of retaining
// their exact byte representation in the durable receipt.
func TestAsyncRun_PersistsAndReloadsCappedNonUTF8Output(t *testing.T) {
	var captured cappedBuffer
	captured.limit = maxStreamBytes
	input := append(bytes.Repeat([]byte{0xff}, maxStreamBytes-1), 0xe2, 0x82, 0xac)
	if written, err := captured.Write(input); err != nil || written != len(input) {
		t.Fatalf("capped capture = (%d, %v), want full consumption", written, err)
	}
	wantOutput := captured.String()
	if !captured.truncated || len(wantOutput) != maxStreamBytes+len(truncationMarker) || !bytes.HasSuffix([]byte(wantOutput), []byte(truncationMarker)) {
		t.Fatalf("capped output lost byte-count/truncation contract: truncated=%v len=%d", captured.truncated, len(wantOutput))
	}

	statePath := filepath.Join(t.TempDir(), "runs-v1.json")
	first, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: statePath,
		Execute: func(context.Context, oneAPIRunRequest) asyncExecution {
			return asyncExecution{Result: runResult{ExitCode: 0, Stdout: wantOutput, Stderr: wantOutput}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	start, err := first.Start(asyncRequest("non-utf8-persist", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	initial := waitForAsyncTerminal(t, first, start.RunID)
	if initial.Result == nil || !bytes.Equal([]byte(initial.Result.Stdout), []byte(wantOutput)) || !bytes.Equal([]byte(initial.Result.Stderr), []byte(wantOutput)) {
		t.Fatalf("initial persisted result did not retain raw captured bytes: %#v", initial.Result)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first owner close: %v", err)
	}
	persistedRaw, err := os.ReadFile(statePath)
	if err != nil || int64(len(persistedRaw)) > maxAsyncPersistedStateBytes {
		t.Fatalf("checked persisted receipt = (%d bytes, %v), want bounded same-path state", len(persistedRaw), err)
	}
	var persisted asyncPersistedRunSnapshot
	if err := json.Unmarshal(persistedRaw, &persisted); err != nil || len(persisted.Runs) != 1 || persisted.Runs[0].Result == nil {
		t.Fatalf("canonical persisted receipt decode = (%#v, %v)", persisted, err)
	}
	if persisted.Runs[0].Result.StdoutBase64 == "" || persisted.Runs[0].Result.StderrBase64 == "" || persisted.Runs[0].Result.Stdout != "" || persisted.Runs[0].Result.Stderr != "" || persisted.Runs[0].Result.StdoutLegacyExpanded || persisted.Runs[0].Result.StderrLegacyExpanded {
		t.Fatalf("new capture persisted non-canonical result: %#v", persisted.Runs[0].Result)
	}

	reloaded, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: statePath,
		Execute:   func(context.Context, oneAPIRunRequest) asyncExecution { return asyncExecution{} },
	})
	if err != nil {
		t.Fatalf("fresh owner must reload capped non-UTF-8 receipt: %v", err)
	}
	t.Cleanup(func() { _ = reloaded.Close() })
	result, err := reloaded.Result(start.RunID)
	if err != nil || result.Result == nil || !bytes.Equal([]byte(result.Result.Stdout), []byte(wantOutput)) || !bytes.Equal([]byte(result.Result.Stderr), []byte(wantOutput)) {
		t.Fatalf("reloaded result = (%#v, %v), want exact raw captured bytes", result, err)
	}
}

// TestAsyncRun_LoadsLegacyExpandedTextIntoSamePathBase64 proves the reported
// expanded legacy receipt can be recovered without executing a command, then
// rewritten at the same state path with explicit migration-only provenance.
func TestAsyncRun_LoadsLegacyExpandedTextIntoSamePathBase64(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "runs-v1.json")
	request := asyncRequest("legacy-text", time.Second)
	legacyExpanded := strings.Repeat("\uFFFD", maxStreamBytes) + truncationMarker
	legacy := asyncRunSnapshot{Version: asyncRunSnapshotVersion, TerminalSequence: 1, Runs: []asyncRunRecord{{
		RunID: "legacy-run", IdempotencyKey: request.IdempotencyKey, RequestDigest: mustAsyncRequestDigest(t, request),
		State: asyncStateCompleted, AcceptedAt: time.Unix(100, 0).UTC(), FinishedAt: timePtr(time.Unix(101, 0).UTC()), FinishSeq: 1,
		Result: &runResult{ExitCode: 0, Stdout: legacyExpanded, Stderr: legacyExpanded},
	}}}
	legacyRaw, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyExpanded) <= maxStreamBytes+len(truncationMarker) || len(legacyExpanded) > maxLegacyExpandedStreamBytes || len(legacyRaw) <= 1<<20 {
		t.Fatalf("legacy fixture does not exercise expanded same-path recovery: stream=%d file=%d", len(legacyExpanded), len(legacyRaw))
	}
	if err := api.WriteStateFileBytesAtomic(statePath, legacyRaw); err != nil {
		t.Fatal(err)
	}
	var launches atomic.Int32
	owner, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: statePath,
		Execute: func(context.Context, oneAPIRunRequest) asyncExecution {
			launches.Add(1)
			return asyncExecution{}
		},
	})
	if err != nil {
		t.Fatalf("load legacy valid receipt: %v", err)
	}
	if launches.Load() != 0 {
		t.Fatalf("legacy recovery launched %d commands, want zero", launches.Load())
	}
	result, err := owner.Result("legacy-run")
	if err != nil || result.Result == nil || result.Result.Stdout != legacyExpanded || result.Result.Stderr != legacyExpanded {
		t.Fatalf("legacy reloaded result = (%#v, %v)", result, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	canonicalRaw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var canonical asyncPersistedRunSnapshot
	if err := json.Unmarshal(canonicalRaw, &canonical); err != nil || len(canonical.Runs) != 1 || canonical.Runs[0].Result == nil {
		t.Fatalf("same-path canonical receipt = (%#v, %v)", canonical, err)
	}
	if canonical.Runs[0].Result.StdoutBase64 == "" || canonical.Runs[0].Result.StderrBase64 == "" || !canonical.Runs[0].Result.StdoutLegacyExpanded || !canonical.Runs[0].Result.StderrLegacyExpanded {
		t.Fatalf("legacy migration did not preserve expanded provenance: %#v", canonical.Runs[0].Result)
	}
	second, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{StatePath: statePath, Execute: func(context.Context, oneAPIRunRequest) asyncExecution { launches.Add(1); return asyncExecution{} }})
	if err != nil {
		t.Fatalf("canonical reload: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	secondResult, err := second.Result("legacy-run")
	if err != nil || secondResult.Result == nil || secondResult.Result.Stdout != legacyExpanded || secondResult.Result.Stderr != legacyExpanded || launches.Load() != 0 {
		t.Fatalf("canonical reload result=(%#v,%v) launches=%d", secondResult, err, launches.Load())
	}

}

func TestAsyncPersistedOutputLegacyExpansionPolarity(t *testing.T) {
	normalLimit := maxStreamBytes + len(truncationMarker)
	cases := []struct {
		name     string
		output   string
		expanded bool
	}{
		{name: "absent-provenance-above-normal", output: strings.Repeat("x", normalLimit+1)},
		{name: "redundant-provenance-at-normal", output: strings.Repeat("x", normalLimit), expanded: true},
		{name: "expanded-provenance-above-migration-ceiling", output: strings.Repeat("x", maxLegacyExpandedStreamBytes+1), expanded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record, _, err := asyncRecordFromPersisted(asyncPersistedRunRecord{RunID: "run", IdempotencyKey: "key", RequestDigest: mustAsyncRequestDigest(t, asyncRequest("key", time.Second)), State: asyncStateCompleted, AcceptedAt: time.Unix(1, 0).UTC(), FinishedAt: timePtr(time.Unix(2, 0).UTC()), FinishSeq: 1, Result: &asyncPersistedRunResult{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(tc.output)), StdoutLegacyExpanded: tc.expanded}})
			if err != nil {
				t.Fatalf("decode error = %v, want validation error", err)
			}
			if err := validateAsyncRecord(record); !errors.Is(err, errAsyncInvalidRequest) {
				t.Fatalf("validate error = %v, want errAsyncInvalidRequest", err)
			}
		})
	}
}

func TestAsyncPersistedOutputRejectsDirectTextWithExpandedFlag(t *testing.T) {
	_, _, err := asyncRecordFromPersisted(asyncPersistedRunRecord{RunID: "run", IdempotencyKey: "key", RequestDigest: mustAsyncRequestDigest(t, asyncRequest("key", time.Second)), State: asyncStateCompleted, AcceptedAt: time.Unix(1, 0).UTC(), FinishedAt: timePtr(time.Unix(2, 0).UTC()), FinishSeq: 1, Result: &asyncPersistedRunResult{Stdout: "legacy text", StdoutLegacyExpanded: true}})
	if !errors.Is(err, errAsyncInvalidRequest) {
		t.Fatalf("direct text with expanded flag error = %v, want errAsyncInvalidRequest", err)
	}
}

func TestAsyncPersistedSnapshotOversizeDoesNotMutateRecordsOrPriorBytes(t *testing.T) {
	oversized := strings.Repeat("x", int(maxAsyncPersistedStateBytes)+1)
	records := map[string]*asyncRunRecord{"run": {RunID: "run", IdempotencyKey: "key", RequestDigest: mustAsyncRequestDigest(t, asyncRequest("key", time.Second)), State: asyncStateCompleted, AcceptedAt: time.Unix(1, 0).UTC(), FinishedAt: timePtr(time.Unix(2, 0).UTC()), FinishSeq: 1, Result: &runResult{EnvSource: oversized}}}
	before := cloneAsyncRunMap(records)
	prior := []byte("last durable receipt")
	if _, err := marshalAsyncPersistedSnapshot(records, 1, maxAsyncPersistedStateBytes); !errors.Is(err, errAsyncPersist) {
		t.Fatalf("oversize marshal error = %v, want errAsyncPersist", err)
	}
	if !bytes.Equal(prior, []byte("last durable receipt")) || records["run"].Result.EnvSource != before["run"].Result.EnvSource || len(records) != len(before) {
		t.Fatal("oversize check mutated candidate records or prior durable bytes")
	}
}

func TestAsyncPersistedSnapshotRetains64MaximumOutputTerminalsAndActive(t *testing.T) {
	output := strings.Repeat("x", maxStreamBytes)
	records := map[string]*asyncRunRecord{
		"active": {RunID: "active", IdempotencyKey: "active", RequestDigest: mustAsyncRequestDigest(t, asyncRequest("active", time.Second)), State: asyncStateRunning, AcceptedAt: time.Unix(100, 0).UTC()},
	}
	for i := 1; i <= maxAsyncTerminalRuns; i++ {
		id := "terminal-" + time.Unix(0, int64(i)).UTC().Format("150405.000000000")
		records[id] = &asyncRunRecord{
			RunID: id, IdempotencyKey: id, RequestDigest: mustAsyncRequestDigest(t, asyncRequest(id, time.Second)), State: asyncStateCompleted,
			AcceptedAt: time.Unix(int64(i), 0).UTC(), FinishedAt: timePtr(time.Unix(int64(i+1), 0).UTC()), FinishSeq: uint64(i),
			Result: &runResult{ExitCode: 0, Stdout: output, Stderr: output},
		}
	}
	if raw, err := marshalAsyncPersistedSnapshot(records, uint64(maxAsyncTerminalRuns), maxAsyncPersistedStateBytes); err != nil || int64(len(raw)) > maxAsyncPersistedStateBytes {
		t.Fatalf("64-terminal maximum-output snapshot = (%d bytes, %v)", len(raw), err)
	}
	active := records["active"]
	active.State, active.FinishedAt, active.FinishSeq = asyncStateCompleted, timePtr(time.Unix(200, 0).UTC()), maxAsyncTerminalRuns+1
	active.Result = &runResult{ExitCode: 0, Stdout: output, Stderr: output}
	evictAsyncTerminals(records)
	if len(records) != maxAsyncTerminalRuns || records["active"] == nil {
		t.Fatalf("count retention after active terminal = %d active=%v, want %d retained including active", len(records), records["active"] != nil, maxAsyncTerminalRuns)
	}
	if _, err := marshalAsyncPersistedSnapshot(records, active.FinishSeq, maxAsyncPersistedStateBytes); err != nil {
		t.Fatalf("post-count-eviction maximum-output snapshot: %v", err)
	}
}

func TestAsyncRun_CloseConcurrentCallersWaitForDeferredWorkerAndRetryLeaseUnlock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	lease := &asyncTestLease{unlockErrs: []error{errors.New("first unlock fails"), nil}}
	owner, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: filepath.Join(t.TempDir(), "oneapi-run", "runs-v1.json"),
		Execute: func(context.Context, oneAPIRunRequest) asyncExecution {
			return asyncExecution{Result: runResult{ExitCode: 0}}
		},
		NewLease: func(string) asyncRunLease { return lease },
		BeforeWorkerDone: func() {
			close(entered)
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	start, err := owner.Start(asyncRequest("close-window", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- owner.Close() }()
	go func() { secondDone <- owner.Close() }()
	select {
	case err := <-firstDone:
		t.Fatalf("first Close returned before deferred worker completion: %v", err)
	case err := <-secondDone:
		t.Fatalf("second Close returned before deferred worker completion: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err == nil {
		t.Fatal("first Close hid lease unlock failure")
	}
	if err := <-secondDone; err == nil {
		t.Fatal("concurrent Close hid lease unlock failure")
	}
	if lease.unlockCalls != 1 {
		t.Fatalf("concurrent Close unlock calls=%d, want one", lease.unlockCalls)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("retry Close did not retry lease unlock: %v", err)
	}
	if lease.unlockCalls != 2 {
		t.Fatalf("retry Close unlock calls=%d, want two", lease.unlockCalls)
	}
	if _, err := owner.Result(start.RunID); err != nil {
		t.Fatalf("terminal result lost after Close: %v", err)
	}
}

func TestAsyncRun_CloseWaitsForEvictedTerminalWorker(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var blockFirst atomic.Bool
	lease := &asyncTestLease{}
	owner, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: filepath.Join(t.TempDir(), "oneapi-run", "runs-v1.json"),
		Execute: func(context.Context, oneAPIRunRequest) asyncExecution {
			return asyncExecution{Result: runResult{ExitCode: 0}}
		},
		NewLease: func(string) asyncRunLease { return lease },
		BeforeWorkerDone: func() {
			if blockFirst.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldest, err := owner.Start(asyncRequest("evicted-worker-0", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	for i := 1; i <= maxAsyncTerminalRuns; i++ {
		key := "evicted-worker-" + time.Unix(0, int64(i)).UTC().Format("150405.000000000")
		start, err := owner.Start(asyncRequest(key, time.Second))
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		_ = waitForAsyncTerminal(t, owner, start.RunID)
	}
	if _, err := owner.Status(oldest.RunID); !errors.Is(err, errAsyncNotFound) {
		t.Fatalf("oldest retained after eviction: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before evicted worker completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if lease.unlockCalls != 0 {
		t.Fatalf("Close unlocked before evicted worker completion: calls=%d", lease.unlockCalls)
	}
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if lease.unlockCalls != 1 {
		t.Fatalf("Close unlock calls=%d, want one", lease.unlockCalls)
	}
}

type asyncTestLease struct {
	unlockErrs  []error
	unlockCalls int
}

func (*asyncTestLease) TryLock() (bool, error) { return true, nil }

func (l *asyncTestLease) Unlock() error {
	l.unlockCalls++
	if len(l.unlockErrs) == 0 {
		return nil
	}
	err := l.unlockErrs[0]
	l.unlockErrs = l.unlockErrs[1:]
	return err
}

func timePtr(value time.Time) *time.Time { return &value }

func mustAsyncRequestDigest(t *testing.T, request oneAPIRunRequest) string {
	t.Helper()
	digest, err := asyncRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
