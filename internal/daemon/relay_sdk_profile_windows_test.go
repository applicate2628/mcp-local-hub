//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type relaySDKProfileMemory struct {
	HeapAlloc, HeapInuse, HeapObjects, TotalAlloc, Mallocs uint64
	WorkingSet, PrivateWorkingSet, PrivateCommit           uint64
}

func readRelaySDKProfileMemory() (relaySDKProfileMemory, error) {
	var heap runtime.MemStats
	runtime.ReadMemStats(&heap)
	process, err := observeRelayPrivateMemory(os.Getpid())
	return relaySDKProfileMemory{
		heap.HeapAlloc, heap.HeapInuse, heap.HeapObjects, heap.TotalAlloc, heap.Mallocs,
		process.WorkingSet, process.PrivateWorkingSet, process.PrivateCommit,
	}, err
}

type relaySDKProfilePeer struct {
	session *mcp.ClientSession
	input   *io.PipeWriter
	output  *io.PipeReader
	done    chan error
	cancel  context.CancelFunc
	once    sync.Once
	err     error
}

func newRelaySDKProfilePeer(t *testing.T, ctx context.Context, url string, client *http.Client) *relaySDKProfilePeer {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	runCtx, cancel := context.WithCancel(ctx)
	p := &relaySDKProfilePeer{input: inputWriter, output: outputReader, done: make(chan error, 1), cancel: cancel}
	r := &HTTPToStdioRelay{URL: url, Stdin: inputReader, Stdout: outputWriter, Stderr: io.Discard,
		InterruptStdin: inputWriter.Close, HTTPClient: client}
	go func() {
		err := r.Run(runCtx)
		_ = outputWriter.CloseWithError(err)
		_ = inputReader.Close()
		p.done <- err
	}()
	t.Cleanup(func() {
		if err := p.close(); err != nil {
			t.Errorf("owned Relay lifetime did not settle cleanly: %v", err)
		}
	})
	sdk := mcp.NewClient(&mcp.Implementation{Name: "relay-profile-consumer", Version: "1"}, nil)
	var err error
	p.session, err = sdk.Connect(ctx, &mcp.IOTransport{Reader: outputReader, Writer: inputWriter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *relaySDKProfilePeer) close() error {
	p.once.Do(func() {
		// End owned stdin while keeping the SDK reader alive for late accepted
		// responses. SDK Close closes that reader first, so it must follow Relay
		// drain, GET/POST joins and HTTP session deletion.
		_ = p.input.Close()
		select {
		case err := <-p.done:
			p.err = errors.Join(p.err, err)
		case <-time.After(5 * time.Second):
			p.cancel()
			_ = p.output.Close()
			select {
			case err := <-p.done:
				p.err = errors.Join(p.err, errors.New("Relay exceeded graceful close deadline"), err)
			case <-time.After(5 * time.Second):
				p.err = errors.Join(p.err, errors.New("Relay did not settle after owned pipe interruption"))
			}
		}
		p.cancel()
		if p.session != nil {
			p.err = errors.Join(p.err, p.session.Close())
		}
		_ = p.output.Close()
	})
	return p.err
}

// This opt-in profile uses the real SDK on both sides of Relay. The tool's data
// is synthetic; the SDK framing, HTTP session, dispatch, cancellation and close
// paths are real. Memory includes this entire test process (client, server,
// Relay and measurement driver), not an attributed Relay-only working set.
func TestRelaySDKConsumerProfile(t *testing.T) {
	if os.Getenv("MCPHUB_RELAY_SDK_PROFILE") != "1" {
		t.Skip("opt-in real SDK consumer measurement")
	}
	for _, cfg := range []struct{ clients, workers, calls, bytes int }{
		{1, 1, 64, 1024}, {8, 1, 32, 1024}, {4, 4, 16, 65536},
	} {
		t.Run(fmt.Sprintf("clients_%d/workers_%d/bytes_%d", cfg.clients, cfg.workers, cfg.bytes), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			workers := cfg.clients * cfg.workers
			payload := strings.Repeat("x", cfg.bytes)
			heldEntered, cancelEntered := make(chan struct{}, workers), make(chan struct{}, cfg.clients)
			heldRelease, cancelFinished := make(chan struct{}), make(chan struct{}, cfg.clients)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(heldRelease) }) }
			defer release()
			var active, maxActive, calls, httpActive, gets, deletes atomic.Int64
			server := mcp.NewServer(&mcp.Implementation{Name: "relay-profile-server", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
				func(callCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					n := active.Add(1)
					cancelled := false
					defer func() {
						active.Add(-1)
						if cancelled {
							cancelFinished <- struct{}{}
						}
					}()
					for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
					}
					calls.Add(1)
					var args struct {
						Sequence string `json:"sequence"`
						Payload  string `json:"payload"`
						Hold     bool   `json:"hold"`
						Cancel   bool   `json:"cancel"`
					}
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return nil, err
					}
					if args.Cancel {
						cancelEntered <- struct{}{}
						<-callCtx.Done()
						cancelled = true
						return nil, callCtx.Err()
					}
					if args.Hold {
						heldEntered <- struct{}{}
						select {
						case <-heldRelease:
						case <-callCtx.Done():
							return nil, callCtx.Err()
						}
					}
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: args.Sequence + ":" + args.Payload}}}, nil
				})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpActive.Add(1)
				defer httpActive.Add(-1)
				if r.Method == http.MethodGet {
					gets.Add(1)
				}
				if r.Method == http.MethodDelete {
					deletes.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(httpServer.Close)
			t.Cleanup(httpServer.CloseClientConnections)
			transport := &http.Transport{Proxy: nil, MaxIdleConns: 64, MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport}
			peers := make([]*relaySDKProfilePeer, cfg.clients)
			call := func(callCtx context.Context, worker, iteration int, hold bool) error {
				sequence := fmt.Sprintf("worker-%d-call-%d", worker, iteration)
				result, err := peers[worker/cfg.workers].session.CallTool(callCtx, &mcp.CallToolParams{
					Name: "echo", Arguments: map[string]any{"sequence": sequence, "payload": payload, "hold": hold},
				})
				if err != nil {
					return err
				}
				if result.IsError || len(result.Content) != 1 {
					return fmt.Errorf("invalid result for %s", sequence)
				}
				text, ok := result.Content[0].(*mcp.TextContent)
				if !ok || text.Text != sequence+":"+payload {
					return fmt.Errorf("lost, duplicated or corrupted response for %s", sequence)
				}
				return nil
			}
			for i := range peers {
				peers[i] = newRelaySDKProfilePeer(t, ctx, httpServer.URL, client)
				if err := call(ctx, i*cfg.workers, -1, false); err != nil {
					t.Fatal(err)
				}
			}
			memory := func() relaySDKProfileMemory {
				t.Helper()
				m, err := readRelaySDKProfileMemory()
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			wait := func(ch <-chan struct{}, n int) {
				t.Helper()
				for range n {
					select {
					case <-ch:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			}
			runtime.GC()
			baseline := memory()
			heldDone := make(chan error, workers)
			for worker := range workers {
				go func() { heldDone <- call(ctx, worker, -2, true) }()
			}
			wait(heldEntered, workers)
			runtime.GC()
			held := memory()
			release()
			for range workers {
				if err := <-heldDone; err != nil {
					t.Fatal(err)
				}
			}
			runtime.GC()
			beforeCalls := memory()
			// Sampling is deliberately part of the measured process. These maxima
			// are lower bounds on transient peaks, not OS lifetime high-water marks.
			stopSamples := make(chan struct{})
			samplesDone := make(chan struct{})
			peak, samples, sampleErr := beforeCalls, 0, error(nil)
			go func() {
				defer close(samplesDone)
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopSamples:
						return
					case <-ticker.C:
						m, err := readRelaySDKProfileMemory()
						if err != nil {
							sampleErr = err
							return
						}
						samples++
						peak.HeapAlloc = max(peak.HeapAlloc, m.HeapAlloc)
						peak.HeapInuse = max(peak.HeapInuse, m.HeapInuse)
						peak.WorkingSet = max(peak.WorkingSet, m.WorkingSet)
						peak.PrivateWorkingSet = max(peak.PrivateWorkingSet, m.PrivateWorkingSet)
						peak.PrivateCommit = max(peak.PrivateCommit, m.PrivateCommit)
					}
				}
			}()
			latencies := make([]int64, workers*cfg.calls)
			failures := make(chan error, workers)
			started := time.Now()
			for worker := range workers {
				go func() {
					for iteration := range cfg.calls {
						start := time.Now()
						if err := call(ctx, worker, iteration, false); err != nil {
							failures <- err
							return
						}
						latencies[worker*cfg.calls+iteration] = time.Since(start).Nanoseconds()
					}
					failures <- nil
				}()
			}
			var callsErr error
			for range workers {
				callsErr = errors.Join(callsErr, <-failures)
			}
			elapsed := time.Since(started)
			close(stopSamples)
			<-samplesDone
			afterCalls := memory()
			runtime.GC()
			retainedOpen := memory()
			if err := errors.Join(callsErr, sampleErr); err != nil {
				t.Fatal(err)
			}
			cancelCtx, cancelCalls := context.WithCancel(ctx)
			defer cancelCalls()
			cancelDone := make(chan error, cfg.clients)
			for _, peer := range peers {
				go func() {
					_, err := peer.session.CallTool(cancelCtx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"cancel": true}})
					cancelDone <- err
				}()
			}
			wait(cancelEntered, cfg.clients)
			cancelStart := time.Now()
			cancelCalls()
			for range cfg.clients {
				if err := <-cancelDone; !errors.Is(err, context.Canceled) {
					t.Fatalf("SDK cancellation result: %v", err)
				}
			}
			wait(cancelFinished, cfg.clients)
			cancelLatency := time.Since(cancelStart)
			for _, peer := range peers {
				if err := peer.close(); err != nil {
					t.Fatal(err)
				}
			}
			transport.CloseIdleConnections()
			httpServer.CloseClientConnections()
			httpServer.Close()
			serverSessions := 0
			for range server.Sessions() {
				serverSessions++
			}
			if httpActive.Load() != 0 || active.Load() != 0 || deletes.Load() != int64(cfg.clients) || serverSessions != 0 {
				t.Fatalf("unsettled HTTP=%d tools=%d DELETE=%d sessions=%d", httpActive.Load(), active.Load(), deletes.Load(), serverSessions)
			}
			wantCalls := int64(cfg.clients + workers + workers*cfg.calls + cfg.clients)
			if calls.Load() != wantCalls || maxActive.Load() != int64(workers) {
				t.Fatalf("tool calls=%d want=%d max active=%d want=%d", calls.Load(), wantCalls, maxActive.Load(), workers)
			}
			runtime.GC()
			retainedClosed := memory()
			slices.Sort(latencies)
			report := map[string]any{
				"scope":   "whole test process: SDK client + Relay + SDK HTTP server + instrumented driver",
				"load":    "closed loop; synthetic payload through real SDK transports; no production SLO claim",
				"clients": cfg.clients, "workers_per_client": cfg.workers, "payload_bytes": cfg.bytes,
				"warmup_calls": cfg.clients, "held_calls": workers, "measured_calls": len(latencies),
				"completed_calls": len(latencies), "lost_or_corrupted": 0, "server_calls": calls.Load(),
				"allocation_bytes": afterCalls.TotalAlloc - beforeCalls.TotalAlloc, "allocations": afterCalls.Mallocs - beforeCalls.Mallocs,
				"max_active_tools": maxActive.Load(), "elapsed_ns": elapsed.Nanoseconds(),
				"latency_ns":        map[string]int64{"p50": latencies[(len(latencies)-1)/2], "p95": latencies[(95*len(latencies)+99)/100-1], "max": latencies[len(latencies)-1]},
				"baseline_after_gc": baseline, "held_after_gc": held, "before_calls_after_gc": beforeCalls,
				"sampled_peak_fields": map[string]uint64{"heap_alloc": peak.HeapAlloc, "heap_inuse": peak.HeapInuse,
					"working_set": peak.WorkingSet, "private_working_set": peak.PrivateWorkingSet, "private_commit": peak.PrivateCommit},
				"sample_interval_ns": (5 * time.Millisecond).Nanoseconds(), "samples": samples, "sampled_peak_available": samples > 0,
				"after_calls": afterCalls, "retained_open_after_gc": retainedOpen, "retained_closed_after_gc": retainedClosed,
				"cancelled_calls": cfg.clients, "cancelled_handler_cleanups_observed": cfg.clients, "cancel_handler_cleanup_ns": cancelLatency.Nanoseconds(),
				"http_active_after_close": httpActive.Load(), "tool_active_after_close": active.Load(), "server_sessions_after_close": serverSessions,
				"deleted_sessions": deletes.Load(), "get_requests": gets.Load(),
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("RELAY_SDK_PROFILE " + string(encoded))
		})
	}
}
