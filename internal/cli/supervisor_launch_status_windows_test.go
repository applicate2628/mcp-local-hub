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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

const supervisorPeerSenderChildEnv = "MCPHUB_TEST_PEER_SENDER_CHILD"

// Faults are SYNTHETIC provider outcomes over a real native pipe. The wrapper
// forwards the original Fd and closes the actual connection exactly once.
type supervisorPeerSenderFaultConn struct {
	net.Conn
	fd               uintptr
	closeErr         error
	deadlineErr      error
	closes           int
	writeErr         error
	writeN           int
	writes           int
	cancelAfterHello context.CancelFunc
}

func (c *supervisorPeerSenderFaultConn) Fd() uintptr { return c.fd }
func (c *supervisorPeerSenderFaultConn) Read(raw []byte) (int, error) {
	n, err := c.Conn.Read(raw)
	if c.cancelAfterHello != nil && n != 0 && raw[n-1] == '\n' {
		c.cancelAfterHello()
	}
	return n, err
}
func (c *supervisorPeerSenderFaultConn) Write(raw []byte) (int, error) {
	c.writes++
	if c.writeErr != nil {
		return c.writeN, c.writeErr
	}
	return c.Conn.Write(raw)
}
func (c *supervisorPeerSenderFaultConn) Close() error {
	c.closes++
	return errors.Join(c.Conn.Close(), c.closeErr)
}
func (c *supervisorPeerSenderFaultConn) SetDeadline(deadline time.Time) error {
	if c.deadlineErr != nil {
		return c.deadlineErr
	}
	return c.Conn.SetDeadline(deadline)
}

