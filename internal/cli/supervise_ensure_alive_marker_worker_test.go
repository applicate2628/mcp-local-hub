package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

func TestGUIOwnerUnknownConfirmationMarkerWorker_RealCurrentBinaryWrites(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	markerPath := filepath.Join(stateDir, guiOwnerUnknownConfirmationFileLeaf)
	if err := writeGUIOwnerUnknownConfirmationMarkerContained(markerPath, observedAt); err != nil {
		var workerErr *guiOwnerUnknownConfirmationWorkerError
		if errors.As(err, &workerErr) {
			t.Fatalf("contained marker write: failure=%s cause=%v stdout_bytes=%d stderr_bytes=%d", workerErr.failure, workerErr.cause, workerErr.stdout.Bytes, workerErr.stderr.Bytes)
		}
		t.Fatal(err)
	}
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != observedAt.Format(time.RFC3339Nano) {
		t.Fatalf("marker=%q want=%q", raw, observedAt.Format(time.RFC3339Nano))
	}
}

func TestGUIOwnerUnknownConfirmationMarkerWorker_Protocol(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	now := time.Now().UTC()
	tests := []struct {
		name    string
		request any
		want    guiOwnerUnknownConfirmationWorkerResult
	}{
		{name: "valid", request: guiOwnerUnknownConfirmationWorkerRequest{Version: 1, StateDir: stateDir, ObservedAt: now}, want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "written"}},
		{name: "valid reset", request: guiOwnerUnknownConfirmationWorkerRequest{Version: 1, Operation: guiOwnerUnknownConfirmationOperationReset, StateDir: stateDir, ObservedAt: now}, want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "reset"}},
		{name: "zero time", request: guiOwnerUnknownConfirmationWorkerRequest{Version: 1, StateDir: stateDir}, want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "rejected", Failure: "protocol_invalid"}},
		{name: "future time", request: guiOwnerUnknownConfirmationWorkerRequest{Version: 1, StateDir: stateDir, ObservedAt: now.Add(time.Hour)}, want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "rejected", Failure: "protocol_invalid"}},
		{name: "relative state", request: guiOwnerUnknownConfirmationWorkerRequest{Version: 1, StateDir: "relative", ObservedAt: now}, want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "rejected", Failure: "protocol_invalid"}},
		{name: "unknown version", request: guiOwnerUnknownConfirmationWorkerRequest{Version: 2, StateDir: stateDir, ObservedAt: now}, want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "rejected", Failure: "protocol_invalid"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := runGUIOwnerUnknownConfirmationMarkerWorker(bytes.NewReader(raw), &out); err != nil {
				t.Fatal(err)
			}
			var got guiOwnerUnknownConfirmationWorkerResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("result=%+v want=%+v", got, test.want)
			}
		})
	}
}

func TestGUIOwnerUnknownConfirmationMarkerWorker_ProtocolRejectsMalformedAndOversized(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	blockedDir := filepath.Join(stateDir, "not-a-directory")
	if err := os.WriteFile(blockedDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
		want guiOwnerUnknownConfirmationWorkerResult
	}{
		{name: "unknown field", raw: []byte(`{"version":1,"state_dir":` + mustJSONTestString(t, stateDir) + `,"observed_at":"` + now + `","extra":true}`), want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "rejected", Failure: "protocol_invalid"}},
		{name: "malformed", raw: []byte(`{"version":`), want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "rejected", Failure: "protocol_invalid"}},
		{name: "multiple values", raw: []byte(`{} {}`), want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "rejected", Failure: "protocol_invalid"}},
		{name: "oversized", raw: bytes.Repeat([]byte("x"), guiOwnerUnknownConfirmationWorkerMessageMax+1), want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "rejected", Failure: "protocol_invalid"}},
		{name: "secure write failure", raw: []byte(`{"version":1,"state_dir":` + mustJSONTestString(t, blockedDir) + `,"observed_at":"` + now + `"}`), want: guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "failed", Failure: "state_write_failed"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runGUIOwnerUnknownConfirmationMarkerWorker(bytes.NewReader(test.raw), &out); err != nil {
				t.Fatal(err)
			}
			var got guiOwnerUnknownConfirmationWorkerResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("result=%+v want=%+v", got, test.want)
			}
		})
	}
}

