package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestLazyProxyReadinessPublisher_OrganicToolsSuccessPublishesReady(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := seedReadinessTestRegistry(t)
	events := make(chan api.LazyProxyReadinessEventV1, 4)
	fatal := make(chan error, 1)
	cfg := LazyProxyConfig{
		WorkspaceKey:          "abcd1234",
		WorkspacePath:         "D:/test/ws",
		Language:              "python",
		BackendKind:           "mcp-language-server",
		Lifecycle:             f,
		RegistryPath:          regPath,
		InflightMinRetryGap:   time.Millisecond,
		ToolsCallDebounce:     time.Millisecond,
		ColdStartMaxProbation: -1,
	}
	cfg.Readiness = readinessRecorderConfig(events, fatal)
	p := NewLazyProxy(cfg)
	stopProxyOnCleanup(t, p)

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != 200 {
		t.Fatalf("tools/call status = %d, body=%s", rr.Code, rr.Body.String())
	}

	got := make([]api.LazyProxyReadinessEventV1, 0, 2)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(got) < 2 {
		select {
		case event := <-events:
			got = append(got, event)
		case err := <-fatal:
			t.Fatalf("unexpected fatal readiness failure: %v", err)
		case <-deadline.C:
			t.Fatalf("readiness events = %v, want starting then ready", got)
		}
	}
	if got[0].Transition != "starting" || got[1].Transition != "ready" || got[0].BackendEpoch == 0 || got[1].BackendEpoch != got[0].BackendEpoch {
		t.Fatalf("readiness events = %+v, want same-epoch starting then ready", got)
	}
}

func TestLazyProxyReadinessPublisher_OrganicSuccessArms(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*fakeLifecycle)
		invoke     func(*testing.T, http.Handler) *httptest.ResponseRecorder
		wantStatus int
	}{
		{name: "tools call", invoke: func(t *testing.T, h http.Handler) *httptest.ResponseRecorder { return postRPC(t, h, "tools/call", 1) }, wantStatus: http.StatusOK},
		{name: "other forwarded request", invoke: func(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
			return postRPC(t, h, "workspace/symbol", 1)
		}, wantStatus: http.StatusOK},
		{name: "forwarded document notification", invoke: func(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
			return postDocNotification(t, h, "textDocument/didOpen", "file:///D:/test/ws/a.py")
		}, wantStatus: http.StatusAccepted},
		{name: "nil transport error with JSON-RPC error payload", prepare: func(f *fakeLifecycle) {
			f.sendRPCError = &JSONRPCError{Code: -32001, Message: "backend rejected request"}
		}, invoke: func(t *testing.T, h http.Handler) *httptest.ResponseRecorder { return postRPC(t, h, "tools/call", 1) }, wantStatus: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLifecycle{kind: "mcp-language-server"}
			if tc.prepare != nil {
				tc.prepare(f)
			}
			events := make(chan api.LazyProxyReadinessEventV1, 4)
			fatal := make(chan error, 1)
			p := newReadinessTestProxy(t, f, events, fatal)
			rr := tc.invoke(t, p.Handler())
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			got := collectReadinessEvents(t, events, 2)
			if got[0].Transition != "starting" || got[1].Transition != "ready" || got[0].BackendEpoch != got[1].BackendEpoch {
				t.Fatalf("events = %+v, want same-epoch starting then ready", got)
			}
			select {
			case err := <-fatal:
				t.Fatalf("unexpected fatal readiness failure: %v", err)
			default:
			}
		})
	}
}

func TestLazyProxyReadinessPublisher_StartingAcknowledgedBeforeEndpointExposure(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	starting := make(chan struct{})
	release := make(chan struct{})
	events := make(chan api.LazyProxyReadinessEventV1, 4)
	readiness := readinessRecorderConfig(events, make(chan error, 1))
	baseCommit := readiness.Commit
	readiness.Commit = func(ctx context.Context, event api.LazyProxyReadinessEventV1) (api.LazyProxyReadinessReceiptV1, error) {
		if event.Transition == "starting" {
			select {
			case <-starting:
			default:
				close(starting)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return api.LazyProxyReadinessReceiptV1{}, ctx.Err()
			}
		}
		return baseCommit(ctx, event)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Lifecycle: f,
		RegistryPath: seedReadinessTestRegistry(t), MaterializeWaitBudget: time.Second,
		ColdRequestHoldCeiling: 2 * time.Second, ColdStartMaxProbation: -1,
		Readiness: readiness,
	})
	stopProxyOnCleanup(t, p)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- postRPC(t, p.Handler(), "tools/call", 1) }()
	select {
	case <-starting:
	case <-time.After(time.Second):
		t.Fatal("starting commit was not attempted")
	}
	p.mu.Lock()
	exposed := p.endpoint != nil
	p.mu.Unlock()
	if exposed {
		t.Fatal("endpoint exposed before starting acknowledgement")
	}
	select {
	case <-result:
		t.Fatal("forward returned before starting acknowledgement")
	default:
	}
	close(release)
	select {
	case rr := <-result:
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("forward did not resume after starting acknowledgement")
	}
	assertReadinessTransitions(t, events, "starting", "ready")
}