func TestSupervisorPeerSenderResponseOutcomes(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("Root's bounded actor must provide a test deadline")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	peer, err := api.ObserveSupervisorProcessIdentityV1(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	owner := api.SupervisorLockOwner{PID: peer.PID, StartedAt: peer.CreationTime}
	for _, scenario := range []string{"final-and-close-error", "successful-final-and-close-error", "accepted-and-real-server-eof", "deadline-and-close-error", "hello-and-close-error", "canceled-before-write-and-close-error", "marshal-and-close-error", "zero-write-and-close-error", "partial-write-and-close-error"} {
		t.Run(scenario, func(t *testing.T) {
			requestCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			closeFailure := errors.New("synthetic sender close result")
			deadlineFailure := errors.New("synthetic sender deadline result")
			writeFailure := errors.New("synthetic sender write result")
			noWrite := scenario == "deadline-and-close-error" || scenario == "hello-and-close-error" || scenario == "canceled-before-write-and-close-error" || scenario == "marshal-and-close-error"
			address, receive := supervisorPeerSenderPipe(t, ctx, func(conn net.Conn) error {
				if scenario == "deadline-and-close-error" {
					return supervisorPeerSenderNoRequest(conn, nil)
				}
				helloOwner := owner
				if scenario == "hello-and-close-error" {
					helloOwner.StartedAt = "synthetic-other-generation"
				}
				if err := (&SupervisorIPCListener{pid: helloOwner.PID, startedAt: helloOwner.StartedAt}).WriteHello(conn); err != nil {
					return err
				}
				if noWrite || scenario == "zero-write-and-close-error" || scenario == "partial-write-and-close-error" {
					return supervisorPeerSenderNoRequest(conn, nil)
				}
				var request api.IPCRequest
				if err := json.NewDecoder(conn).Decode(&request); err != nil {
					return err
				}
				if request.Cmd != "exit" {
					return errors.New("response fixture received wrong command")
				}
				response := api.IPCResponse{ID: request.ID, OK: true, Result: map[string]any{"accepted": true}}
				if scenario == "successful-final-and-close-error" {
					response.Final = true
				}
				if scenario == "final-and-close-error" {
					response.OK, response.Final = false, true
					response.Error = &api.IPCErr{Code: "FIXTURE_FINAL", Message: "synthetic final response"}
				}
				return json.NewEncoder(conn).Encode(response)
			})
			timeout := 5 * time.Second
			conn, err := winio.DialPipe(address, &timeout)
			if err != nil {
				t.Fatal(err)
			}
			fdConn, ok := conn.(interface{ Fd() uintptr })
			if !ok {
				t.Fatal(errors.Join(errors.New("real pipe Fd unavailable"), conn.Close()))
			}
			wrapped := &supervisorPeerSenderFaultConn{Conn: conn, fd: fdConn.Fd()}
			t.Cleanup(func() {
				if wrapped.closes == 0 {
					t.Error("exchange failed to settle its actual connection")
					if err := wrapped.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			if scenario != "accepted-and-real-server-eof" {
				wrapped.closeErr = closeFailure
			}
			if scenario == "deadline-and-close-error" {
				wrapped.deadlineErr = deadlineFailure
			}
			if scenario == "canceled-before-write-and-close-error" {
				wrapped.cancelAfterHello = cancelRequest
			}
			if scenario == "zero-write-and-close-error" || scenario == "partial-write-and-close-error" {
				wrapped.writeErr = writeFailure
				if scenario == "partial-write-and-close-error" {
					wrapped.writeN = 1
				}
			}
			args := map[string]any{"graceful": true, "timeout_ms": 5000}
			if scenario == "marshal-and-close-error" {
				args["unsupported"] = make(chan int)
			}
			response, err := exchangeSupervisorIPCResponse(requestCtx, wrapped, owner, &peer, "exit", args, timeout)
			if wrapped.closes != 1 {
				t.Fatalf("actual connection closes=%d, want one", wrapped.closes)
			}
			dispatch, ok := err.(*supervisorIPCDispatchError)
			if !ok || dispatch.requestWriteEntered == noWrite || dispatch.owner != owner || dispatch.peer != peer || dispatch.command != "exit" {
				t.Fatalf("actual exchange dispatch binding lost: %v", err)
			}
			if (noWrite && wrapped.writes != 0) || (!noWrite && wrapped.writes != 1) {
				t.Fatalf("request writes=%d for %s", wrapped.writes, scenario)
			}
			if scenario != "accepted-and-real-server-eof" && !errors.Is(err, closeFailure) {
				t.Fatalf("post-Close envelope lost actual cause: %v", err)
			}
			switch scenario {
			case "successful-final-and-close-error":
				if !response.OK || !response.Final || response.Error != nil || response.ID == 0 {
					t.Fatalf("actual successful response lost after Close: %+v / %v", response, err)
				}
			case "final-and-close-error":
				if !errors.Is(err, closeFailure) || !response.Final || response.OK || response.Error == nil || response.Error.Code != "FIXTURE_FINAL" || response.ID == 0 {
					t.Fatalf("actual final response/close cause lost: %+v / %v", response, err)
				}
			case "accepted-and-real-server-eof":
				result, ok := response.Result.(map[string]any)
				if !errors.Is(err, io.EOF) || !response.OK || response.Final || response.ID == 0 || !ok || result["accepted"] != true {
					t.Fatalf("actual accepted response/EOF lost: %+v / %v", response, err)
				}
			case "deadline-and-close-error":
				if !errors.Is(err, deadlineFailure) || !errors.Is(err, closeFailure) || response.OK || response.Final {
					t.Fatalf("deadline/close causes blurred: %+v / %v", response, err)
				}
			case "canceled-before-write-and-close-error":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("pre-write cancellation cause lost: %v", err)
				}
			case "zero-write-and-close-error", "partial-write-and-close-error":
				if !errors.Is(err, writeFailure) {
					t.Fatalf("entered write cause lost: %v", err)
				}
			}
			if err := receive(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Native transport/real response-returning sender proof, never Product command
// role or private control-proof issuance. Root separately admits this actor.
func TestSupervisorPeerSenderFreshWrongPeer(t *testing.T) {
	if os.Getenv(supervisorPeerSenderChildEnv) != "" {
		t.Fatal("unexpected sender helper sentinel")
	}
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("Root's bounded actor must provide a test deadline")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	stateDir, original := startSupervisorPeerSenderFixture(t, ctx, deadline)
	owner := api.SupervisorLockOwner{PID: original.PID, StartedAt: original.CreationTime}
	if err := api.WriteStateFileAtomic(filepath.Join(stateDir, "supervisor.lock.owner.json"), owner); err != nil {
		t.Fatal(err)
	}
	if err := api.VerifySupervisorControlGenerationForPeerFromStateDir(ctx, stateDir, owner, original); err != nil {
		t.Fatalf("initial actual-original strict status refused: %v", err)
	}
	response, err := sendIPCWithResponseForPeer(ctx, api.SupervisorIPCAddress(stateDir), owner, original, "exit", map[string]any{"graceful": true, "timeout_ms": 5000}, 5*time.Second)
	if err != nil || !response.OK || !response.Final || response.Error != nil {
		t.Fatalf("actual original response-returning sender: response=%+v error=%v", response, err)
	}
	if original.PID == os.Getpid() {
		t.Fatal("original helper and retained wrong server must be different processes")
	}
	for _, candidate := range []struct {
		name string
		peer api.SupervisorProcessIdentityV1
	}{{"actual-wrong-server", original}, {"missing-original-expectation", api.SupervisorProcessIdentityV1{}}} {
		t.Run(candidate.name, func(t *testing.T) {
			address, receive := supervisorPeerSenderPipe(t, ctx, func(conn net.Conn) error {
				helloErr := (&SupervisorIPCListener{pid: owner.PID, startedAt: owner.StartedAt}).WriteHello(conn)
				return supervisorPeerSenderNoRequest(conn, helloErr)
			})
			response, err := sendIPCWithResponseForPeer(ctx, address, owner, candidate.peer, "exit", map[string]any{"graceful": true, "timeout_ms": 5000}, 5*time.Second)
			if !errors.Is(err, api.ErrSupervisorControlPeerIdentity) || response.OK || response.Final {
				t.Fatalf("wrong/missing original accepted: response=%+v error=%v", response, err)
			}
			if err := receive(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func supervisorPeerSenderDisconnected(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED)
}

func supervisorPeerSenderNoRequest(conn net.Conn, helloErr error) error {
	var raw [1]byte
	for {
		n, readErr := conn.Read(raw[:])
		if n != 0 {
			return errors.New("refused sender peer received client request bytes")
		}
		if readErr != nil {
			if supervisorPeerSenderDisconnected(readErr) && supervisorPeerSenderDisconnected(helloErr) {
				return nil
			}
			return errors.Join(helloErr, readErr)
		}
	}
}

// A single test-discriminated in-process listener; Root owns the test parent.
// Returning receive joins actual connection processing, so zero-frame checks
// observe a completed read/close rather than a transient absence of a request.
func supervisorPeerSenderPipe(t *testing.T, ctx context.Context, serve func(net.Conn) error) (string, func() error) {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	address := api.SupervisorIPCAddress(stateDir)
	if !strings.HasPrefix(address, `\\.\pipe\mcphub-supervisor-test-`) {
		t.Fatal("refusing a non-test sender pipe")
	}
	listener, err := winio.ListenPipe(address, &winio.PipeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var closeCurrent func() error
	stopping := false
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			mu.Lock()
			expected := stopping
			mu.Unlock()
			if expected && errors.Is(err, net.ErrClosed) {
				err = nil
			}
			done <- err
			return
		}
		var closeOnce sync.Once
		var closeErr error
		closeConn := func() error { closeOnce.Do(func() { closeErr = conn.Close() }); return closeErr }
		mu.Lock()
		closeCurrent = closeConn
		stop := stopping
		mu.Unlock()
		if stop {
			done <- closeConn()
			return
		}
		if deadline, ok := ctx.Deadline(); ok {
			err = conn.SetDeadline(deadline)
		}
		if err == nil {
			err = serve(conn)
		}
		done <- errors.Join(err, closeConn())
	}()
	var receiveOnce sync.Once
	var received error
	receive := func() error { receiveOnce.Do(func() { received = <-done }); return received }
	t.Cleanup(func() {
		mu.Lock()
		stopping = true
		if closeCurrent != nil {
			if err := closeCurrent(); err != nil {
				t.Error(err)
			}
		}
		mu.Unlock()
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		if err := receive(); err != nil {
			t.Error(err)
		}
	})
	return address, receive
}

type supervisorPeerSenderFixtureConfig struct {
	Owner    api.SupervisorLockOwner
	Deadline time.Time
	StateDir string
}

func startSupervisorPeerSenderFixture(t *testing.T, ctx context.Context, deadline time.Time) (string, api.SupervisorProcessIdentityV1) {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	address := api.SupervisorIPCAddress(stateDir)
	if !strings.HasPrefix(address, `\\.\pipe\mcphub-supervisor-test-`) {
		t.Fatal("refusing a non-test original sender pipe")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestSupervisorPeerSenderChild$")
	cmd.Env = append(os.Environ(), supervisorPeerSenderChildEnv+"=1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(errors.Join(err, input.Close()))
	}
	t.Cleanup(func() {
		closeErr := input.Close()
		if err := errors.Join(closeErr, cmd.Wait()); err != nil {
			t.Errorf("original sender helper settlement: %v", err)
		}
	})
	owner, err := captureGUISupervisorGeneration(cmd.Process)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := captureCreatedSupervisorPeer(cmd.Process, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(input).Encode(supervisorPeerSenderFixtureConfig{Owner: owner, Deadline: deadline, StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := os.ReadFile(filepath.Join(stateDir, "peer-sender-ready"))
		if err == nil {
			if string(raw) != address {
				t.Fatal("original helper pipe discriminator changed")
			}
			return stateDir, peer
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestSupervisorPeerSenderChild(t *testing.T) {
	if os.Getenv(supervisorPeerSenderChildEnv) != "1" {
		return
	}
	var config supervisorPeerSenderFixtureConfig
	if err := json.NewDecoder(os.Stdin).Decode(&config); err != nil {
		if !errors.Is(err, io.EOF) {
			t.Error(err)
		}
		return
	}
	// Normal CLI TestMain installs its own state override. Restore only this
	// helper's explicitly passed private test namespace after that setup, never
	// weaken TestMain or assume the inherited discriminator survived it.
	stateDir := config.StateDir
	if !filepath.IsAbs(stateDir) || config.Deadline.IsZero() {
		t.Fatal("sender child requires private namespace and original deadline")
	}
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	address := api.SupervisorIPCAddress(stateDir)
	if !strings.HasPrefix(address, `\\.\pipe\mcphub-supervisor-test-`) {
		t.Fatal("sender child refuses non-test pipe")
	}
	listener, err := winio.ListenPipe(address, &winio.PipeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var listenerOnce sync.Once
	closeListener := func() {
		listenerOnce.Do(func() {
			if err := listener.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	defer closeListener()
	var mu sync.Mutex
	var closeCurrent func()
	stopping := false
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Error(err)
		}
		mu.Lock()
		stopping = true
		if closeCurrent != nil {
			closeCurrent()
		}
		mu.Unlock()
		closeListener()
	}()
	defer func() { <-controlDone }()
	ready := filepath.Join(stateDir, "peer-sender-ready")
	if err := os.WriteFile(ready+".pending", []byte(address), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ready+".pending", ready); err != nil {
		t.Fatal(err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			mu.Lock()
			expected := stopping
			mu.Unlock()
			if !expected {
				t.Error(err)
			}
			return
		}
		var closeOnce sync.Once
		closeConn := func() {
			closeOnce.Do(func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		mu.Lock()
		closeCurrent = closeConn
		if stopping {
			closeConn()
		}
		mu.Unlock()
		err = conn.SetDeadline(config.Deadline)
		if err == nil {
			err = (&SupervisorIPCListener{pid: config.Owner.PID, startedAt: config.Owner.StartedAt}).WriteHello(conn)
		}
		var request api.IPCRequest
		if err == nil {
			err = json.NewDecoder(conn).Decode(&request)
		}
		if err == nil {
			var result any
			switch request.Cmd {
			case "status":
				result = map[string]any{"state": "running", "daemons": []any{}}
			case "exit":
				if request.Args["graceful"] != true || request.Args["timeout_ms"] != float64(5000) {
					err = errors.New("original sender fixture received changed exit operands")
				}
				result = map[string]any{"graceful_exit_initiated": true}
			default:
				err = fmt.Errorf("unexpected sender fixture command %q", request.Cmd)
			}
			if err == nil {
				err = json.NewEncoder(conn).Encode(api.IPCResponse{ID: request.ID, OK: true, Final: true, Result: result})
			}
		}
		closeConn()
		mu.Lock()
		closeCurrent = nil
		mu.Unlock()
		if err != nil {
			t.Error(err)
		}
	}
}

type capturedSupervisorStatusConn struct {
	net.Conn
	frame []byte
}

func (c *capturedSupervisorStatusConn) Write(raw []byte) (int, error) {
	c.frame = append(c.frame, raw...)
	return c.Conn.Write(raw)
}

func TestSupervisorLaunchStatusActualProducerSingleFrame(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	owner := api.SupervisorLockOwner{PID: 4242, StartedAt: "2026-10-09T00:00:00Z"}
	if err := api.WriteStateFileAtomic(filepath.Join(stateDir, "supervisor.lock.owner.json"), owner); err != nil {
		t.Fatal(err)
	}
	address := api.SupervisorIPCAddress(stateDir)
	if !strings.Contains(address, "-test-") {
		t.Fatal("refusing non-test status pipe")
	}
	listener, err := winio.ListenPipe(address, &winio.PipeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	frames := make(chan []byte, 1)
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("status producer fixture: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("status producer fixture did not close")
		}
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		helloProducer := &SupervisorIPCListener{pid: owner.PID, startedAt: owner.StartedAt}
		if err := helloProducer.WriteHello(conn); err != nil {
			done <- err
			return
		}
		var request api.IPCRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		captured := &capturedSupervisorStatusConn{Conn: conn}
		var ready, loaded atomic.Bool // valid control does not require reconcile readiness.
		err = dispatchIPCRequest(captured, request, ipcDispatchDeps{stateDir: stateDir, reconcileReady: &ready, intentFilesLoaded: &loaded})
		frames <- captured.frame
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := api.VerifySupervisorControlGenerationFromStateDir(ctx, stateDir, owner); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-frames:
		var actual map[string]json.RawMessage
		if err := json.Unmarshal(raw, &actual); err != nil {
			t.Fatal(err)
		}
		if _, present := actual["final"]; present {
			t.Fatalf("real status producer changed to streaming final: %s", raw)
		}
		if strings.Count(string(raw), "\n") != 1 {
			t.Fatalf("status was not one frame: %s", raw)
		}
		t.Logf("actual serialized producer status: %s", strings.TrimSpace(string(raw)))
	case <-ctx.Done():
		t.Fatal("actual producer frame not retained")
	}
}
