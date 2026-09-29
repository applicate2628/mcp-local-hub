package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"mcp-local-hub/internal/process"
)

const (
	maxTimeoutSeconds = 3600
	cleanupTimeout    = 5 * time.Second
)

var noneCapabilityJSON = []byte(`{"v":1,"tools":[],"mcpServers":[],"subagents":[],"permission":"reject","cwd":null}`)

var errWrapperUnavailable = errors.New("Kimi wrapper unavailable")

type CapabilityChoice struct {
	Kind string `json:"kind"`
	File string `json:"file,omitempty"`
}

type RunRequest struct {
	Topic            string
	PromptFile       string
	Cwd              string
	TaskClass        string
	Role             string
	Effort           string
	TimeoutSeconds   int
	CapabilityChoice CapabilityChoice
}

type RunOutput struct {
	Receipt providerReceipt
}

type Failure struct {
	ID    string
	cause error
}

func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	return f.ID
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.cause
}

func (f *Failure) JSON() []byte {
	payload, _ := json.Marshal(struct {
		FailureID string `json:"failure_id"`
	}{FailureID: f.ID})
	return payload
}

func newFailure(id string, cause error) *Failure { return &Failure{ID: id, cause: cause} }

func failureID(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.ID
	}
	return ""
}

type containedRunner func(context.Context, *exec.Cmd, process.ContainedStreamOptions, func(io.Reader) error) error

type runnerDependencies struct {
	homeDir      func() (string, error)
	lookPath     func(string) (string, error)
	command      func(string, ...string) *exec.Cmd
	runContained containedRunner
	mkdirTemp    func(string, string) (string, error)
	removeAll    func(string) error
}

type Runner struct{ deps runnerDependencies }

func NewRunner() *Runner {
	return &Runner{deps: runnerDependencies{
		homeDir:      os.UserHomeDir,
		lookPath:     exec.LookPath,
		command:      func(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) },
		runContained: process.RunContainedStream,
		mkdirTemp:    os.MkdirTemp,
		removeAll:    os.RemoveAll,
	}}
}

func (r *Runner) Run(ctx context.Context, request RunRequest) (RunOutput, error) {
	return runWithDependencies(ctx, request, r.deps)
}

func runWithDependencies(ctx context.Context, request RunRequest, deps runnerDependencies) (output RunOutput, retErr error) {
	home, err := deps.homeDir()
	if err != nil {
		return RunOutput{}, newFailure("E_KIMI_WRAPPER_UNAVAILABLE", err)
	}
	wrapper, err := resolveWrapper(home)
	if err != nil {
		return RunOutput{}, newFailure("E_KIMI_WRAPPER_UNAVAILABLE", err)
	}
	python, err := resolvePython(deps.lookPath)
	if err != nil {
		return RunOutput{}, newFailure("E_KIMI_WRAPPER_UNAVAILABLE", err)
	}

	receiptDir, err := deps.mkdirTemp("", "mcphub-kimi-receipt-")
	if err != nil {
		return RunOutput{}, newFailure("E_KIMI_RECEIPT_INVALID", err)
	}
	defer func() {
		if cleanupErr := deps.removeAll(receiptDir); cleanupErr != nil {
			output = RunOutput{}
			retErr = newFailure("E_KIMI_CLEANUP_FAILED", cleanupErr)
		}
	}()

	receiptPath := filepath.Join(receiptDir, "terminal-receipt.txt")
	capabilityPath := request.CapabilityChoice.File
	if request.CapabilityChoice.Kind == "none" {
		capabilityPath = filepath.Join(receiptDir, "capabilities-none-v1.json")
		if err := writePrivateFile(capabilityPath, noneCapabilityJSON); err != nil {
			return RunOutput{}, newFailure("E_KIMI_RUN_FAILED", err)
		}
	}
	args := buildWrapperArgs(wrapper, receiptPath, capabilityPath, request)
	cmd := deps.command(python, args...)
	cmd.Dir = request.Cwd
	process.NoConsole(cmd)

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(request.TimeoutSeconds)*time.Second)
	defer cancel()
	runErr := deps.runContained(runCtx, cmd, process.ContainedStreamOptions{CleanupTimeout: cleanupTimeout, Stderr: io.Discard}, func(reader io.Reader) error {
		_, err := io.Copy(io.Discard, reader)
		return err
	})

	var contained *process.ContainedRunError
	if errors.As(runErr, &contained) && (contained.CleanupCause != nil || errors.Is(runErr, process.ErrCleanupTimeout)) {
		return RunOutput{}, newFailure("E_KIMI_CLEANUP_FAILED", runErr)
	}
	record, readErr := readReceiptFile(receiptPath)
	if readErr == nil {
		receipt, parseErr := parseProviderReceipt(record, request.Effort, request.CapabilityChoice)
		if parseErr != nil {
			return RunOutput{}, newFailure("E_KIMI_RECEIPT_INVALID", parseErr)
		}
		return RunOutput{Receipt: receipt}, nil
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return RunOutput{}, newFailure("E_KIMI_TIMEOUT", runCtx.Err())
	}
	if ctx.Err() != nil {
		return RunOutput{}, newFailure("E_KIMI_CANCELLED", ctx.Err())
	}
	if runErr != nil {
		return RunOutput{}, newFailure("E_KIMI_RUN_FAILED", runErr)
	}
	return RunOutput{}, newFailure("E_KIMI_RECEIPT_INVALID", readErr)
}

