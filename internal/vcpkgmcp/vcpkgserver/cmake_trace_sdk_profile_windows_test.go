//go:build windows && amd64

package vcpkgserver

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows"
)

const (
	cmakeSDKTraceSHA    = "a79b7793b5f384f885afb60fb513402d4523bcc0779546aa1e199e15a80fd286"
	cmakeSDKTraceBytes  = 11302778
	cmakeSDKOracleSHA   = "66edc3230924cb22f31235ebdc0a607e4a1e227db117209f56c24b7d49139ace"
	cmakeSDKTool        = "vcpkg_cmake_trace"
	cmakeSDKCallKey     = "mcp-local-hub/cmake-sdk-profile-call"
	cmakeSDKCaseTimeout = 45 * time.Second
	cmakeSDKSampleEvery = 5 * time.Millisecond
)

//go:embed testdata/cmake_trace_sdk_profile_oracle.json
var cmakeSDKOracleJSON []byte

type cmakeSDKCall struct {
	ID                      string `json:"id"`
	Stage                   string `json:"stage"`
	SubmittedNS             int64  `json:"submitted_unix_ns"`
	HandlerEnteredNS        int64  `json:"handler_entered_unix_ns"`
	CancelRequestedNS       int64  `json:"cancel_requested_unix_ns"`
	HandlerReturnedNS       int64  `json:"handler_returned_unix_ns"`
	ClientReturnedNS        int64  `json:"client_returned_unix_ns"`
	ClientLatencyNS         int64  `json:"client_latency_ns"`
	HandlerLatencyNS        int64  `json:"handler_latency_ns"`
	CancelToClientNS        int64  `json:"cancel_to_client_return_ns"`
	CancelToHandlerNS       int64  `json:"cancel_to_handler_return_ns"`
	Received                int    `json:"received"`
	Completed               int    `json:"completed"`
	Outcome                 string `json:"outcome"`
	ClientError             string `json:"client_error,omitempty"`
	HandlerError            string `json:"handler_error,omitempty"`
	HandlerContextError     string `json:"handler_context_error,omitempty"`
	HandlerResultStatus     string `json:"handler_result_status,omitempty"`
	HandlerResultReason     string `json:"handler_result_reason,omitempty"`
	HandlerToolError        bool   `json:"handler_tool_error"`
	HandlerObservationError string `json:"handler_observation_error,omitempty"`
	ResponseBytes           int    `json:"response_bytes"`
	ResponseSHA256          string `json:"response_sha256,omitempty"`
}

type cmakeSDKCallState struct {
	call                        cmakeSDKCall
	started, settled            chan struct{}
	entered, canceled, returned time.Time
}

// Observation surrounds the real registered handler and delegates unchanged.
// Neither Trace dependencies nor the tool implementation are replaced.
type cmakeSDKObserver struct {
	mu                sync.Mutex
	calls             map[string]*cmakeSDKCallState
	order             []*cmakeSDKCallState
	active, maxActive int
	unrecognized      int
}

func (o *cmakeSDKObserver) add(id, stage string) *cmakeSDKCallState {
	state := &cmakeSDKCallState{call: cmakeSDKCall{ID: id, Stage: stage}, started: make(chan struct{}), settled: make(chan struct{})}
	o.calls[id] = state
	o.order = append(o.order, state)
	return state
}

func (o *cmakeSDKObserver) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, request)
		}
		id, _ := request.GetParams().GetMeta()[cmakeSDKCallKey].(string)
		o.mu.Lock()
		state := o.calls[id]
		if state == nil {
			o.unrecognized++
			o.mu.Unlock()
			return next(ctx, method, request)
		}
		state.call.Received++
		if state.call.Received == 1 {
			state.entered = time.Now()
			state.call.HandlerEnteredNS = state.entered.UnixNano()
			close(state.started)
		}
		o.active++
		o.maxActive = max(o.maxActive, o.active)
		o.mu.Unlock()
		result, err := next(ctx, method, request)
		returned := time.Now()
		var verdict struct{ Status, Reason string }
		tool, _ := result.(*mcp.CallToolResult)
		observationErr := "missing or invalid handler tool result"
		if tool != nil && len(tool.Content) == 1 {
			if text, ok := tool.Content[0].(*mcp.TextContent); ok {
				observationErr = cmakeSDKError(json.Unmarshal([]byte(text.Text), &verdict))
			}
		}
		o.mu.Lock()
		state.call.Completed++
		state.returned = returned
		state.call.HandlerReturnedNS = returned.UnixNano()
		state.call.HandlerLatencyNS = returned.Sub(state.entered).Nanoseconds()
		if !state.canceled.IsZero() {
			state.call.CancelToHandlerNS = returned.Sub(state.canceled).Nanoseconds()
		}
		state.call.HandlerError = cmakeSDKError(err)
		state.call.HandlerContextError = cmakeSDKError(ctx.Err())
		state.call.HandlerResultStatus, state.call.HandlerResultReason = verdict.Status, verdict.Reason
		state.call.HandlerToolError = tool != nil && tool.IsError
		state.call.HandlerObservationError = observationErr
		o.active--
		if state.call.Completed == 1 {
			close(state.settled)
		}
		o.mu.Unlock()
		return result, err
	}
}

