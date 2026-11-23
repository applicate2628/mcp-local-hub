package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

type routeAdmissionFixture struct {
	ctx          context.Context
	controller   *supervisorController
	owner        *routeAdmissionReservation
	before       routeAdmissionPreimage
	current      bool
	locks        []routeAdmissionLeaseRole
	releases     []routeAdmissionLeaseRole
	fault        func(routeAdmissionLeaseRole) (bool, error)
	releaseFault func(routeAdmissionLeaseRole) error
	beforeRename func()
	afterRename  func()
	loseReadback bool
	entered      bool
	raw          []byte
}

func newRouteAdmissionFixture(t *testing.T) *routeAdmissionFixture {
	t.Helper()
	from := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 1, Port: 9555}
	to := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 2, Port: 9556}
	digest, started := strings.Repeat("a", 64), "2026-01-01T00:00:00Z"
	selection := api.RouteActivationSelection{FromEpoch: from, ToEpoch: to, DescriptorSHA256: digest,
		Supervisor: api.SupervisorLockOwner{PID: 40, StartedAt: started}, PriorGeneration: 2}
	prior := &mcpFrontReconcileReport{Version: mcpFrontReconcileVersion4, SnapshotComplete: true, Generation: 1,
		Rows: map[string]mcpFrontReconcileRow{}, ActivePlan: &mcpFrontReconcilePlan{Generation: 1, Port: from.Port}, Settled: true}
	raw, err := marshalMCPFrontReconcileReport(prior)
	if err != nil {
		t.Fatal(err)
	}
	before := routeAdmissionPreimage{report: prior, raw: raw, selection: selection, incumbent: &daemonCreatorOperation{},
		receipt: mcpFrontRouteAdmissionReceipt{JournalBeforeSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), SettingsSHA256: digest,
			IntentSHA256: digest, IncumbentDescriptorSHA256: digest, Incumbent: api.SupervisorLockOwner{PID: 41, StartedAt: started}}}
	f := &routeAdmissionFixture{ctx: t.Context(), controller: &supervisorController{}, before: before, current: true, raw: bytes.Clone(raw)}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	// The genuine read finalizer reports no owned resources before native
	// entry. Fixture bytes below are only the separate modeled content.
	_, closedReader, _ := api.ReadStateFileInodeAnchoredObserved(cancelled, "", 1)
	f.owner = &routeAdmissionReservation{before: before, current: func() bool { return f.current }, check: func(ctx context.Context) error { return ctx.Err() }, ops: routeAdmissionOps{
		observe: func(context.Context) (routeAdmissionPreimage, error) { return f.before, nil },
		lock: func(_ context.Context, role routeAdmissionLeaseRole) (*routeAdmissionLease, error) {
			f.locks = append(f.locks, role)
			var acquireErr error
			if f.fault != nil {
				entered, err := f.fault(role)
				if !entered {
					return nil, err
				}
				acquireErr = err
			}
			return &routeAdmissionLease{
				verify: func(ctx context.Context) error { return ctx.Err() },
				release: func() routeAdmissionLeaseClose {
					f.releases = append(f.releases, role)
					if f.releaseFault != nil {
						if err := f.releaseFault(role); err != nil {
							return routeAdmissionLeaseClose{err: err}
						}
					}
					return routeAdmissionLeaseClose{closed: true}
				},
			}, acquireErr
		},
		write: func(ctx context.Context, _ *routeAdmissionLease, _ string, raw []byte, guard func(context.Context) error) (routeAdmissionWrite, error) {
			if f.beforeRename != nil {
				f.beforeRename()
			}
			if err := guard(ctx); err != nil {
				return routeAdmissionWrite{}, err
			}
			f.entered = true
			f.raw = bytes.Clone(raw)
			if f.afterRename != nil {
				f.afterRename()
			}
			return routeAdmissionWrite{entered: true, validate: func(context.Context, string) error {
				if len(f.releases) != int(routeAdmissionLeaseCount) {
					return errors.New("received publication before inverse release")
				}
				return nil
			}}, nil
		},
		read: func(context.Context) routeAdmissionRead {
			if f.loseReadback {
				return routeAdmissionRead{err: errors.New("fixture readback unknown")}
			}
			report, err := decodeMCPFrontReconcileReport(f.raw, "fixture")
			if err == nil {
				err = validateMCPFrontReconcileReport(report, "fixture")
			}
			return routeAdmissionRead{report: report, raw: f.raw, observation: closedReader, err: err}
		},
		live: func(context.Context, routeAdmissionPreimage) error {
			if !f.current {
				return errAggregatePeerChanged
			}
			return nil
		},
		after: func(context.Context, routeAdmissionPreimage) (routeAdmissionPreimage, error) { return f.before, nil },
	}}
	f.owner.selection.Store(&before)
	return f
}

