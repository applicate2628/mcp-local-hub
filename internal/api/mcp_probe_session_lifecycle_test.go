package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminateMCPProbeSession_ClassifiesOnlyExactAllowedResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		want    mcpProbeSessionCleanupDisposition
		wantErr bool
	}{
		{name: "2xx terminated", status: http.StatusNoContent, want: mcpProbeSessionTerminated},
		{name: "404 already absent", status: http.StatusNotFound, want: mcpProbeSessionAlreadyAbsent},
		{name: "405 termination unsupported", status: http.StatusMethodNotAllowed, want: mcpProbeSessionTerminationUnsupported},
		{name: "redirect rejected", status: http.StatusFound, wantErr: true},
		{name: "other client error rejected", status: http.StatusBadRequest, wantErr: true},
		{name: "server error rejected", status: http.StatusBadGateway, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotID string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("method=%s, want DELETE", r.Method)
				}
				gotID = r.Header.Get("Mcp-Session-Id")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			result, err := terminateMCPProbeSession(srv.Client(), srv.URL, "synthetic-session", time.Second)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("result=%+v, want error", result)
				}
				var responseErr *mcpProbeSessionCleanupResponseError
				if !errors.As(err, &responseErr) {
					t.Fatalf("error=%T %v, want response error", err, err)
				}
				return
			}
			if err != nil || result.Disposition != tc.want || result.StatusCode != tc.status || gotID != "synthetic-session" {
				t.Fatalf("result=%+v err=%v id=%q", result, err, gotID)
			}
		})
	}
}

func TestTerminateMCPProbeSession_RejectsEmptyIDAndClosesBody(t *testing.T) {
	if _, err := terminateMCPProbeSession(http.DefaultClient, "http://127.0.0.1:1/mcp", "", time.Second); err == nil {
		t.Fatal("empty session id was accepted")
	}
	closed := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: closeTracker{ReadCloser: io.NopCloser(http.NoBody), closed: &closed}}, nil
	})}
	if _, err := terminateMCPProbeSession(client, "http://127.0.0.1:1/mcp", "synthetic-session", time.Second); err != nil || !closed {
		t.Fatalf("cleanup err=%v bodyClosed=%v", err, closed)
	}
}

func TestTerminateMCPProbeSession_RejectsRedirectWithoutForwardingSessionHeader(t *testing.T) {
	var destinationRequests atomic.Int32
	var destinationSessionID atomic.Value
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationRequests.Add(1)
		destinationSessionID.Store(r.Header.Get("Mcp-Session-Id"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	var originRequests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests.Add(1)
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	client := origin.Client()
	originalRedirectCalls := atomic.Int32{}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		originalRedirectCalls.Add(1)
		return nil
	}
	result, err := terminateMCPProbeSession(client, origin.URL, "synthetic-session", time.Second)
	var responseErr *mcpProbeSessionCleanupResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusFound || result.StatusCode != http.StatusFound {
		t.Fatalf("result=%+v err=%T %v, want typed original 302 response", result, err, err)
	}
	if got := originRequests.Load(); got != 1 {
		t.Fatalf("origin requests=%d, want exactly 1", got)
	}
	if got := destinationRequests.Load(); got != 0 {
		t.Fatalf("redirect destination requests=%d, want 0", got)
	}
	if got := originalRedirectCalls.Load(); got != 0 {
		t.Fatalf("caller redirect policy invoked %d times, want 0", got)
	}
	if got, _ := destinationSessionID.Load().(string); got != "" {
		t.Fatalf("redirect destination observed session id %q", got)
	}
}

func TestTerminateMCPProbeSession_ReportsDrainAndCloseFailures(t *testing.T) {
	drainErr := errors.New("synthetic drain failure")
	closeErr := errors.New("synthetic close failure")
	for _, tc := range []struct {
		name      string
		status    int
		drainErr  error
		closeErr  error
		wantReply bool
	}{
		{name: "drain only", status: http.StatusNoContent, drainErr: drainErr},
		{name: "close only", status: http.StatusNoContent, closeErr: closeErr},
		{name: "combined", status: http.StatusNoContent, drainErr: drainErr, closeErr: closeErr},
		{name: "unsafe status and combined", status: http.StatusBadGateway, drainErr: drainErr, closeErr: closeErr, wantReply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &failingCleanupBody{drainErr: tc.drainErr, closeErr: tc.closeErr}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body}, nil
			})}
			result, err := terminateMCPProbeSession(client, "http://127.0.0.1:1/mcp", "synthetic-session", time.Second)
			var bodyErr *mcpProbeSessionCleanupBodyError
			if !errors.As(err, &bodyErr) || bodyErr.StatusCode != tc.status || bodyErr.DrainErr != tc.drainErr || bodyErr.CloseErr != tc.closeErr {
				t.Fatalf("result=%+v err=%T %v bodyErr=%+v", result, err, err, bodyErr)
			}
			if body.closeCalls != 1 {
				t.Fatalf("Close calls=%d, want exactly 1", body.closeCalls)
			}
			var responseErr *mcpProbeSessionCleanupResponseError
			if got := errors.As(err, &responseErr); got != tc.wantReply {
				t.Fatalf("response error present=%v, want %v; err=%v", got, tc.wantReply, err)
			}
			if tc.drainErr != nil && !errors.Is(err, tc.drainErr) {
				t.Fatalf("joined error does not expose drain cause: %v", err)
			}
			if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
				t.Fatalf("joined error does not expose close cause: %v", err)
			}
		})
	}
}

type failingCleanupBody struct {
	drainErr   error
	closeErr   error
	readDone   bool
	closeCalls int
}

func (b *failingCleanupBody) Read([]byte) (int, error) {
	if b.readDone {
		return 0, io.EOF
	}
	b.readDone = true
	if b.drainErr != nil {
		return 0, b.drainErr
	}
	return 0, io.EOF
}

func (b *failingCleanupBody) Close() error {
	b.closeCalls++
	return b.closeErr
}

type closeTracker struct {
	io.ReadCloser
	closed *bool
}

func (c closeTracker) Close() error {
	*c.closed = true
	return c.ReadCloser.Close()
}