func mustJSONTestString(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestGUIOwnerUnknownConfirmationMarkerWorker_ResultAndRunFailureClassification(t *testing.T) {
	valid, err := json.Marshal(guiOwnerUnknownConfirmationWorkerResult{Version: 1, Outcome: "written"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		result  process.StrictRunResult
		want    guiOwnerUnknownConfirmationWorkerFailure
		wantNil bool
	}{
		{name: "valid", result: process.StrictRunResult{Stdout: process.StrictRunCapture{Prefix: valid, Bytes: len(valid)}}, wantNil: true},
		{name: "extra output", result: process.StrictRunResult{Stdout: process.StrictRunCapture{Prefix: append(append([]byte(nil), valid...), []byte(` {}`)...), Bytes: len(valid) + 3}}, want: guiOwnerUnknownConfirmationFailureProtocol},
		{name: "truncated output", result: process.StrictRunResult{Stdout: process.StrictRunCapture{Prefix: valid[:len(valid)-1], Bytes: len(valid) + 1, Truncated: true}}, want: guiOwnerUnknownConfirmationFailureProtocol},
		{name: "unknown result", result: process.StrictRunResult{Stdout: process.StrictRunCapture{Prefix: []byte(`{"version":1,"outcome":"other"}`), Bytes: 31}}, want: guiOwnerUnknownConfirmationFailureProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := interpretGUIOwnerUnknownConfirmationWorkerResult(test.result)
			if test.wantNil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var workerErr *guiOwnerUnknownConfirmationWorkerError
			if !errors.As(err, &workerErr) || workerErr.failure != test.want || workerErr.stdout.Bytes != test.result.Stdout.Bytes {
				t.Fatalf("error=%v typed=%+v", err, workerErr)
			}
		})
	}

	runResult := process.StrictRunResult{Stdout: process.StrictRunCapture{Bytes: 7}, Stderr: process.StrictRunCapture{Bytes: 20 * 1024, Truncated: true}}
	for _, test := range []struct {
		name string
		err  error
		want guiOwnerUnknownConfirmationWorkerFailure
	}{
		{name: "timeout", err: &process.StrictRunError{Kind: process.StrictRunTimeout, Cause: errors.New("timeout")}, want: guiOwnerUnknownConfirmationFailureTimeout},
		{name: "containment", err: &process.StrictRunError{Kind: process.StrictRunContainmentFailed, Cause: errors.New("containment")}, want: guiOwnerUnknownConfirmationFailureContainment},
		{name: "invalid invocation", err: &process.StrictRunError{Kind: process.StrictRunInvalidInvocation, Cause: errors.New("invalid")}, want: guiOwnerUnknownConfirmationFailureInvalidInvocation},
		{name: "nonzero execution", err: &process.StrictRunError{Kind: process.StrictRunExecutionFailed, Cause: errors.New("exit 9")}, want: guiOwnerUnknownConfirmationFailureExecution},
	} {
		t.Run(test.name, func(t *testing.T) {
			classified := newGUIOwnerUnknownConfirmationWorkerRunError(nil, runResult, test.err)
			var workerErr *guiOwnerUnknownConfirmationWorkerError
			if !errors.As(classified, &workerErr) || workerErr.failure != test.want || workerErr.stderr.Bytes != 20*1024 || !workerErr.stderr.Truncated || strings.Contains(workerErr.Error(), "exit 9") {
				t.Fatalf("classified=%v typed=%+v", classified, workerErr)
			}
		})
	}
}

func TestGUIOwnerUnknownConfirmationMarkerWorker_HeldLockTimesOutAndReaps(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	markerPath := filepath.Join(stateDir, guiOwnerUnknownConfirmationFileLeaf)
	holder := flock.New(markerPath + ".lock")
	if err := holder.Lock(); err != nil {
		t.Fatal(err)
	}
	holderReleased := false
	t.Cleanup(func() {
		if !holderReleased {
			_ = holder.Unlock()
		}
	})

	started := time.Now()
	err := writeGUIOwnerUnknownConfirmationMarkerContained(markerPath, time.Now().UTC())
	elapsed := time.Since(started)
	var workerErr *guiOwnerUnknownConfirmationWorkerError
	if !errors.As(err, &workerErr) || workerErr.failure != guiOwnerUnknownConfirmationFailureTimeout {
		t.Fatalf("held-lock error=%v want typed timeout", err)
	}
	if elapsed < guiOwnerUnknownConfirmationWriteBudget-500*time.Millisecond || elapsed > guiOwnerUnknownConfirmationWriteBudget+3*time.Second {
		t.Fatalf("held-lock elapsed=%s want bounded around %s", elapsed, guiOwnerUnknownConfirmationWriteBudget)
	}
	if workerErr.pid <= 0 || process.IsPidAlive(workerErr.pid) {
		t.Fatalf("worker pid=%d still alive after timeout return", workerErr.pid)
	}
	if err := holder.Unlock(); err != nil {
		t.Fatal(err)
	}
	holderReleased = true
	probe := flock.New(markerPath + ".lock")
	locked, err := probe.TryLock()
	if err != nil || !locked {
		t.Fatalf("fresh lock after timeout locked=%v err=%v", locked, err)
	}
	if err := probe.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestGUIOwnerUnknownConfirmationMarkerResetSerializesAfterPublishedWrite(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	markerPath := filepath.Join(stateDir, guiOwnerUnknownConfirmationFileLeaf)
	holder := flock.New(markerPath + ".lock")
	if err := holder.Lock(); err != nil {
		t.Fatal(err)
	}
	holderReleased := false
	t.Cleanup(func() {
		if !holderReleased {
			_ = holder.Unlock()
		}
	})

	started := make(chan struct{})
	resetDone := make(chan struct {
		outcome string
		failure string
	}, 1)
	go func() {
		close(started)
		outcome, failure := resetGUIOwnerUnknownConfirmationMarkerLockHeld(markerPath, time.Now().UTC())
		resetDone <- struct {
			outcome string
			failure string
		}{outcome: outcome, failure: failure}
	}()
	<-started
	select {
	case result := <-resetDone:
		t.Fatalf("reset bypassed held marker flock: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}

	if err := api.WriteStateFileBytesLockHeld(markerPath, []byte(time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		t.Fatal(err)
	}
	if err := holder.Unlock(); err != nil {
		t.Fatal(err)
	}
	holderReleased = true
	select {
	case result := <-resetDone:
		if result.outcome != "reset" || result.failure != "" {
			t.Fatalf("reset result=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("reset did not finish after marker flock release")
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker survived serialized reset: %v", err)
	}
}

func TestGUIOwnerUnknownConfirmationMarkerWorker_HeldLockBoundsAllWriteSites(t *testing.T) {
	for _, test := range []struct {
		name      string
		prepare   func(t *testing.T, stateDir, markerPath string)
		invoke    func(stateDir, markerPath string, observedAt time.Time) error
		wantEvent string
	}{
		{
			name: "first arm",
			invoke: func(stateDir, _ string, observedAt time.Time) error {
				restore := setGUIOwnerLockUnheldProbeFnForTest(func() (bool, error) { return true, nil })
				defer restore()
				if runEnsureAliveGUIOwnerUnknownEscalationAt(stateDir, &bytes.Buffer{}, 4242, 0, 0, false, observedAt) {
					return errors.New("first arm escalated")
				}
				return nil
			},
			wantEvent: "gui-owner-unknown-confirmation-write-failed",
		},
		{
			name: "future repair",
			prepare: func(t *testing.T, _ string, markerPath string) {
				if err := writeGUIOwnerUnknownConfirmationMarkerDirectForTest(markerPath, time.Now().UTC().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			},
			invoke: func(stateDir, _ string, observedAt time.Time) error {
				restore := setGUIOwnerLockUnheldProbeFnForTest(func() (bool, error) { return true, nil })
				defer restore()
				if runEnsureAliveGUIOwnerUnknownEscalationAt(stateDir, &bytes.Buffer{}, 4242, 0, 0, false, observedAt) {
					return errors.New("future repair escalated")
				}
				return nil
			},
			wantEvent: "gui-owner-unknown-confirmation-write-failed",
		},
		{
			name: "reset transaction",
			prepare: func(t *testing.T, _ string, markerPath string) {
				if err := writeGUIOwnerUnknownConfirmationMarkerDirectForTest(markerPath, time.Now().UTC().Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			},
			invoke: func(_ string, markerPath string, observedAt time.Time) error {
				return defaultResetGUIOwnerUnknownConfirmationMarker(markerPath, observedAt)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := ensureAliveTestStateDir(t)
			markerPath := filepath.Join(stateDir, guiOwnerUnknownConfirmationFileLeaf)
			if test.prepare != nil {
				test.prepare(t, stateDir, markerPath)
			}
			holder := flock.New(markerPath + ".lock")
			if err := holder.Lock(); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			err := test.invoke(stateDir, markerPath, time.Now().UTC())
			elapsed := time.Since(start)
			if test.name == "reset transaction" {
				var workerErr *guiOwnerUnknownConfirmationWorkerError
				if !errors.As(err, &workerErr) || workerErr.failure != guiOwnerUnknownConfirmationFailureTimeout || workerErr.pid <= 0 || process.IsPidAlive(workerErr.pid) {
					t.Fatalf("reset error=%v typed=%+v", err, workerErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if elapsed < guiOwnerUnknownConfirmationWriteBudget-500*time.Millisecond || elapsed > guiOwnerUnknownConfirmationWriteBudget+3*time.Second {
				t.Fatalf("elapsed=%s want bounded around %s", elapsed, guiOwnerUnknownConfirmationWriteBudget)
			}
			if test.wantEvent != "" {
				assertSupervisorEventBody(t, stateDir, test.wantEvent, `"failure_id":"timeout"`)
			}
			if err := holder.Unlock(); err != nil {
				t.Fatal(err)
			}
			probe := flock.New(markerPath + ".lock")
			locked, err := probe.TryLock()
			if err != nil || !locked {
				t.Fatalf("post-timeout lock locked=%v err=%v", locked, err)
			}
			if err := probe.Unlock(); err != nil {
				t.Fatal(err)
			}
			if test.name == "reset transaction" {
				if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)); test.wantEvent != "" && err != nil {
				t.Fatal(err)
			}
		})
	}
}
