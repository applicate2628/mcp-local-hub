//go:build windows

package main

import (
	"bytes"
	"errors"
	"io"
	"mcp-local-hub/internal/process"
	"strings"
	"testing"
)

type failingDiagnosticWriter struct {
	calls int
	err   error
	short bool
}

func (w *failingDiagnosticWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.short {
		return len(p) - 1, w.err
	}
	return 0, w.err
}

func TestForegroundReceivingPreservesRootOutcomeAndSinkFailure(t *testing.T) {
	execution := errors.New("original execution cause")
	policy := errors.New("original member policy cause")
	sink := errors.New("diagnostic write cause")
	result := process.ForegroundResult{Root: process.OriginalRootWait{Known: true, ExitCode: 37}, Outcome: process.ForegroundOutcome{OriginalAcquired: true, MemberObservationPresent: true, MemberPolicyError: policy,
		Members: process.CreatorJobObservation{CapturedMemberPIDs: []uint32{21, 22}, Members: []process.CreatorMemberObservation{{PID: 21, HandleClosed: true}, {PID: 22, ControlledFailure: "member-handle-close-failed"}}}, Pending: []string{"member-handle[pid=22]"}}}
	var text bytes.Buffer
	received := receiveForeground(result, execution, &text)
	for _, want := range []string{"mcphub launcher: original execution cause", "root-exit=37", "original member policy cause", "pid=21 membership-known=false wait-known=false", "handle-closed=true", "pid=22 membership-known=false wait-known=false", "member-handle-close-failed", "original settlement unproved", "no recovery handoff"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("missing %q in %s", want, text.String())
		}
	}
	if received.ExitCode != 37 || received.DiagnosticError != nil || !errors.Is(received.ExecutionError, execution) || !errors.Is(received.Result.Outcome.MemberPolicyError, policy) || received.Result.Settlement.Complete() {
		t.Fatal("receiving lost original result or invented settlement")
	}
	for _, short := range []bool{false, true} {
		writer := &failingDiagnosticWriter{err: sink, short: short}
		got := receiveForeground(result, execution, writer)
		if got.ExitCode != 37 || writer.calls != 1 || !errors.Is(got.ExecutionError, execution) || !errors.Is(got.DiagnosticError, sink) || got.Result.Outcome.Members.Members[1].HandleClosed {
			t.Fatal("sink failure replaced outcome or retried")
		}
		if short && !errors.Is(got.DiagnosticError, io.ErrShortWrite) {
			t.Fatal("short write lost")
		}
	}
	short := &failingDiagnosticWriter{short: true}
	if got := receiveForeground(result, execution, short); !errors.Is(got.DiagnosticError, io.ErrShortWrite) || short.calls != 1 {
		t.Fatal("nil-error short write lost")
	}
}

func TestForegroundReceivingUnknownAndCancellationStayFailures(t *testing.T) {
	for _, result := range []process.ForegroundResult{{}, {Root: process.OriginalRootWait{Known: true}}, {Root: process.OriginalRootWait{Known: true}, Cancelled: true}} {
		var text bytes.Buffer
		got := receiveForeground(result, nil, &text)
		if got.ExitCode != 1 || text.Len() == 0 || got.Result.Settlement.Complete() {
			t.Fatal("unknown/incomplete/cancelled outcome became clean")
		}
		missing := receiveForeground(result, nil, nil)
		if missing.DiagnosticError == nil || missing.ExitCode != 1 {
			t.Fatal("absent stderr delivery claimed")
		}
	}
}