func TestLazyProxyReadinessPublisher_NonOrganicPathsDoNotPublishReady(t *testing.T) {
	t.Run("synthetic and absorbed", func(t *testing.T) {
		f := &fakeLifecycle{kind: "mcp-language-server"}
		events := make(chan api.LazyProxyReadinessEventV1, 8)
		p := newReadinessTestProxy(t, f, events, make(chan error, 1))
		for i, method := range []string{"initialize", "tools/list", "ping", "notifications/progress"} {
			rr := postRPC(t, p.Handler(), method, i+1)
			if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted {
				t.Fatalf("%s status = %d; body=%s", method, rr.Code, rr.Body.String())
			}
		}
		if len(events) != 0 || f.materializeCount.Load() != 0 {
			t.Fatalf("synthetic paths materialized=%d events=%d, want zero", f.materializeCount.Load(), len(events))
		}
	})

	t.Run("pre-delivery saturation refusal", func(t *testing.T) {
		f := &fakeLifecycle{kind: "mcp-language-server", sendRequestErr: ErrTooManyPending}
		events := make(chan api.LazyProxyReadinessEventV1, 4)
		p := newReadinessTestProxy(t, f, events, make(chan error, 1))
		rr := postRPC(t, p.Handler(), "tools/call", 1)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
		}
		assertReadinessTransitions(t, events, "starting")
	})

	t.Run("delivered notification without response", func(t *testing.T) {
		f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
		events := make(chan api.LazyProxyReadinessEventV1, 4)
		p := newReadinessTestProxyWithBudget(t, f, events, make(chan error, 1), 10*time.Millisecond)
		rr := postDocNotification(t, p.Handler(), "textDocument/didOpen", "file:///D:/test/ws/a.py")
		if rr.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
		}
		assertReadinessTransitions(t, events, "starting")
	})

	t.Run("client cancellation", func(t *testing.T) {
		f := &fakeLifecycle{kind: "mcp-language-server"}
		events := make(chan api.LazyProxyReadinessEventV1, 8)
		p := newReadinessTestProxy(t, f, events, make(chan error, 1))
		if rr := postRPC(t, p.Handler(), "tools/call", 1); rr.Code != http.StatusOK {
			t.Fatalf("warmup status = %d; body=%s", rr.Code, rr.Body.String())
		}
		_ = collectReadinessEvents(t, events, 2)
		f.sendRequestErr = context.Canceled
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		p.Handler().ServeHTTP(rr, req)
		if len(events) != 0 {
			t.Fatalf("client-canceled request published %d additional events", len(events))
		}
	})
}

