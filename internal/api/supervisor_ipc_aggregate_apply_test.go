package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestAggregateApplyExactWireAndReadOnlyCompatibility(t *testing.T) {
	req := IPCRequest{Version: 1, ID: 1<<63 - 1, Cmd: SupervisorAggregateApplyCommand,
		Args: map[string]any{"receipt_sha256": strings.Repeat("a", 64), "endpoint_sha256": strings.Repeat("b", 64)}}
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	input := bytes.NewBuffer(append(wire, '\n', 'x'))
	got, err := ReadOperatorAggregateStatusOpen(input)
	if err != nil || got.Cmd != req.Cmd || input.String() != "x" {
		t.Fatalf("typed apply request not received as one frame: %v", err)
	}
	selection, err := DecodeRouteAggregateApply(got)
	if err != nil || !selection.Valid() {
		t.Fatalf("apply selection invalid: %v", err)
	}

	req.ID = 2
	wire, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := "{\"id\":0," + string(wire[1:]) + "\n"
	if _, err := ReadOperatorAggregateStatusOpen(strings.NewReader(duplicate)); err == nil {
		t.Fatal("duplicate apply field accepted")
	}
	req.Cmd = SupervisorAggregateVerifyCommand
	req.Args = map[string]any{"generation": "42", "challenge": strings.Repeat("c", 32)}
	wire, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	verify, err := ReadOperatorAggregateStatusOpen(bytes.NewReader(append(wire, '\n')))
	if err != nil {
		t.Fatal(err)
	}
	generation, nonce, err := DecodeRouteAggregateVerification(verify)
	if err != nil || generation != 42 || nonce != req.Args["challenge"] {
		t.Fatalf("original-generation verification envelope differs: %v", err)
	}
	verify.Args["generation"] = "042"
	if _, _, err := DecodeRouteAggregateVerification(verify); err == nil {
		t.Fatal("noncanonical generation accepted")
	}
	old := RouteAggregateStatus{Ownership: RouteAggregateNotOwner, Generation: 3}
	ready := RouteAggregateStatus{Ownership: RouteAggregateOwned, Generation: 3, Revision: 1, Effective: "ready"}
	if !old.Valid() || !ready.Valid() {
		t.Fatal("prior non-owner and typed ready must both be valid")
	}
	ready.Revision = 0
	if ready.Valid() {
		t.Fatal("ready without original role revision accepted")
	}
}

type routeAggregateFixtureCloseReceipt struct {
	observed bool
	err      error
	reads    int
}

func (r *routeAggregateFixtureCloseReceipt) NativeResult() (error, bool) {
	r.reads++
	return r.err, r.observed
}

func TestRouteAggregateLaterStopReceivesOriginalReader(t *testing.T) {
	for _, tc := range []struct {
		name           string
		nativeObserved bool
		nativeError    error
	}{
		{name: "known native close", nativeObserved: true},
		{name: "failed native close", nativeObserved: true, nativeError: errors.New("fixture native close failure")},
		{name: "unknown native close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalReaderDone := make(chan struct{})
			originalReceipt := &routeAggregateFixtureCloseReceipt{observed: tc.nativeObserved, err: tc.nativeError}
			out := RouteAggregateControlExit{IPCError: io.EOF}
			calls := 0
			firstRetireError := errors.New("fixture original listener pending")
			first, finishFirst := context.WithDeadline(t.Context(), time.Time{})
			defer finishFirst()
			finishRouteAggregateAttachment(first, &out, true, originalReceipt, nil, originalReaderDone, func(context.Context) RouteAggregateRetirement {
				calls++
				if calls == 1 {
					return RouteAggregateRetirement{Pending: "fixture original listener", Err: firstRetireError}
				}
				return RouteAggregateRetirement{}
			})
			firstRevoke := out.RevokeError
			if !errors.Is(firstRevoke, context.DeadlineExceeded) || out.Retirement.Settled() || calls != 0 {
				t.Fatalf("first detach did not retain the outstanding reader: %+v", out)
			}
			stopped, cancelStop := context.WithCancel(t.Context())
			cancelStop()
			if err := out.AdvanceOriginalRetirement(stopped); !errors.Is(err, context.Canceled) ||
				out.Retirement.Settled() || calls != 0 || out.RevokeError != firstRevoke {
				t.Fatalf("bounded late stop changed original custody: err=%v exit=%+v", err, out)
			}
			close(originalReaderDone)
			if err := out.AdvanceOriginalRetirement(stopped); tc.nativeObserved && tc.nativeError == nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled stop advanced the original listener: %v", err)
			}
			if calls != 0 || !out.attachmentClose.readerJoined || out.attachmentClose.readerDone != originalReaderDone {
				t.Fatalf("failed to receive the exact reader without retirement: calls=%d exit=%+v", calls, out)
			}
			if err := out.AdvanceOriginalRetirement(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tc.nativeObserved && tc.nativeError == nil {
				if calls != 1 || out.Retirement.Settled() || !errors.Is(out.Retirement.Err, firstRetireError) {
					t.Fatalf("actual reader did not advance the original listener: calls=%d exit=%+v", calls, out)
				}
				if err := out.AdvanceOriginalRetirement(t.Context()); err != nil || calls != 2 ||
					!out.Retirement.Settled() || !errors.Is(out.Retirement.Err, firstRetireError) {
					t.Fatalf("subsequent original listener retirement lost history: err=%v calls=%d exit=%+v", err, calls, out)
				}
				if err := out.AdvanceOriginalRetirement(t.Context()); err != nil || calls != 2 {
					t.Fatalf("settled original listener repeated retirement: err=%v calls=%d", err, calls)
				}
			} else if calls != 0 || out.Retirement.Settled() {
				t.Fatalf("late reader granted authority with unresolved native close: calls=%d exit=%+v", calls, out)
			}
			if out.IPCError != io.EOF || out.RevokeError != firstRevoke ||
				!errors.Is(out.attachmentClose.readerError, context.DeadlineExceeded) ||
				out.attachmentClose.receipt != originalReceipt || originalReceipt.reads != 1 {
				t.Fatalf("first attachment outcomes changed after reader completion: %+v", out)
			}
		})
	}
}

