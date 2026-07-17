package gui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestRestartV3_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.events.DisableGUIEventLog = true
	hubComp := liveRestartTestComp(3439)
	s.hubEndpointGateFn = func(*api.API) bool { return true }
	s.startHubMcpListenerFn = func(context.Context, bool, *api.API, startHubMcpListenerOptions) (*HubListenerComponents, error) {
		return hubComp, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(ctx, ready) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Start did not signal GUI readiness")
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.hubMcpComp.Load() != hubComp && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.hubMcpComp.Load(); got != hubComp {
		t.Fatalf("hub component = %#v, want startup component %#v", got, hubComp)
	}

	eventsCtx, cancelEvents := context.WithTimeout(context.Background(), 5*time.Second)
	request, err := http.NewRequestWithContext(eventsCtx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/events", s.Port()), nil)
	if err != nil {
		t.Fatalf("new SSE request: %v", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatalf("open SSE subscription: %v", err)
	}
	t.Cleanup(func() {
		cancelEvents()
		_ = response.Body.Close()
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", response.StatusCode)
	}

	owner := s.GUIListenerOwner()
	oldPort := s.Port()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	if err := owner.CloseListener(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("CloseListener: %v", err)
	}
	closeCancel()
	select {
	case err := <-startDone:
		t.Fatalf("listener-only close returned full Server.Start lifecycle: %v", err)
	default:
	}
	if got := s.hubMcpComp.Load(); got != hubComp || !got.Alive() {
		t.Fatalf("hub component after GUI listener close = %#v alive=%v, want original live component", got, got != nil && got.Alive())
	}

	eventSeen := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if scanner.Text() == "event: listener-owner-test" {
				eventSeen <- nil
				return
			}
		}
		eventSeen <- scanner.Err()
	}()
	s.Broadcaster().Publish(Event{Type: "listener-owner-test", Body: map[string]any{"alive": true}})
	select {
	case err := <-eventSeen:
		if err != nil {
			t.Fatalf("SSE subscription ended after listener-only close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("existing SSE subscription did not survive GUI listener close")
	}

	bindCtx, bindCancel := context.WithTimeout(context.Background(), time.Second)
	bound, err := owner.BindForRecovery(bindCtx, oldPort)
	bindCancel()
	if err != nil {
		t.Fatalf("BindForRecovery(%d): %v", oldPort, err)
	}
	if err := owner.ServeFull(bound, s.httpHandler()); err != nil {
		t.Fatalf("ServeFull after rebind: %v", err)
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
	pingRequest, err := http.NewRequestWithContext(pingCtx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/ping", oldPort), nil)
	if err != nil {
		pingCancel()
		t.Fatalf("new ping request: %v", err)
	}
	ping, err := (&http.Client{}).Do(pingRequest)
	pingCancel()
	if err != nil {
		t.Fatalf("ping rebound listener: %v", err)
	}
	_, _ = io.Copy(io.Discard, ping.Body)
	_ = ping.Body.Close()
	if ping.StatusCode != http.StatusOK {
		t.Fatalf("rebound ping status = %d, want 200", ping.StatusCode)
	}

	cancelEvents()
	cancel()
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Server.Start shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Start did not stop after context cancellation")
	}
}

func TestGUIListenerOwner_EnterGraceRejectsNewMutatorsAndDrainsAdmitted(t *testing.T) {
	owner := NewGUIListenerOwner(10 * time.Second)
	bindCtx, bindCancel := context.WithTimeout(context.Background(), time.Second)
	bound, err := owner.BindForRecovery(bindCtx, 0)
	bindCancel()
	if err != nil {
		t.Fatalf("BindForRecovery: %v", err)
	}

	mutatorEntered := make(chan struct{})
	releaseMutator := make(chan struct{})
	full := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			close(mutatorEntered)
			<-releaseMutator
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := owner.ServeFull(bound, full); err != nil {
		t.Fatalf("ServeFull: %v", err)
	}
	select {
	case <-owner.Activated():
	case <-time.After(time.Second):
		t.Fatal("ServeFull did not close Activated gate")
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/mutate", bound.Addr().(*net.TCPAddr).Port)
	client := &http.Client{Timeout: time.Second}
	firstDone := make(chan error, 1)
	go func() {
		resp, err := client.Post(url, "text/plain", strings.NewReader("first"))
		if err == nil {
			_ = resp.Body.Close()
		}
		firstDone <- err
	}()
	select {
	case <-mutatorEntered:
	case <-time.After(time.Second):
		t.Fatal("first mutator did not enter full handler")
	}

	grace := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "GUI restart in progress", http.StatusServiceUnavailable)
	})
	graceDone := make(chan error, 1)
	graceCtx, graceCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer graceCancel()
	go func() { graceDone <- owner.EnterGrace(graceCtx, grace) }()

	deadline := time.Now().Add(time.Second)
	for {
		resp, postErr := client.Post(url, "text/plain", strings.NewReader("new"))
		if postErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusServiceUnavailable {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("new mutator was not rejected by grace handler; last error=%v", postErr)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-graceDone:
		t.Fatalf("EnterGrace returned before admitted mutator drained: %v", err)
	default:
	}

	close(releaseMutator)
	if err := <-firstDone; err != nil {
		t.Fatalf("admitted mutator request: %v", err)
	}
	select {
	case err := <-graceDone:
		if err != nil {
			t.Fatalf("EnterGrace: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EnterGrace did not finish after admitted mutator drained")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := owner.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestGUIListenerOwner_ShutdownClosesBoundButUnservedListenerFreeingPort proves the
// Shutdown(done == nil) fix (Sol commission finding): a generation bound via
// BindForRecovery but never adopted/served must be closed by Shutdown so its port is
// released for a subsequent same-port recovery bind. net/http Server.Shutdown does
// NOT close an unserved listener, so without the explicit close the port would stay
// bound and the recovery bind below would fail.
func TestGUIListenerOwner_ShutdownClosesBoundButUnservedListenerFreeingPort(t *testing.T) {
	owner := NewGUIListenerOwner(10 * time.Second)
	bindCtx, bindCancel := context.WithTimeout(context.Background(), time.Second)
	bound, err := owner.BindForRecovery(bindCtx, 0)
	bindCancel()
	if err != nil {
		t.Fatalf("BindForRecovery: %v", err)
	}
	port := bound.Addr().(*net.TCPAddr).Port

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := owner.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown of an unserved bound generation: %v", err)
	}

	rebind, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d still bound after Shutdown of an unserved listener (leak): %v", port, err)
	}
	_ = rebind.Close()
}