func (f *routeAdmissionFixture) reserve(t *testing.T) {
	t.Helper()
	if open := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, reserveRouteAdmission); !open.accepted || len(f.locks) != 0 {
		t.Fatalf("fixture reserve entered the writer: %v", open.out.err)
	}
	if other := f.controller.routeAdmissionOnLoop(f.ctx, &routeAdmissionReservation{}, confirmRouteAdmission); other.accepted {
		t.Fatal("a second pipe confirmed the original reservation")
	}
}

func (f *routeAdmissionFixture) confirm(t *testing.T) {
	t.Helper()
	if replied := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, confirmRouteAdmission); !replied.accepted || len(f.locks) != 0 {
		t.Fatalf("FIFO confirmation waited on the writer: %v", replied.out.err)
	}
	if again := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, confirmRouteAdmission); again.accepted {
		t.Fatal("same confirmation issued a second permit")
	}
}

func (f *routeAdmissionFixture) finish(t *testing.T, out routeAdmissionOutcome) routeAdmissionOutcome {
	t.Helper()
	sealed := out.sealForFIFO(nil)
	f.owner.outcome.Store(&sealed)
	first := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, receiveRouteAdmissionResult)
	if !first.accepted || f.controller.routeAdmission != f.owner {
		t.Fatalf("operation receipt changed original transport custody: %v", first.out.err)
	}
	transport := routeAdmissionFixtureTransport(first)
	f.owner.transport.Store(transport)
	f.owner.closed.Store(true)
	final := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, finishRouteAdmission)
	if !final.accepted || f.controller.routeAdmissionOnLoop(f.ctx, f.owner, finishRouteAdmission).accepted {
		t.Fatal("FIFO final outcome did not settle exactly once")
	}
	if !errors.Is(final.out.err, errRouteAdmissionNativePipeCloseUnavailable) || f.controller.routeAdmission != f.owner {
		t.Fatal("Go transport return fabricated native pipe closure")
	}
	return first.out
}

type routeAdmissionTestConn struct {
	net.Conn
	closeFn func() error
	writeFn func([]byte) (int, error)
}

func (c *routeAdmissionTestConn) Close() error {
	if c.closeFn != nil {
		return c.closeFn()
	}
	return nil
}

func (c *routeAdmissionTestConn) Write(b []byte) (int, error) {
	if c.writeFn != nil {
		return c.writeFn(b)
	}
	return len(b), nil
}

func routeAdmissionFixtureTransport(first routeAdmissionReply) *routeAdmissionTransport {
	conn := &aggregateIPCConn{Conn: &routeAdmissionTestConn{}}
	transport := &routeAdmissionTransport{projectionErr: first.out.err, cleanProjection: first.accepted && first.out.err == nil}
	if first.out.result.Phase != "" {
		frame := writeIPCFrameObserved(conn, api.IPCResponse{ID: 1, OK: transport.cleanProjection, Final: true})
		transport.finalFrame = &frame
	}
	transport.receiveClose(context.Background(), conn)
	return transport
}

func TestRouteAdmissionInverseContentionUnwindsOnce(t *testing.T) {
	for _, busyAt := range []routeAdmissionLeaseRole{routeAdmissionIntent, routeAdmissionReport} {
		t.Run(fmt.Sprint(busyAt), func(t *testing.T) {
			f := newRouteAdmissionFixture(t)
			f.reserve(t)
			f.confirm(t)
			busy := errors.New("fixture inverse holder is busy")
			f.fault = func(role routeAdmissionLeaseRole) (bool, error) {
				if role == busyAt {
					return true, busy // one native attempt, including its returned partial owner
				}
				return true, nil
			}
			out := f.finish(t, admitRouteActivation(f.ctx, f.owner))
			if !errors.Is(out.err, busy) || len(f.locks) != int(busyAt)+1 || len(f.releases) != len(f.locks) ||
				f.entered || !bytes.Equal(f.raw, f.before.raw) || !f.owner.outcome.Load().provedClosedNoEffect {
				t.Fatalf("inverse contention reached publication or retained a closed owner: %v", out.err)
			}
			for i, role := range f.locks {
				if role != routeAdmissionLeaseRole(i) || f.releases[len(f.releases)-1-i] != role {
					t.Fatalf("unexpected lock and release order: %v, %v", f.locks, f.releases)
				}
			}
		})
	}
}