func (o *cmakeSDKObserver) snapshot() ([]cmakeSDKCall, map[string]int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	calls := make([]cmakeSDKCall, 0, len(o.order))
	counts := map[string]int{"max_active_handlers": o.maxActive, "active_handlers": o.active, "unrecognized": o.unrecognized,
		"planned": 0, "submitted": 0, "client_returned": 0, "received": 0, "completed": 0, "delivered": 0,
		"cancelled": 0, "error": 0, "unreturned": 0, "server_canceled": 0, "server_canceled_result": 0,
		"cancel_requested": 0, "unaccounted": 0, "handler_errors": 0, "handler_observation_errors": 0}
	for _, state := range o.order {
		call := state.call
		calls = append(calls, call)
		if call.Stage != "measured" {
			continue
		}
		counts["planned"]++
		if call.SubmittedNS != 0 {
			counts["submitted"]++
		}
		if call.ClientReturnedNS != 0 {
			counts["client_returned"]++
		}
		counts["received"] += call.Received
		counts["completed"] += call.Completed
		if call.Outcome == "" {
			counts["unreturned"]++
		} else {
			counts[call.Outcome]++
		}
		if call.HandlerContextError == context.Canceled.Error() {
			counts["server_canceled"]++
		}
		if call.HandlerResultStatus == "unknown" && call.HandlerResultReason == "canceled" {
			counts["server_canceled_result"]++
		}
		if call.CancelRequestedNS != 0 {
			counts["cancel_requested"]++
		}
		if call.HandlerError != "" || call.HandlerToolError {
			counts["handler_errors"]++
		}
		if call.HandlerObservationError != "" {
			counts["handler_observation_errors"]++
		}
		if call.Received != 1 || call.Completed != 1 {
			counts["unaccounted"]++
		}
	}
	return calls, counts
}

type cmakeSDKSession struct {
	client *mcp.ClientSession
	server *mcp.ServerSession
	pipes  []io.Closer
}

func (s *cmakeSDKSession) closePipes() {
	for _, pipe := range s.pipes {
		_ = pipe.Close()
	}
}

// Keep the client reader alive until server Close finishes: handler return
// precedes the SDK's final response processing, especially for retired calls.
// After five seconds, close only our four pipe ends, start any pending client
// Close, and observe both owners for five more seconds. This does not promise
// delivery of late canceled responses or interruption of a stuck file read.
func (s *cmakeSDKSession) close() (forced bool, err error) {
	type closed struct {
		server bool
		err    error
	}
	done := make(chan closed, 2)
	remaining := 0
	clientStarted := false
	startClient := func() {
		if s.client != nil && !clientStarted {
			clientStarted = true
			remaining++
			client := s.client
			go func() { done <- closed{err: client.Close()} }()
		}
	}
	if s.server != nil {
		remaining++
		server := s.server
		go func() { done <- closed{server: true, err: server.Close()} }()
	} else {
		startClient()
	}
	for attempt := 0; attempt < 2 && remaining != 0; attempt++ {
		timer := time.NewTimer(5 * time.Second)
		for remaining != 0 {
			select {
			case result := <-done:
				remaining--
				err = errors.Join(err, result.err)
				if result.server {
					startClient()
				}
			case <-timer.C:
				goto deadline
			}
		}
	deadline:
		timer.Stop()
		if remaining != 0 {
			forced = true
			s.closePipes()
			startClient()
		}
	}
	s.closePipes()
	if remaining != 0 {
		return forced, errors.Join(err, fmt.Errorf("%d SDK closes unresolved", remaining))
	}
	s.client, s.server = nil, nil
	return forced, err
}

