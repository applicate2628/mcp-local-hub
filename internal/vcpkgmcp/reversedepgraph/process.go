package reversedepgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type ExecRunner struct{}

func DefaultRunner() Runner { return ExecRunner{} }

func lookupSafeEnvironment(key string) (string, bool) {
	value, ok := os.LookupEnv(key)
	return value, ok && value != "" && !strings.ContainsRune(value, '\x00')
}

type cappedCapture struct {
	mu        sync.Mutex
	prefix    []byte
	total     int64
	digest    hash.Hash
	truncated bool
}

func newCappedCapture() *cappedCapture {
	return &cappedCapture{prefix: make([]byte, 0, 4096), digest: sha256.New()}
}

func (capture *cappedCapture) Write(data []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.total += int64(len(data))
	_, _ = capture.digest.Write(data)
	remaining := MaxStreamBytes - len(capture.prefix)
	if remaining > 0 {
		retain := len(data)
		if retain > remaining {
			retain = remaining
		}
		capture.prefix = append(capture.prefix, data[:retain]...)
	}
	if capture.total > MaxStreamBytes {
		capture.truncated = true
	}
	return len(data), nil
}

func (capture *cappedCapture) result() CapturedStream {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return CapturedStream{
		Data: append([]byte(nil), capture.prefix...), Bytes: capture.total,
		SHA256: hex.EncodeToString(capture.digest.Sum(nil)), Truncated: capture.truncated,
	}
}

func (ExecRunner) Run(ctx context.Context, command Command) RunOutput {
	if ctx == nil {
		ctx = context.Background()
	}
	process := exec.Command(command.Executable, command.Args...)
	process.Dir = command.Dir
	process.Env = append([]string(nil), command.Env...)
	stdout, stderr := newCappedCapture(), newCappedCapture()
	process.Stdout, process.Stderr = stdout, stderr
	lifecycle, err := startPlatformProcess(process)
	if err != nil {
		return RunOutput{Stdout: stdout.result(), Stderr: stderr.result(), ExitCode: -1, Err: err}
	}
	waited := make(chan error, 1)
	go func() { waited <- process.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		_ = terminatePlatformProcess(lifecycle)
		waitErr = <-waited
		if waitErr == nil {
			waitErr = ctx.Err()
		}
	}
	closePlatformProcess(lifecycle)
	exitCode := process.ProcessState.ExitCode()
	if ctx.Err() != nil {
		waitErr = ctx.Err()
	}
	return RunOutput{
		Stdout: stdout.result(), Stderr: stderr.result(), ExitCode: exitCode,
		Started: true, Reaped: true, Err: waitErr,
	}
}
