package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

func TestBuildWrapperArgsExactDefaultHighAndExplicitMax(t *testing.T) {
	root := t.TempDir()
	base := RunRequest{
		Topic: "topic", PromptFile: filepath.Join(root, "prompt.md"), Cwd: root,
		TaskClass: "review", Role: "external-reviewer", Effort: "high", TimeoutSeconds: 30,
		CapabilityChoice: CapabilityChoice{Kind: "none"},
	}
	receipt := filepath.Join(root, "receipt.jsonl")
	noneCaps := filepath.Join(root, "none-capabilities.json")
	wrapper := filepath.Join(root, "invoke-kimi-prompt.py")
	want := []string{wrapper, "topic", "--prompt-file", base.PromptFile, "--terminal-receipt", receipt, "--task-class", "review", "--role", "external-reviewer", "--timeout-secs", "30", "--kimi-effort", "high", "--kimi-capabilities-file", noneCaps}
	if got := buildWrapperArgs(wrapper, receipt, noneCaps, base); !reflect.DeepEqual(got, want) {
		t.Fatalf("default argv\ngot  %#v\nwant %#v", got, want)
	}
	base.Effort = "max"
	base.CapabilityChoice = CapabilityChoice{Kind: "file", File: filepath.Join(root, "caps.json")}
	want[13] = "max"
	want[len(want)-1] = base.CapabilityChoice.File
	if got := buildWrapperArgs(wrapper, receipt, base.CapabilityChoice.File, base); !reflect.DeepEqual(got, want) {
		t.Fatalf("max argv\ngot  %#v\nwant %#v", got, want)
	}
}