func newCMakeSDKSession(ctx context.Context, observer *cmakeSDKObserver) (*cmakeSDKSession, error) {
	vs, err := newRegisteredServer(registerTools)
	if err != nil {
		return nil, err
	}
	vs.server.AddReceivingMiddleware(observer.middleware)
	requestsR, requestsW := io.Pipe()
	responsesR, responsesW := io.Pipe()
	session := &cmakeSDKSession{pipes: []io.Closer{requestsR, requestsW, responsesR, responsesW}}
	session.server, err = vs.server.Connect(ctx, &mcp.IOTransport{Reader: requestsR, Writer: responsesW}, nil)
	if err == nil {
		client := mcp.NewClient(&mcp.Implementation{Name: "cmake-sdk-profile", Version: "1"}, nil)
		session.client, err = client.Connect(ctx, &mcp.IOTransport{Reader: responsesR, Writer: requestsW}, nil)
	}
	if err != nil {
		session.closePipes()
		_, closeErr := session.close()
		return nil, errors.Join(err, closeErr)
	}
	return session, nil
}

func cmakeSDKError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// SDK v1.6.1 joins context cancellation with a possible notification error.
// Accept only cancellation leaves; errors.Is alone would hide the latter.
func cmakeSDKOnlyCancellation(err error) bool {
	if err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cmakeSDKOnlyCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return cmakeSDKOnlyCancellation(wrapped.Unwrap())
	}
	return false
}

