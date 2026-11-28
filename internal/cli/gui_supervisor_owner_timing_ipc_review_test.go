package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// The canceled context returns before filesystem, identity and IPC access.
func TestGUISupervisorControlTimingIPCReview(t *testing.T) {
	var sink bytes.Buffer
	prior := supervisorMonitorStderr
	supervisorMonitorStderr = &sink
	t.Cleanup(func() { supervisorMonitorStderr = prior })
	t.Setenv("MCPHUB_GUI_CONTROL_TIMING", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expected := SupervisorControlExpectation{}
	original := api.SupervisorLockOwner{}
	peer := api.SupervisorProcessIdentityV1{}
	if got, err := observeGUISupervisorControl(ctx, expected, original, peer); got != original || !errors.Is(err, context.Canceled) {
		t.Fatalf("default observation changed result: owner=%+v err=%v", got, err)
	}
	if sink.Len() != 0 {
		t.Fatal("disabled observer produced output")
	}

	var timing guiSupervisorControlTiming
	timing.report(false)
	if sink.Len() != 0 {
		t.Fatal("empty timing produced output")
	}
	const attempts = 32
	for i := 0; i < attempts; i++ {
		if got, err := timing.observe(ctx, expected, original, peer); got != original || !errors.Is(err, context.Canceled) {
			t.Fatalf("timed observation changed result: owner=%+v err=%v", got, err)
		}
	}
	if sink.Len() != 0 || timing.attempts != attempts || timing.controlCalls != attempts ||
		timing.reads != 0 || timing.nativeChecks != 0 || timing.ipcCalls != 0 || timing.ipc != 0 {
		t.Fatal("timed cancellation logged a poll or performed unnecessary observation")
	}
	timing.report(false)
	line := sink.String()
	if strings.Count(line, "\n") != 1 || len(line) > 320 ||
		!strings.HasPrefix(line, "supervisor GUI admission timing: success=false attempts=32 control_calls=32 control_elapsed=") ||
		!strings.HasSuffix(line, " owner_reads=0 owner_read_elapsed=0s native_checks=0 native_elapsed=0s ipc_calls=0 ipc_elapsed=0s\n") {
		t.Fatalf("unexpected timing summary: %q", line)
	}
}

// Exercise the existing native veto seam, never an injected authenticator.
// The fake owner matches, but the veto must precede actual peer/IPC operations.
func TestGUISupervisorControlTimingNativeVeto(t *testing.T) {
	stateDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	product := filepath.Join(stateDir, "private-fixture")
	if err := os.WriteFile(product, nil, 0600); err != nil {
		t.Fatal(err)
	}
	expected := SupervisorControlExpectation{
		StateDir:      stateDir,
		ProductPath:   product,
		ProductSHA256: strings.Repeat("a", 64),
	}
	owner := api.SupervisorLockOwner{PID: 1, StartedAt: "synthetic"}
	peer := api.SupervisorProcessIdentityV1{}
	veto := errors.New("fixture native veto")
	reads, native, ipc := 0, 0, 0
	read := func(path string) (api.SupervisorLockOwner, error) {
		reads++
		if path != filepath.Join(stateDir, "supervisor.lock") {
			t.Fatalf("unexpected owner read: %q", path)
		}
		return owner, nil
	}
	validate := func(actual api.SupervisorLockOwner) error {
		native++
		if actual != owner {
			t.Fatal("native validation received the wrong owner")
		}
		return veto
	}
	record := func(time.Duration) { ipc++ }
	ctx := context.Background()
	got, err := observeCreatedSupervisorControl(ctx, expected, owner, peer, validate, read)
	if got != (api.SupervisorLockOwner{}) || err != veto || reads != 1 || native != 1 || ipc != 0 {
		t.Fatalf("default native veto changed: err=%v reads=%d native=%d ipc=%d", err, reads, native, ipc)
	}
	got, err = observeCreatedSupervisorControlTimed(ctx, expected, owner, peer, validate, read, record)
	if got != (api.SupervisorLockOwner{}) || err != veto || reads != 2 || native != 2 || ipc != 0 {
		t.Fatalf("timed native veto changed: err=%v reads=%d native=%d ipc=%d", err, reads, native, ipc)
	}
}
