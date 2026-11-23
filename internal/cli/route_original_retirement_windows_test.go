package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestRouteStopJoinsOriginalTransportBeforePendingRetirement(t *testing.T) {
	stateDir, server, launch := makeOriginalRouteRetirePipe(t)
	bind, parent := bindOriginalRouteRetireToken(t, stateDir)
	path, err := windows.UTF16PtrFromString(filepath.Join(stateDir, "hub-mcp-control.token"))
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	unblock := sync.OnceFunc(func() {
		if err := windows.CloseHandle(blocker); err != nil {
			t.Error("close fixture blocker:", err)
		}
	})
	defer unblock()
	t.Cleanup(func() { _ = bind.CloseUnserved(context.WithoutCancel(t.Context())) })

	encoded, err := json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(routeAggregateLaunchEnv, string(encoded))
	firstRetire := make(chan struct{})
	var retireCalls atomic.Int32
	stop := startRouteAggregateStatus(t.Context(), io.Discard, func(api.RouteAggregateLaunch) api.RouteAggregateControl {
		return api.RouteAggregateControl{
			Status: func() api.RouteAggregateStatus {
				return api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner}
			},
			Retire: func(ctx context.Context) api.RouteAggregateRetirement {
				first := retireCalls.Add(1) == 1
				err := bind.CloseUnserved(ctx)
				if first {
					close(firstRetire)
					<-ctx.Done()
					err = errors.Join(err, ctx.Err())
				}
				if err != nil {
					return api.RouteAggregateRetirement{Pending: "fixture original Hub reload file", Err: err}
				}
				return api.RouteAggregateRetirement{}
			},
		}
	})
	peer := acceptOriginalRouteRetirePipe(t, server)
	defer peer.Close()
	firstStop := make(chan error, 1)
	go func() { firstStop <- stop() }()
	<-firstRetire
	if err := peer.SetReadDeadline(time.Now().Add(api.SupervisorAggregateStatusTimeout)); err != nil {
		t.Fatal(err)
	}
	_, peerErr := peer.Read(make([]byte, 1))
	var timeout net.Error
	if peerErr == nil || errors.As(peerErr, &timeout) && timeout.Timeout() {
		t.Fatalf("original pipe remained available during H retirement: %v", peerErr)
	}
	sharing := func(err error) bool {
		return errors.Is(err, windows.STATUS_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
	}
	overlap := stop()
	initial := <-firstStop
	if retireCalls.Load() != 1 || !sharing(initial) || !sharing(overlap) || !errors.Is(overlap, context.DeadlineExceeded) ||
		!strings.Contains(overlap.Error(), "retirement unresolved: fixture original Hub reload file") {
		t.Fatalf("overlapping stop did not share original pending result: calls=%d first=%v second=%v", retireCalls.Load(), initial, overlap)
	}
	if err := api.WithHubRouteState(t.Context(), stateDir, parent, func(h *api.HubRouteState) error {
		if err := h.CheckReloadTokenAbsent(); err == nil {
			return errors.New("pending reload disappeared")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unblock()
	later := stop()
	if retireCalls.Load() != 2 || !sharing(later) || strings.Contains(later.Error(), "retirement unresolved:") {
		t.Fatalf("independent stop did not settle the original receiver while retaining the first error: calls=%d err=%v", retireCalls.Load(), later)
	}
	if err := api.WithHubRouteState(t.Context(), stateDir, parent, func(h *api.HubRouteState) error {
		return h.CheckReloadTokenAbsent()
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRouteAggregateReattachesAfterSettledDiagnostic(t *testing.T) {
	_, server, launch := makeOriginalRouteRetirePipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var retireCalls atomic.Int32
	abnormal := errors.New("fixture original Serve diagnostic after physical settlement")
	done := make(chan api.RouteAggregateControlExit, 1)
	go func() {
		done <- api.ServeRouteAggregateControlObserved(ctx, launch, api.RouteAggregateControl{
			Status: func() api.RouteAggregateStatus {
				return api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner}
			},
			Retire: func(context.Context) api.RouteAggregateRetirement {
				if retireCalls.Add(1) == 1 {
					return api.RouteAggregateRetirement{Err: abnormal}
				}
				return api.RouteAggregateRetirement{}
			},
		})
	}()
	first := acceptOriginalRouteRetirePipe(t, server)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := acceptOriginalRouteRetirePipe(t, server)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	outcome := <-done
	if !outcome.Retirement.Settled() || outcome.RevokeError != nil || !errors.Is(outcome.Failure(), abnormal) || retireCalls.Load() != 2 {
		t.Fatalf("reattachment used a diagnostic as custody or lost it: %+v calls=%d", outcome, retireCalls.Load())
	}
}

func makeOriginalRouteRetirePipe(t *testing.T) (string, *SupervisorIPCListener, api.RouteAggregateLaunch) {
	t.Helper()
	dir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", dir)
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	lock, err := api.AcquireSupervisorLock(filepath.Join(dir, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	owner, err := newRouteAggregateSupervisor(t.Context(), dir, lock.Owner())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewSupervisorIPCListener(api.SupervisorIPCAddress(dir), lock.Owner())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return dir, server, owner.launch
}

func acceptOriginalRouteRetirePipe(t *testing.T, server *SupervisorIPCListener) net.Conn {
	t.Helper()
	deadline := time.Now().Add(api.SupervisorAggregateStatusTimeout)
	deadlineTimer := time.AfterFunc(time.Until(deadline), func() { _ = server.Close() })
	conn, err := server.Accept()
	deadlineTimer.Stop()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := server.WriteHello(conn); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(conn, marker[:]); err != nil || marker[0] != api.RouteAggregateStatusMarker {
		t.Fatalf("attach marker=%x err=%v", marker, err)
	}
	request, err := api.ReadRouteAggregateStatusOpen(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeIPCFrame(conn, api.IPCResponse{ID: request.ID, OK: true, Final: true,
		Result: api.RouteAggregateControlAttached{Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	challenge := api.RouteAggregateStatusChallenge{ID: 1, Challenge: strings.Repeat("1", 2*api.RouteAggregateChallengeBytes), Generation: 1}
	if err := writeIPCFrame(conn, api.IPCResponse{ID: challenge.ID, OK: true, Result: challenge}); err != nil {
		t.Fatal(err)
	}
	proof, err := api.ReadRouteAggregateStatusProof(conn)
	if err != nil || proof.Challenge != challenge.Challenge || proof.Ownership != api.RouteAggregateNotOwner {
		t.Fatalf("original proof=%+v err=%v", proof, err)
	}
	return conn
}

func bindOriginalRouteRetireToken(t *testing.T, dir string) (*api.HubMcpBindResult, api.ConfigParentDirectoryIdentityV1) {
	t.Helper()
	if _, err := api.EnsureHubTokens([]string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	probe, err := api.NewListenerWithSOExclusiveContext(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := api.EnsureHubEndpoint(port, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	parent, err := api.ObserveHubRouteStateDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var result *api.HubMcpBindResult
	if err := api.WithHubRouteState(t.Context(), dir, parent, func(h *api.HubRouteState) error {
		endpoint, err := h.ReadEndpoint()
		if err != nil {
			return err
		}
		row := api.HubRouteQualification{
			TransactionID: strings.Repeat("a", 32), SuccessorSHA256: strings.Repeat("b", 64), ScopeSHA256: strings.Repeat("c", 64),
			EndpointAuthoritySHA256: endpoint.AuthoritySHA256(), EndpointProvenanceSHA256: endpoint.ProvenanceSHA256(),
		}
		if err := h.StageQualification("", endpoint, row); err != nil {
			return err
		}
		result, err = h.BindExistingHubMcpListener(endpoint, row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return result, parent
}

type routeDiagnosticWriteBarrier struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (w *routeDiagnosticWriteBarrier) Write(p []byte) (int, error) {
	if w.calls.Add(1) == 1 {
		close(w.entered)
	}
	<-w.release
	return len(p), nil
}

func TestRouteStopReceivesTerminalWithoutDiagnosticWrite(t *testing.T) {
	_, server, launch := makeOriginalRouteRetirePipe(t)
	encoded, err := json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(routeAggregateLaunchEnv, string(encoded))

	writer := &routeDiagnosticWriteBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	releaseWriter := sync.OnceFunc(func() { close(writer.release) })
	defer releaseWriter()
	retired := make(chan struct{})
	var retireCalls atomic.Int32
	original := errors.New("fixture original settled retirement error")
	stop := startRouteAggregateStatus(t.Context(), writer, func(api.RouteAggregateLaunch) api.RouteAggregateControl {
		return api.RouteAggregateControl{
			Status: func() api.RouteAggregateStatus {
				return api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner}
			},
			Retire: func(context.Context) api.RouteAggregateRetirement {
				if retireCalls.Add(1) == 1 {
					close(retired)
				}
				return api.RouteAggregateRetirement{Err: original}
			},
		}
	})
	defer func() { releaseWriter(); _ = stop() }()

	peer := acceptOriginalRouteRetirePipe(t, server)
	defer peer.Close()
	invalid := api.RouteAggregateStatusChallenge{ID: 2, Generation: 1}
	if err := writeIPCFrame(peer, api.IPCResponse{ID: invalid.ID, OK: true, Final: true, Result: invalid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-retired:
	case <-time.After(api.SupervisorAggregateStatusTimeout):
		t.Fatal("original native attachment did not retire")
	}
	_, peerErr := peer.Read(make([]byte, 1))
	var timeout net.Error
	if peerErr == nil || errors.As(peerErr, &timeout) && timeout.Timeout() {
		t.Fatalf("original transport remained open at retirement: %v", peerErr)
	}
	// The predecessor's actual writer entry gives it a deterministic block
	// before Stop; the observation expires when the worker has no writer.
	select {
	case <-writer.entered:
	case <-time.After(250 * time.Millisecond):
	}
	done := make(chan error, 1)
	go func() { done <- stop() }()
	select {
	case err = <-done:
	case <-time.After(time.Second):
		wasBlocked := writer.calls.Load() > 0
		releaseWriter()
		err = <-done
		t.Fatalf("Route stop waited after physical settlement, writer blocked=%t outcome=%v", wasBlocked, err)
	}
	for _, outcome := range []error{err, stop()} {
		if !errors.Is(outcome, original) || !strings.Contains(outcome.Error(), "route aggregate status: invalid query") ||
			strings.Contains(outcome.Error(), "retirement unresolved:") {
			t.Fatalf("stored terminal outcome lost its original causes: %v", outcome)
		}
	}
	if retireCalls.Load() != 1 || writer.calls.Load() != 0 {
		t.Fatalf("unexpected second retirement or diagnostic write: retire=%d writes=%d", retireCalls.Load(), writer.calls.Load())
	}
}
