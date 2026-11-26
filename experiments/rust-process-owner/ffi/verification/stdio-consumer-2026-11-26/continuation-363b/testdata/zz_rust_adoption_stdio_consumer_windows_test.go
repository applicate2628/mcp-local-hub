//go:build windows && amd64

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/process"
)

const rustAdoptionCommit = "3ebe335a2f666a5279d7fafc31a6f4dc3feedff4"
const rustAdoptionDLLSHA = "dfa87a10a39e9110cda735ae6a987bfa64b03446529072710723e9b6ea0dca56"

// This experiment adds no constructor, launch, reader, or loader seam. Both
// arms use the same unmarked TestStdioOriginalHelper image and exactly the
// existing stdioRustOriginalRPC body. Only the host's linked Rust mark differs.
func TestRustAdoptionStdioConsumer(t *testing.T) {
	if os.Getenv("MCPHUB_RUST_ADOPTION") != "1" {
		t.Skip("requires the isolated real-consumer campaign")
	}
	arm := os.Getenv("MCPHUB_RUST_ADOPTION_ARM")
	if arm != "go" && arm != "rust" {
		t.Fatal("expected arm missing")
	}
	_, commit, _ := buildinfo.Get()
	if commit != rustAdoptionCommit {
		t.Fatalf("compiled source commit differs: %q", commit)
	}
	mark, markErr := buildinfo.CurrentRustOwnerSHA256()
	if arm == "go" && (mark != "" || !errors.Is(markErr, buildinfo.ErrRustOwnerBuildUnmarked)) {
		t.Fatal("Go arm is not an unmarked Product")
	}
	if arm == "rust" && (markErr != nil || mark != rustAdoptionDLLSHA) {
		t.Fatal("Rust arm lacks the exact compiled Product mark")
	}
	selected, err := process.ProductRustOwnerEnabled()
	if err != nil || selected != (arm == "rust") {
		t.Fatalf("production selection differs: selected=%t err=%v", selected, err)
	}
	exe, err := os.Executable()
	if err != nil || filepath.Base(exe) != "mcphub.exe" {
		t.Fatal("host must be the admitted mcphub.exe image")
	}
	helper := os.Getenv("MCPHUB_RUST_ADOPTION_HELPER")
	helperSHA := os.Getenv("MCPHUB_RUST_ADOPTION_HELPER_SHA256")
	root := os.Getenv("MCPHUB_RUST_ADOPTION_ROOT")
	if !filepath.IsAbs(helper) || !filepath.IsAbs(root) || len(helperSHA) != 64 {
		t.Fatal("absolute helper, case root and measured helper hash required")
	}
	helperBytes, err := os.ReadFile(helper)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(helperBytes)) != helperSHA {
		t.Fatal("shared helper image differs")
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		t.Fatalf("exclusive campaign case root missing: %v", err)
	}
	var results []map[string]any
	for _, name := range []string{"healthy-after-canceled-observers", "final-reply", "lifetime-cancel"} {
		var result map[string]any
		if !t.Run(name, func(t *testing.T) {
			result = rustAdoptionCase(t, arm, helper, root, name)
		}) {
			return
		}
		results = append(results, result)
	}
	report := map[string]any{
		"schema":                           "rust-real-stdio-consumer-v1",
		"arm":                              arm,
		"source_commit":                    commit,
		"helper_sha256":                    helperSHA,
		"compiled_rust_mark":               mark,
		"cases":                            results,
		"case_count":                       len(results),
		"rpc_count":                        66,
		"production_launch_seams_used":     false,
		"session_setup":                    "existing-createSession-without-initialize-RPC",
		"performance_improvement_asserted": false,
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RUST_ADOPTION_RECEIPT %s\n", raw)
}

