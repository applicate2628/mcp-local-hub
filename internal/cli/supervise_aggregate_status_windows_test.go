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

	operation, err := creators.begin(api.BuiltinRouteTaskName)
	if err != nil {
		t.Fatal(err)
	}
	exe := owner.launch.Peer.ImagePath
	fixtureDir := t.TempDir()
	cmd := exec.Command(exe, "-test.run=^TestRouteAggregateStatusNativeChild$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), routeAggregateFixtureChild+"="+fixtureDir)
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
	go func() {
		defer close(session.acceptDone)
		acceptIPCConnectionsWithSession(listener, ipcDispatchDeps{
			daemonCreators: creators, runtimeTracker: tracker, events: eventLog,
		}, session)
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
	first := waitBinding(nil)
	heldCleanup.Store(first)

	query := func(qctx context.Context) (api.RouteAggregateStatus, error) {
		return api.QuerySupervisorAggregateStatus(qctx, stateDir, lock.Owner(), owner.launch.Peer, func(context.Context) error {
			got, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
			if err != nil || got != lock.Owner() {
				return fmt.Errorf("original singleton changed: %w", err)
			}
			return nil
		})
	}
	expectLive := func() {
		t.Helper()
		status, err := query(ctx)
		if err != nil || status != (api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner, Generation: operation.generation}) {
			t.Fatalf("live original route result = %+v, error = %v", status, err)
		}
	}
	expectLive()
	expectLive()
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "observation-2")); err != nil {
		t.Fatal(err)
	}

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
	go func() {
		_, err := query(operator)
		finished <- err
	}()
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "observation-3")); err != nil {
		t.Fatal(err)
	}
	if _, err := query(ctx); err == nil || err.Error() != "OPERATOR_AGGREGATE_BUSY" {
		t.Fatalf("parallel operator query did not receive busy: %v", err)
	}

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

	cancelOperator()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("operator cancel = %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
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
	if err := os.WriteFile(filepath.Join(fixtureDir, "allow-third"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	next := waitBinding(first)
	expectLive()
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
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "observation-4")); err != nil {
		t.Fatal(err)
	}
	if first == next {
		t.Fatal("a canceled binding was reused")
	}

	// Hold the exact old-generation reply, advance the runtime generation,
	// then verify there is no successful final from the replaced binding.
	go func() {
		_, err := query(ctx)
		finished <- err
	}()
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "observation-5")); err != nil {
		t.Fatal(err)
	}
	creators.mu.Lock()
	newGeneration := tracker.MarkSpawned(api.BuiltinRouteTaskName, original.PID, birth)
	creators.mu.Unlock()
	if newGeneration == operation.generation {
		t.Fatal("fixture retained the old generation")
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "allow-fifth"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil || err.Error() != "OPERATOR_AGGREGATE_PEER_CHANGED" {
			t.Fatalf("old-generation result survived replacement: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-next.done:
	case <-ctx.Done():
		t.Fatal("replaced generation resource was not settled", ctx.Err())
	}

	wrong := api.SupervisorLockOwner{PID: original.PID, StartedAt: original.CreationTime}
	_, err = api.QuerySupervisorAggregateStatus(ctx, stateDir, wrong, original, func(context.Context) error {
		t.Error("wrong supervisor peer reached current-owner check")
		return nil
	})
	if !errors.Is(err, api.ErrSupervisorControlPeerIdentity) {
		t.Fatalf("wrong server was not refused on the connected pipe: %v", err)
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
		var observed int
		serveDone <- api.ServeRouteAggregateStatus(ctx, launch, func() string {
			observed++
			if err := os.WriteFile(filepath.Join(fixtureDir, fmt.Sprintf("observation-%d", observed)), nil, 0o600); err != nil {
				t.Error(err)
			}
			switch observed {
			case 3, 5:
				gate := map[int]string{3: "allow-third", 5: "allow-fifth"}
				if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, gate[observed])); err != nil {
					t.Error(err)
				}
			}
			return api.RouteAggregateNotOwner
		})
	}()
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "release")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-serveDone; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, winio.ErrTimeout) && !errors.Is(err, io.EOF) {
		t.Fatal(err)
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
