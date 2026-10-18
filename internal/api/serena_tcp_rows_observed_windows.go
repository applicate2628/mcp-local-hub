//go:build windows

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mcp-local-hub/internal/process"
	"os/exec"
	"path/filepath"
	"time"
)

func ObserveOriginalLoopbackTCPRowsObserved(ctx context.Context) (o *LoopbackTCPRowsObservation, retErr error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("ORIGINAL_TCP_QUERY_CONTEXT_INVALID")
	}
	end, finite := ctx.Deadline()
	if !finite {
		return nil, errors.New("ORIGINAL_TCP_QUERY_DEADLINE_REQUIRED")
	}
	// Same installed netstat selection and argv as the existing Windows scanner.
	path, err := exec.LookPath("netstat")
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, _, err := qualifyPreparedSerenaFile(ctx, path, 64<<20)
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: file.Path, Args: []string{file.Path, "-ano"}}
	var stdout, stderr bytes.Buffer
	o = &LoopbackTCPRowsObservation{issuer: &struct{ marker byte }{1}, sourceSHA256: file.SHA256}
	remaining := time.Until(end)
	if remaining <= 0 {
		return o, context.DeadlineExceeded
	}
	o.receipt, retErr = process.RunContainedStreamObserved(ctx, cmd, process.ContainedStreamOptions{CleanupTimeout: remaining, AbsoluteCleanupDeadline: end, StdoutByteLimit: 64 << 20, StderrByteLimit: 8 << 20, Stderr: &stderr}, func(r io.Reader) error { _, err := io.Copy(&stdout, r); return err })
	defer func() { o.err = retErr }()
	if retErr != nil || !o.receipt.RootWaitKnown || o.receipt.RootExitCode != 0 || !o.receipt.NativeSettlement.Complete() || !o.receipt.IOSettlement.Complete() {
		return o, errors.Join(retErr, errors.New("ORIGINAL_TCP_QUERY_CUSTODY_PENDING"))
	}
	raw := append([]byte(nil), stdout.Bytes()...)
	o.frameSHA256 = preparedSerenaHash(raw)
	o.rows, retErr = parseOriginalLoopbackTCPRows(raw)
	if retErr != nil {
		return o, retErr
	}
	o.receiptSHA256, retErr = tcpObservedReceiptSHA(o)
	return o, errors.Join(retErr, ctx.Err())
}