func buildWrapperArgs(wrapper, receipt, capabilityFile string, request RunRequest) []string {
	return []string{
		wrapper,
		request.Topic,
		"--prompt-file", request.PromptFile,
		"--terminal-receipt", receipt,
		"--task-class", request.TaskClass,
		"--role", request.Role,
		"--timeout-secs", strconv.Itoa(request.TimeoutSeconds),
		"--kimi-effort", request.Effort,
		"--kimi-capabilities-file", capabilityFile,
	}
}

func resolveWrapper(home string) (string, error) {
	scriptsDir, err := filepath.Abs(filepath.Join(home, ".agents", "skills", "lead", "scripts"))
	if err != nil {
		return "", errors.Join(errWrapperUnavailable, err)
	}
	wrapper, err := resolveScriptPair(scriptsDir)
	if err != nil {
		return "", errors.Join(errWrapperUnavailable, err)
	}
	return wrapper, nil
}

func resolveScriptPair(scriptsDir string) (string, error) {
	wrapper := filepath.Join(scriptsDir, "invoke-kimi-prompt.py")
	owner := filepath.Join(scriptsDir, "provider_prompt.py")
	resolvedWrapper, err := resolveTrustedGlobalLeaf(wrapper, scriptsDir)
	if err != nil {
		return "", err
	}
	if _, err := resolveTrustedGlobalLeaf(owner, scriptsDir); err != nil {
		return "", err
	}
	return resolvedWrapper, nil
}

func resolveTrustedGlobalLeaf(path, scriptsDir string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if filepath.Dir(abs) != filepath.Clean(scriptsDir) {
		return "", errWrapperUnavailable
	}
	before, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return "", errWrapperUnavailable
	}
	after, err := os.Stat(abs)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return "", errWrapperUnavailable
	}
	file, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return abs, nil
}

func resolvePython(lookPath func(string) (string, error)) (string, error) {
	for _, name := range []string{"python3", "python"} {
		candidate, err := lookPath(name)
		if err != nil {
			continue
		}
		resolved, err := resolveOrdinaryFile(candidate)
		if err != nil || !filepath.IsAbs(resolved) {
			continue
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(resolved)
			if err != nil || info.Mode().Perm()&0o111 == 0 {
				continue
			}
		}
		return resolved, nil
	}
	return "", errWrapperUnavailable
}

func resolveOrdinaryFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errWrapperUnavailable
	}
	return resolved, nil
}

func readReceiptFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errReceiptInvalid
	}
	return os.ReadFile(path)
}

func writePrivateFile(path string, body []byte) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	_, err = file.Write(body)
	return err
}
