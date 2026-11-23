package mcproute

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHubMcpHTTPServerHasSlowClientTimeouts(t *testing.T) {
	srv := newHubMcpHTTPServer(http.NewServeMux())
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout=%v want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Fatalf("ReadTimeout must bound slow request bodies")
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout=%v want 0 so large fan-out compute is not capped by server write timeout", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Fatalf("IdleTimeout=%v want 120s", srv.IdleTimeout)
	}
}

type fatalHubAcceptListener struct{}

func (fatalHubAcceptListener) Accept() (net.Conn, error) {
	return nil, errors.New("fatal accept failure")
}
func (fatalHubAcceptListener) Close() error   { return nil }
func (fatalHubAcceptListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestHubListenerServeFailureClearsAlive(t *testing.T) {
	listener := &HubListener{srv: newHubMcpHTTPServer(http.NewServeMux()), serveDone: make(chan struct{})}
	listener.alive.Store(true)
	var reported error
	listener.serve(fatalHubAcceptListener{}, func(err error) {
		reported = err
		if listener.Alive() {
			t.Error("listener still reported alive when Serve failure was delivered")
		}
	})
	if listener.Alive() || reported == nil {
		t.Fatalf("alive=%t, callback error=%v", listener.Alive(), reported)
	}
	select {
	case <-listener.serveDone:
		if !errors.Is(listener.serveErr, reported) {
			t.Fatalf("original Serve result=%v, reported=%v", listener.serveErr, reported)
		}
	default:
		t.Fatal("original Serve result not settled")
	}
	listener.Shutdown(context.Background())
}