func verifyCMakeSDKInput(path string) error {
	if !filepath.IsAbs(path) || os.Getenv("MCPHUB_CMAKE_SDK_PROFILE_SHA256") != cmakeSDKTraceSHA {
		return fmt.Errorf("explicit absolute public fixture path and exact SHA256 opt-in required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("fixture is not a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		return errors.Join(fmt.Errorf("fixture identity changed at open"), statErr, file.Close())
	}
	hash := sha256.New()
	n, readErr := io.Copy(hash, io.LimitReader(file, cmakeSDKTraceBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if n != cmakeSDKTraceBytes || hex.EncodeToString(hash.Sum(nil)) != cmakeSDKTraceSHA {
		return fmt.Errorf("public fixture bytes/SHA256 differ")
	}
	return nil
}

func cmakeSDKExpected(path string) (map[string]any, map[string]any, error) {
	if fmt.Sprintf("%x", sha256.Sum256(cmakeSDKOracleJSON)) != cmakeSDKOracleSHA {
		return nil, nil, fmt.Errorf("independent oracle bytes differ")
	}
	var oracle struct {
		Full      map[string]any `json:"full_max_records_8"`
		Projected map[string]any `json:"projected_default_records_1000"`
	}
	if err := json.Unmarshal(cmakeSDKOracleJSON, &oracle); err != nil {
		return nil, nil, err
	}
	for _, body := range []map[string]any{oracle.Full, oracle.Projected} {
		body["evidence"] = map[string]any{"paths": []any{path}}
	}
	return oracle.Full, oracle.Projected, nil
}

func verifyCMakeSDKResponse(result *mcp.CallToolResult, expected map[string]any) (int, string, error) {
	if result == nil || result.IsError || len(result.Content) != 1 {
		return 0, "", fmt.Errorf("missing or error MCP tool response")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || len(text.Text) > 262144 {
		return 0, "", fmt.Errorf("invalid or oversized public tool body")
	}
	var actual map[string]any
	if err := json.Unmarshal([]byte(text.Text), &actual); err != nil {
		return 0, "", err
	}
	if !reflect.DeepEqual(actual, expected) {
		return len(text.Text), "", fmt.Errorf("consumer response differs from independent fixture oracle")
	}
	return len(text.Text), fmt.Sprintf("%x", sha256.Sum256([]byte(text.Text))), nil
}

func callCMakeSDK(ctx context.Context, client *mcp.ClientSession, observer *cmakeSDKObserver, state *cmakeSDKCallState,
	path string, maxRecords int, expected map[string]any, cancelOnEntry bool) (*mcp.CallToolResult, error) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcherDone := make(chan struct{})
	if cancelOnEntry {
		go func() {
			defer close(watcherDone)
			select {
			case <-state.started:
				observer.mu.Lock()
				state.canceled = time.Now()
				state.call.CancelRequestedNS = state.canceled.UnixNano()
				if !state.returned.IsZero() {
					state.call.CancelToHandlerNS = state.returned.Sub(state.canceled).Nanoseconds()
				}
				observer.mu.Unlock()
				cancel()
			case <-callCtx.Done():
			}
		}()
	} else {
		close(watcherDone)
	}
	started := time.Now()
	observer.mu.Lock()
	state.call.SubmittedNS = started.UnixNano()
	observer.mu.Unlock()
	result, callErr := client.CallTool(callCtx, &mcp.CallToolParams{Meta: mcp.Meta{cmakeSDKCallKey: state.call.ID},
		Name: cmakeSDKTool, Arguments: map[string]any{"trace_path": path, "max_records": maxRecords}})
	returned := time.Now()
	cancel()
	<-watcherDone
	var bodyBytes int
	var bodySHA string
	outcome := "error"
	err := callErr
	if callErr == nil {
		bodyBytes, bodySHA, err = verifyCMakeSDKResponse(result, expected)
		if err == nil {
			outcome = "delivered"
		}
	} else if cancelOnEntry && cmakeSDKOnlyCancellation(callErr) && ctx.Err() == nil && result == nil {
		outcome, err = "cancelled", nil
	}
	observer.mu.Lock()
	state.call.ClientReturnedNS, state.call.ClientLatencyNS = returned.UnixNano(), returned.Sub(started).Nanoseconds()
	state.call.ClientError, state.call.Outcome = cmakeSDKError(callErr), outcome
	state.call.ResponseBytes, state.call.ResponseSHA256 = bodyBytes, bodySHA
	if !state.canceled.IsZero() {
		state.call.CancelToClientNS = returned.Sub(state.canceled).Nanoseconds()
	}
	observer.mu.Unlock()
	// Retired client requests may still have a live handler. Keep each worker's
	// slot until server completion, so cancellation cannot accumulate handlers.
	select {
	case <-state.settled:
		return result, err
	case <-ctx.Done():
		return result, errors.Join(err, fmt.Errorf("handler settlement: %w", ctx.Err()))
	}
}

type cmakeSDKPeak struct {
	Samples           int    `json:"samples"`
	HeapAlloc         uint64 `json:"heap_alloc_bytes"`
	WorkingSet        uint64 `json:"working_set_bytes"`
	PrivateWorkingSet uint64 `json:"private_working_set_bytes"`
	PrivateCommit     uint64 `json:"private_commit_bytes"`
	Error             string `json:"error,omitempty"`
}

func sampleCMakeSDKMemory(probe *cmakeSDKMemoryProbe, initial cmakeSDKMemorySample) (chan<- struct{}, <-chan cmakeSDKPeak) {
	stop, done := make(chan struct{}), make(chan cmakeSDKPeak, 1)
	go func() {
		ticker := time.NewTicker(cmakeSDKSampleEvery)
		peak := cmakeSDKPeak{Samples: 1, HeapAlloc: initial.HeapAlloc, WorkingSet: initial.WorkingSet,
			PrivateWorkingSet: initial.PrivateWorkingSet, PrivateCommit: initial.PrivateCommit}
		defer func() { ticker.Stop(); done <- peak }()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sample, err := probe.sample()
				if err != nil {
					peak.Error = err.Error()
					return
				}
				peak.Samples++
				peak.HeapAlloc = max(peak.HeapAlloc, sample.HeapAlloc)
				peak.WorkingSet = max(peak.WorkingSet, sample.WorkingSet)
				peak.PrivateWorkingSet = max(peak.PrivateWorkingSet, sample.PrivateWorkingSet)
				peak.PrivateCommit = max(peak.PrivateCommit, sample.PrivateCommit)
			}
		}
	}()
	return stop, done
}

func profileCMakeSDKCase(path string, workers, perWorker int, cancelOnEntry bool, probe *cmakeSDKMemoryProbe,
	full, projected map[string]any) (report map[string]any, resultErr error) {
	report = map[string]any{"state": "failed", "workers": workers, "calls_per_worker": perWorker, "cancel_on_handler_entry": cancelOnEntry}
	ctx, cancel := context.WithTimeout(context.Background(), cmakeSDKCaseTimeout)
	defer cancel()
	observer := &cmakeSDKObserver{calls: make(map[string]*cmakeSDKCallState)}
	warmFull, warmProjected := observer.add("warm-full", "warmup"), observer.add("warm-projected", "warmup")
	states := make([]*cmakeSDKCallState, workers*perWorker)
	for i := range states {
		states[i] = observer.add(fmt.Sprintf("call-%d", i), "measured")
	}
	defer func() {
		report["calls"], report["counts"] = observer.snapshot()
		report["error"] = cmakeSDKError(resultErr)
	}()
	session, err := newCMakeSDKSession(ctx, observer)
	if err != nil {
		return report, err
	}
	defer func() {
		if session != nil {
			forced, closeErr := session.close()
			report["close_forced"], report["close_error"] = forced, cmakeSDKError(closeErr)
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	tools, err := session.client.ListTools(ctx, nil)
	if err != nil {
		return report, err
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == cmakeSDKTool {
			found = true
		}
	}
	if !found {
		return report, fmt.Errorf("real registered cmake trace tool not advertised")
	}
	if _, err := callCMakeSDK(ctx, session.client, observer, warmFull, path, 8, full, false); err != nil {
		return report, err
	}
	if _, err := callCMakeSDK(ctx, session.client, observer, warmProjected, path, 0, projected, false); err != nil {
		return report, err
	}
	runtime.GC()
	before, err := probe.sample()
	if err != nil {
		return report, err
	}
	report["before_open_session_after_gc"] = before
	responses := make([]*mcp.CallToolResult, len(states))
	stopSamples, sampleDone := sampleCMakeSDKMemory(probe, before)
	workersDone := make(chan error, workers)
	client := session.client
	start := time.Now()
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			var workerErr error
			defer func() { workersDone <- workerErr }()
			for index := worker * perWorker; index < (worker+1)*perWorker; index++ {
				if ctx.Err() != nil {
					workerErr = ctx.Err()
					return
				}
				responses[index], workerErr = callCMakeSDK(ctx, client, observer, states[index], path, 0, projected, cancelOnEntry)
				if workerErr != nil {
					return
				}
			}
		}(worker)
	}
	remaining := workers
	for remaining > 0 {
		select {
		case workerErr := <-workersDone:
			remaining--
			resultErr = errors.Join(resultErr, workerErr)
		case <-ctx.Done():
			session.closePipes()
			resultErr = errors.Join(resultErr, ctx.Err())
			goto settleWorkers
		}
	}
settleWorkers:
	if remaining > 0 {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for remaining > 0 {
			select {
			case workerErr := <-workersDone:
				remaining--
				resultErr = errors.Join(resultErr, workerErr)
			case <-timer.C:
				goto workersObserved
			}
		}
	}
workersObserved:
	report["window_ns"], report["unsettled_workers"] = time.Since(start).Nanoseconds(), remaining
	close(stopSamples)
	peak := <-sampleDone
	report["sampled_peak_lower_bounds"] = peak
	report["sampler_settled"] = true
	if peak.Error != "" {
		resultErr = errors.Join(resultErr, errors.New(peak.Error))
	}
	if remaining != 0 {
		return report, errors.Join(resultErr, fmt.Errorf("worker settlement incomplete"))
	}
	after, err := probe.sample()
	if err != nil {
		return report, errors.Join(resultErr, err)
	}
	report["after_calls_before_gc"] = after
	report["allocated_bytes_whole_process"] = after.TotalAlloc - before.TotalAlloc
	report["allocations_whole_process"] = after.Mallocs - before.Mallocs
	runtime.GC()
	held, err := probe.sample()
	runtime.KeepAlive(responses)
	if err != nil {
		return report, errors.Join(resultErr, err)
	}
	report["responses_held_open_session_after_gc"] = held
	clear(responses)
	responses = nil
	runtime.GC()
	released, err := probe.sample()
	if err != nil {
		return report, errors.Join(resultErr, err)
	}
	report["responses_released_open_session_after_gc"] = released
	forced, closeErr := session.close()
	report["close_forced"], report["close_error"] = forced, cmakeSDKError(closeErr)
	session = nil
	client = nil
	if closeErr != nil {
		return report, errors.Join(resultErr, closeErr)
	}
	runtime.GC()
	closed, err := probe.sample()
	if err != nil {
		return report, errors.Join(resultErr, err)
	}
	report["responses_released_closed_session_after_gc"] = closed
	_, counts := observer.snapshot()
	if counts["submitted"] != len(states) || counts["received"] != len(states) || counts["completed"] != len(states) ||
		counts["client_returned"] != len(states) || counts["unaccounted"] != 0 || counts["error"] != 0 ||
		counts["active_handlers"] != 0 || counts["max_active_handlers"] > workers || counts["unrecognized"] != 0 ||
		counts["handler_errors"] != 0 || counts["handler_observation_errors"] != 0 {
		resultErr = errors.Join(resultErr, fmt.Errorf("call accounting or concurrency contract failed"))
	}
	if cancelOnEntry && (counts["cancelled"] == 0 || counts["server_canceled"] == 0 || counts["server_canceled_result"] == 0) {
		resultErr = errors.Join(resultErr, fmt.Errorf("actual client/server cancellation not observed"))
	}
	if !cancelOnEntry && counts["delivered"] != len(states) {
		resultErr = errors.Join(resultErr, fmt.Errorf("normal response loss"))
	}
	if forced {
		resultErr = errors.Join(resultErr, fmt.Errorf("graceful SDK close exceeded observation bound"))
	}
	if resultErr == nil {
		report["state"] = "complete"
	}
	return report, resultErr
}

// BenchmarkCMakeTraceSDKConsumer is an explicitly admitted, fixed campaign.
// Use -run '^$' -bench '^BenchmarkCMakeTraceSDKConsumer$' -benchtime=1x -count=1.
// JSON is the primary result; ordinary ns/op describes the entire campaign.
func BenchmarkCMakeTraceSDKConsumer(b *testing.B) {
	if os.Getenv("MCPHUB_CMAKE_SDK_PROFILE_TRACE") == "" {
		b.Skip("opt-in public CMake fixture profile")
	}
	report := map[string]any{"schema": 1, "state": "failed", "input_sha256": cmakeSDKTraceSHA, "input_bytes": cmakeSDKTraceBytes,
		"oracle_sha256": cmakeSDKOracleSHA, "go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"gomaxprocs": runtime.GOMAXPROCS(0), "num_cpu": runtime.NumCPU(), "gogc_env": os.Getenv("GOGC"), "gomemlimit_env": os.Getenv("GOMEMLIMIT"),
		"sample_interval_ns": cmakeSDKSampleEvery.Nanoseconds(), "case_deadline_ns": cmakeSDKCaseTimeout.Nanoseconds(),
		"scope": "whole current process: official SDK client + IOTransport server + measurement and semantic oracle driver; warmed file cache",
		"limits": []string{"CMake-generated published redacted fixture, not production load", "sampled peaks include baseline and are lower bounds, not continuous maxima",
			"private commit is not resident RAM; working set includes shared pages", "default 1000-record public projection is intentional, not transport loss",
			"sampling and response-oracle validation contribute to measured driver work; 8 calls per worker are not SLO estimates",
			"latencies use monotonic time; negative cancellation-to-return deltas mean completion won the cancellation race",
			"server Close precedes client Close; late canceled responses are already retired and their delivery is not promised",
			"no subprocess, installed product, system Scheduler, HTTP/network or CMake execution", "file reads are not promised hard-interruptible; outer runner owns process deadline"}}
	defer func() {
		encoded, err := json.Marshal(report)
		if err != nil {
			b.Errorf("profile JSON: %v", err)
			return
		}
		fmt.Printf("CMAKE_TRACE_SDK_PROFILE_V1 %s\n", encoded)
	}()
	if b.N != 1 {
		b.Fatal("fixed campaign requires -benchtime=1x")
	}
	path := os.Getenv("MCPHUB_CMAKE_SDK_PROFILE_TRACE")
	if err := verifyCMakeSDKInput(path); err != nil {
		b.Fatal(err)
	}
	full, projected, err := cmakeSDKExpected(path)
	if err != nil {
		b.Fatal(err)
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			if dependency.Path == "github.com/modelcontextprotocol/go-sdk" {
				report["sdk_module_version"] = dependency.Version
				if dependency.Replace != nil {
					report["sdk_module_replace"] = dependency.Replace
				}
			}
		}
	}
	probe, err := newCMakeSDKMemoryProbe()
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		closeErr := windows.CloseHandle(probe.handle)
		report["observer_handle_close_error"] = cmakeSDKError(closeErr)
		if closeErr != nil {
			report["state"] = "failed"
			b.Error(closeErr)
		}
	}()
	var cases []map[string]any
	for _, setting := range []struct {
		workers, perWorker int
		cancel             bool
	}{{1, 8, false}, {2, 8, false}, {2, 4, true}} {
		result, err := profileCMakeSDKCase(path, setting.workers, setting.perWorker, setting.cancel, probe, full, projected)
		cases = append(cases, result)
		report["cases"] = cases
		if err != nil {
			b.Fatal(err)
		}
	}
	if err := verifyCMakeSDKInput(path); err != nil {
		b.Fatal(err)
	}
	report["input_after_verified"] = true
	report["state"] = "complete"
}