func TestRouteAdmissionCancellationPreservesPartialRelease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entered bool
		pending bool
	}{{"before native attempt", false, false}, {"partial native acquisition", true, false}, {"uncertain partial release", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRouteAdmissionFixture(t)
			f.reserve(t)
			f.confirm(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			f.fault = func(role routeAdmissionLeaseRole) (bool, error) {
				if role == routeAdmissionIntent {
					cancel()
					return tc.entered, context.Canceled
				}
				return true, nil
			}
			if tc.pending {
				f.releaseFault = func(role routeAdmissionLeaseRole) error {
					if role == routeAdmissionIntent {
						return errors.New("physical lease closure pending")
					}
					return nil
				}
			}
			out := f.finish(t, admitRouteActivation(ctx, f.owner))
			wantReleases := int(routeAdmissionIntent)
			if tc.entered {
				wantReleases++
			}
			if !errors.Is(out.err, context.Canceled) || len(f.releases) != wantReleases || f.entered ||
				!bytes.Equal(f.raw, f.before.raw) || out.leasesClosed() == tc.pending ||
				f.owner.outcome.Load().provedClosedNoEffect == tc.pending {
				t.Fatalf("canceled acquisition lost its real release or preimage: %v", out.err)
			}
		})
	}
	t.Run("pre-admit disconnect", func(t *testing.T) {
		f := newRouteAdmissionFixture(t)
		f.reserve(t)
		_ = f.finish(t, routeAdmissionOutcome{})
		if len(f.locks) != 0 || f.entered || !f.owner.outcome.Load().provedClosedNoEffect || !bytes.Equal(f.raw, f.before.raw) {
			t.Fatal("pre-admit disconnect left a transaction effect")
		}
	})
}

func TestRouteAdmissionIncumbentBoundaryAndSiblingFIFO(t *testing.T) {
	for _, tc := range []struct {
		name         string
		loseBefore   bool
		loseAfter    bool
		loseReadback bool
	}{{name: "same incumbent"}, {name: "before rename", loseBefore: true}, {name: "after rename", loseAfter: true},
		{name: "uncertain readback", loseReadback: true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRouteAdmissionFixture(t)
			ctx, cancel := context.WithTimeout(f.ctx, api.SupervisorAggregateStatusTimeout)
			defer cancel()
			f.controller.eventLoop = api.NewEventLoop(16)
			f.controller.eventLoop.RegisterHandler(f.controller.handleLoopEvent)
			loopDone := make(chan struct{})
			go func() { defer close(loopDone); f.controller.eventLoop.Run(ctx) }()
			defer func() { cancel(); <-loopDone }()
			if open := f.controller.postRouteAdmission(ctx, f.owner, reserveRouteAdmission); !open.accepted {
				t.Fatal(open.out.err)
			}
			if confirm := f.controller.postRouteAdmission(ctx, f.owner, confirmRouteAdmission); !confirm.accepted {
				t.Fatal(confirm.out.err)
			}
			waiting, resume := make(chan struct{}), make(chan struct{})
			pause := func() {
				close(waiting)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			}
			if tc.loseAfter {
				f.afterRename = pause
			} else {
				f.beforeRename = pause
			}
			f.loseReadback = tc.loseReadback
			result := make(chan routeAdmissionOutcome, 1)
			go func() { result <- admitRouteActivation(ctx, f.owner) }()
			defer func() {
				cancel()
				if result != nil {
					<-result
				}
			}()
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal("original worker did not reach rename boundary")
			}
			barrier := make(chan struct{})
			if err := f.controller.eventLoop.PostCtx(ctx, api.LoopEvent{Kind: evControllerBarrier, Body: map[string]any{controllerBarrierResultBodyKey: barrier}}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-barrier:
			case <-ctx.Done():
				t.Fatal("sibling FIFO work was blocked by Route journal I/O")
			}
			if tc.loseBefore || tc.loseAfter {
				f.owner.conflicted.Store(true)
			}
			close(resume)
			out := <-result
			result = nil
			original := out.sealForFIFO(nil)
			f.owner.outcome.Store(&original)
			first := f.controller.postRouteAdmission(ctx, f.owner, receiveRouteAdmissionResult)
			f.owner.transport.Store(routeAdmissionFixtureTransport(first))
			f.owner.closed.Store(true)
			final := f.controller.postRouteAdmission(ctx, f.owner, finishRouteAdmission)
			if !final.accepted || !errors.Is(final.out.err, errRouteAdmissionNativePipeCloseUnavailable) || f.controller.routeAdmission != f.owner {
				t.Fatal("terminal receipt did not retain unresolved pipe ownership")
			}
			out = first.out
			if tc.loseBefore {
				if f.entered || out.result.Phase != "" || out.err == nil || !bytes.Equal(f.raw, f.before.raw) {
					t.Fatalf("conflict before native entry changed journal: %v", out.err)
				}
				return
			}
			wantPhase := mcpFrontRouteActivationAdmitted
			if tc.loseAfter || tc.loseReadback {
				wantPhase = routeAdmissionPending
			}
			if !f.entered || !final.accepted || out.result.Phase != wantPhase || (out.err != nil) != (wantPhase == routeAdmissionPending) {
				t.Fatalf("wrong original outcome after native entry: %+v", out)
			}
			if (out.result.JournalSHA256 == "") != tc.loseReadback {
				t.Fatal("pending path fabricated or lost strict readback")
			}
			report, err := decodeMCPFrontReconcileReport(f.raw, "fixture")
			if err != nil || report.RouteActivation == nil || report.RouteActivation.Phase != mcpFrontRouteActivationAdmitted ||
				report.GenerationAdmission != nil || report.RouteOriginal != nil {
				t.Fatalf("caller/R3 recovery changed factual receipt: %v", err)
			}
		})
	}
}

