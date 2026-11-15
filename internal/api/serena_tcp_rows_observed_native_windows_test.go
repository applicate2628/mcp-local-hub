//go:build windows

package api

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

// Exercise the actual bounded netstat owner; parser fixtures cannot detect
// admission failures before its native process is created.
func TestOriginalTCPRowsObservedSettlesNativeQueryAndFindsOwnedListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on an OS-assigned loopback port: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close owned listener: %v", err)
		}
	})
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.IP.String() != "127.0.0.1" || address.Port < 1 {
		t.Fatalf("unexpected owned listener address: %v", listener.Addr())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	observation, err := ObserveOriginalLoopbackTCPRowsObserved(ctx)
	if err != nil {
		t.Fatalf("observe original TCP rows: %v; invalid_cleanup_timeout=%t",
			err, errors.Is(err, process.ErrInvalidCleanupTimeout))
	}
	if err := observation.Validate(ctx); err != nil {
		t.Fatalf("validate original TCP observation: %v", err)
	}
	receipt := observation.receipt
	if !receipt.StartAttempted || !receipt.RootIdentity.Known || !receipt.RootWaitKnown ||
		receipt.RootExitCode != 0 || receipt.RootExitError != nil ||
		receipt.PrimaryError != nil || receipt.CleanupError != nil {
		t.Fatalf("query did not complete an original successful wait: %+v", receipt)
	}
	if !receipt.NativeSettlement.Complete() || !receipt.IOSettlement.Complete() {
		t.Fatal("original query native or stream custody did not settle")
	}
	native := receipt.NativeSettlement.Snapshot()
	if native.NoChildKnown || native.RootIdentity != receipt.RootIdentity ||
		!native.MembersExited || !native.AccountingKnown || native.FinalActive != 0 || !native.HandlesClosed {
		t.Fatalf("incomplete original native settlement: %+v", native)
	}
	for _, slot := range []process.ContainedStreamSlot{
		receipt.IOSettlement.Stdin, receipt.IOSettlement.StdoutRead, receipt.IOSettlement.StdoutWrite,
		receipt.IOSettlement.StderrRead, receipt.IOSettlement.StderrWrite,
	} {
		if !slot.Acquired || !slot.CloseAttempted || !slot.Closed || slot.CloseError != nil {
			t.Fatalf("original query stream handle did not close: %+v", slot)
		}
	}
	if !receipt.IOSettlement.Stdout.WorkerStarted || !receipt.IOSettlement.Stderr.WorkerStarted ||
		receipt.IOSettlement.Stdout.Bytes == 0 {
		t.Fatal("query did not produce and settle its original output workers")
	}
	digest, err := tcpObservedReceiptSHA(observation)
	if err != nil || !preparedSerenaDigest(digest) || observation.ReceiptSHA256() != digest {
		t.Fatalf("original TCP receipt digest is not bound to its observation: %v", err)
	}
	rows, err := observation.Rows(ctx)
	if err != nil {
		t.Fatalf("receive original TCP rows: %v", err)
	}
	matches := 0
	for _, row := range rows {
		if row.State == "LISTENING" && row.LocalHost == "127.0.0.1" &&
			int(row.LocalPort) == address.Port && row.PID == uint32(os.Getpid()) {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("owned loopback listener matches=%d, want exactly one", matches)
	}
	t.Logf("original query settled: pid=%d, stdout_bytes=%d, stderr_bytes=%d, owned_listener_matches=%d",
		receipt.RootIdentity.PID, receipt.IOSettlement.Stdout.Bytes, receipt.IOSettlement.Stderr.Bytes, matches)
}
