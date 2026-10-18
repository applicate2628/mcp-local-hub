package sonnet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/process"
)

const strictDriverHelperExitEnv = "MCPHUB_SONNET_STRICT_DRIVER_HELPER_EXIT"

func TestMain(m *testing.M) {
	if raw := os.Getenv(strictDriverHelperExitEnv); raw != "" {
		code, err := strconv.Atoi(raw)
		if err != nil || code < 0 || code > 125 {
			os.Exit(125)
		}
		_, _ = os.Stdout.Write([]byte("sonnet-helper-stdout"))
		_, _ = os.Stderr.Write([]byte("sonnet-helper-stderr"))
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestStrictDriver_OrdinaryExitThreeIsCompletedNonzero(t *testing.T) {
	t.Setenv(strictDriverHelperExitEnv, "3")
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := writeProject(t)
	project := filepath.Join(projectRoot, "sample.son")
	driverResult, driverErr := (strictDriver{}).Run(context.Background(), helper, project)
	if driverErr != nil || driverResult.ExitCode != 3 {
		t.Fatalf("driver result=(%+v, %v), want clean observed exit 3", driverResult, driverErr)
	}
	if string(driverResult.Stdout) != "sonnet-helper-stdout" || string(driverResult.Stderr) != "sonnet-helper-stderr" {
		t.Fatalf("driver streams=(%q, %q)", driverResult.Stdout, driverResult.Stderr)
	}

	s := NewServer(Config{
		RunRoot: t.TempDir(),
		Locator: func() (Product, error) {
			return Product{Available: true, Path: helper}, nil
		},
		Driver: strictDriver{},
	})
	defer s.Close(context.Background())
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, s, started.RunID)
	status, err := s.Status(started.RunID)
	if err != nil || status.State != StateCompletedNonzero || status.FailureID != "SONNET_EM_EXIT_NONZERO" || status.ExitCode == nil || *status.ExitCode != 3 || status.Reportable {
		t.Fatalf("status=(%+v, %v), want nonreportable completed_nonzero exit 3", status, err)
	}
	wire, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatal(err)
	}
	if body["exit_code"] != float64(3) || body["state"] != string(StateCompletedNonzero) || body["failure_id"] != "SONNET_EM_EXIT_NONZERO" {
		t.Fatalf("status wire=%s", wire)
	}

	directResult, err := s.Result(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if directResult.RunID != started.RunID || directResult.State != StateCompletedNonzero || directResult.FailureID != "SONNET_EM_EXIT_NONZERO" || directResult.ExitCode == nil || *directResult.ExitCode != 3 || directResult.Reportable {
		t.Fatalf("direct result identity=%+v, want nonreportable completed_nonzero exit 3", directResult)
	}
	if directResult.Stdout != "sonnet-helper-stdout" || directResult.Stderr != "sonnet-helper-stderr" || directResult.StdoutBytes != 20 || directResult.StderrBytes != 20 {
		t.Fatalf("direct result streams=(%q/%d, %q/%d)", directResult.Stdout, directResult.StdoutBytes, directResult.Stderr, directResult.StderrBytes)
	}

	client := wireClient(t, s)
	mcpResult, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_result", Arguments: map[string]any{"run_id": started.RunID}})
	if err != nil || mcpResult.IsError {
		t.Fatalf("MCP result=(%+v, %v)", mcpResult, err)
	}
	mcpBody := resultJSON(t, mcpResult)
	if mcpBody["run_id"] != started.RunID || mcpBody["state"] != string(StateCompletedNonzero) || mcpBody["failure_id"] != "SONNET_EM_EXIT_NONZERO" || mcpBody["exit_code"] != float64(3) || mcpBody["reportable"] != false {
		t.Fatalf("MCP result identity=%v, want nonreportable completed_nonzero exit 3", mcpBody)
	}
	if mcpBody["stdout"] != "sonnet-helper-stdout" || mcpBody["stderr"] != "sonnet-helper-stderr" || mcpBody["stdout_bytes"] != float64(20) || mcpBody["stderr_bytes"] != float64(20) {
		t.Fatalf("MCP result streams=%v", mcpBody)
	}
}

func TestStrictDriver_MissingExecutableIsLaunchFailure(t *testing.T) {
	projectRoot := writeProject(t)
	missing := filepath.Join(t.TempDir(), "missing-sonnet-helper.exe")
	result, err := (strictDriver{}).Run(context.Background(), missing, filepath.Join(projectRoot, "sample.son"))
	if result.ExitCode != 0 {
		t.Fatalf("missing executable exit code=%d, want absent/default zero", result.ExitCode)
	}
	failure := requireStrictDriverFailure(t, err, "SONNET_LAUNCH_FAILED", 0, false)
	var strictErr *process.StrictRunError
	if !errors.As(failure, &strictErr) || strictErr.Kind != process.StrictRunExecutionFailed {
		t.Fatalf("launch error=%v, want original strict execution tree", failure)
	}
	var startErr *process.StartWithJobError
	if !errors.As(failure, &startErr) || startErr.Phase != process.StartWithJobLaunch {
		t.Fatalf("launch error=%v, want StartWithJobLaunch", failure)
	}
	if !errors.Is(failure, os.ErrNotExist) {
		t.Fatalf("launch error=%v, want original not-found cause", failure)
	}
}

func TestStrictDriver_ContainmentFailureIsDistinct(t *testing.T) {
	sentinel := errors.New("injected containment failure")
	runErr := &process.StrictRunError{Kind: process.StrictRunContainmentFailed, Cause: sentinel}
	_, err := classifyStrictDriverResult(exec.Command(os.Args[0]), runErr)
	failure := requireStrictDriverFailure(t, err, "SONNET_PROCESS_CONTAINMENT_FAILED", 0, false)
	if !errors.Is(failure, sentinel) {
		t.Fatalf("containment error=%v, want original cause", failure)
	}
}

func TestStrictDriver_ExitThreeJoinedCleanupStaysFailed(t *testing.T) {
	cmd, exitErr := runStrictDriverHelper(t, 3)
	cleanupErr := errors.New("injected stream cleanup failure")
	runErr := &process.StrictRunError{Kind: process.StrictRunExecutionFailed, Cause: errors.Join(exitErr, cleanupErr)}
	_, err := classifyStrictDriverResult(cmd, runErr)
	failure := requireStrictDriverFailure(t, err, "SONNET_DRIVER_CLEANUP_FAILED", 3, true)
	var preservedExit *exec.ExitError
	if !errors.As(failure, &preservedExit) || !errors.Is(failure, cleanupErr) {
		t.Fatalf("mixed failure=%v, want exit and cleanup leaves", failure)
	}

	status := settleStrictDriverOutcome(t, DriverResult{}, failure)
	if status.State != StateFailed || status.FailureID != "SONNET_DRIVER_CLEANUP_FAILED" || status.ExitCode == nil || *status.ExitCode != 3 || status.Reportable || status.Error != "" {
		t.Fatalf("mixed status=%+v", status)
	}
}

func TestStrictDriver_ZeroExitJoinedCleanupStaysFailed(t *testing.T) {
	cmd, runResult := runStrictDriverHelper(t, 0)
	if runResult != nil {
		t.Fatalf("zero helper error=%v", runResult)
	}
	cleanupErr := errors.New("injected job close failure")
	runErr := &process.StrictRunError{Kind: process.StrictRunExecutionFailed, Cause: cleanupErr}
	_, err := classifyStrictDriverResult(cmd, runErr)
	failure := requireStrictDriverFailure(t, err, "SONNET_DRIVER_CLEANUP_FAILED", 0, true)
	if !errors.Is(failure, cleanupErr) {
		t.Fatalf("zero-exit cleanup failure=%v, want original cause", failure)
	}
}

func TestStrictDriver_PipeOrPrewaitFailureHasNoExit(t *testing.T) {
	sentinel := errors.New("injected stdout pipe failure")
	runErr := &process.StrictRunError{Kind: process.StrictRunExecutionFailed, Cause: sentinel}
	_, err := classifyStrictDriverResult(exec.Command(os.Args[0]), runErr)
	failure := requireStrictDriverFailure(t, err, "SONNET_DRIVER_IO_FAILED", 0, false)
	if !errors.Is(failure, sentinel) {
		t.Fatalf("pipe failure=%v, want original cause", failure)
	}
}

func TestStrictDriver_InvalidOrUnknownTreeFailsClosed(t *testing.T) {
	t.Run("invalid invocation", func(t *testing.T) {
		sentinel := errors.New("injected invalid invocation")
		runErr := &process.StrictRunError{Kind: process.StrictRunInvalidInvocation, Cause: sentinel}
		_, err := classifyStrictDriverResult(exec.Command(os.Args[0]), runErr)
		failure := requireStrictDriverFailure(t, err, "SONNET_DRIVER_INVALID_INVOCATION", 0, false)
		if !errors.Is(failure, sentinel) {
			t.Fatalf("invalid failure=%v, want original cause", failure)
		}
	})

	t.Run("unknown strict kind", func(t *testing.T) {
		runErr := &process.StrictRunError{Kind: process.StrictRunFailureKind("future-kind"), Cause: errors.New("future")}
		_, err := classifyStrictDriverResult(exec.Command(os.Args[0]), runErr)
		requireStrictDriverFailure(t, err, "SONNET_DRIVER_RESULT_INVALID", 0, false)
	})

	t.Run("untyped error", func(t *testing.T) {
		_, err := classifyStrictDriverResult(exec.Command(os.Args[0]), errors.New("untyped driver error"))
		requireStrictDriverFailure(t, err, "SONNET_DRIVER_RESULT_INVALID", 0, false)
	})

	t.Run("mismatched process states", func(t *testing.T) {
		cmdThree, _ := runStrictDriverHelper(t, 3)
		_, exitFour := runStrictDriverHelper(t, 4)
		runErr := &process.StrictRunError{Kind: process.StrictRunExecutionFailed, Cause: exitFour}
		_, err := classifyStrictDriverResult(cmdThree, runErr)
		requireStrictDriverFailure(t, err, "SONNET_DRIVER_RESULT_INVALID", 0, false)
	})

	t.Run("nil error without process state", func(t *testing.T) {
		_, err := classifyStrictDriverResult(exec.Command(os.Args[0]), nil)
		requireStrictDriverFailure(t, err, "SONNET_DRIVER_RESULT_INVALID", 0, false)
	})

	t.Run("exit error with zero code", func(t *testing.T) {
		cmd, runResult := runStrictDriverHelper(t, 0)
		if runResult != nil {
			t.Fatal(runResult)
		}
		runErr := &process.StrictRunError{Kind: process.StrictRunExecutionFailed, Cause: &exec.ExitError{ProcessState: cmd.ProcessState}}
		_, err := classifyStrictDriverResult(cmd, runErr)
		requireStrictDriverFailure(t, err, "SONNET_DRIVER_RESULT_INVALID", 0, true)
	})
}

func TestStrictDriver_TimeoutAndCancelPolarityUnchanged(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		driver := &fakeDriver{done: make(chan struct{})}
		s := NewServer(testConfig(t.TempDir(), driver))
		defer s.Close(context.Background())
		started, err := s.Start(context.Background(), StartRequest{ProjectRoot: writeProject(t), ProjectFile: "sample.son", TimeoutSeconds: 1})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, 3*time.Second, func() bool {
			status, err := s.Status(started.RunID)
			return err == nil && terminal(status.State)
		})
		status, err := s.Status(started.RunID)
		if err != nil || status.State != StateTimedOut || status.FailureID != "SONNET_RUN_TIMED_OUT" || status.ExitCode != nil {
			t.Fatalf("timeout status=(%+v, %v)", status, err)
		}
	})

	t.Run("force cancel", func(t *testing.T) {
		driver := &fakeDriver{start: make(chan struct{}), done: make(chan struct{})}
		s := NewServer(testConfig(t.TempDir(), driver))
		defer s.Close(context.Background())
		started, err := s.Start(context.Background(), StartRequest{ProjectRoot: writeProject(t), ProjectFile: "sample.son", TimeoutSeconds: 10})
		if err != nil {
			t.Fatal(err)
		}
		<-driver.start
		if _, err := s.Cancel(context.Background(), started.RunID, "driver-classification-cancel", true); err != nil {
			t.Fatal(err)
		}
		status, err := s.Status(started.RunID)
		if err != nil || status.State != StateForceCancelled || status.FailureID != "SONNET_FORCE_CANCELLED" || status.ExitCode != nil {
			t.Fatalf("cancel status=(%+v, %v)", status, err)
		}
	})
}

