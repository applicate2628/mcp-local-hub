package gui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestServer_StartAndShutdown(t *testing.T) {
	s := NewServer(Config{Port: 0}) // 0 = OS picks free port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, ready)
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server never signaled ready")
	}
	if s.Port() == 0 {
		t.Fatal("Port() returned 0 after ready")
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(s.Port()) + "/api/ping")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ping status %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

// The client keeps its SSE context alive through terminal Server shutdown. A
// listener-only restart has a separate oracle and must preserve this stream.
func TestServer_ShutdownDrainsBrowserSSE(t *testing.T) {
	listenerFailure := errors.New("injected GUI listener failure")
	for _, tc := range []struct {
		name        string
		holdStream  bool
		listenerErr error
	}{
		{name: "no stream control"},
		{name: "held browser SSE", holdStream: true},
		{name: "held browser SSE listener error", holdStream: true, listenerErr: listenerFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holdStream := tc.holdStream
			s := newEphemeralServer(t, Config{Port: 0, GUIShutdownDrainTimeout: 30 * time.Millisecond})
			installIsolatedAuditLock(t, s)
			s.hubEndpointGateFn = func(*api.API) bool { return false }
			s.startHubMcpListenerFn = func(context.Context, bool, *api.API, startHubMcpListenerOptions) (*HubListenerComponents, error) {
				return nil, nil
			}
			observed := make(chan error, 1)
			s.shutdownDrainObserved = func(err error) { observed <- err }
			cancelServer, serverDone, serverResult := startRecoverySettlementServer(t, s)
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			transport := &http.Transport{ResponseHeaderTimeout: 2 * time.Second}
			client := &http.Client{Transport: transport}
			var response *http.Response
			var readerDone chan struct{}
			var readerErr error
			defer func() {
				cancelRequest()
				if response != nil {
					_ = response.Body.Close()
				}
				cancelServer()
				transport.CloseIdleConnections()
				select {
				case <-serverDone:
				case <-time.After(2 * time.Second):
					t.Error("cleanup: Server.Start did not settle")
				}
				if readerDone != nil {
					select {
					case <-readerDone:
					case <-time.After(2 * time.Second):
						t.Error("cleanup: SSE body reader did not settle")
					}
				}
			}()

			path := "/api/ping"
			if holdStream {
				path = "/api/events"
			}
			request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(s.Port())+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept", "text/event-stream")
			response, err = client.Do(request)
			if err != nil {
				t.Fatalf("open live HTTP request: %v", err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("live HTTP status=%d, want 200", response.StatusCode)
			}
			var subscription chan Event
			if holdStream {
				if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
					t.Fatalf("SSE Content-Type=%q", got)
				}
				s.events.mu.Lock()
				registered := len(s.events.subs)
				for ch := range s.events.subs {
					subscription = ch
				}
				s.events.mu.Unlock()
				if registered != 1 {
					t.Fatalf("registered subscriptions=%d, want exactly the held HTTP stream", registered)
				}
				readerDone = make(chan struct{})
				go func() {
					_, readerErr = io.Copy(io.Discard, response.Body)
					close(readerDone)
				}()
			} else {
				if _, err := io.Copy(io.Discard, response.Body); err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
			}

			if tc.listenerErr != nil {
				s.GUIListenerOwner().errors <- tc.listenerErr
			} else {
				cancelServer()
			}
			drainErr := waitShutdownDrain(t, observed)
			startErr := waitRecoverySettlementServer(t, serverDone, serverResult)
			if holdStream {
				select {
				case <-readerDone:
					if readerErr != nil {
						t.Errorf("SSE did not end with clean EOF: %v", readerErr)
					}
				case <-time.After(2 * time.Second):
					t.Error("SSE body remained open after Server.Start returned")
				}
				retired := time.NewTimer(2 * time.Second)
				defer retired.Stop()
			retirement:
				for {
					select {
					case _, ok := <-subscription:
						if !ok {
							break retirement
						}
					case <-retired.C:
						t.Error("SSE subscription did not retire after Server shutdown")
						break retirement
					}
				}
			}
			if err := requestCtx.Err(); err != nil {
				t.Errorf("client context ended before shutdown receiving: %v", err)
			}
			t.Logf("held_stream=%t GUI_drain=%v Server_Start=%v", holdStream, drainErr, startErr)
			if drainErr != nil {
				t.Errorf("GUI HTTP graceful drain: %v, want nil", drainErr)
			}
			if tc.listenerErr != nil {
				if !errors.Is(startErr, tc.listenerErr) {
					t.Errorf("Server.Start: %v, want original listener failure %v", startErr, tc.listenerErr)
				}
			} else if startErr != nil {
				t.Errorf("Server.Start: %v, want nil", startErr)
			}
		})
	}
}