func rustAdoptionCase(t *testing.T, arm, helper, root, name string) map[string]any {
	t.Helper()
	caseRoot := filepath.Join(root, name)
	if err := os.Mkdir(caseRoot, 0700); err != nil {
		t.Fatalf("case directory must be new: %v", err)
	}
	mode := "echo"
	rpcCount := 1
	if name == "final-reply" {
		mode = "final"
	}
	if name == "healthy-after-canceled-observers" {
		rpcCount = 64
	}
	h, err := NewStdioHost(HostConfig{
		Command:    helper,
		Args:       []string{"-test.run=^TestStdioOriginalHelper$"},
		WorkingDir: caseRoot,
		Env:        map[string]string{stdioOriginalHelperMode: mode, stdioOriginalHelperRoot: caseRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Observer bounds do not replace the host's 2-second stop or 5-second
	// natural drain deadline. Cancel functions release their own resources:
	// https://pkg.go.dev/context@go1.27.4#WithTimeout
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	t.Cleanup(func() {
		h.mu.Lock()
		retained := h.original != nil
		h.mu.Unlock()
		if !retained {
			return
		}
		observer, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		settled, stopErr := h.StopObserved(observer)
		if stopErr != nil || !settled.Complete() {
			t.Errorf("same-owner fixture cleanup incomplete: %+v err=%v", settled, stopErr)
		}
	})
	started := time.Now()
	if err := h.Start(lifetime); err != nil {
		t.Fatal(err)
	}
	originalTestBarrier(t, filepath.Join(caseRoot, "root-ready"))
	startNS := time.Since(started).Nanoseconds()
	h.mu.Lock()
	owner := h.original
	if owner == nil || owner.original == nil || !owner.original.Created() || h.originalLaunch != nil || h.cfg.LaunchCapability != nil {
		h.mu.Unlock()
		t.Fatal("production startup did not retain its actual original owner")
	}
	original := owner.original
	identity := owner.identity
	goJob := owner.job != nil
	h.mu.Unlock()
	if !identity.Known || identity.PID <= 0 || identity.NativeBirthFILETIME == 0 || goJob != (arm == "go") {
		t.Fatal("actual original creation identity or selected backend differs")
	}
	if arm == "rust" {
		retained := stdioRustOriginalOwner(t, h)
		if retained.lifecycle != owner || retained.original != original || retained.identity != identity {
			t.Fatal("Rust receiving replaced the original owner")
		}
	}
	if name == "healthy-after-canceled-observers" {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if source, err := h.OriginalMemberSource(canceled); source != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled source observer: source=%v err=%v", source, err)
		}
		if observed, err := h.ObserveOriginalMembers(canceled); observed != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled member observer: observation=%v err=%v", observed, err)
		}
		if stopped, err := h.StopObserved(canceled); !stopped.Pending || !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled stop observer: %+v err=%v", stopped, err)
		}
		owner.stopMu.Lock()
		stopStarted := owner.stopStarted
		owner.stopMu.Unlock()
		if stopStarted {
			t.Fatal("observer cancellation started lifetime termination")
		}
	}
	rpcStarted := time.Now()
	for index := 0; index < rpcCount; index++ {
		stdioRustOriginalRPC(t, h, fmt.Sprintf("%s-%03d", name, index))
	}
	rpcNS := time.Since(rpcStarted).Nanoseconds()
	if name == "lifetime-cancel" {
		cancelLifetime()
	}
	stopStarted := time.Now()
	observer, cancelObserver := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelObserver()
	if name != "healthy-after-canceled-observers" {
		if err := originalJoin(observer, h.ChildExited()); err != nil {
			t.Fatalf("original lifetime completion not received: %v", err)
		}
	}
	settled, stopErr := h.StopObserved(observer)
	stopNS := time.Since(stopStarted).Nanoseconds()
	if stopErr != nil || !settled.Complete() || settled.NoNativeAttempt || settled.NativeSettlement == nil ||
		!settled.RootWait.Known || settled.RootWait.Error != nil || settled.OriginalIdentity != identity || settled.Deadline.IsZero() {
		t.Fatalf("same-owner terminal receiving incomplete: %+v err=%v", settled, stopErr)
	}
	if name == "final-reply" && settled.RootWait.ExitCode != 0 {
		t.Fatalf("final-reply helper exit differs: %+v", settled.RootWait)
	}
	if name == "lifetime-cancel" && !errors.Is(settled.PrimaryError, context.Canceled) {
		t.Fatalf("lifetime cancellation was lost: %v", settled.PrimaryError)
	}
	if name != "lifetime-cancel" && settled.PrimaryError != nil {
		t.Fatalf("unexpected primary error: %v", settled.PrimaryError)
	}
	if h.original != owner || owner.original != original {
		t.Fatal("terminal receiving replaced custody")
	}
	for _, done := range []<-chan struct{}{
		owner.rootDone, owner.stdoutDone, owner.stderrDone, owner.watcherDone,
		owner.lifetimeDone, owner.stopDone, owner.input.joined, h.ChildExited(),
	} {
		select {
		case <-done:
		default:
			t.Fatal("terminal result preceded an owned worker's completion")
		}
	}
	for index, file := range owner.files {
		if file == nil || file.file == nil || file.closeError != nil {
			t.Fatalf("original file %d missing or close failed", index)
		}
		if fd := file.file.Fd(); fd != uintptr(syscall.InvalidHandle) {
			t.Fatalf("original file %d retains a live Go descriptor after close", index)
		}
		// Windows Stat probes the invalidated native handle after os.File.Close.
		if _, err := file.file.Stat(); !errors.Is(err, os.ErrClosed) && !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			t.Fatalf("original file %d did not report a closed state: %v", index, err)
		}
	}
	again, againErr := h.StopObserved(observer)
	if againErr != nil || !again.Complete() || !again.Deadline.Equal(settled.Deadline) ||
		again.NativeSettlement != settled.NativeSettlement || again.RootWait != settled.RootWait ||
		again.OriginalIdentity != settled.OriginalIdentity {
		t.Fatalf("repeat receiving renewed or replaced settlement: %+v err=%v", again, againErr)
	}
	native := settled.NativeSettlement.Snapshot()
	if native.Error != nil || native.RootIdentity != identity || !native.HandlesClosed || !native.RootWaitKnown || native.FinalActive != 0 ||
		!(native.OriginalJobExited || native.MembersExited && native.AccountingKnown) {
		t.Fatalf("original process/tree/handle evidence incomplete: %+v", native)
	}
	if time.Now().After(settled.Deadline) {
		t.Fatal("complete process, tree and worker evidence arrived after the original stop deadline")
	}
	return map[string]any{
		"name":                              name,
		"rpc_count":                         rpcCount,
		"start_ns":                          startNS,
		"rpc_ns":                            rpcNS,
		"stop_ns":                           stopNS,
		"case_ns":                           time.Since(started).Nanoseconds(),
		"original_identity":                 identity,
		"original_exit_code":                settled.RootWait.ExitCode,
		"go_job_selected":                   goJob,
		"original_job_exited":               native.OriginalJobExited,
		"members_exited":                    native.MembersExited,
		"accounting_known":                  native.AccountingKnown,
		"final_active":                      native.FinalActive,
		"handles_closed":                    native.HandlesClosed,
		"files_closed":                      6,
		"owned_completion_signals_received": 8,
		"same_original_owner":               true,
		"same_terminal_receipt":             true,
		"same_stop_deadline":                true,
		"received_before_stop_deadline":     true,
		"stop_deadline":                     settled.Deadline,
		"primary_canceled":                  errors.Is(settled.PrimaryError, context.Canceled),
		"complete":                          settled.Complete(),
	}
}