func TestResolveWrapperUsesOnlyGlobalPairAndRefusesIrregularGlobal(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	projectWrapper := filepath.Join(project, ".agents", "skills", "lead", "scripts", "invoke-kimi-prompt.py")
	globalWrapper := filepath.Join(home, ".agents", "skills", "lead", "scripts", "invoke-kimi-prompt.py")
	for _, wrapper := range []string{projectWrapper, globalWrapper} {
		if err := os.MkdirAll(filepath.Dir(wrapper), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wrapper, []byte("# fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(wrapper), "provider_prompt.py"), []byte("# owner"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveWrapper(home)
	if err != nil || !sameTestPath(got, globalWrapper) {
		t.Fatalf("global owner = %q, %v", got, err)
	}
	if err := os.Remove(globalWrapper); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(globalWrapper, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWrapper(home); !errors.Is(err, errWrapperUnavailable) {
		t.Fatalf("irregular wrapper error = %v", err)
	}
}

func TestRunWithDependenciesReturnsReceiptAndExactArgv(t *testing.T) {
	root := t.TempDir()
	prompt := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("secret prompt bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, ".agents", "skills", "lead", "scripts", "invoke-kimi-prompt.py")
	python := filepath.Join(root, "python.exe")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{wrapper, filepath.Join(filepath.Dir(wrapper), "provider_prompt.py"), python} {
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var gotName string
	var gotArgs []string
	var gotNoneCapabilities []byte
	deps := runnerDependencies{
		homeDir:  func() (string, error) { return root, nil },
		lookPath: func(string) (string, error) { return python, nil },
		command: func(name string, args ...string) *exec.Cmd {
			gotName, gotArgs = name, append([]string(nil), args...)
			return exec.Command(name, args...)
		},
		runContained: func(_ context.Context, _ *exec.Cmd, _ process.ContainedStreamOptions, consume func(io.Reader) error) error {
			if err := consume(strings.NewReader(strings.Repeat("x", 2<<20))); err != nil {
				return err
			}
			receipt := argumentValue(gotArgs, "--terminal-receipt")
			var readErr error
			gotNoneCapabilities, readErr = os.ReadFile(argumentValue(gotArgs, "--kimi-capabilities-file"))
			if readErr != nil {
				return readErr
			}
			return os.WriteFile(receipt, []byte(ownerReceiptRecord(t, "max", "sensitive final result")), 0o600)
		},
		mkdirTemp: os.MkdirTemp,
		removeAll: os.RemoveAll,
	}
	req := RunRequest{Topic: "topic", PromptFile: prompt, Cwd: root, TaskClass: "review", Role: "external-reviewer", Effort: "max", TimeoutSeconds: 30, CapabilityChoice: CapabilityChoice{Kind: "none"}}
	got, err := runWithDependencies(context.Background(), req, deps)
	if err != nil || got.Receipt.ResultText != "sensitive final result" || got.Receipt.IsError() {
		t.Fatalf("run = %+v, %v", got, err)
	}
	if !sameTestPath(gotName, python) || argumentValue(gotArgs, "--kimi-effort") != "max" || strings.Contains(strings.Join(gotArgs, "\x00"), "secret prompt bytes") {
		t.Fatalf("argv/name mismatch: %q %#v", gotName, gotArgs)
	}
	if string(gotNoneCapabilities) != string(noneCapabilityJSON) {
		t.Fatalf("none capabilities = %s", gotNoneCapabilities)
	}
}

func TestRunWithDependenciesMapsTimeoutCancelAndCleanupFailure(t *testing.T) {
	root := t.TempDir()
	receiptBase := t.TempDir()
	prompt := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, ".agents", "skills", "lead", "scripts", "invoke-kimi-prompt.py")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{wrapper, filepath.Join(filepath.Dir(wrapper), "provider_prompt.py"), filepath.Join(root, "python.exe")} {
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	base := runnerDependencies{
		homeDir:   func() (string, error) { return root, nil },
		lookPath:  func(string) (string, error) { return filepath.Join(root, "python.exe"), nil },
		command:   func(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) },
		mkdirTemp: func(_, pattern string) (string, error) { return os.MkdirTemp(receiptBase, pattern) },
		removeAll: os.RemoveAll,
	}
	req := RunRequest{Topic: "topic", PromptFile: prompt, Cwd: root, TaskClass: "review", Role: "external-reviewer", Effort: "high", TimeoutSeconds: 1, CapabilityChoice: CapabilityChoice{Kind: "none"}}

	t.Run("timeout", func(t *testing.T) {
		deps := base
		deps.runContained = func(ctx context.Context, _ *exec.Cmd, _ process.ContainedStreamOptions, _ func(io.Reader) error) error {
			<-ctx.Done()
			return ctx.Err()
		}
		_, err := runWithDependencies(context.Background(), req, deps)
		if failureID(err) != "E_KIMI_TIMEOUT" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		deps := base
		deps.runContained = func(ctx context.Context, _ *exec.Cmd, _ process.ContainedStreamOptions, _ func(io.Reader) error) error {
			<-ctx.Done()
			return ctx.Err()
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := runWithDependencies(ctx, req, deps)
		if failureID(err) != "E_KIMI_CANCELLED" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		deps := base
		primaryErr := errors.New("synthetic run failure")
		cleanupErr := errors.New("synthetic cleanup failure")
		var args []string
		deps.command = func(name string, values ...string) *exec.Cmd {
			args = append([]string(nil), values...)
			return exec.Command(name, values...)
		}
		deps.runContained = func(_ context.Context, _ *exec.Cmd, _ process.ContainedStreamOptions, _ func(io.Reader) error) error {
			if err := os.WriteFile(argumentValue(args, "--terminal-receipt"), []byte(ownerReceiptRecord(t, "high", "acquired result")), 0o600); err != nil {
				return err
			}
			return primaryErr
		}
		deps.removeAll = func(string) error { return cleanupErr }
		output, err := runWithDependencies(context.Background(), req, deps)
		if failureID(err) != "E_KIMI_CLEANUP_FAILED" {
			t.Fatalf("error = %v", err)
		}
		if output.Receipt.Payload == "" || !strings.Contains(output.Receipt.Payload, `"resultText":"acquired result"`) {
			t.Fatalf("acquired receipt was not retained (payload_present=%t)", output.Receipt.Payload != "")
		}
		if !errors.Is(err, primaryErr) || !errors.Is(err, cleanupErr) {
			t.Fatalf("joined causes = %v", err)
		}
		var failure *Failure
		if !errors.As(err, &failure) || failure == nil {
			t.Fatalf("cleanup failure metadata missing: %#v", err)
		}
		if string(failure.JSON()) != `{"failure_id":"E_KIMI_CLEANUP_FAILED","primary_failure_id":"E_KIMI_RUN_FAILED","cleanup_failures":["receipt_directory"]}` {
			t.Fatalf("cleanup failure JSON = %s", failure.JSON())
		}
	})
}

func TestRunWithDependenciesHoldsReceiptIntakeAfterContainedCleanupFailure(t *testing.T) {
	root := t.TempDir()
	prompt := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, ".agents", "skills", "lead", "scripts", "invoke-kimi-prompt.py")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{wrapper, filepath.Join(filepath.Dir(wrapper), "provider_prompt.py"), filepath.Join(root, "python.exe")} {
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	receiptBase := t.TempDir()
	var args []string
	base := runnerDependencies{
		homeDir:  func() (string, error) { return root, nil },
		lookPath: func(string) (string, error) { return filepath.Join(root, "python.exe"), nil },
		command: func(name string, values ...string) *exec.Cmd {
			args = append([]string(nil), values...)
			return exec.Command(name, values...)
		},
		mkdirTemp: func(_, pattern string) (string, error) { return os.MkdirTemp(receiptBase, pattern) },
		removeAll: os.RemoveAll,
	}
	req := RunRequest{Topic: "topic", PromptFile: prompt, Cwd: root, TaskClass: "review", Role: "external-reviewer", Effort: "high", TimeoutSeconds: 1, CapabilityChoice: CapabilityChoice{Kind: "none"}}

	t.Run("receipt only with directory cleanup failure", func(t *testing.T) {
		cleanupErr := errors.New("synthetic directory cleanup failure")
		runCalls := 0
		deps := base
		deps.runContained = func(_ context.Context, _ *exec.Cmd, _ process.ContainedStreamOptions, _ func(io.Reader) error) error {
			runCalls++
			return os.WriteFile(argumentValue(args, "--terminal-receipt"), []byte(ownerReceiptRecord(t, "high", "acquired result")), 0o600)
		}
		deps.removeAll = func(string) error { return cleanupErr }
		output, err := runWithDependencies(context.Background(), req, deps)
		if output.Receipt.Payload == "" || !strings.Contains(output.Receipt.Payload, `"resultText":"acquired result"`) {
			t.Fatalf("acquired receipt was not retained (payload_present=%t)", output.Receipt.Payload != "")
		}
		if failureID(err) != "E_KIMI_CLEANUP_FAILED" || !errors.Is(err, cleanupErr) {
			t.Fatalf("cleanup failure = %v", err)
		}
		if runCalls != 1 {
			t.Fatalf("contained run calls = %d", runCalls)
		}
		var failure *Failure
		if !errors.As(err, &failure) || failure == nil {
			t.Fatalf("cleanup failure metadata missing: %#v", err)
		}
		if string(failure.JSON()) != `{"failure_id":"E_KIMI_CLEANUP_FAILED","cleanup_failures":["receipt_directory"]}` {
			t.Fatalf("cleanup failure JSON = %s", failure.JSON())
		}
	})

	t.Run("primary plus contained cleanup and directory cleanup", func(t *testing.T) {
		primaryErr := errors.New("synthetic contained primary failure")
		containedCleanupErr := errors.New("synthetic contained cleanup failure")
		directoryCleanupErr := errors.New("synthetic directory cleanup failure")
		runCalls := 0
		deps := base
		deps.runContained = func(_ context.Context, _ *exec.Cmd, _ process.ContainedStreamOptions, _ func(io.Reader) error) error {
			runCalls++
			if err := os.WriteFile(argumentValue(args, "--terminal-receipt"), []byte(ownerReceiptRecord(t, "high", "must remain unacquired")), 0o600); err != nil {
				return err
			}
			code := 17
			return &process.ContainedRunError{Stage: process.ContainedStageWait, Cause: primaryErr, ExitCode: &code, CleanupStage: process.ContainedStageCleanup, CleanupCause: containedCleanupErr}
		}
		deps.removeAll = func(string) error { return directoryCleanupErr }
		output, err := runWithDependencies(context.Background(), req, deps)
		if output.Receipt.Payload != "" {
			t.Fatal("unsettled receipt was acquired")
		}
		if failureID(err) != "E_KIMI_CLEANUP_FAILED" || !errors.Is(err, primaryErr) || !errors.Is(err, containedCleanupErr) || !errors.Is(err, directoryCleanupErr) {
			t.Fatalf("failure/cause chain = %v", err)
		}
		if runCalls != 1 {
			t.Fatalf("contained run calls = %d", runCalls)
		}
		var contained *process.ContainedRunError
		if !errors.As(err, &contained) || contained.ExitCode == nil || *contained.ExitCode != 17 {
			t.Fatalf("contained outcome was not retained: %#v", contained)
		}
		var failure *Failure
		if !errors.As(err, &failure) || failure == nil {
			t.Fatalf("cleanup failure metadata missing: %#v", err)
		}
		if string(failure.JSON()) != `{"failure_id":"E_KIMI_CLEANUP_FAILED","primary_failure_id":"E_KIMI_RUN_FAILED","cleanup_failures":["contained_process","receipt_directory"]}` {
			t.Fatalf("cleanup failure JSON = %s", failure.JSON())
		}
	})

	t.Run("cleanup-only stage cause", func(t *testing.T) {
		containedCleanupErr := errors.New("synthetic cleanup-only failure")
		deps := base
		deps.runContained = func(_ context.Context, _ *exec.Cmd, _ process.ContainedStreamOptions, _ func(io.Reader) error) error {
			if err := os.WriteFile(argumentValue(args, "--terminal-receipt"), []byte(ownerReceiptRecord(t, "high", "must remain unacquired")), 0o600); err != nil {
				return err
			}
			return &process.ContainedRunError{Stage: process.ContainedStageCleanup, Cause: containedCleanupErr}
		}
		deps.removeAll = os.RemoveAll
		output, err := runWithDependencies(context.Background(), req, deps)
		if output.Receipt.Payload != "" || failureID(err) != "E_KIMI_CLEANUP_FAILED" || !errors.Is(err, containedCleanupErr) {
			t.Fatalf("cleanup-only result retained payload=%t failure_id=%s", output.Receipt.Payload != "", failureID(err))
		}
		var failure *Failure
		if !errors.As(err, &failure) || failure == nil {
			t.Fatalf("cleanup-only metadata missing: %#v", err)
		}
		if string(failure.JSON()) != `{"failure_id":"E_KIMI_CLEANUP_FAILED","cleanup_failures":["contained_process"]}` {
			t.Fatalf("cleanup-only JSON = %s", failure.JSON())
		}
	})
}

func TestRunWithDependenciesRetainsParseRunAndCleanupCauses(t *testing.T) {
	root := t.TempDir()
	prompt := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, ".agents", "skills", "lead", "scripts", "invoke-kimi-prompt.py")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{wrapper, filepath.Join(filepath.Dir(wrapper), "provider_prompt.py"), filepath.Join(root, "python.exe")} {
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	receiptBase := t.TempDir()
	var args []string
	runCause := errors.New("synthetic contained run cause")
	cleanupCause := errors.New("synthetic directory cleanup cause")
	directoryCleanupFailure := error(cleanupCause)
	code := 23
	contained := &process.ContainedRunError{Stage: process.ContainedStageWait, Cause: runCause, ExitCode: &code}
	deps := runnerDependencies{
		homeDir:  func() (string, error) { return root, nil },
		lookPath: func(string) (string, error) { return filepath.Join(root, "python.exe"), nil },
		command: func(name string, values ...string) *exec.Cmd {
			args = append([]string(nil), values...)
			return exec.Command(name, values...)
		},
		runContained: func(_ context.Context, _ *exec.Cmd, _ process.ContainedStreamOptions, _ func(io.Reader) error) error {
			if err := os.WriteFile(argumentValue(args, "--terminal-receipt"), []byte("NOT_A_VALID_RECEIPT\n"), 0o600); err != nil {
				return err
			}
			return contained
		},
		mkdirTemp: func(_, pattern string) (string, error) { return os.MkdirTemp(receiptBase, pattern) },
		removeAll: func(path string) error {
			if directoryCleanupFailure != nil {
				return directoryCleanupFailure
			}
			return os.RemoveAll(path)
		},
	}
	req := RunRequest{Topic: "topic", PromptFile: prompt, Cwd: root, TaskClass: "review", Role: "external-reviewer", Effort: "high", TimeoutSeconds: 1, CapabilityChoice: CapabilityChoice{Kind: "none"}}
	output, err := runWithDependencies(context.Background(), req, deps)
	if output.Receipt.Payload != "" {
		t.Fatal("invalid receipt produced output")
	}
	if failureID(err) != "E_KIMI_CLEANUP_FAILED" || !errors.Is(err, errReceiptInvalid) || !errors.Is(err, runCause) || !errors.Is(err, cleanupCause) {
		t.Fatalf("failure identity or cause chain was lost: failure_id=%s", failureID(err))
	}
	var gotContained *process.ContainedRunError
	if !errors.As(err, &gotContained) || gotContained != contained || gotContained.ExitCode == nil || *gotContained.ExitCode != code {
		t.Fatal("original contained outcome or exit code was not retained")
	}

	directoryCleanupFailure = nil
	output, err = runWithDependencies(context.Background(), req, deps)
	if output.Receipt.Payload != "" || failureID(err) != "E_KIMI_RECEIPT_INVALID" || !errors.Is(err, errReceiptInvalid) || !errors.Is(err, runCause) {
		t.Fatalf("parse/run causes were not retained without directory cleanup failure: failure_id=%s payload_present=%t", failureID(err), output.Receipt.Payload != "")
	}
	if errors.Is(err, cleanupCause) || !errors.As(err, &gotContained) || gotContained != contained || gotContained.ExitCode == nil || *gotContained.ExitCode != code {
		t.Fatal("no-cleanup failure duplicated cleanup or lost the original contained outcome")
	}
}

func argumentValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func sameTestPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func TestFailureJSONIsTypedAndDetailFree(t *testing.T) {
	f := newFailure("E_KIMI_WRAPPER_UNAVAILABLE", errors.New("sensitive path and prompt"))
	if string(f.JSON()) != `{"failure_id":"E_KIMI_WRAPPER_UNAVAILABLE"}` || strings.Contains(string(f.JSON()), "sensitive") {
		t.Fatalf("failure JSON = %s", f.JSON())
	}
}

func TestRunWithContainedStreamSettlesStubbornChild(t *testing.T) {
	root := t.TempDir()
	wrapper := filepath.Join(root, ".agents", "skills", "lead", "scripts", "invoke-kimi-prompt.py")
	python := filepath.Join(root, "python.exe")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{wrapper, filepath.Join(filepath.Dir(wrapper), "provider_prompt.py"), python} {
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	prompt := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(root, "child.pid")
	receiptBase := filepath.Join(root, "receipts")
	if err := os.Mkdir(receiptBase, 0o700); err != nil {
		t.Fatal(err)
	}
	var helperStderr bytes.Buffer
	deps := runnerDependencies{
		homeDir:  func() (string, error) { return root, nil },
		lookPath: func(string) (string, error) { return python, nil },
		command: func(_ string, args ...string) *exec.Cmd {
			encoded, err := json.Marshal(args)
			if err != nil {
				panic(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestKimiFakeWrapperProcess$")
			cmd.Env = append(os.Environ(), "GO_WANT_KIMI_WRAPPER=1", "KIMI_HELPER_PID_FILE="+pidFile, "KIMI_HELPER_ARGS="+string(encoded))
			return cmd
		},
		runContained: func(ctx context.Context, cmd *exec.Cmd, options process.ContainedStreamOptions, consume func(io.Reader) error) error {
			options.Stderr = &helperStderr
			return process.RunContainedStream(ctx, cmd, options, consume)
		},
		mkdirTemp: func(_, pattern string) (string, error) { return os.MkdirTemp(receiptBase, pattern) },
		removeAll: os.RemoveAll,
	}
	req := RunRequest{Topic: "stubborn", PromptFile: prompt, Cwd: root, TaskClass: "review", Role: "external-reviewer", Effort: "high", TimeoutSeconds: 1, CapabilityChoice: CapabilityChoice{Kind: "none"}}
	_, err := runWithDependencies(context.Background(), req, deps)
	if failureID(err) != "E_KIMI_TIMEOUT" {
		var contained *process.ContainedRunError
		_ = errors.As(err, &contained)
		t.Fatalf("error = %v cause=%+v contained=%#v helper_stderr=%q", err, errors.Unwrap(err), contained, helperStderr.String())
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for process.IsPidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if process.IsPidAlive(pid) {
		t.Fatalf("stubborn descendant %d survived contained timeout", pid)
	}
	entries, err := os.ReadDir(receiptBase)
	if err != nil || len(entries) != 0 {
		t.Fatalf("receipt namespace residue: entries=%v err=%v", entries, err)
	}
}

func TestKimiFakeWrapperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_KIMI_CHILD") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("GO_WANT_KIMI_WRAPPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("KIMI_HELPER_ARGS")), &args); err != nil || len(args) < 2 {
		t.Fatalf("missing fake wrapper argv: %v", err)
	}
	topic := args[1]
	if topic == "stubborn" {
		child := exec.Command(os.Args[0], "-test.run=^TestKimiFakeWrapperProcess$")
		child.Env = append(os.Environ(), "GO_WANT_KIMI_CHILD=1")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("KIMI_HELPER_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	receipt := argumentValue(args, "--terminal-receipt")
	effort := argumentValue(args, "--kimi-effort")
	if receipt == "" || effort == "" {
		t.Fatal("missing receipt or effort")
	}
	if _, err := fmt.Fprint(os.Stdout, strings.Repeat("x", 2<<20)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt, []byte(ownerReceiptRecord(t, effort, "synthetic result")), 0o600); err != nil {
		t.Fatal(err)
	}
}