func TestRouteAdmissionRetainsUncertainOriginalEvidence(t *testing.T) {
	for _, nativeEntered := range []bool{false, true} {
		t.Run(fmt.Sprint(nativeEntered), func(t *testing.T) {
			f := newRouteAdmissionFixture(t)
			f.reserve(t)
			f.confirm(t)
			capture := &clients.ConfigWriteCaptureTransaction{}
			publication := &api.StateFilePublicationObservation{}
			closeProof := &clients.ConfigWriteParentCloseObservation{}
			readProof := &api.StateFileReadObservation{}
			closeErr := errors.New("original capture close unresolved")
			readErr := errors.New("original anchored readback unresolved")
			f.owner.ops.write = func(_ context.Context, _ *routeAdmissionLease, _ string, _ []byte, _ func(context.Context) error) (routeAdmissionWrite, error) {
				return routeAdmissionWrite{entered: nativeEntered, capture: capture, publication: publication,
					close: closeProof, closeErr: closeErr}, closeErr
			}
			f.owner.ops.read = func(context.Context) routeAdmissionRead {
				return routeAdmissionRead{observation: readProof, err: readErr}
			}
			out := f.finish(t, admitRouteActivation(f.ctx, f.owner))
			stored := f.owner.outcome.Load()
			if !errors.Is(out.err, closeErr) || f.controller.routeAdmission != f.owner || stored == nil ||
				stored.write.capture != capture || stored.write.publication != publication ||
				stored.write.close != closeProof || !errors.Is(stored.write.closeErr, closeErr) ||
				stored.provedClosedNoEffect || stored.leases[routeAdmissionReport].owner == nil {
				t.Fatalf("lost original pending writer or improperly credited closure: %v", out.err)
			}
			if nativeEntered && (out.result.Phase != routeAdmissionPending || !errors.Is(out.err, readErr) ||
				stored.readback == nil || stored.readback.observation != readProof || !errors.Is(stored.readback.err, readErr)) {
				t.Fatalf("lost uncertain original readback: %v", out.err)
			}
			next := newRouteAdmissionFixture(t).owner
			if f.controller.routeAdmissionOnLoop(f.ctx, next, reserveRouteAdmission).accepted {
				t.Fatal("uncertain operation was reclaimed for another pipe")
			}
		})
	}
}

