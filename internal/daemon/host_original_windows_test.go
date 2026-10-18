//go:build windows

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

const stdioOriginalHelperMode = "MCPHUB_STDIO_ORIGINAL_HELPER_MODE"
const stdioOriginalHelperRoot = "MCPHUB_STDIO_ORIGINAL_HELPER_ROOT"

func TestStdioOriginalHelper(t *testing.T) {
	mode := os.Getenv(stdioOriginalHelperMode)
	if mode == "" {
		return
	}
	root := os.Getenv(stdioOriginalHelperRoot)
	if root == "" {
		t.Fatal("helper root missing")
	}
	marker := func(name string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(name string) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(root, name)); err == nil {
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("helper barrier %s deadline", name)
	}
	switch mode {
	case "echo", "final":
		marker("root-ready")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var message map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"value\":\"received\"}}\n", message["id"]); err != nil {
				t.Fatal(err)
			}
			if mode == "final" {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
	case "wrapper":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(exe, "-test.run=^TestStdioOriginalHelper$")
		child.Env = composeChildEnv(map[string]string{stdioOriginalHelperMode: "descendant", stdioOriginalHelperRoot: root}, nil)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		defer child.Process.Release()
		wait("descendant-ready")
		marker("root-ready")
		wait("root-release")
		os.Exit(17)
	case "descendant":
		marker("descendant-ready")
		wait("descendant-release")
	case "sentinel":
		marker("root-ready")
		wait("root-release")
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func originalTestHost(t *testing.T, mode string) (*StdioHost, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	h, err := NewStdioHost(HostConfig{Command: exe, Args: []string{"-test.run=^TestStdioOriginalHelper$"}, Env: map[string]string{stdioOriginalHelperMode: mode, stdioOriginalHelperRoot: root}})
	if err != nil {
		t.Fatal(err)
	}
	return h, root
}

func originalTestBarrier(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("native helper barrier not received: %s", filepath.Base(path))
}

func TestStdioOriginalMemberBridgeUnavailableStates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, tc := range []struct {
		name string
		host *StdioHost
	}{
		{"nil-host", nil},
		{"unstarted", &StdioHost{}},
		{"legacy-no-original", &StdioHost{started: true}},
		{"no-process-acquisition", &StdioHost{started: true, original: newStdioOriginalLifecycle()}},
		{"partial-process", &StdioHost{started: true, original: &stdioOriginalLifecycle{original: &process.AtCreateJobProcess{}}}},
		{"specialized", &StdioHost{started: true, cfg: HostConfig{LaunchCapability: &LaunchCapabilityConfig{}}, original: &stdioOriginalLifecycle{original: &process.AtCreateJobProcess{}}}},
		{"terminal-started", &StdioHost{started: true, original: &stdioOriginalLifecycle{original: &process.AtCreateJobProcess{}, stopStarted: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if source, err := tc.host.OriginalMemberSource(ctx); source != nil || err == nil {
				t.Fatal("unavailable host issued an original source")
			}
			if observation, err := tc.host.ObserveOriginalMembers(ctx); observation != nil || err == nil {
				t.Fatal("unavailable host issued an original observation")
			}
		})
	}
	h := &StdioHost{started: true, original: &stdioOriginalLifecycle{original: &process.AtCreateJobProcess{}}}
	if source, err := h.OriginalMemberSource(nil); source != nil || err == nil {
		t.Fatal("nil context issued an original source")
	}
	if observation, err := h.ObserveOriginalMembers(nil); observation != nil || err == nil {
		t.Fatal("nil context issued an original observation")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if source, err := h.OriginalMemberSource(canceled); source != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("source bridge lost caller cancellation")
	}
	if observation, err := h.ObserveOriginalMembers(canceled); observation != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("observation bridge lost caller cancellation")
	}
}

func TestStdioOriginalContainmentRefusalBeforeExecution(t *testing.T) {
	h, root := originalTestHost(t, "echo")
	failure := errors.New("injected Job configuration refusal")
	h.originalLaunch = &stdioOriginalLaunchOps{job: func(process.JobOptions) (*process.Job, error) { return nil, failure }}
	if err := h.Start(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("required containment error lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "root-ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("child executed before containment")
	}
	r, err := h.StopObserved(context.Background())
	if !r.NoNativeAttempt || !r.Complete() || err != nil || !errors.Is(r.PrimaryError, failure) {
		t.Fatalf("actual refusal/close/repeat outcome missing: %+v %v", r, err)
	}
}

func TestStdioOriginalFastWrapperDescendantAndForeignSentinel(t *testing.T) {
	h, root := originalTestHost(t, "wrapper")
	sentinel, sentinelRoot := originalTestHost(t, "sentinel")
	if err := sentinel.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sentinel.Stop(); err != nil {
			t.Error(err)
		}
	}()
	originalTestBarrier(t, filepath.Join(sentinelRoot, "root-ready"))
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Stop(); err != nil {
			t.Error(err)
		}
	}()
	originalTestBarrier(t, filepath.Join(root, "root-ready"))
	identity := h.original.identity
	if err := os.WriteFile(filepath.Join(root, "root-release"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.ChildExited():
	case <-time.After(6 * time.Second):
		t.Fatal("native descendant/pipe settlement did not publish exit")
	}
	r, err := h.StopObserved(context.Background())
	if err != nil || !r.Complete() || r.OriginalIdentity != identity || !r.RootWait.Known || r.RootWait.ExitCode != 17 {
		t.Fatalf("same original Job/exit17 receiving failed: %+v %v", r, err)
	}
	liveContext, liveCancel := context.WithTimeout(context.Background(), time.Second)
	defer liveCancel()
	live, err := sentinel.original.original.OriginalLive(liveContext)
	if err != nil || !live {
		t.Fatalf("foreign sibling affected: live=%v err=%v", live, err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("cached clean result changed: %v", err)
	}
}

type originalReportedCloser struct {
	io.Closer
	fault error
	calls int
}

func (c *originalReportedCloser) Close() error {
	c.calls++
	return errors.Join(c.Closer.Close(), c.fault)
}

func TestStdioOriginalConcurrentStopRetainsCloseFailure(t *testing.T) {
	h, root := originalTestHost(t, "echo")
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	originalTestBarrier(t, filepath.Join(root, "root-ready"))
	file, err := os.CreateTemp(t.TempDir(), "log-close-")
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected reported log close failure")
	closer := &originalReportedCloser{Closer: file, fault: failure}
	h.logFile = closer
	var wg sync.WaitGroup
	errorsReceived := make(chan error, 2)
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func() { defer wg.Done(); errorsReceived <- h.Stop() }()
	}
	wg.Wait()
	close(errorsReceived)
	for err := range errorsReceived {
		if !errors.Is(err, failure) {
			t.Errorf("first close cause disappeared: %v", err)
		}
	}
	r, err := h.StopObserved(context.Background())
	if !errors.Is(err, failure) || r.Complete() || !r.Pending || closer.calls != 1 || !r.NativeSettlement.Complete() {
		t.Fatalf("repeated close lied or retried: %+v %v calls=%d", r, err, closer.calls)
	}
}

func TestStdioOriginalFinalReplyPrecedesExit(t *testing.T) {
	h, root := originalTestHost(t, "final")
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Stop(); err != nil {
			t.Error(err)
		}
	}()
	originalTestBarrier(t, filepath.Join(root, "root-ready"))
	response := make(chan json.RawMessage, 1)
	h.pendingMu.Lock()
	h.pending[1] = response
	h.pendingMu.Unlock()
	if err := h.writeStdin([]byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.ChildExited():
	case <-time.After(5 * time.Second):
		t.Fatal("final reply child exit missing")
	}
	select {
	case reply := <-response:
		if !json.Valid(reply) {
			t.Fatal("invalid final reply")
		}
	default:
		t.Fatal("child exit published before final reply receipt")
	}
}

