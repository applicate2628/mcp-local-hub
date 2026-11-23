package mcproute

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

type originalRetireAcceptBarrier struct {
	net.Listener
	entered  chan struct{}
	returned chan struct{}
	release  chan struct{}
	cause    error
}

func (l *originalRetireAcceptBarrier) Accept() (net.Conn, error) {
	close(l.entered)
	conn, err := l.Listener.Accept()
	close(l.returned)
	<-l.release
	if l.cause != nil {
		return conn, errors.Join(l.cause, err)
	}
	return conn, err
}

func TestFixedHubPublishesTerminalBeforeDiagnostic(t *testing.T) {
	socket, err := api.NewListenerWithSOExclusiveContext(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("fixture actual Serve termination after native Accept")
	accept := &originalRetireAcceptBarrier{
		Listener: socket, entered: make(chan struct{}), returned: make(chan struct{}), release: make(chan struct{}), cause: fault,
	}
	hub := &HubListener{strict: true, srv: newHubMcpHTTPServer(nil), serveDone: make(chan struct{})}
	hub.alive.Store(true)
	diagnostic, releaseDiagnostic, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(completed)
		hub.serve(accept, func(error) {
			close(diagnostic)
			<-releaseDiagnostic
		})
	}()
	defer func() { close(releaseDiagnostic); <-completed }()
	<-accept.entered
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	<-accept.returned
	close(accept.release)
	<-diagnostic
	select {
	case <-hub.serveDone:
		if !errors.Is(hub.serveErr, fault) || hub.Alive() {
			t.Fatal("original terminal or error was lost")
		}
	default:
		t.Fatal("original Serve terminal still depends on completing diagnostics")
	}
}

func TestFixedHubRetirementWaitsForOriginalServe(t *testing.T) {
	for _, name := range []string{"native-accept-held", "stop-before-serve", "abnormal-original-serve"} {
		t.Run(name, func(t *testing.T) {
			hub, address, stateDir, identity := bindNativeRetirementFixture(t)
			fault := errors.New("fixture abnormal original Serve termination")
			accept := &originalRetireAcceptBarrier{
				Listener: hub.socket.Listener, entered: make(chan struct{}), returned: make(chan struct{}), release: make(chan struct{}),
			}
			if name == "abnormal-original-serve" {
				accept.cause = fault
			}
			hub.socket.Listener = accept
			releaseAccept := sync.OnceFunc(func() { close(accept.release) })
			diagnostic, releaseDiagnostic := make(chan struct{}), make(chan struct{})
			unblockDiagnostic := sync.OnceFunc(func() { close(releaseDiagnostic) })
			workerDone := make(chan struct{})
			start := sync.OnceFunc(func() {
				go func() {
					defer close(workerDone)
					hub.serve(hub.socket, func(error) {
						close(diagnostic)
						<-releaseDiagnostic
					})
				}()
			})
			defer func() {
				releaseAccept()
				unblockDiagnostic()
				start()
				_ = hub.socket.close(true)
				<-workerDone
				_ = hub.RetireOriginal(t.Context())
			}()
			if name != "stop-before-serve" {
				start()
				<-accept.entered
			}

			expired, cancel := context.WithDeadline(t.Context(), time.Now())
			first := hub.RetireOriginal(expired)
			cancel()
			if first.Settled() || first.Pending != "original Hub Serve" || !errors.Is(first.Err, context.DeadlineExceeded) {
				t.Fatalf("first observation lost original Serve custody: %+v", first)
			}
			contender, err := api.NewListenerWithSOExclusiveContext(t.Context(), address)
			if err != nil {
				t.Fatalf("held Accept prevented native socket close: %v", err)
			}
			if err := contender.Close(); err != nil {
				t.Fatal(err)
			}
			if err := api.WithHubRouteState(t.Context(), stateDir, identity, func(h *api.HubRouteState) error {
				return hub.reload.VerifyOriginalReload(h)
			}); err != nil {
				t.Fatalf("retired original reload before Serve terminal: %v", err)
			}
			start()
			<-accept.returned
			releaseAccept()
			if name == "abnormal-original-serve" {
				<-diagnostic
			}
			<-hub.serveDone
			next := hub.RetireOriginal(t.Context())
			if !next.Settled() || !errors.Is(next.Err, context.DeadlineExceeded) || hub.Alive() {
				t.Fatalf("later observation did not settle original owner or preserve the first failure: %+v", next)
			}
			if errors.Is(next.Err, fault) != (name == "abnormal-original-serve") {
				t.Fatalf("abnormal Serve was misclassified: %v", next.Err)
			}
			if name != "abnormal-original-serve" && errors.Is(next.Err, net.ErrClosed) {
				t.Fatalf("attributable close was reported as abnormal Serve: %v", next.Err)
			}
			if err := api.WithHubRouteState(t.Context(), stateDir, identity, func(h *api.HubRouteState) error {
				return h.CheckReloadTokenAbsent()
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func bindNativeRetirementFixture(t *testing.T) (*HubListener, string, string, api.ConfigParentDirectoryIdentityV1) {
	t.Helper()
	dir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", dir)
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	if _, err := api.EnsureHubTokens([]string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	probe, err := api.NewListenerWithSOExclusiveContext(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port, address := probe.Addr().(*net.TCPAddr).Port, probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := api.EnsureHubEndpoint(port, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	identity, err := api.ObserveHubRouteStateDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var result *api.HubMcpBindResult
	if err := api.WithHubRouteState(t.Context(), dir, identity, func(h *api.HubRouteState) error {
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
	_, cancel := context.WithCancel(t.Context())
	watchersDone := make(chan struct{})
	close(watchersDone)
	hub := &HubListener{
		strict: true, srv: newHubMcpHTTPServer(nil), serveDone: make(chan struct{}),
		watchersDone: watchersDone, requestsDone: make(chan struct{}),
		store: api.NewHubSessionStore(api.SessionStoreOpts{}), reload: result.Reload, listenerCancel: cancel,
		socket: &hubAcceptObservation{Listener: result.Listener, entered: make(chan struct{})},
	}
	hub.alive.Store(true)
	return hub, address, dir, identity
}
