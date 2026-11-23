//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

const routeAggregateFixtureChild = "MCPHUB_AGGREGATE_STATUS_TEST_CHILD"

func TestRouteAggregateStatusOpenReadsBoundedFrames(t *testing.T) {
	encoded, err := json.Marshal(api.IPCRequest{Version: 1, ID: 1<<63 - 1, Cmd: api.RouteAggregateControlAttachCommand})
	if err != nil {
		t.Fatal(err)
	}
	valid := string(encoded) + "\n"
	t.Run("underlying discriminator read", func(t *testing.T) {
		input := &routeAggregateReadTrace{source: strings.NewReader(string(api.RouteAggregateStatusMarker) + valid)}
		first, err := readSupervisorIPCFirstByte(input)
		if err != nil || first[0] != api.RouteAggregateStatusMarker || input.maxRead != 1 || input.bytesRead != 1 {
			t.Fatalf("discriminator consumed %d bytes with max Read size %d: %v", input.bytesRead, input.maxRead, err)
		}
	})
	for _, tc := range []struct {
		name      string
		frame     string
		wantBytes int
		wantError bool
	}{
		{name: "largest valid query", frame: valid, wantBytes: len(valid)},
		{name: "short malformed JSON", frame: "{\n", wantBytes: 2, wantError: true},
		{name: "one byte over", frame: string(encoded) + " \n", wantBytes: len(valid), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &routeAggregateReadTrace{source: strings.NewReader(tc.frame)}
			req, err := api.ReadRouteAggregateStatusOpen(input)
			if (err != nil) != tc.wantError || input.maxRead != 1 || input.bytesRead != tc.wantBytes {
				t.Fatalf("bounded open consumed %d/%d bytes with max Read size %d: %v", input.bytesRead, tc.wantBytes, input.maxRead, err)
			}
			if !tc.wantError && (req.ID != 1<<63-1 || req.Cmd != api.RouteAggregateControlAttachCommand) {
				t.Fatalf("valid frame changed: %+v", req)
			}
		})
	}
}

type routeAggregateReadTrace struct {
	source    *strings.Reader
	maxRead   int
	bytesRead int
}

func (r *routeAggregateReadTrace) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	n, err := r.source.Read(p)
	r.bytesRead += n
	return n, err
}

func TestRouteAggregateStatusNativePeers(t *testing.T) {
	testRouteAggregateStatusNativePeers(t, "")
}

func TestRouteAggregateStatusNativeFrameCounts(t *testing.T) {
	for _, mode := range []string{"zero_and_full", "partial"} {
		t.Run(mode, func(t *testing.T) { testRouteAggregateStatusNativePeers(t, mode) })
	}
}

func TestRouteAggregateStatusNativeFrameCountsSecurity(t *testing.T) {
	for _, mode := range []string{"expired_apply_verify", "zero_peer_check_timeout"} {
		t.Run(mode, func(t *testing.T) { testRouteAggregateStatusNativePeers(t, mode) })
	}
}

func TestRouteAggregateApplyWaitsForOriginalAttach(t *testing.T) {
	for _, mode := range []string{"ready", "cancel", "expiry", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			testRouteAggregateStatusNativePeers(t, "apply_before_ack_"+mode)
		})
	}
}