func TestRouteAdmissionFIFOHoldsWithoutNativePipeCloseProof(t *testing.T) {
	f := newRouteAdmissionFixture(t)
	f.reserve(t)
	f.controller.eventLoop = api.NewEventLoop(16)
	for f.controller.eventLoop.TryPost(api.LoopEvent{Kind: evControllerBarrier}) {
	}
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	if confirm := f.controller.postRouteAdmission(cancelled, f.owner, confirmRouteAdmission); confirm.accepted ||
		!errors.Is(confirm.out.err, context.Canceled) {
		t.Fatal("saturated FIFO confirmation granted a worker")
	}
	original := (routeAdmissionOutcome{err: context.Canceled}).sealForFIFO(nil)
	f.owner.closed.Store(true)
	f.owner.outcome.Store(&original)
	exactClose := &aggregateIPCClose{done: make(chan struct{})}
	exactReader := &routeAdmissionReader{done: make(chan struct{}), err: net.ErrClosed}
	f.owner.transport.Store(&routeAdmissionTransport{firstClose: exactClose, reader: exactReader})
	if finish := f.controller.postRouteAdmission(cancelled, f.owner, finishRouteAdmission); finish.accepted ||
		!errors.Is(finish.out.err, context.Canceled) {
		t.Fatal("saturated FIFO finish manufactured an acknowledgement")
	}
	next := newRouteAdmissionFixture(t).owner
	if reply := f.controller.routeAdmissionOnLoop(f.ctx, next, reserveRouteAdmission); reply.accepted {
		t.Fatal("incomplete original pipe close was credited as settled")
	}
	close(exactClose.done)
	if reply := f.controller.routeAdmissionOnLoop(f.ctx, next, reserveRouteAdmission); reply.accepted {
		t.Fatal("physical close alone credited an outstanding reader")
	}
	close(exactReader.done)
	if reply := f.controller.routeAdmissionOnLoop(f.ctx, next, reserveRouteAdmission); reply.accepted ||
		f.controller.routeAdmission != f.owner || f.owner.outcome.Load() != &original || len(f.locks) != 0 {
		t.Fatal("Go pipe close and reader join created native reuse authority")
	}
	if late := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, finishRouteAdmission); !late.accepted ||
		!errors.Is(late.out.err, errRouteAdmissionNativePipeCloseUnavailable) || f.controller.routeAdmission != f.owner {
		t.Fatal("old terminal receipt lost its unresolved original")
	}
}

func TestRouteAdmissionShortFrameKeepsOriginalTerminalPending(t *testing.T) {
	f := newRouteAdmissionFixture(t)
	f.reserve(t)
	f.confirm(t)
	original := admitRouteActivation(f.ctx, f.owner).sealForFIFO(nil)
	f.owner.outcome.Store(&original)
	first := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, receiveRouteAdmissionResult)
	if !first.accepted || first.out.err != nil || f.controller.routeAdmission != f.owner {
		t.Fatalf("first acknowledgement settled pipe ownership: %v", first.out.err)
	}
	conn := &aggregateIPCConn{Conn: &routeAdmissionTestConn{writeFn: func(b []byte) (int, error) { return len(b) - 1, nil }}}
	written := writeIPCFrameObserved(conn, api.IPCResponse{ID: 1, OK: true, Final: true})
	if written.encoded <= 0 || written.written != written.encoded-1 || written.writeErr != nil || !written.attempted {
		t.Fatal("lost exact short write evidence")
	}
	transport := &routeAdmissionTransport{cleanProjection: true, finalFrame: &written}
	transport.receiveClose(f.ctx, conn)
	f.owner.transport.Store(transport)
	f.owner.closed.Store(true)
	final := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, finishRouteAdmission)
	if !final.accepted || !errors.Is(final.out.err, io.ErrShortWrite) || final.out.result.Phase != routeAdmissionPending ||
		!transport.closeReturned || !errors.Is(final.out.err, errRouteAdmissionNativePipeCloseUnavailable) ||
		f.controller.routeAdmission != f.owner || f.owner.transport.Load().finalFrame != &written {
		t.Fatalf("short frame with an entered journal lost the original pending result: %v", final.out.err)
	}
	if next := newRouteAdmissionFixture(t).owner; f.controller.routeAdmissionOnLoop(f.ctx, next, reserveRouteAdmission).accepted {
		t.Fatal("closed pipe with entered rename was reused after incomplete delivery")
	}
}

func TestRouteAdmissionSharesFirstPhysicalCloseWithShutdown(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	closeErr := errors.New("fixture original pipe close failed")
	conn := &aggregateIPCConn{Conn: &routeAdmissionTestConn{closeFn: func() error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return closeErr
	}}}
	originalDone, shutdownDone := make(chan *aggregateIPCClose, 1), make(chan struct{})
	go func() { originalDone <- conn.startClose() }()
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), api.SupervisorAggregateStatusTimeout)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first physical close did not start")
	}
	go func() { closeAcceptedIPCConn(conn); close(shutdownDone) }()
	select {
	case <-shutdownDone:
	case <-ctx.Done():
		t.Fatal("shutdown waited for a second physical close")
	}
	sameAttempt := conn.startClose()
	if calls.Load() != 1 {
		t.Fatal("shutdown replaced the original close attempt")
	}
	select {
	case <-sameAttempt.done:
		t.Fatal("incomplete original close was reported returned")
	default:
	}
	// Release the one modeled physical call and receive its cached result.
	release <- struct{}{}
	finished := <-originalDone
	if finished != sameAttempt || !errors.Is(finished.err, closeErr) || !errors.Is(conn.Close(), closeErr) || calls.Load() != 1 {
		t.Fatal("original pipe close error was erased or physically repeated")
	}
}