type fixedStrictDriverOutcome struct {
	result DriverResult
	err    error
}

func (d fixedStrictDriverOutcome) Run(context.Context, string, string) (DriverResult, error) {
	return d.result, d.err
}

func settleStrictDriverOutcome(t *testing.T, result DriverResult, driverErr error) StatusResponse {
	t.Helper()
	s := NewServer(testConfig(t.TempDir(), fixedStrictDriverOutcome{result: result, err: driverErr}))
	defer s.Close(context.Background())
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: writeProject(t), ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, s, started.RunID)
	status, err := s.Status(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func runStrictDriverHelper(t *testing.T, exitCode int) (*exec.Cmd, error) {
	t.Helper()
	t.Setenv(strictDriverHelperExitEnv, strconv.Itoa(exitCode))
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := writeProject(t)
	cmd := exec.Command(helper, filepath.Join(projectRoot, "sample.son"))
	cmd.Dir = projectRoot
	return cmd, cmd.Run()
}

func requireStrictDriverFailure(t *testing.T, err error, failureID string, exitCode int, exitCodeKnown bool) *strictDriverFailure {
	t.Helper()
	var failure *strictDriverFailure
	if !errors.As(err, &failure) || failure.FailureID != failureID {
		t.Fatalf("driver error=%v, want %s", err, failureID)
	}
	if exitCodeKnown {
		if failure.ExitCode == nil || *failure.ExitCode != exitCode {
			t.Fatalf("driver failure exit=%v, want %d", failure.ExitCode, exitCode)
		}
	} else if failure.ExitCode != nil {
		t.Fatalf("driver failure exit=%d, want absent", *failure.ExitCode)
	}
	return failure
}
