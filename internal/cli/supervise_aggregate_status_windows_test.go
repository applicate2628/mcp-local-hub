//go:build windows

package cli

import (
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
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

const routeAggregateFixtureChild = "MCPHUB_AGGREGATE_STATUS_TEST_CHILD"

func TestRouteAggregateStatusOpenReadsBoundedFrames(t *testing.T) {
	encoded, err := json.Marshal(api.IPCRequest{Version: 1, ID: 1<<63 - 1, Cmd: api.SupervisorAggregateStatusCommand})
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
			if !tc.wantError && (req.ID != 1<<63-1 || req.Cmd != api.SupervisorAggregateStatusCommand) {
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

	serve := func(handle func(net.Conn) error) <-chan error {
		done := make(chan error, 1)
		go func() {
			defer close(done)
			conn, err := listener.Accept()
			if err == nil {
				defer conn.Close()
				err = handle(conn)
			}
			done <- err
		}()
		t.Cleanup(func() {
			_ = listener.Close()
			select {
			case <-done:
			case <-ctx.Done():
			}
		})
		return done
	}
	startExchange := func() <-chan error {
		return serve(func(conn net.Conn) error {
			if err := conn.SetDeadline(time.Now().Add(api.SupervisorAggregateStatusTimeout)); err != nil {
				return err
			}
			if err := listener.WriteHello(conn); err != nil {
				return err
			}
			handleIPCConn(conn, ipcDispatchDeps{daemonCreators: creators, runtimeTracker: tracker})
			return nil
		})
	}
	receive := func(done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	positive := startExchange()
	creators.mu.Lock()
	operation.generation = tracker.MarkSpawned(api.BuiltinRouteTaskName, original.PID, birth)
	creators.mu.Unlock()
	creators.endStart(operation, nil)
	receive(positive)

	observed, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "received.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Status api.RouteAggregateStatus `json:"status"`
		Error  string                   `json:"error"`
	}
	if err := json.Unmarshal(observed, &report); err != nil || report.Error != "" ||
		report.Status.Effective != api.RouteAggregateOff || report.Status.Generation != operation.generation {
		t.Fatalf("original route status = %q: %v", observed, err)
	}

	// This process has the same image, user and session, but it is not the
	// route creator's live root. It must receive no challenge or local status.
	rejected := startExchange()
	_, err = api.DialRouteAggregateStatus(ctx, owner.launch, func() string {
		t.Error("wrong route peer was challenged")
		return api.RouteAggregateOff
	})
	var refusal *api.IPCErr
	if !errors.As(err, &refusal) || refusal.Code != "ROUTE_AGGREGATE_PEER_DENIED" {
		t.Fatalf("wrong client was not refused: %v", err)
	}
	receive(rejected)

	// A peer with only the mode marker must be refused without waiting for
	// any body bytes. This is the same real pipe used by the original child.
	modeOnly := startExchange()
	dialFixture := func() (net.Conn, *json.Decoder) {
		t.Helper()
		timeout := api.SupervisorAggregateStatusTimeout
		conn, err := winio.DialPipe(api.SupervisorIPCAddress(stateDir), &timeout)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(conn)
		var hello struct {
			Hello api.IPCHello
		}
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
		t.Fatalf("mode-only peer was not refused before body: %+v, %v", denied, err)
	}
	_ = conn.Close()
	receive(modeOnly)

	// The existing framed stream remains usable across more than one legacy
	// request. An ordinary frame cannot invoke the new privileged command.
	legacy := startExchange()
	conn, decoder = dialFixture()
	for id, command := range []string{api.SupervisorAggregateStatusCommand, "capabilities", "capabilities"} {
		req := api.IPCRequest{ID: int64(id) + 1, Cmd: command}
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			t.Fatal(err)
		}
		var reply api.IPCResponse
		if err := decoder.Decode(&reply); err != nil || reply.ID != req.ID || !reply.Final {
			t.Fatalf("legacy response: %+v, %v", reply, err)
		}
		if command == api.SupervisorAggregateStatusCommand {
			if reply.OK || reply.Error == nil || reply.Error.Code != "UNKNOWN_COMMAND" {
				t.Fatalf("unmarked command was accepted: %+v", reply)
			}
		} else if !reply.OK || reply.Error != nil {
			t.Fatalf("legacy capabilities was changed: %+v", reply)
		}
	}
	_ = conn.Close()
	receive(legacy)

	// Supply independently captured identity of a different still-live
	// process. The real named pipe must receive zero query bytes.
	wrong := api.RouteAggregateLaunch{
		StateDir: stateDir, Owner: api.SupervisorLockOwner{PID: original.PID, StartedAt: original.CreationTime}, Peer: original,
	}
	wrongServer := serve(func(conn net.Conn) error {
		if err := conn.SetReadDeadline(time.Now().Add(api.SupervisorAggregateStatusTimeout)); err != nil {
			return err
		}
		var b [1]byte
		n, err := conn.Read(b[:])
		if n != 0 || !errors.Is(err, io.EOF) {
			return fmt.Errorf("wrong server received %d request bytes, read error %v", n, err)
		}
		return nil
	})
	_, err = api.DialRouteAggregateStatus(ctx, wrong, func() string {
		t.Error("wrong supervisor peer reached route observation")
		return api.RouteAggregateOff
	})
	if !errors.Is(err, api.ErrSupervisorControlPeerIdentity) {
		t.Fatalf("wrong server was not refused on the connected pipe: %v", err)
	}
	receive(wrongServer)

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
	status, callErr := api.DialRouteAggregateStatus(ctx, launch, func() string { return api.RouteAggregateOff })
	report := struct {
		Status api.RouteAggregateStatus `json:"status"`
		Error  string                   `json:"error"`
	}{Status: status}
	if callErr != nil {
		report.Error = callErr.Error()
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(fixtureDir, "receiving.pending")
	if err := os.WriteFile(pending, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pending, filepath.Join(fixtureDir, "received.json")); err != nil {
		t.Fatal(err)
	}
	if callErr != nil {
		t.Fatal(callErr)
	}
	if _, err := waitRouteAggregateFixtureFile(ctx, filepath.Join(fixtureDir, "release")); err != nil {
		t.Fatal(err)
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