func TestRouteAdmissionLateReaderCancellationRefusesCleanProjection(t *testing.T) {
	f := newRouteAdmissionFixture(t)
	f.reserve(t)
	f.confirm(t)
	request, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.owner.request = request
	original := admitRouteActivation(request, f.owner).sealForFIFO(nil)
	if original.err != nil || !f.entered || original.result.JournalSHA256 == "" {
		t.Fatalf("fixture did not settle before late reader cancellation: %v", original.err)
	}
	f.owner.closed.Store(true)
	f.owner.outcome.Store(&original)
	f.owner.wire.readerEnded(io.EOF, cancel)
	// Terminal disposal uses a different context and must not lend it
	// to the original wire outcome.
	finish := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, receiveRouteAdmissionResult)
	wire := f.owner.wire.project(request, finish)
	if !finish.accepted || wire.result.Phase != routeAdmissionPending || wire.result.JournalSHA256 != original.result.JournalSHA256 ||
		!errors.Is(wire.err, context.Canceled) || !errors.Is(wire.err, io.EOF) ||
		f.controller.routeAdmission != f.owner || f.owner.outcome.Load() != &original {
		t.Fatalf("late reader cancellation fabricated clean wire outcome or lost original: %v", wire.err)
	}
	// An undelivered FIFO finish race also cannot produce a clean frame.
	var awaiting routeAdmissionWire
	if out := awaiting.project(f.ctx, routeAdmissionReply{out: original}); out.err == nil || out.result.Phase != routeAdmissionPending {
		t.Fatal("wire projection credited an unacknowledged finish")
	}
	afterAck, cancelAfterAck := context.WithCancel(f.ctx)
	cancelAfterAck()
	var acknowledged routeAdmissionWire
	if out := acknowledged.project(afterAck, routeAdmissionReply{accepted: true, out: original}); out.result.Phase != routeAdmissionPending || !errors.Is(out.err, context.Canceled) {
		t.Fatal("accepted FIFO finish replaced the original wire context")
	}
}

func TestRouteAdmissionConfirmCannotOpenAnOperatorStream(t *testing.T) {
	open := "{\"version\":1,\"id\":1,\"cmd\":\"aggregate-route-admission\"}\n"
	challenge := strings.Repeat("a", api.RouteAggregateChallengeBytes*2)
	confirm := "{\"version\":1,\"id\":1,\"cmd\":\"aggregate-route-admit\",\"args\":{\"challenge\":\"" + challenge + "\"}}\n"
	if _, err := api.ReadOperatorAggregateStatusOpen(strings.NewReader(open)); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ReadOperatorAggregateStatusOpen(strings.NewReader(confirm)); err == nil {
		t.Fatal("confirmation was accepted as a reconnect open")
	}
	if err := api.ReadRouteActivationAdmissionConfirm(strings.NewReader(confirm), 1, challenge); err != nil {
		t.Fatal(err)
	}
	if err := api.ReadRouteActivationAdmissionConfirm(strings.NewReader(open), 1, challenge); err == nil {
		t.Fatal("opening phase admitted the transaction")
	}
}

type routeAdmissionFixtureNativeProof struct{ closed bool }

func (p *routeAdmissionFixtureNativeProof) ResourcesFinalized() bool { return p.closed }

func TestRouteAdmissionRetainsEarlierNativeClosureFailure(t *testing.T) {
	var phases routeAdmissionNativeObservations
	partial := &routeAdmissionFixtureNativeProof{}
	later := &routeAdmissionFixtureNativeProof{closed: true}
	denied := errors.New("fixture original identity denied")
	phases.add("operator-open-pipe", partial, denied)
	phases.add("binding-pipe", later, nil)
	out := (routeAdmissionOutcome{native: phases, err: denied}).sealForFIFO(nil)
	if out.operationResourcesClosed || out.provedClosedNoEffect || len(out.native) != 2 ||
		out.native[0].evidence != partial || !errors.Is(out.native[0].err, denied) || out.native[1].evidence != later {
		t.Fatal("later successful query erased earlier unresolved native ownership")
	}
}