func TestLazyProxyReadinessPublisher_LifecycleOrdering(t *testing.T) {
	t.Run("idle and rematerialize", func(t *testing.T) {
		f := &fakeLifecycle{kind: "mcp-language-server"}
		events := make(chan api.LazyProxyReadinessEventV1, 10)
		p := newReadinessTestProxy(t, f, events, make(chan error, 1))
		if rr := postRPC(t, p.Handler(), "tools/call", 1); rr.Code != http.StatusOK {
			t.Fatalf("first call status = %d", rr.Code)
		}
		p.cfg.IdleBackendTTL = time.Second
		p.reapIdleBackend(time.Now().UTC().Add(2 * time.Second))
		if rr := postRPC(t, p.Handler(), "tools/call", 2); rr.Code != http.StatusOK {
			t.Fatalf("second call status = %d; body=%s", rr.Code, rr.Body.String())
		}
		got := collectReadinessEvents(t, events, 5)
		want := []struct {
			transition string
			epoch      uint64
		}{{"starting", 1}, {"ready", 1}, {"idle", 2}, {"starting", 3}, {"ready", 3}}
		for i := range want {
			if got[i].Transition != want[i].transition || got[i].BackendEpoch != want[i].epoch {
				t.Fatalf("events[%d] = %+v, want %s epoch %d; all=%+v", i, got[i], want[i].transition, want[i].epoch, got)
			}
		}
	})

	t.Run("send failure", func(t *testing.T) {
		f := &fakeLifecycle{kind: "mcp-language-server", sendRequestErr: errors.New("stdio failed")}
		events := make(chan api.LazyProxyReadinessEventV1, 4)
		p := newReadinessTestProxy(t, f, events, make(chan error, 1))
		_ = postRPC(t, p.Handler(), "tools/call", 1)
		got := collectReadinessEvents(t, events, 2)
		if got[0].Transition != "starting" || got[0].BackendEpoch != 1 || got[1].Transition != "failed" || got[1].BackendEpoch != 2 {
			t.Fatalf("events = %+v, want starting/1 then failed/2", got)
		}
	})

	t.Run("probation reap", func(t *testing.T) {
		f := &fakeLifecycle{kind: "mcp-language-server"}
		events := make(chan api.LazyProxyReadinessEventV1, 4)
		p := newReadinessTestProxy(t, f, events, make(chan error, 1))
		_, _, err := p.ensureMaterialized(context.Background())
		if err != nil {
			t.Fatalf("ensure materialized: %v", err)
		}
		p.endBackendRequest()
		p.cfg.ColdStartMaxProbation = time.Second
		p.reapWedgedProbation(time.Now().UTC().Add(2 * time.Second))
		got := collectReadinessEvents(t, events, 2)
		if got[0].Transition != "starting" || got[0].BackendEpoch != 1 || got[1].Transition != "failed" || got[1].BackendEpoch != 2 {
			t.Fatalf("events = %+v, want starting/1 then failed/2", got)
		}
	})
}

func TestLazyProxyReadinessPublisher_SupersededInvalidationIsHarmless(t *testing.T) {
	fatal := make(chan error, 1)
	commits := make(chan api.LazyProxyReadinessEventV1, 2)
	cfg := readinessRecorderConfig(commits, fatal)
	cfg.Commit = func(_ context.Context, event api.LazyProxyReadinessEventV1) (api.LazyProxyReadinessReceiptV1, error) {
		commits <- event
		return api.LazyProxyReadinessReceiptV1{
			Disposition: "superseded", WrapperPID: event.WrapperPID,
			WrapperStartedAt: event.WrapperStartedAt, PIDGeneration: 7,
			BackendEpoch: event.BackendEpoch + 1, Transition: "ready",
		}, nil
	}
	p := newLazyProxyReadinessPublisher(cfg, nil)
	t.Cleanup(p.stop)
	if err := p.publish(2, "failed", nil, -1); err != nil {
		t.Fatalf("superseded failed publication: %v", err)
	}
	select {
	case err := <-fatal:
		t.Fatalf("superseded invalidation signaled fatal: %v", err)
	default:
	}
}

func TestLazyProxyReadinessPublisher_MailboxKeepsGreatestPendingState(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	commits := make(chan api.LazyProxyReadinessEventV1, 4)
	var calls atomic.Int32
	cfg := readinessRecorderConfig(commits, make(chan error, 1))
	cfg.Commit = func(ctx context.Context, event api.LazyProxyReadinessEventV1) (api.LazyProxyReadinessReceiptV1, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return api.LazyProxyReadinessReceiptV1{}, ctx.Err()
			}
		}
		commits <- event
		return api.LazyProxyReadinessReceiptV1{
			Disposition: "accepted", WrapperPID: event.WrapperPID,
			WrapperStartedAt: event.WrapperStartedAt, PIDGeneration: 7,
			BackendEpoch: event.BackendEpoch, Transition: event.Transition,
		}, nil
	}
	p := newLazyProxyReadinessPublisher(cfg, nil)
	t.Cleanup(p.stop)
	_ = p.publish(1, "starting", nil, 0)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first commit did not start")
	}
	_ = p.publish(2, "failed", nil, 0)
	_ = p.publish(3, "starting", nil, 0)
	close(releaseFirst)
	got := collectReadinessEvents(t, commits, 2)
	if got[0].BackendEpoch != 1 || got[0].Transition != "starting" || got[1].BackendEpoch != 3 || got[1].Transition != "starting" {
		t.Fatalf("commits = %+v, want starting/1 then greatest pending starting/3", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("commit calls = %d, want 2", calls.Load())
	}
}

