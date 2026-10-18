//go:build windows

package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"mcp-local-hub/internal/process"
)

// Called under h.mu: publish the partial acquisition before any native entry.
func (h *StdioHost) startOriginal(ctx context.Context, cmd *exec.Cmd) (selected bool, retErr error) {
	if h.cfg.LaunchCapability != nil {
		return false, nil
	}
	p := newStdioOriginalLifecycle()
	h.original, h.cmd, h.started = p, cmd, true
	readersStarted, waiterStarted := false, false
	defer func() {
		p.primary = retErr
		if !readersStarted {
			close(p.stdoutDone)
			close(p.stderrDone)
		}
		if !waiterStarted {
			close(p.rootDone)
			close(p.watcherDone)
			close(p.lifetimeDone)
		}
	}()
	if ctx == nil {
		return true, errors.New("stdio start context missing")
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	// exec.Command resolves Path and Err; the native builder must receive the
	// same lookup refusal without attempting CreateProcess on an unresolved path.
	if cmd.Err != nil {
		return true, fmt.Errorf("start subprocess: %w", cmd.Err)
	}
	ops := stdioOriginalLaunchOps{pipe: os.Pipe, job: process.NewJobObserved, start: process.StartWithJobFilesOriginal}
	if h.originalLaunch != nil {
		if h.originalLaunch.pipe != nil {
			ops.pipe = h.originalLaunch.pipe
		}
		if h.originalLaunch.job != nil {
			ops.job = h.originalLaunch.job
		}
		if h.originalLaunch.start != nil {
			ops.start = h.originalLaunch.start
		}
		ops.beforeStdout = h.originalLaunch.beforeStdout
		ops.observeOriginal = h.originalLaunch.observeOriginal
	}
	p.observeOriginal = ops.observeOriginal
	if p.observeOriginal == nil {
		p.observeOriginal = func(owner *process.AtCreateJobProcess, ctx context.Context, natural bool) (process.ContainedNativeSnapshot, error) {
			return owner.ObserveOriginal(ctx, natural)
		}
	}
	for index := 0; index < len(p.files); index += 2 {
		r, w, err := ops.pipe()
		if r != nil {
			p.files[index] = &stdioOriginalFile{file: r}
		}
		if w != nil {
			p.files[index+1] = &stdioOriginalFile{file: w}
		}
		if err != nil {
			return true, fmt.Errorf("stdio pipe acquisition: %w", err)
		}
	}
	p.input = &stdioOriginalInput{file: p.files[1], joined: make(chan struct{})}
	h.stdin = p.input
	var err error
	p.job, err = ops.job(process.JobOptions{KillOnClose: true})
	if err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	p.nativeAttempted = true
	p.original, err = ops.start(p.job, cmd, p.files[0].file, p.files[3].file, p.files[5].file)
	err = errors.Join(err, ctx.Err())
	if p.original == nil {
		return true, errors.Join(err, errors.New("stdio original creator unavailable"))
	}
	// Parent copies of child standard files are never delegated to exec.Cmd.
	err = errors.Join(err, p.files[0].close(), p.files[3].close(), p.files[5].close())
	if p.original.Created() {
		var identityErr error
		p.identity, identityErr = p.original.Identity()
		err = errors.Join(err, identityErr)
	}
	stderrSink := h.openStderrSink()
	h.stdoutScan = bufio.NewScanner(p.files[2].file)
	h.stdoutScan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	h.wg.Add(1)
	go func() {
		if ops.beforeStdout != nil {
			ops.beforeStdout()
		}
		h.readStdoutLoop()
		p.stdoutError = h.stdoutScan.Err()
		close(p.stdoutDone)
	}()
	go func() {
		defer close(p.stderrDone)
		scanner := bufio.NewScanner(p.files[4].file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if _, writeErr := fmt.Fprintf(stderrSink, "[subproc stderr] %s\n", scanner.Bytes()); writeErr != nil {
				p.stderrError = errors.Join(p.stderrError, writeErr)
			}
		}
		p.stderrError = errors.Join(p.stderrError, scanner.Err())
	}()
	readersStarted = true
	if !p.original.Created() {
		return true, err
	}
	go func() {
		defer close(p.watcherDone)
		p.root = p.original.WaitOriginal()
		close(p.rootDone)
		close(h.procExited)
		// Original native cleanup and final response drain share the one
		// post-exit deadline. The finalizer publishes childExited after receipt.
		h.beginOriginalStop(context.Background(), p.root.Error)
	}()
	go func() {
		defer close(p.lifetimeDone)
		select {
		case <-ctx.Done():
			h.beginOriginalStop(context.Background(), ctx.Err())
		case <-h.done:
		}
	}()
	waiterStarted = true
	return true, err
}