func TestRouteAdmissionJoinedReaderRetainsProtocolFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		err  error
	}{{"timeout", 0, context.DeadlineExceeded}, {"trailing byte", 1, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &routeAdmissionReader{done: make(chan struct{}), n: tc.n, err: tc.err}
			close(reader.done)
			conn := &aggregateIPCConn{Conn: &routeAdmissionTestConn{}}
			transport := &routeAdmissionTransport{reader: reader}
			transport.receiveClose(t.Context(), conn)
			if !transport.readerJoined || transport.readerJoinErr != nil || transport.readerProtocolErr == nil ||
				transport.reader != reader || !transport.closeReturned || transport.closeErr != nil {
				t.Fatal("reader protocol error replaced the observed join or Go close return")
			}
			f := newRouteAdmissionFixture(t)
			f.reserve(t)
			original := (routeAdmissionOutcome{}).sealForFIFO(nil)
			f.owner.outcome.Store(&original)
			f.owner.transport.Store(transport)
			f.owner.closed.Store(true)
			result := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, finishRouteAdmission)
			if !result.accepted || !errors.Is(result.out.err, errRouteAdmissionNativePipeCloseUnavailable) ||
				f.controller.routeAdmission != f.owner {
				t.Fatal("joined original reader supplied unavailable native pipe closure")
			}
		})
	}
}

// This fake models a result returned by the lower owner. It supplies no
// Windows handle or evidence that an OS close was ever attempted.
type routeAdmissionFixturePipeReceipt struct {
	err      error
	observed bool
}

func (r *routeAdmissionFixturePipeReceipt) NativeResult() (error, bool) { return r.err, r.observed }

func TestRouteAdmissionNativePipeReceiptKeepsWaitAndNativeErrorsSeparate(t *testing.T) {
	incomplete := &routeAdmissionFixturePipeReceipt{}
	paused := receiveRouteAdmissionNativePipeClose(incomplete, context.Canceled)
	if paused.receipt != incomplete || paused.observed || paused.nativeErr != nil ||
		!errors.Is(paused.failure(), context.Canceled) || !errors.Is(paused.failure(), errRouteAdmissionNativePipeCloseUnavailable) {
		t.Fatal("canceled observer erased the pending original or produced native closure")
	}
	nativeErr := errors.New("fixture original native close failed")
	failed := &routeAdmissionFixturePipeReceipt{err: nativeErr, observed: true}
	observation := receiveRouteAdmissionNativePipeClose(failed, nil)
	if !observation.observed || observation.receipt != failed || observation.waitErr != nil ||
		!errors.Is(observation.failure(), nativeErr) || errors.Is(observation.failure(), errRouteAdmissionNativePipeCloseUnavailable) {
		t.Fatal("actual native failure was replaced by observer uncertainty")
	}
}

func TestRouteAdmissionTerminalRequiresOriginalNativeClosureAndDelivery(t *testing.T) {
	for _, tc := range []struct {
		name            string
		change          func(*routeAdmissionFixture, *routeAdmissionTransport)
		missTerminal    bool
		short, protocol bool
		clean           bool
	}{
		{name: "exact original result", clean: true},
		{name: "lower proof unavailable", change: func(_ *routeAdmissionFixture, transport *routeAdmissionTransport) {
			transport.nativeClose = receiveRouteAdmissionNativePipeClose(nil, errRouteAdmissionNativePipeCloseUnavailable)
		}},
		{name: "short original frame", short: true},
		{name: "joined reader protocol error", protocol: true},
		{name: "original first FIFO receipt lost", change: func(_ *routeAdmissionFixture, transport *routeAdmissionTransport) {
			transport.operationReceipt = nil
		}},
		{name: "incumbent changed after delivery", change: func(f *routeAdmissionFixture, _ *routeAdmissionTransport) {
			f.owner.conflicted.Store(true)
		}},
		{name: "second FIFO disposition unavailable", missTerminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRouteAdmissionFixture(t)
			f.reserve(t)
			f.confirm(t)
			original := admitRouteActivation(f.ctx, f.owner).sealForFIFO(nil)
			f.owner.outcome.Store(&original)
			first := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, receiveRouteAdmissionResult)
			if !first.accepted || first.receipt == nil || first.out.err != nil || f.controller.routeAdmission != f.owner {
				t.Fatalf("first receipt settled more than the original operation: %v", first.out.err)
			}
			var writes int
			conn := &aggregateIPCConn{Conn: &routeAdmissionTestConn{writeFn: func(b []byte) (int, error) {
				writes++
				if tc.short && writes == 2 {
					return len(b) - 1, nil
				}
				return len(b), nil
			}}}
			offer := writeIPCFrameObserved(conn, api.IPCResponse{ID: 1, OK: true})
			frame := writeIPCFrameObserved(conn, api.IPCResponse{ID: 1, OK: true, Final: true})
			reader := &routeAdmissionReader{done: make(chan struct{}), err: net.ErrClosed}
			if tc.protocol {
				reader.err = context.DeadlineExceeded
			}
			close(reader.done)
			transport := &routeAdmissionTransport{reader: reader, openFrame: &offer, finalFrame: &frame,
				operationReceipt: first.receipt, cleanProjection: true}
			transport.receiveClose(f.ctx, conn)
			transport.nativeClose = receiveRouteAdmissionNativePipeClose(&routeAdmissionFixturePipeReceipt{observed: true}, nil)
			if tc.change != nil {
				tc.change(f, transport)
			}
			f.owner.transport.Store(transport)
			f.owner.closed.Store(true)
			if tc.missTerminal {
				f.controller.eventLoop = api.NewEventLoop(16)
				for f.controller.eventLoop.TryPost(api.LoopEvent{Kind: evControllerBarrier}) {
				}
				ctx, cancel := context.WithCancel(f.ctx)
				cancel()
				if terminal := f.controller.postRouteAdmission(ctx, f.owner, finishRouteAdmission); terminal.accepted ||
					f.controller.routeAdmission != f.owner {
					t.Fatal("failed second FIFO disposition settled an entered original")
				}
			} else {
				final := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, finishRouteAdmission)
				if !final.accepted || (final.out.err == nil) != tc.clean ||
					(f.controller.routeAdmission == nil) != tc.clean {
					t.Fatalf("wrong original terminal settlement: %v", final.out.err)
				}
			}
			if !tc.clean {
				next := newRouteAdmissionFixture(t).owner
				if f.controller.routeAdmissionOnLoop(f.ctx, next, reserveRouteAdmission).accepted {
					t.Fatal("a native-rename original could be reclaimed as no effect")
				}
			}
		})
	}
}