func TestLazyProxyReadinessPublisher_CurrentInvalidationFailureRetriesThenSignalsFatal(t *testing.T) {
	fatal := make(chan error, 1)
	var attempts atomic.Int32
	cfg := readinessRecorderConfig(make(chan api.LazyProxyReadinessEventV1, 1), fatal)
	cfg.Commit = func(context.Context, api.LazyProxyReadinessEventV1) (api.LazyProxyReadinessReceiptV1, error) {
		attempts.Add(1)
		return api.LazyProxyReadinessReceiptV1{}, errors.New("transport unavailable")
	}
	p := newLazyProxyReadinessPublisher(cfg, nil)
	t.Cleanup(p.stop)
	if err := p.publish(2, "failed", nil, -1); err == nil {
		t.Fatal("current failed invalidation returned nil")
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want exactly 2", attempts.Load())
	}
	select {
	case err := <-fatal:
		if !strings.Contains(err.Error(), "lazy-readiness-publish-failed") {
			t.Fatalf("fatal error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("current invalidation failure did not signal composition")
	}
}

func TestLazyProxyReadinessPublisher_StaleInvalidationFailureDoesNotRetryOrSignalFatal(t *testing.T) {
	failedStarted := make(chan struct{})
	releaseFailed := make(chan struct{})
	startingThree := make(chan struct{})
	fatal := make(chan error, 1)
	var failedAttempts atomic.Int32
	cfg := readinessRecorderConfig(make(chan api.LazyProxyReadinessEventV1, 1), fatal)
	cfg.Commit = func(_ context.Context, event api.LazyProxyReadinessEventV1) (api.LazyProxyReadinessReceiptV1, error) {
		if event.BackendEpoch == 2 {
			failedAttempts.Add(1)
			close(failedStarted)
			<-releaseFailed
			return api.LazyProxyReadinessReceiptV1{}, errors.New("old invalidation transport failure")
		}
		close(startingThree)
		return api.LazyProxyReadinessReceiptV1{
			Disposition: "accepted", WrapperPID: event.WrapperPID,
			WrapperStartedAt: event.WrapperStartedAt, PIDGeneration: 7,
			BackendEpoch: event.BackendEpoch, Transition: event.Transition,
		}, nil
	}
	p := newLazyProxyReadinessPublisher(cfg, nil)
	t.Cleanup(p.stop)
	_ = p.publish(2, "failed", nil, 0)
	select {
	case <-failedStarted:
	case <-time.After(time.Second):
		t.Fatal("failed invalidation did not start")
	}
	_ = p.publish(3, "starting", nil, 0)
	close(releaseFailed)
	select {
	case <-startingThree:
	case <-time.After(time.Second):
		t.Fatal("newer starting state was not committed")
	}
	if failedAttempts.Load() != 1 {
		t.Fatalf("stale invalidation attempts = %d, want 1", failedAttempts.Load())
	}
	select {
	case err := <-fatal:
		t.Fatalf("stale invalidation signaled fatal: %v", err)
	default:
	}
}

func TestLazyProxyReadinessPublisher_StopCancelsAndJoinsWorker(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	cfg := readinessRecorderConfig(make(chan api.LazyProxyReadinessEventV1, 1), make(chan error, 1))
	cfg.Commit = func(ctx context.Context, _ api.LazyProxyReadinessEventV1) (api.LazyProxyReadinessReceiptV1, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return api.LazyProxyReadinessReceiptV1{}, ctx.Err()
	}
	p := newLazyProxyReadinessPublisher(cfg, nil)
	if err := p.publish(1, "starting", nil, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("publisher worker did not start commit")
	}
	before := time.Now()
	p.stop()
	if elapsed := time.Since(before); elapsed > time.Second {
		t.Fatalf("Stop join took %s after cancel-aware commit", elapsed)
	}
	select {
	case <-p.done:
	default:
		t.Fatal("publisher worker not joined")
	}
}

func TestLazyProxyReadinessPublisher_BackendExitInvalidatesCurrentEpoch(t *testing.T) {
	exit := make(chan struct{})
	ep := &readinessExitEndpoint{exit: exit}
	lifecycle := &readinessExitLifecycle{endpoint: ep}
	events := make(chan api.LazyProxyReadinessEventV1, 6)
	p := newReadinessTestProxyWithLifecycle(t, lifecycle, events, make(chan error, 1), 50*time.Millisecond)
	if rr := postRPC(t, p.Handler(), "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	close(exit)
	got := collectReadinessEvents(t, events, 3)
	if got[2].Transition != "failed" || got[2].BackendEpoch != 2 {
		t.Fatalf("events = %+v, want backend exit failed epoch 2", got)
	}
	if lifecycle.stops.Load() != 1 || !ep.closed.Load() {
		t.Fatalf("backend exit cleanup: stops=%d endpoint_closed=%v, want 1/true", lifecycle.stops.Load(), ep.closed.Load())
	}
}

func seedReadinessTestRegistry(t *testing.T) string {
	t.Helper()
	regPath := t.TempDir() + "/r.yaml"
	reg := api.NewRegistry(regPath)
	reg.Put(api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python",
	})
	if err := reg.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	return regPath
}

func readinessRecorderConfig(events chan<- api.LazyProxyReadinessEventV1, fatal chan error) LazyProxyReadinessConfig {
	return LazyProxyReadinessConfig{
		TaskName: "mcp-local-hub-lsp-abcd1234-python", WrapperPID: 4242,
		WrapperStartedAt: time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC),
		Fatal:            fatal,
		Commit: func(_ context.Context, event api.LazyProxyReadinessEventV1) (api.LazyProxyReadinessReceiptV1, error) {
			events <- event
			return api.LazyProxyReadinessReceiptV1{
				Disposition: "accepted", WrapperPID: event.WrapperPID,
				WrapperStartedAt: event.WrapperStartedAt, PIDGeneration: 7,
				BackendEpoch: event.BackendEpoch, Transition: event.Transition,
			}, nil
		},
	}
}

func newReadinessTestProxy(t *testing.T, lifecycle *fakeLifecycle, events chan<- api.LazyProxyReadinessEventV1, fatal chan error) *LazyProxy {
	t.Helper()
	return newReadinessTestProxyWithBudget(t, lifecycle, events, fatal, 50*time.Millisecond)
}

func newReadinessTestProxyWithBudget(t *testing.T, lifecycle *fakeLifecycle, events chan<- api.LazyProxyReadinessEventV1, fatal chan error, budget time.Duration) *LazyProxy {
	t.Helper()
	return newReadinessTestProxyWithLifecycle(t, lifecycle, events, fatal, budget)
}

func newReadinessTestProxyWithLifecycle(t *testing.T, lifecycle BackendLifecycle, events chan<- api.LazyProxyReadinessEventV1, fatal chan error, budget time.Duration) *LazyProxy {
	t.Helper()
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Lifecycle: lifecycle,
		RegistryPath: seedReadinessTestRegistry(t), InflightMinRetryGap: time.Millisecond,
		ToolsCallDebounce: time.Millisecond, MaterializeWaitBudget: budget,
		ColdRequestHoldCeiling: time.Second, ColdStartMaxProbation: -1,
		Readiness: readinessRecorderConfig(events, fatal),
	})
	stopProxyOnCleanup(t, p)
	return p
}