func TestRouteAggregateAttachmentRequiresObservedCloseAndJoinedReader(t *testing.T) {
	nativeFailure := errors.New("fixture native close")
	retirementFailure := errors.New("fixture retirement")
	nativeSuccess := &routeAggregateFixtureCloseReceipt{observed: true}
	closeFailed := &routeAggregateFixtureCloseReceipt{observed: true, err: nativeFailure}
	closePending := &routeAggregateFixtureCloseReceipt{}
	for _, tc := range []struct {
		name        string
		receipt     *routeAggregateFixtureCloseReceipt
		observerErr error
		attached    bool
		readerDone  bool
		wantRetire  int
		wantErrors  []error
	}{
		{name: "native close and original reader joined", receipt: nativeSuccess, attached: true, readerDone: true, wantRetire: 1},
		{name: "native close failed", receipt: closeFailed, attached: true, readerDone: true, wantErrors: []error{nativeFailure}},
		{name: "native observer canceled", receipt: closePending, observerErr: context.Canceled, attached: true, readerDone: true,
			wantErrors: []error{context.Canceled, errRouteAggregateNativeCloseUnobserved}},
		{name: "legacy nil close supplies no native evidence", attached: true, readerDone: true,
			wantErrors: []error{errRouteAggregateNativeCloseUnobserved}},
		{name: "original reader still pending", receipt: nativeSuccess, attached: true, wantErrors: []error{context.Canceled}},
		{name: "unattached connection has no retirement", receipt: nativeSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var readerDone chan struct{}
			if tc.attached {
				readerDone = make(chan struct{})
				if tc.readerDone {
					close(readerDone)
				} else {
					cancel()
				}
			}
			var receipt routeAggregateAttachmentCloseReceipt
			if tc.receipt != nil {
				receipt = tc.receipt
			}
			out := RouteAggregateControlExit{IPCError: io.EOF}
			retired := 0
			finishRouteAggregateAttachment(ctx, &out, tc.attached, receipt, tc.observerErr, readerDone, func(context.Context) RouteAggregateRetirement {
				retired++
				return RouteAggregateRetirement{Err: retirementFailure}
			})
			if retired != tc.wantRetire || out.IPCError != io.EOF || !errors.Is(out.Failure(), io.EOF) {
				t.Fatalf("retirement or primary error changed: retired=%d, exit=%+v", retired, out)
			}
			wantPending := len(tc.wantErrors) != 0
			if out.Retirement.Settled() == wantPending || (out.RevokeError != nil) != wantPending {
				t.Fatalf("invalid transport settlement: %+v", out)
			}
			for _, wantErr := range tc.wantErrors {
				if !errors.Is(out.RevokeError, wantErr) {
					t.Errorf("missing original transport error %v in %v", wantErr, out.RevokeError)
				}
			}
			if (tc.wantRetire != 0) != errors.Is(out.Failure(), retirementFailure) {
				t.Errorf("original retirement outcome changed: %v", out.Failure())
			}
			wantNative := tc.receipt != nil && tc.receipt.observed
			got := out.attachmentClose
			if got.receipt != receipt || got.observerError != tc.observerErr || got.nativeObserved != wantNative ||
				got.readerJoined != (!tc.attached || tc.readerDone) {
				t.Errorf("original attachment observations changed: %+v", got)
			}
		})
	}
}