func TestStdioOriginalDelayedReaderRetainsPendingAttempt(t *testing.T) {
	h, root := originalTestHost(t, "echo")
	entered, release := make(chan struct{}), make(chan struct{})
	h.originalLaunch = &stdioOriginalLaunchOps{beforeStdout: func() { close(entered); <-release }}
	if err := h.Start(context.Background()); err != nil {
		close(release)
		t.Fatal(err)
	}
	<-entered
	originalTestBarrier(t, filepath.Join(root, "root-ready"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	r, err := h.StopObserved(ctx)
	cancel()
	cleanup, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cleanupCancel()
	// Receive the first terminal attempt while the deliberate barrier stays
	// held, then release it. Scheduler timing cannot turn this window clean.
	if joinErr := originalJoin(cleanup, h.original.stopDone); joinErr != nil {
		t.Error(joinErr)
	}
	close(release)
	for _, done := range []<-chan struct{}{h.original.stopDone, h.original.rootDone, h.original.stdoutDone, h.original.stderrDone, h.original.watcherDone, h.original.lifetimeDone} {
		if joinErr := originalJoin(cleanup, done); joinErr != nil {
			t.Errorf("test-owned delayed worker cleanup: %v", joinErr)
		}
	}
	// If the first bounded closer could not receive the native waiter, this
	// explicit test cleanup uses the same retained original capability.
	_, releaseErr := h.original.original.ReleaseOriginal()
	if releaseErr != nil && !errors.Is(releaseErr, context.DeadlineExceeded) {
		t.Errorf("test original release: %v", releaseErr)
	}
	if err == nil || !r.Pending {
		t.Fatalf("unreceived reader became clean: %+v %v", r, err)
	}
	first := h.original.terminal
	again, againErr := h.StopObserved(context.Background())
	if againErr == nil || again.Complete() || !again.Pending || again.Deadline != first.Deadline || again.ReadersJoined {
		t.Fatalf("repeat renewed or erased pending attempt: first=%+v again=%+v err=%v", first, again, againErr)
	}
}

func TestStdioOriginalNativeObservationFailureIsCached(t *testing.T) {
	h, root := originalTestHost(t, "echo")
	failure := errors.New("injected reported native observation failure")
	calls := 0
	h.originalLaunch = &stdioOriginalLaunchOps{observeOriginal: func(owner *process.AtCreateJobProcess, ctx context.Context, natural bool) (process.ContainedNativeSnapshot, error) {
		calls++
		actual, err := owner.ObserveOriginal(ctx, natural)
		return actual, errors.Join(err, failure)
	}}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	originalTestBarrier(t, filepath.Join(root, "root-ready"))
	first, err := h.StopObserved(context.Background())
	if !errors.Is(err, failure) || first.Complete() || !first.NativeSettlement.Complete() {
		t.Fatalf("reported observation failure hid actual native close: %+v %v", first, err)
	}
	again, againErr := h.StopObserved(context.Background())
	if calls != 1 || !errors.Is(againErr, failure) || again.Deadline != first.Deadline || again.OriginalIdentity != first.OriginalIdentity {
		t.Fatalf("native observation repeated or lost outcome: calls=%d %v", calls, againErr)
	}
}

func TestStdioOriginalRepeatedStartPreservesRunningOwner(t *testing.T) {
	h, root := originalTestHost(t, "echo")
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Stop(); err != nil {
			t.Error(err)
		}
	}()
	originalTestBarrier(t, filepath.Join(root, "root-ready"))
	owner := h.original
	if err := h.Start(context.Background()); err == nil {
		t.Fatal("second Start succeeded")
	}
	if h.original != owner {
		t.Fatal("second Start replaced original custody")
	}
	response := make(chan json.RawMessage, 1)
	h.pendingMu.Lock()
	h.pending[1] = response
	h.pendingMu.Unlock()
	if err := h.writeStdin([]byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-response:
	case <-time.After(3 * time.Second):
		t.Fatal("second Start retired the live service")
	}
}

func TestStdioOriginalCanceledAcquisitionAndConcurrentStop(t *testing.T) {
	h, root := originalTestHost(t, "echo")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acquired, release := make(chan struct{}), make(chan struct{})
	startCalls := 0
	h.originalLaunch = &stdioOriginalLaunchOps{
		job: func(options process.JobOptions) (*process.Job, error) {
			job, err := process.NewJobObserved(options)
			close(acquired)
			<-release
			return job, err
		},
		start: func(job *process.Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (*process.AtCreateJobProcess, error) {
			startCalls++
			return process.StartWithJobFilesOriginal(job, cmd, stdin, stdout, stderr)
		},
	}
	startResult, stopResult := make(chan error, 1), make(chan error, 1)
	go func() { startResult <- h.Start(ctx) }()
	<-acquired
	cancel()
	go func() { stopResult <- h.Stop() }()
	close(release)
	select {
	case err := <-startResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled acquisition cause lost: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Start acquisition did not return")
	}
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("already-settled no-child Stop failed: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("concurrent Stop did not receive same acquisition")
	}
	if startCalls != 0 {
		t.Fatal("canceled acquisition entered native execution")
	}
	if _, err := os.Stat(filepath.Join(root, "root-ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled acquisition executed helper")
	}
	r, err := h.StopObserved(context.Background())
	if err != nil || !r.NoNativeAttempt || !r.Complete() || !errors.Is(r.PrimaryError, context.Canceled) {
		t.Fatalf("same partial owner/close/cancel outcome not retained: %+v %v", r, err)
	}
}