func testRouteAggregateStatusNativePeers(t *testing.T, mode string) {
	t.Helper()
	fullFaults := mode == "zero_and_full"
	attachCase, delayedAttach := strings.CutPrefix(mode, "apply_before_ack_")
	deadline, finite := t.Deadline()
	if !finite {
		t.Fatal("aggregate status transport test requires its runner deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	lock, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	owner, err := newRouteAggregateSupervisor(ctx, stateDir, lock.Owner())
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewDaemonRuntimeTracker()
	creators := &daemonCreatorRecords{routeAggregate: owner}
	var heldCleanup atomic.Pointer[routeAggregateBinding]
	cleanupEntered := make(chan struct{})
	releaseCleanup := make(chan struct{})
	releaseOldCleanup := sync.OnceFunc(func() { close(releaseCleanup) })
	defer releaseOldCleanup()
	owner.beforeDetach = func(b *routeAggregateBinding) {
		if b == heldCleanup.Load() {
			close(cleanupEntered)
			<-releaseCleanup
		}
	}
	listener, err := NewSupervisorIPCListener(api.SupervisorIPCAddress(stateDir), lock.Owner())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	partialStarted := make(chan struct{})
	partialRelease := make(chan struct{})
	releasePartial := sync.OnceFunc(func() { close(partialRelease) })
	defer releasePartial()

	operation, err := creators.begin(api.BuiltinRouteTaskName)
	if err != nil {
		t.Fatal(err)
	}
	exe := owner.launch.Peer.ImagePath
	fixtureDir := t.TempDir()
	cmd := exec.Command(exe, "-test.run=^TestRouteAggregateStatusNativeChild$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), routeAggregateFixtureChild+"="+fixtureDir,
		"MCPHUB_AGGREGATE_STATUS_TEST_VERIFY="+mode)
	creators.prepareRouteAggregateLaunch(operation, api.BuildBuiltinRouteDaemon(exe, api.DefaultMCPFrontPort), cmd)
	if operation.routeAggregateStarted == nil {
		t.Fatal("canonical route launch was not admitted")
	}
	process.NoConsole(cmd)
	job, err := process.NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	var creator *process.ProvisionalProcess
	var settled bool
	t.Cleanup(func() {
		if creator != nil && !settled {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), api.SupervisorAggregateStatusTimeout)
			defer cleanupCancel()
			if result, err := creator.TerminateCreatorAndWait(cleanupCtx); err != nil || result.Closure() == nil {
				t.Errorf("close original aggregate transport fixture: %v", err)
			}
		} else if creator == nil {
			_ = job.Close()
		}
	})
	creator, err = process.StartProvisionalWithJob(job, cmd, func(original *process.ProvisionalProcess) error {
		creator = original
		return creators.retain(operation, original)
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := api.ObserveSupervisorProcessIdentityV1(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	birth, err := time.Parse(time.RFC3339Nano, original.CreationTime)
	if err != nil {
		t.Fatal(err)
	}

	eventLog, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eventLog.Close() })
	session := newIPCSession(listener, nil)
	session.acceptDone = make(chan struct{})
	deps := ipcDispatchDeps{daemonCreators: creators, runtimeTracker: tracker, events: eventLog}
	pipe := &routeAggregateFixtureAcceptor{
		SupervisorIPCListener: listener,
		partialStarted:        partialStarted, partialRelease: partialRelease,
		operatorResult: make(chan error, 1), operatorDeps: deps, session: session,
		expiredWrite: make(chan struct{}),
	}
	applyWaiting, advanceWait := make(chan struct{}), make(chan struct{})
	allowWait := sync.OnceFunc(func() { close(advanceWait) })
	ackStarted, ackRelease := make(chan struct{}), make(chan struct{})
	allowAck := sync.OnceFunc(func() { close(ackRelease) })
	if delayedAttach {
		defer allowWait()
		defer allowAck()
		reconcileReady, intentFilesLoaded := &atomic.Bool{}, &atomic.Bool{}
		reconcileReady.Store(true)
		intentFilesLoaded.Store(true)
		deps.stateDir, deps.reconcileReady, deps.intentFilesLoaded = stateDir, reconcileReady, intentFilesLoaded
		pipe.operatorDeps = deps
		pipe.attachAckStarted, pipe.attachAckRelease = ackStarted, ackRelease
		pipe.holdAttachAck.Store(true)
		owner.beforeApplyWait = sync.OnceFunc(func() {
			close(applyWaiting)
			if attachCase == "replacement" {
				<-advanceWait
			}
		})
	}
	go func() {
		defer close(session.acceptDone)
		acceptIPCConnectionsWithSession(pipe, deps, session)
	}()
	t.Cleanup(session.stop)

	creators.mu.Lock()
	operation.generation = tracker.MarkSpawned(api.BuiltinRouteTaskName, original.PID, birth)
	creators.mu.Unlock()
	creators.endStart(operation, nil)
	waitBinding := func(prior *routeAggregateBinding) *routeAggregateBinding {
		t.Helper()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			creators.mu.Lock()
			b := owner.binding
			ready := b != nil && b != prior && b.ready
			creators.mu.Unlock()
			if ready {
				return b
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	defer func() {
		if t.Failed() || settled {
			return
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, "release"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := creator.WaitCreatorRoot()
		if err != nil || !root.Known || root.ExitCode != 0 {
			t.Fatalf("original route fixture exit: %+v, %v", root, err)
		}
		result, err := creator.CompleteCreatorAfterExit(ctx, true)
		if err != nil || result.Closure() == nil {
			t.Fatalf("original route fixture closure: %v", err)
		}
		settled = true
	}()

	dialFixture := func() (net.Conn, *json.Decoder) {
		t.Helper()
		conn, err := winio.DialPipeContext(ctx, api.SupervisorIPCAddress(stateDir))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.SetDeadline(time.Now().Add(api.SupervisorAggregateStatusTimeout)); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(conn)
		var hello struct{ Hello api.IPCHello }
		if err := decoder.Decode(&hello); err != nil || !api.ValidateHandshake(hello.Hello, lock.Owner()) {
			t.Fatalf("fixture hello: %+v, %v", hello, err)
		}
		return conn, decoder
	}

	if delayedAttach {
		select {
		case <-ackStarted:
		case <-ctx.Done():
			t.Fatal("original attachment did not reach its held acknowledgment", ctx.Err())
		}
		creators.mu.Lock()
		held := owner.binding
		provedOriginal := held != nil && held.operation == operation && !held.ready
		creators.mu.Unlock()
		if !provedOriginal {
			t.Fatal("fixture did not hold the actual original attach acknowledgment")
		}
		conn, decoder := dialFixture()
		if err := json.NewEncoder(conn).Encode(api.IPCRequest{Version: 1, ID: 27, Cmd: "status"}); err != nil {
			t.Fatal(err)
		}
		var legacy api.IPCResponse
		if err := decoder.Decode(&legacy); err != nil || legacy.ID != 27 || !legacy.OK || legacy.Error != nil {
			t.Fatalf("ordinary supervisor readiness query failed: %v", err)
		}
		_ = conn.Close()
		result, _ := legacy.Result.(map[string]any)
		if result["reconcile_ready"] != true {
			t.Fatal("ordinary supervisor readiness was not true during the held Route ACK")
		}

		selection := api.RouteAggregateApply{ReceiptSHA256: strings.Repeat("ab", 32), EndpointSHA256: strings.Repeat("cd", 32)}
		type applyOutcome struct {
			status api.RouteAggregateStatus
			err    error
		}
		finished := make(chan applyOutcome, 1)
		operator, cancelOperator := context.WithCancel(ctx)
		defer cancelOperator()
		pipe.captureNextOperator.Store(true)
		if attachCase == "expiry" {
			conn, _ := dialFixture()
			defer conn.Close()
			// Keep the real marked connection open until the server's own
			// absolute deadline. A client timer must not supply this result.
			if err := conn.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(api.IPCRequest{Version: 1, ID: 28, Cmd: api.SupervisorAggregateApplyCommand,
				Args: map[string]any{"receipt_sha256": selection.ReceiptSHA256, "endpoint_sha256": selection.EndpointSHA256}})
			if err != nil {
				t.Fatal(err)
			}
			frame := append([]byte{api.OperatorAggregateStatusMarker}, body...)
			frame = append(frame, '\n')
			if n, err := conn.Write(frame); err != nil || n != len(frame) {
				t.Fatalf("write exact marked Apply: %d bytes, %v", n, err)
			}
		} else {
			go func() {
				status, err := api.ApplySupervisorAggregate(operator, stateDir, lock.Owner(), owner.launch.Peer, func(context.Context) error {
					current, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
					if err != nil || current != lock.Owner() {
						return errors.Join(errors.New("fixture Supervisor owner changed"), err)
					}
					return nil
				}, selection)
				finished <- applyOutcome{status, err}
			}()
		}

		select {
		case <-applyWaiting:
		case result := <-finished:
			t.Fatalf("Apply completed before its original ACK was released: %+v", result)
		case err := <-pipe.operatorResult:
			t.Fatal("server returned before the original attach wait", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		creators.mu.Lock()
		unreserved := owner.binding == held && !held.ready && held.pending == nil
		creators.mu.Unlock()
		if !unreserved || pipe.challengeWrites.Load() != 0 {
			t.Fatal("Apply reserved or wrote before the original attach ACK")
		}

		switch attachCase {
		case "ready":
			allowAck()
		case "cancel":
			cancelOperator()
		case "replacement":
			if err := os.WriteFile(filepath.Join(fixtureDir, "release"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := creator.WaitCreatorRoot()
			if err != nil || !root.Known || root.ExitCode != 0 {
				t.Fatalf("original Route fixture exit: %+v, %v", root, err)
			}
			terminal, err := creator.CompleteCreatorAfterExit(ctx, true)
			if err != nil || terminal.Closure() == nil {
				t.Fatalf("original Route fixture closure: %v", err)
			}
			settled = true
			creators.mu.Lock()
			operation.root, operation.terminal = root, terminal
			creators.mu.Unlock()
			creators.retireRouteAggregateBinding(operation)
			replacement, err := creators.begin(api.BuiltinRouteTaskName)
			if err != nil {
				t.Fatal(err)
			}
			defer creators.endStart(replacement, errors.New("replacement fixture stopped before process creation"))
			allowWait()
		}

		if attachCase != "expiry" {
			select {
			case result := <-finished:
				switch attachCase {
				case "ready":
					want := api.RouteAggregateStatus{Ownership: api.RouteAggregateOwned, Generation: operation.generation, Revision: 1, Effective: "ready"}
					if result.err != nil || result.status != want {
						t.Fatalf("one original Apply proof = %+v, want %+v", result, want)
					}
				case "cancel":
					if !errors.Is(result.err, context.Canceled) || result.status != (api.RouteAggregateStatus{}) {
						t.Fatalf("canceled operator received %+v", result)
					}
				case "replacement":
					if result.err == nil || result.err.Error() != "OPERATOR_AGGREGATE_PEER_CHANGED" ||
						result.status != (api.RouteAggregateStatus{}) {
						t.Fatalf("replacement operator received %+v", result)
					}
				}
			case <-ctx.Done():
				t.Fatal("operator did not settle", ctx.Err())
			}
		}
		select {
		case serverErr := <-pipe.operatorResult:
			switch attachCase {
			case "ready", "replacement":
				if serverErr != nil {
					t.Fatal("server failed sending the terminal result", serverErr)
				}
			case "cancel":
				if !errors.Is(serverErr, context.Canceled) {
					t.Fatal("server retained the canceled Apply", serverErr)
				}
			case "expiry":
				if !errors.Is(serverErr, context.DeadlineExceeded) {
					t.Fatal("server lost the original Apply deadline", serverErr)
				}
			}
		case <-ctx.Done():
			t.Fatal("server Apply handler did not settle", ctx.Err())
		}
		if attachCase == "cancel" {
			allowAck()
			if waitBinding(nil) != held {
				t.Fatal("pre-ACK cancellation detached the same original Route")
			}
		} else if attachCase == "expiry" {
			// The native attach clock started before the Apply clock. Its
			// own timeout may already have closed this still-unacked pipe.
			allowAck()
		}
		wantWrites := int32(0)
		if attachCase == "ready" {
			wantWrites = 1
		}
		creators.mu.Lock()
		idle := held.pending == nil
		creators.mu.Unlock()
		if writes := pipe.challengeWrites.Load(); !idle || writes != wantWrites {
			t.Fatalf("original Apply slot idle=%t, challenge writes=%d, want=%d", idle, writes, wantWrites)
		}
		return
	}
	first := waitBinding(nil)
	heldCleanup.Store(first)

	waitFinished := func(q *routeAggregateQuery) {
		t.Helper()
		select {
		case <-q.finished:
		case <-ctx.Done():
			t.Fatal("original query did not settle", ctx.Err())
		}
	}
	awaitObservation := func(revision uint64, b *routeAggregateBinding) *routeAggregateQuery {
		t.Helper()
		if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, fmt.Sprintf("observation-%d", revision))); err != nil {
			t.Fatal(err)
		}
		creators.mu.Lock()
		q := b.pending
		creators.mu.Unlock()
		if q == nil {
			t.Fatalf("revision %d escaped its original slot", revision)
		}
		return q
	}
	cancelObservedOperator := func(cancel context.CancelFunc, finished <-chan error) {
		t.Helper()
		cancel()
		for _, result := range []<-chan error{finished, pipe.operatorResult} {
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("marked operator cancellation = %v", err)
				}
			case <-ctx.Done():
				t.Fatal("marked operator did not return while its proof was held", ctx.Err())
			}
		}
	}

	query := func(qctx context.Context) (api.RouteAggregateStatus, error) {
		return api.QuerySupervisorAggregateStatus(qctx, stateDir, lock.Owner(), owner.launch.Peer, func(context.Context) error {
			got, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
			if err != nil || got != lock.Owner() {
				return fmt.Errorf("original singleton changed: %w", err)
			}
			return nil
		})
	}
	statusAtRevision := func(revision uint64) api.RouteAggregateStatus {
		return api.RouteAggregateStatus{Ownership: api.RouteAggregateOwned, Generation: operation.generation, Revision: revision, Effective: "ready"}
	}
	expectLive := func(revision uint64) {
		t.Helper()
		status, err := query(ctx)
		want := statusAtRevision(revision)
		if err != nil || status != want {
			t.Fatalf("live original route result = %+v, want %+v, error = %v", status, want, err)
		}
	}
	if mode == "expired_apply_verify" {
		defer releaseOldCleanup()
		defer releasePartial()
		pipe.writeFault.Store(routeAggregateFixtureVerifyDeadline)
		digest := strings.Repeat("ab", 32)
		selection := api.RouteAggregateApply{ReceiptSHA256: digest, EndpointSHA256: digest}
		finished := make(chan error, 1)
		go func() {
			_, err := creators.exchangeRouteAggregate(ctx, 102, tracker, &selection)
			finished <- err
		}()
		verifyResult := func(name, want string) {
			t.Helper()
			got, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, name))
			if err != nil || string(got) != want {
				t.Fatalf("original Route verify %s = %q, %v, want %q", name, got, err, want)
			}
		}
		verifyResult("verify-before", "accepted")
		select {
		case <-pipe.expiredWrite:
		case <-ctx.Done():
			t.Fatal("Apply frame did not reach its original native deadline", ctx.Err())
		}
		creators.mu.Lock()
		pending := first.pending
		stillBound := owner.binding == first && pending != nil && pending.challenge.Apply != nil
		creators.mu.Unlock()
		if !stillBound {
			t.Fatal("fixture lost the original Apply slot while the writer was held")
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, "verify-expired"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		verifyResult("verify-after", "OPERATOR_AGGREGATE_PEER_CHANGED")
		releasePartial()
		select {
		case err := <-finished:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expired Apply owner returned %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		t.Log("original Route verified its live Apply nonce; the same kernel peer, nonce and generation were refused after the original deadline with its writer still held")
		return
	}
	if fullFaults || mode == "zero_peer_check_timeout" {
		pipe.writeFault.Store(routeAggregateFixtureZero)
		got, err := creators.exchangeRouteAggregate(ctx, 100, tracker, nil)
		creators.mu.Lock()
		originalIdle := owner.binding == first && first.pending == nil
		creators.mu.Unlock()
		if !errors.Is(err, io.ErrShortWrite) || !originalIdle || got != (api.RouteAggregateStatus{}) {
			t.Fatalf("zero-delivery refusal = %+v, %v, original idle=%t", got, err, originalIdle)
		}
		t.Log("confirmed zero bytes, refusal retained and original slot released")
	}
	expectLive(1)
	if mode == "zero_peer_check_timeout" {
		defer releaseOldCleanup()
		pipe.writeFault.Store(routeAggregateFixtureExpiredZero)
		got, err := creators.exchangeRouteAggregate(ctx, 103, tracker, nil)
		creators.mu.Lock()
		stillBound := owner.binding == first
		creators.mu.Unlock()
		if !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, context.DeadlineExceeded) || stillBound || got != (api.RouteAggregateStatus{}) {
			t.Fatalf("zero bytes with exhausted peer check = %+v, %v, original still bound=%t", got, err, stillBound)
		}
		select {
		case <-cleanupEntered:
		case <-ctx.Done():
			t.Fatal("original zero-byte attachment did not enter teardown", ctx.Err())
		}
		next := waitBinding(first)
		expectLive(2)
		releaseOldCleanup()
		select {
		case <-first.done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		creators.mu.Lock()
		preserved := owner.binding == next
		creators.mu.Unlock()
		if !preserved {
			t.Fatal("old zero-byte settlement retired the successor")
		}
		t.Log("zero-byte refusal with unconfirmed peer preserved the short-write and deadline causes, retired only the original transport, and preserved the successor")
		return
	}
	expectLive(2)
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "observation-2")); err != nil {
		t.Fatal(err)
	}

	if mode == "partial" {
		pipe.writeFault.Store(routeAggregateFixturePartial)
		got, err := creators.exchangeRouteAggregate(ctx, 101, tracker, nil)
		if !errors.Is(err, errRouteAggregateFixtureWrite) || got != (api.RouteAggregateStatus{}) {
			t.Fatalf("partial write = %+v, %v", got, err)
		}
		select {
		case <-cleanupEntered:
		case <-ctx.Done():
			t.Fatal("short frame survived on the original transport", ctx.Err())
		}
		if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "retired-1")); err != nil {
			t.Fatal(err)
		}
		next := waitBinding(first)
		if err := os.WriteFile(filepath.Join(fixtureDir, "allow-third"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		expectLive(3)
		releaseOldCleanup()
		select {
		case <-first.done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		creators.mu.Lock()
		preserved := owner.binding == next
		creators.mu.Unlock()
		if !preserved {
			t.Fatal("partial-frame cleanup removed the successor")
		}
		t.Log("native prefix escaped once, rejected without application or replay; original cleanup preserved its successor")
		return
	}

	conn, decoder := dialFixture()
	if _, err := conn.Write([]byte{api.RouteAggregateStatusMarker}); err != nil {
		t.Fatal(err)
	}
	var denied api.IPCResponse
	if err := decoder.Decode(&denied); err != nil || denied.ID != 0 || !denied.Final || denied.OK ||
		denied.Error == nil || denied.Error.Code != "ROUTE_AGGREGATE_PEER_DENIED" {
		t.Fatalf("noncreator route mode was not refused before body: %+v, %v", denied, err)
	}
	_ = conn.Close()

	conn, decoder = dialFixture()
	if _, err := conn.Write(append([]byte{api.OperatorAggregateStatusMarker}, []byte("{\n")...)); err != nil {
		t.Fatal(err)
	}
	var malformed api.IPCResponse
	if err := decoder.Decode(&malformed); err != nil || malformed.ID != 0 || !malformed.Final || malformed.OK ||
		malformed.Error == nil || malformed.Error.Code != "OPERATOR_AGGREGATE_INVALID" {
		t.Fatalf("malformed marked request: %+v, %v", malformed, err)
	}
	_ = conn.Close()

	expired, endExpired := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer endExpired()
	if status, err := query(expired); !errors.Is(err, context.DeadlineExceeded) || status != (api.RouteAggregateStatus{}) {
		t.Fatalf("expired operator returned status %+v, error %v", status, err)
	}

	operator, cancelOperator := context.WithCancel(ctx)
	defer cancelOperator()
	finished := make(chan error, 1)
	pipe.writeFault.Store(routeAggregateFixtureSplit)
	if fullFaults {
		pipe.writeFault.Store(routeAggregateFixtureSplitFullError)
	}
	pipe.captureNextOperator.Store(true)
	go func() {
		_, err := query(operator)
		finished <- err
	}()
	select {
	case <-partialStarted:
	case <-ctx.Done():
		t.Fatal("original pipe did not write the partial challenge", ctx.Err())
	}
	creators.mu.Lock()
	partialQuery := first.pending
	startedWithoutProof := partialQuery != nil && !partialQuery.deadline.IsZero() && !partialQuery.received
	creators.mu.Unlock()
	if !startedWithoutProof {
		t.Fatal("original frame did not reserve its in-flight query")
	}
	expectBusy := func() {
		t.Helper()
		if _, err := query(ctx); err == nil || err.Error() != "OPERATOR_AGGREGATE_BUSY" {
			t.Fatalf("parallel operator query did not receive busy: %v", err)
		}
	}
	expectBusy()

	// The actual IPC dispatcher still accepts ordinary multi-frame requests
	// while the route is blocked on the original live query.
	conn, decoder = dialFixture()
	for index, command := range []string{api.SupervisorAggregateStatusCommand, "capabilities", "capabilities"} {
		req := api.IPCRequest{ID: int64(index) + 1, Cmd: command}
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			t.Fatal(err)
		}
		var reply api.IPCResponse
		if err := decoder.Decode(&reply); err != nil || reply.ID != req.ID || !reply.Final {
			t.Fatalf("legacy response: %+v, %v", reply, err)
		}
		if command == api.SupervisorAggregateStatusCommand {
			if reply.OK || reply.Error == nil || reply.Error.Code != "UNKNOWN_COMMAND" {
				t.Fatalf("unmarked query was accepted: %+v", reply)
			}
		} else if !reply.OK || reply.Error != nil {
			t.Fatalf("legacy capabilities changed: %+v", reply)
		}
	}
	_ = conn.Close()

	cancelObservedOperator(cancelOperator, finished)
	expectOriginal := func() bool {
		t.Helper()
		creators.mu.Lock()
		bound, pending := owner.binding == first, first.pending != nil
		creators.mu.Unlock()
		if !bound {
			t.Fatal("operator cancellation removed the original binding")
		}
		select {
		case <-cleanupEntered:
			t.Fatal("operator cancellation detached the original handler")
		default:
		}
		if _, err := os.Stat(filepath.Join(fixtureDir, "retired-1")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("operator cancellation retired Route control: %v", err)
		}
		return pending
	}
	expectOriginal()
	creators.mu.Lock()
	sameInFlightQuery := first.pending == partialQuery && !partialQuery.deadline.IsZero() && !partialQuery.received
	creators.mu.Unlock()
	if !sameInFlightQuery {
		t.Fatal("server operator cancellation changed the partial frame's original request slot")
	}
	expectBusy()
	releasePartial()
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "observation-3")); err != nil {
		t.Fatal("Route did not receive the completed original frame", err)
	}
	expectBusy()
	if err := os.WriteFile(filepath.Join(fixtureDir, "allow-third"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFinished(partialQuery)
	if want := statusAtRevision(3); partialQuery.err != nil || partialQuery.status != want {
		t.Fatalf("original late proof = %+v, want %+v, error = %v", partialQuery.status, want, partialQuery.err)
	}
	if fullFaults && !errors.Is(partialQuery.writeErr, errRouteAggregateFixtureWrite) {
		t.Fatalf("exact revision 3 proof lost original full-write error: %v", partialQuery.writeErr)
	}
	if expectOriginal() {
		t.Fatal("the exact late proof did not release its request slot")
	}
	expectLive(4)
	expectOriginal()
	t.Log("the marked server operator returned canceled with the native frame tail held; its exact revision 3 proof drained before revision 4 used the same attachment without Retire")

	// A different request stalls Route control through the original
	// attachment budget. Its native peer loss still retires and reattaches.
	timeoutCaller := ctx
	if fullFaults {
		var cancelTimeoutCaller context.CancelFunc
		timeoutCaller, cancelTimeoutCaller = context.WithCancel(ctx)
		defer cancelTimeoutCaller()
		pipe.writeFault.Store(routeAggregateFixtureExpiredFull)
		pipe.captureNextOperator.Store(true)
		go func() {
			_, err := query(timeoutCaller)
			finished <- err
		}()
		q := awaitObservation(5, first)
		cancelObservedOperator(cancelTimeoutCaller, finished)
		expectBusy()
		select {
		case <-pipe.expiredWrite:
		case <-ctx.Done():
			t.Fatal("full-write fixture did not reach the original native deadline", ctx.Err())
		}
		settleDeadline := time.NewTimer(api.SupervisorAggregateStatusTimeout / 2)
		defer settleDeadline.Stop()
		select {
		case <-q.finished:
		case <-settleDeadline.C:
			t.Fatal("worker restarted the original deadline or waited for its own reader")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if !errors.Is(q.err, context.DeadlineExceeded) || !errors.Is(q.err, errRouteAggregateFixtureWrite) ||
			q.status != (api.RouteAggregateStatus{}) {
			t.Fatalf("missing exact reply = %+v, %v", q.status, q.err)
		}
		t.Log("no reply after marked caller exit: original write deadline settled the same slot with both causes")
	} else {
		go func() {
			_, err := query(ctx)
			finished <- err
		}()
		awaitObservation(5, first)
	}
	select {
	case <-cleanupEntered:
	case <-ctx.Done():
		t.Fatal("original binding did not reach its held cleanup", ctx.Err())
	}
	select {
	case <-first.done:
		t.Fatal("old handler settled before the cleanup barrier")
	default:
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "allow-fifth"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "retired-1")); err != nil {
		t.Fatal("genuine attachment timeout did not retire Route control", err)
	}
	next := waitBinding(first)
	expectLive(6)
	if !fullFaults {
		select {
		case err := <-finished:
			if err == nil {
				t.Fatal("stalled original attachment reported success")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	releaseOldCleanup()
	select {
	case <-first.done:
	case <-ctx.Done():
		t.Fatal("original binding did not join after cleanup release", ctx.Err())
	}
	creators.mu.Lock()
	replacement := owner.binding
	creators.mu.Unlock()
	if replacement != next {
		t.Fatal("old cleanup removed the replacement")
	}
	t.Log("the native attachment timeout retired Route control; delayed old cleanup preserved the reattachment")

	// Hold the exact old-generation reply, advance the runtime generation,
	// then verify there is no successful final from the replaced binding.
	peerCaller, cancelPeerCaller := context.WithCancel(ctx)
	defer cancelPeerCaller()
	if fullFaults {
		pipe.writeFault.Store(routeAggregateFixtureFullError)
		pipe.captureNextOperator.Store(true)
	}
	go func() {
		_, err := query(peerCaller)
		finished <- err
	}()
	peerQuery := awaitObservation(7, next)
	if fullFaults {
		cancelObservedOperator(cancelPeerCaller, finished)
	}
	creators.mu.Lock()
	newGeneration := tracker.MarkSpawned(api.BuiltinRouteTaskName, original.PID, birth)
	creators.mu.Unlock()
	if newGeneration == operation.generation {
		t.Fatal("fixture retained the old generation")
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "allow-seventh"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if fullFaults {
		waitFinished(peerQuery)
		if !peerQuery.received || peerQuery.status != (api.RouteAggregateStatus{}) ||
			!errors.Is(peerQuery.err, errAggregatePeerChanged) || !errors.Is(peerQuery.err, errRouteAggregateFixtureWrite) {
			t.Fatalf("late old-generation proof = %+v, %v, read=%t", peerQuery.status, peerQuery.err, peerQuery.received)
		}
		t.Log("the sole reader correlated the late proof after caller exit; original peer revalidation denied the changed generation")
	} else {
		select {
		case err := <-finished:
			if err == nil || err.Error() != "OPERATOR_AGGREGATE_PEER_CHANGED" {
				t.Fatalf("old-generation result survived replacement: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	select {
	case <-next.done:
	case <-ctx.Done():
		t.Fatal("replaced generation resource was not settled", ctx.Err())
	}
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "retired-2")); err != nil {
		t.Fatal("changed generation did not retire Route control", err)
	}

	wrong := api.SupervisorLockOwner{PID: original.PID, StartedAt: original.CreationTime}
	_, err = api.QuerySupervisorAggregateStatus(ctx, stateDir, wrong, original, func(context.Context) error {
		t.Error("wrong supervisor peer reached current-owner check")
		return nil
	})
	if !errors.Is(err, api.ErrSupervisorControlPeerIdentity) {
		t.Fatalf("wrong server was not refused on the connected pipe: %v", err)
	}

}

type routeAggregateFixtureAcceptor struct {
	*SupervisorIPCListener
	writeFault          atomic.Uint32
	challengeWrites     atomic.Int32
	holdAttachAck       atomic.Bool
	attachAckStarted    chan struct{}
	attachAckRelease    chan struct{}
	expiredWrite        chan struct{}
	partialStarted      chan struct{}
	partialRelease      chan struct{}
	captureNextOperator atomic.Bool
	operatorResult      chan error
	operatorDeps        ipcDispatchDeps
	session             *ipcSession
}

func (a *routeAggregateFixtureAcceptor) Accept() (net.Conn, error) {
	for {
		raw, err := a.SupervisorIPCListener.Accept()
		if err != nil {
			return nil, err
		}
		conn := &routeAggregateFixtureConn{Conn: raw, acceptor: a}
		if !a.captureNextOperator.CompareAndSwap(true, false) {
			return conn, nil
		}
		// The ordinary dispatcher discards the handler's return value.
		// Receive it directly for the one marked native test connection.
		owned := &aggregateIPCConn{Conn: conn}
		if !a.session.register(owned) {
			_ = owned.Close()
			a.operatorResult <- net.ErrClosed
			continue
		}
		go func() {
			defer a.session.finish(owned)
			defer owned.Close()
			a.operatorResult <- a.serveMarkedOperator(owned)
		}()
	}
}

func (a *routeAggregateFixtureAcceptor) serveMarkedOperator(conn net.Conn) error {
	if err := a.WriteHello(conn); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(ipcConnIdleTimeout)); err != nil {
		return err
	}
	marker, err := readSupervisorIPCFirstByte(conn)
	if err != nil {
		return err
	}
	if marker[0] != api.OperatorAggregateStatusMarker {
		return fmt.Errorf("expected the marked operator fixture, got marker %d", marker[0])
	}
	return serveOperatorAggregateStatus(conn, a.operatorDeps)
}

type routeAggregateFixtureConn struct {
	net.Conn
	acceptor      *routeAggregateFixtureAcceptor
	writeDeadline time.Time
}

func (c *routeAggregateFixtureConn) Fd() uintptr {
	return c.Conn.(interface{ Fd() uintptr }).Fd()
}

const (
	routeAggregateFixtureSplit uint32 = iota + 1
	routeAggregateFixtureZero
	routeAggregateFixturePartial
	routeAggregateFixtureSplitFullError
	routeAggregateFixtureFullError
	routeAggregateFixtureExpiredFull
	routeAggregateFixtureVerifyDeadline
	routeAggregateFixtureExpiredZero
)

var errRouteAggregateFixtureWrite = errors.New("fixture route write result")

func (c *routeAggregateFixtureConn) SetWriteDeadline(deadline time.Time) error {
	c.writeDeadline = deadline
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *routeAggregateFixtureConn) Write(frame []byte) (int, error) {
	if bytes.Contains(frame, []byte("\"result\":{\"generation\":")) &&
		c.acceptor.holdAttachAck.CompareAndSwap(true, false) {
		close(c.acceptor.attachAckStarted)
		<-c.acceptor.attachAckRelease
	}
	if !bytes.Contains(frame, []byte(`"challenge":`)) {
		return c.Conn.Write(frame)
	}
	a := c.acceptor
	a.challengeWrites.Add(1)
	fault := a.writeFault.Swap(0)
	switch fault {
	case routeAggregateFixtureZero, routeAggregateFixtureExpiredZero:
		if fault == routeAggregateFixtureExpiredZero {
			<-time.After(time.Until(c.writeDeadline))
		}
		return 0, io.ErrShortWrite
	case routeAggregateFixturePartial, routeAggregateFixtureSplit, routeAggregateFixtureSplitFullError:
		prefix := len(frame) / 2
		n, err := c.Conn.Write(frame[:prefix])
		if err != nil {
			return n, err
		}
		if n != prefix {
			return n, io.ErrShortWrite
		}
		if fault == routeAggregateFixturePartial {
			return n, errRouteAggregateFixtureWrite
		}
		close(a.partialStarted)
		<-a.partialRelease
		rest, err := c.Conn.Write(frame[n:])
		if fault == routeAggregateFixtureSplitFullError && err == nil && n+rest == len(frame) {
			err = errRouteAggregateFixtureWrite
		}
		return n + rest, err
	case routeAggregateFixtureFullError, routeAggregateFixtureExpiredFull, routeAggregateFixtureVerifyDeadline:
		n, err := c.Conn.Write(frame)
		if err != nil || n != len(frame) {
			return n, err
		}
		if fault == routeAggregateFixtureExpiredFull || fault == routeAggregateFixtureVerifyDeadline {
			<-time.After(time.Until(c.writeDeadline))
			close(a.expiredWrite)
			if fault == routeAggregateFixtureVerifyDeadline {
				<-a.partialRelease
			}
		}
		return n, errRouteAggregateFixtureWrite
	default:
		return c.Conn.Write(frame)
	}
}

func TestRouteAggregateStatusNativeChild(t *testing.T) {
	fixtureDir := os.Getenv(routeAggregateFixtureChild)
	if fixtureDir == "" {
		t.Skip("dedicated original-process transport child")
	}
	deadline, finite := t.Deadline()
	if !finite {
		t.Fatal("aggregate proof child requires a deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	var launch api.RouteAggregateLaunch
	if err := json.Unmarshal([]byte(os.Getenv(routeAggregateLaunchEnv)), &launch); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", launch.StateDir)
	serveDone := make(chan error, 1)
	go func() {
		var observed, retired int
		serveDone <- api.ServeRouteAggregateControl(ctx, launch, api.RouteAggregateControl{
			Status: func() api.RouteAggregateStatus {
				observed++
				if err := os.WriteFile(filepath.Join(fixtureDir, fmt.Sprintf("observation-%d", observed)), nil, 0o600); err != nil {
					t.Error(err)
				}
				gates := map[int]string{3: "allow-third", 5: "allow-fifth", 7: "allow-seventh"}
				if gate, ok := gates[observed]; ok {
					if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, gate)); err != nil {
						t.Error(err)
					}
				}
				return api.RouteAggregateStatus{Ownership: api.RouteAggregateOwned, Revision: uint64(observed), Effective: "ready"}
			},
			Apply: func(applyCtx, _ context.Context, challenge api.RouteAggregateStatusChallenge) api.RouteAggregateStatus {
				mode := os.Getenv("MCPHUB_AGGREGATE_STATUS_TEST_VERIFY")
				if mode == "apply_before_ack_ready" {
					apply := challenge.Apply
					if apply == nil || apply.ReceiptSHA256 != strings.Repeat("ab", 32) ||
						apply.EndpointSHA256 != strings.Repeat("cd", 32) {
						t.Error("original Route received a different Apply selection")
						return api.RouteAggregateStatus{}
					}
					if err := api.VerifyRouteAggregateGeneration(applyCtx, launch, challenge); err != nil {
						t.Error("original Route could not verify its Apply nonce", err)
						return api.RouteAggregateStatus{}
					}
					return api.RouteAggregateStatus{Ownership: api.RouteAggregateOwned, Revision: 1, Effective: "ready"}
				}
				if mode != "expired_apply_verify" {
					t.Error("unexpected Apply challenge in the isolated fixture")
					return api.RouteAggregateStatus{}
				}
				verify := func(name string) {
					result := "accepted"
					// The test sends a fresh verifier connection after the original
					// operation expires. Authorization must come from its owner.
					if err := api.VerifyRouteAggregateGeneration(ctx, launch, challenge); err != nil {
						result = err.Error()
					}
					if err := os.WriteFile(filepath.Join(fixtureDir, name), []byte(result), 0o600); err != nil {
						t.Error(err)
					}
				}
				verify("verify-before")
				if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "verify-expired")); err != nil {
					t.Error(err)
					return api.RouteAggregateStatus{}
				}
				verify("verify-after")
				return api.RouteAggregateStatus{Ownership: api.RouteAggregateOwned, Revision: 1, Effective: "ready"}
			},
			Retire: func(context.Context) api.RouteAggregateRetirement {
				retired++
				err := os.WriteFile(filepath.Join(fixtureDir, fmt.Sprintf("retired-%d", retired)), nil, 0o600)
				return api.RouteAggregateRetirement{Err: err}
			},
		})
	}()
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "release")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-serveDone; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, winio.ErrTimeout) && !errors.Is(err, io.EOF) {
		mode := os.Getenv("MCPHUB_AGGREGATE_STATUS_TEST_VERIFY")
		if mode != "apply_before_ack_expiry" ||
			(!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded)) {
			t.Fatal(err)
		}
	}
}

func TestOperatorAggregateMarkedUnknownPeerConsumesOnlyMarker(t *testing.T) {
	input := &routeAggregateReadTrace{source: strings.NewReader(string(api.OperatorAggregateStatusMarker) + `{"version":1,"id":8,"cmd":"aggregate-status"}` + "\n")}
	conn := &aggregateStatusUnknownConn{input: input}
	owner := &routeAggregateSupervisor{ctx: t.Context()}
	handleIPCConn(conn, ipcDispatchDeps{daemonCreators: &daemonCreatorRecords{routeAggregate: owner}})
	var response api.IPCResponse
	err := json.Unmarshal(conn.output.Bytes(), &response)
	if input.bytesRead != 1 || input.maxRead != 1 || err != nil || response.OK || response.ID != 0 ||
		!response.Final || response.Error == nil || response.Error.Code != "OPERATOR_AGGREGATE_PEER_DENIED" {
		t.Fatalf("pre-body refusal consumed %d bytes, response %s, decode %v", input.bytesRead, conn.output.Bytes(), err)
	}
}

type aggregateStatusUnknownConn struct {
	net.Conn
	input  io.Reader
	output bytes.Buffer
}

func (c *aggregateStatusUnknownConn) Read(p []byte) (int, error)      { return c.input.Read(p) }
func (c *aggregateStatusUnknownConn) Write(p []byte) (int, error)     { return c.output.Write(p) }
func (c *aggregateStatusUnknownConn) SetReadDeadline(time.Time) error { return nil }
func (c *aggregateStatusUnknownConn) SetDeadline(time.Time) error     { return nil }
func (c *aggregateStatusUnknownConn) Close() error                    { return nil }

func TestLiveAggregateStatusCommandRoleAndMissingBinding(t *testing.T) {
	const image = "C:\\Program Files\\Hub\\mcphub.exe"
	for _, tc := range []struct {
		args string
		want bool
	}{
		{"supervise", true},
		{"supervise --strict-mode --strict-job-protection", true},
		{"supervise --ensure-alive", false},
		{"supervise --no-ipc", false},
		{"supervise --state-dir elsewhere", false},
		{"supervise dump-stacks", false},
		{"route", false},
		{"gui", false},
		{"", false},
	} {
		if got := isLiveAggregateSupervisorCommand("\""+image+"\" "+tc.args, image); got != tc.want {
			t.Errorf("%q accepted=%v, want %v", tc.args, got, tc.want)
		}
	}
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"hub-mcp", "status", "--live"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); !errors.Is(err, api.ErrSupervisorIPCUnavailable) || out.Len() != 0 {
		t.Fatalf("missing binding returned output=%q error=%v", out.String(), err)
	}
}

func waitRouteAggregateFixtureFile(ctx context.Context, path string) ([]byte, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, err := os.ReadFile(path)
		if !os.IsNotExist(err) {
			return value, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