func TestRouteAdmissionReconcilesOnlyPhysicallyClosedNoEffectOriginal(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			f := newRouteAdmissionFixture(t)
			f.reserve(t)
			original := (routeAdmissionOutcome{err: context.Canceled}).sealForFIFO(nil)
			f.owner.outcome.Store(&original)
			conn := &aggregateIPCConn{Conn: &routeAdmissionTestConn{}}
			reader := &routeAdmissionReader{done: make(chan struct{}), err: context.DeadlineExceeded}
			close(reader.done)
			transport := &routeAdmissionTransport{reader: reader}
			transport.receiveClose(f.ctx, conn)
			if available {
				transport.nativeClose = receiveRouteAdmissionNativePipeClose(&routeAdmissionFixturePipeReceipt{observed: true}, nil)
			}
			f.owner.transport.Store(transport)
			f.owner.closed.Store(true)
			next := newRouteAdmissionFixture(t).owner
			got := f.controller.routeAdmissionOnLoop(f.ctx, next, reserveRouteAdmission)
			if got.accepted != available || (f.controller.routeAdmission == next) != available ||
				f.owner.finalized != available || f.owner.outcome.Load() != &original ||
				!errors.Is(transport.readerProtocolErr, context.DeadlineExceeded) || transport.cleanProjection {
				t.Fatal("closed no-effect reconciliation claimed delivery or replaced original evidence")
			}
			if available {
				if late := f.controller.routeAdmissionOnLoop(f.ctx, f.owner, finishRouteAdmission); late.accepted ||
					f.controller.routeAdmission != next {
					t.Fatal("stale finish repeated old terminal settlement")
				}
			}
		})
	}
}

func TestProtectedPublicReconcileRejectsFreshOrLinkedV5BeforeEffects(t *testing.T) {
	at := func(port int) func() (int, error) { return func() (int, error) { return port, nil } }
	if err := allowMCPFrontPublicReconcile(nil, false, nil); err != nil {
		t.Fatal("initial V4 was refused")
	}
	old := &mcpFrontReconcileReport{Version: mcpFrontReconcileVersion4, ActivePlan: &mcpFrontReconcilePlan{Port: 9555}}
	if err := allowMCPFrontPublicReconcile(old, false, at(9556)); err == nil {
		t.Fatal("fresh V5 with no original authority reached the legacy stage")
	}
	if err := allowMCPFrontPublicReconcile(old, false, at(9555)); err != nil {
		t.Fatal("current historical generation was refused")
	}
	old.RouteActivation = &mcpFrontRouteActivation{Phase: mcpFrontRouteActivationPrepared}
	if err := allowMCPFrontPublicReconcile(old, true, nil); err != nil {
		t.Fatal("historical unlinked rollback was refused")
	}
	old.RouteActivation.Selection = &api.RouteActivationSelection{}
	if err := allowMCPFrontPublicReconcile(old, true, nil); err == nil {
		t.Fatal("old R1 marker gained effect authority")
	}
	old.RouteActivation = nil
	old.RouteOriginal = &api.RouteActivationOriginal{}
	if err := allowMCPFrontPublicReconcile(old, false, at(9555)); err == nil {
		t.Fatal("linked same-port resume reentered the legacy stage")
	}
}