func collectReadinessEvents(t *testing.T, events <-chan api.LazyProxyReadinessEventV1, count int) []api.LazyProxyReadinessEventV1 {
	t.Helper()
	got := make([]api.LazyProxyReadinessEventV1, 0, count)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for len(got) < count {
		select {
		case event := <-events:
			got = append(got, event)
		case <-timer.C:
			t.Fatalf("events = %+v, want %d total", got, count)
		}
	}
	return got
}

func assertReadinessTransitions(t *testing.T, events <-chan api.LazyProxyReadinessEventV1, want ...string) {
	t.Helper()
	got := collectReadinessEvents(t, events, len(want))
	for i := range want {
		if got[i].Transition != want[i] {
			t.Fatalf("events[%d] = %s, want %s; all=%+v", i, got[i].Transition, want[i], got)
		}
	}
	if extra := len(events); extra != 0 {
		t.Fatalf("unexpected additional readiness events: %d", extra)
	}
}

type readinessExitEndpoint struct {
	exit   <-chan struct{}
	closed atomic.Bool
}

func (e *readinessExitEndpoint) SendRequest(_ context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: []byte(`{"ok":true}`)}, nil
}
func (e *readinessExitEndpoint) Close() error                   { e.closed.Store(true); return nil }
func (e *readinessExitEndpoint) BackendExited() <-chan struct{} { return e.exit }

type readinessExitLifecycle struct {
	endpoint *readinessExitEndpoint
	stops    atomic.Int32
}

func (l *readinessExitLifecycle) Kind() string { return "mcp-language-server" }
func (l *readinessExitLifecycle) Materialize(context.Context) (MCPEndpoint, error) {
	return l.endpoint, nil
}
func (l *readinessExitLifecycle) Stop() error { l.stops.Add(1); return nil }
