package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestAdoptLeaseLeafRejectsHardlinkBeforeLock catches the unsafe path-owner
// implementation: a hardlink gives the lease pathname a regular-file shape
// while binding it to a foreign inode. Acquiring that leaf must fail before an
// OS lock is taken and must leave the foreign canary unchanged.
func TestAdoptLeaseLeafRejectsHardlinkBeforeLock(t *testing.T) {
	stateDir := isolateStateDir(t)
	bootstrapSecureAdoptLeaseNamespace(t)
	canary := filepath.Join(stateDir, "foreign-canary")
	const want = "foreign-inode-canary"
	if err := os.WriteFile(canary, []byte(want), 0o600); err != nil {
		t.Fatalf("seed foreign canary: %v", err)
	}
	leasePath, err := adoptManifestLeasePath("hardlink-lease")
	if err != nil {
		t.Fatalf("derive lease path: %v", err)
	}
	if err := os.Link(canary, leasePath); err != nil {
		t.Fatalf("create lease hardlink: %v", err)
	}

	lease, acquired, err := tryAcquireAdoptManifestLease("hardlink-lease")
	if lease != nil {
		t.Cleanup(func() { _ = lease.Unlock() })
	}
	if err == nil || acquired {
		t.Fatalf("hardlinked lease was accepted: acquired=%v err=%v; want rejection before lock", acquired, err)
	}
	if got, readErr := os.ReadFile(canary); readErr != nil || !bytes.Equal(got, []byte(want)) {
		t.Fatalf("foreign canary changed after rejected lease: bytes=%q err=%v", got, readErr)
	}
}

// TestAdoptLeaseLateReadbackReplacementDominatesJoinedCleanup exercises the
// cross-platform late window after the retained handle is closed. A typed
// cleanup cause precedes the readback's SLOT_REPLACED cause in the joined tree;
// replacement classification must still dominate and the foreign bytes survive.
func TestAdoptLeaseLateReadbackReplacementDominatesJoinedCleanup(t *testing.T) {
	_ = isolateStateDir(t)
	const entry = "late-readback-joined-cleanup"
	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("acquire original lease: acquired=%v err=%v", acquired, err)
	}
	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("derive lease path: %v", err)
	}
	const foreign = "cross-platform-late-replacement"
	replacementPath := leasePath + ".late-replacement"
	lateCleanupCause := errors.New("cross-platform-late-cleanup")
	previous := adoptLeaseBeforeFinalReadbackHook
	adoptLeaseBeforeFinalReadbackHook = func() error {
		// Allocate the replacement while the original inode still has its
		// canonical name so POSIX cannot immediately recycle the same inode.
		if err := os.WriteFile(replacementPath, []byte(foreign), 0o600); err != nil {
			return errors.Join(leaseCleanupFailure(lateCleanupCause), err)
		}
		if err := os.Rename(replacementPath, leasePath); err != nil {
			return errors.Join(leaseCleanupFailure(lateCleanupCause), err)
		}
		return leaseCleanupFailure(lateCleanupCause)
	}
	t.Cleanup(func() { adoptLeaseBeforeFinalReadbackHook = previous })

	err = lease.ReleaseAndRemove()
	var leaseFailure *LeaseFailure
	if !errors.As(err, &leaseFailure) || leaseFailure.FailureID != adoptLeaseFailureSlotReplaced || !leaseFailure.RecoveryRequired {
		t.Fatalf("late replacement error=%v, want recovery-required %s", err, adoptLeaseFailureSlotReplaced)
	}
	if !errors.Is(err, lateCleanupCause) {
		t.Fatalf("late replacement lost joined cleanup cause: %v", err)
	}
	if got, readErr := os.ReadFile(leasePath); readErr != nil || string(got) != foreign {
		t.Fatalf("late foreign replacement changed: bytes=%q err=%v", got, readErr)
	}
}

// TestAdoptLeaseReleaseRemoveRace uses three real test processes. B and C are
// both held behind an explicit start barrier while A owns the settlement guard,
// then retried from a second shared barrier after A releases it. This proves the
// guard-busy and manifest-busy outcomes are distinct retryable failures rather
// than a collapsed "busy" result.
func TestAdoptLeaseReleaseRemoveRace(t *testing.T) {
	stateDir := isolateStateDir(t)
	bootstrapSecureAdoptLeaseNamespace(t)
	entry := "three-process-race"
	a := startAdoptLeaseChild(t, stateDir, entry, "A-settle-window")
	waitAdoptLeaseChildLine(t, a.stdout, "acquired")
	if _, err := a.stdin.Write([]byte("settle\n")); err != nil {
		t.Fatalf("start A settlement: %v", err)
	}
	waitAdoptLeaseChildLine(t, a.stdout, "settlement-window")
	b := startAdoptLeaseChild(t, stateDir, entry, "wait-try")
	c := startAdoptLeaseChild(t, stateDir, entry, "wait-try")
	waitAdoptLeaseChildLine(t, b.stdout, "ready")
	waitAdoptLeaseChildLine(t, c.stdout, "ready")
	startAdoptLeaseChildAttempt(t, b)
	startAdoptLeaseChildAttempt(t, c)
	for name, child := range map[string]*adoptLeaseChild{"B": b, "C": c} {
		if got := waitAdoptLeaseChildResult(t, child); got != "failed:E_ADOPT_LEASE_GUARD_BUSY:retry=true" {
			t.Fatalf("%s guard-held result=%q, want exact retryable guard-busy", name, got)
		}
		waitAdoptLeaseChild(t, child)
	}
	if _, err := a.stdin.Write([]byte("continue\n")); err != nil {
		t.Fatalf("complete A settlement: %v", err)
	}
	waitAdoptLeaseChild(t, a)

	b = startAdoptLeaseChild(t, stateDir, entry, "wait-try-hold")
	c = startAdoptLeaseChild(t, stateDir, entry, "wait-try-hold")
	waitAdoptLeaseChildLine(t, b.stdout, "ready")
	waitAdoptLeaseChildLine(t, c.stdout, "ready")
	startAdoptLeaseChildAttempt(t, b)
	startAdoptLeaseChildAttempt(t, c)
	first := map[string]string{"B": waitAdoptLeaseChildResult(t, b), "C": waitAdoptLeaseChildResult(t, c)}
	var owner *adoptLeaseChild
	var retryChild *adoptLeaseChild
	for name, result := range first {
		switch result {
		case "acquired":
			if owner != nil {
				t.Fatalf("two owners admitted after A guard release: results=%v", first)
			}
			if name == "B" {
				owner = b
			} else {
				owner = c
			}
		case "failed:E_ADOPT_LEASE_GUARD_BUSY:retry=true", "failed:E_ADOPT_LEASE_BUSY:retry=true":
			// Concurrent guard acquisition decides which retryable branch the
			// loser reaches; both are explicit typed outcomes, never "busy".
			if name == "B" {
				retryChild = b
			} else {
				retryChild = c
			}
		default:
			t.Fatalf("post-guard result for %s=%q, want acquired or exact manifest-busy", name, result)
		}
	}
	if owner == nil || retryChild == nil {
		t.Fatalf("no owner admitted after A guard release: results=%v", first)
	}
	if _, err := owner.stdin.Write([]byte("release\n")); err != nil {
		t.Fatalf("release post-guard owner: %v", err)
	}
	waitAdoptLeaseChild(t, owner)
	waitAdoptLeaseChild(t, retryChild)
	if out := runAdoptLeaseChild(t, stateDir, entry, "try"); out != "acquired" {
		t.Fatalf("typed post-release retry did not complete its lifecycle: %q", out)
	}
	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("derive lease path: %v", err)
	}
	if runtime.GOOS == "windows" {
		if _, err := os.Lstat(leasePath); !os.IsNotExist(err) {
			t.Fatalf("windows normal race left lease leaf: %v", err)
		}
	} else if _, err := os.Lstat(leasePath); err != nil {
		t.Fatalf("POSIX normal race must retain one canonical lease leaf: %v", err)
	}
}

// TestAdoptLeaseHardCrashOrphanAllowsRetry proves that a hard-crashed owner
// leaves at most an orphaned namespace entry, never a live lock. The next
// acquirer must take the same retained leaf safely and settle it, rather than
// treating a zero-byte path as either success evidence or a reason to delete a
// potentially foreign replacement by pathname.
func TestAdoptLeaseHardCrashOrphanAllowsRetry(t *testing.T) {
	stateDir := isolateStateDir(t)
	bootstrapSecureAdoptLeaseNamespace(t)
	entry := "hard-crash-orphan"
	if out := runAdoptLeaseChild(t, stateDir, entry, "crash"); out != "acquired" {
		t.Fatalf("crash owner result=%q, want acquired", out)
	}

	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("derive crash orphan path: %v", err)
	}
	if _, err := os.Lstat(leasePath); err != nil {
		t.Fatalf("hard crash did not leave its expected orphan leaf: %v", err)
	}

	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("retry acquisition after hard crash: acquired=%v err=%v", acquired, err)
	}
	if err := lease.ReleaseAndRemove(); err != nil {
		t.Fatalf("retry settlement after hard crash: %v", err)
	}
	if runtime.GOOS == "windows" {
		if _, err := os.Lstat(leasePath); !os.IsNotExist(err) {
			t.Fatalf("windows retry left crash orphan leaf behind: %v", err)
		}
	} else if _, err := os.Lstat(leasePath); err != nil {
		t.Fatalf("POSIX retry must retain its canonical lease leaf: %v", err)
	}
}

// TestAdoptLeaseUnlockFailureIsTypedAndReleased proves the lock-only lifecycle
// used by de-adopt, forget, and GC cannot discard a cleanup failure or retain
// the actual OS lock after reporting it.
func TestAdoptLeaseUnlockFailureIsTypedAndReleased(t *testing.T) {
	_ = isolateStateDir(t)
	const entry = "unlock-failure"
	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("acquire: acquired=%v err=%v", acquired, err)
	}
	previous := adoptLeaseUnlockFailureHook
	adoptLeaseUnlockFailureHook = func() error { return errors.New("injected unlock-observation failure") }
	t.Cleanup(func() { adoptLeaseUnlockFailureHook = previous })
	if err := lease.Unlock(); err == nil {
		t.Fatal("Unlock succeeded despite injected owner cleanup failure")
	} else {
		var leaseFailure *LeaseFailure
		if !errors.As(err, &leaseFailure) || leaseFailure.FailureID != adoptLeaseFailureCleanup {
			t.Fatalf("Unlock error=%v, want typed %s", err, adoptLeaseFailureCleanup)
		}
	}
	adoptLeaseUnlockFailureHook = previous
	reacquired, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("owner failure stranded the OS lock: acquired=%v err=%v", acquired, err)
	}
	if err := reacquired.ReleaseAndRemove(); err != nil {
		t.Fatalf("settle reacquired lease: %v", err)
	}
}

func TestAdoptLeaseRaceChild(t *testing.T) {
	role, stateDir, entry := os.Getenv("MCPHUB_A2_ROLE"), os.Getenv("MCPHUB_A2_STATE"), os.Getenv("MCPHUB_A2_ENTRY")
	if role == "" {
		return
	}
	daemonStateRootOverride = stateDir
	if strings.HasPrefix(role, "wait-try") {
		_, _ = os.Stdout.WriteString("ready\n")
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatalf("child wait-try command: %v", err)
		}
	}
	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil {
		var leaseFailure *LeaseFailure
		if errors.As(err, &leaseFailure) && leaseFailure.Retryable {
			_, _ = os.Stdout.WriteString("failed:" + leaseFailure.FailureID + ":retry=true\n")
			return
		}
		t.Fatalf("child acquire: %v", err)
	}
	if !acquired {
		_, _ = os.Stdout.WriteString("failed:acquire=false:retry=false\n")
		return
	}
	_, _ = os.Stdout.WriteString("acquired\n")
	if role == "crash" {
		// Deliberately bypass ReleaseAndRemove: this is the process-crash
		// boundary the parent test needs to exercise. The kernel must release
		// the exact file lock, while the namespace leaf remains for B's retry.
		os.Exit(0)
	}
	if strings.HasSuffix(role, "settle-window") {
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatalf("child start settlement command: %v", err)
		}
		adoptLeaseBeforeSettlementHook = func() error {
			_, _ = os.Stdout.WriteString("settlement-window\n")
			_, err := bufio.NewReader(os.Stdin).ReadString('\n')
			return err
		}
	}
	if strings.HasSuffix(role, "hold") {
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatalf("child release command: %v", err)
		}
	}
	if err := lease.ReleaseAndRemove(); err != nil {
		t.Fatalf("child settlement: %v", err)
	}
}

const adoptLeaseChildTimeout = 10 * time.Second

type adoptLeaseChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr bytes.Buffer
	cancel context.CancelFunc
	role   string
}

func startAdoptLeaseChild(t *testing.T, stateDir, entry, role string) *adoptLeaseChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), adoptLeaseChildTimeout)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAdoptLeaseRaceChild$")
	cmd.Env = append(os.Environ(), "MCPHUB_A2_ROLE="+role, "MCPHUB_A2_STATE="+stateDir, "MCPHUB_A2_ENTRY="+entry)
	child := &adoptLeaseChild{cmd: cmd, cancel: cancel, role: role}
	cmd.Stderr = &child.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin: %v", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start child: %v", err)
	}
	child.stdin = in
	child.stdout = bufio.NewReader(out)
	t.Cleanup(func() {
		child.cancel()
		if child.cmd.Process != nil && child.cmd.ProcessState == nil {
			_ = child.cmd.Process.Kill()
			_, _ = child.cmd.Process.Wait()
		}
	})
	return child
}

func waitAdoptLeaseChildLine(t *testing.T, out *bufio.Reader, want string) {
	t.Helper()
	line, err := out.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != want {
		t.Fatalf("child line=%q err=%v want=%q", line, err, want)
	}
}

func startAdoptLeaseChildAttempt(t *testing.T, child *adoptLeaseChild) {
	t.Helper()
	if _, err := child.stdin.Write([]byte("try\n")); err != nil {
		t.Fatalf("start child %s attempt: %v", child.role, err)
	}
}

func waitAdoptLeaseChildResult(t *testing.T, child *adoptLeaseChild) string {
	t.Helper()
	line, err := child.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("child %s result: %v", child.role, err)
	}
	return strings.TrimSpace(line)
}

func waitAdoptLeaseChild(t *testing.T, child *adoptLeaseChild) {
	t.Helper()
	err := child.cmd.Wait()
	child.cancel()
	if err != nil {
		t.Fatalf("child %s exit: %v; stderr=%q", child.role, err, child.stderr.String())
	}
}

func runAdoptLeaseChild(t *testing.T, stateDir, entry, role string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), adoptLeaseChildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAdoptLeaseRaceChild$")
	cmd.Env = append(os.Environ(), "MCPHUB_A2_ROLE="+role, "MCPHUB_A2_STATE="+stateDir, "MCPHUB_A2_ENTRY="+entry)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("child %s timed out after %s: stdout/stderr=%q", role, adoptLeaseChildTimeout, out)
		}
		t.Fatalf("child %s exit: %v; stdout/stderr=%q", role, err, out)
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// TestAdoptLeaseReleaseRemoveIdentitySwapDoesNotDeleteForeignInode uses the
// owner's deterministic post-acquisition seam. A replacement must survive the
// failed settlement: only the retained original handle may be released.
func TestAdoptLeaseReleaseRemoveIdentitySwapDoesNotDeleteForeignInode(t *testing.T) {
	_ = isolateStateDir(t)
	lease, acquired, err := tryAcquireAdoptManifestLease("swap-lease")
	if err != nil || !acquired {
		t.Fatalf("acquire original lease: acquired=%v err=%v", acquired, err)
	}
	leasePath, err := adoptManifestLeasePath("swap-lease")
	if err != nil {
		t.Fatalf("derive lease path: %v", err)
	}
	const foreign = "replacement-must-survive"
	previous := adoptLeaseBeforeSettlementHook
	adoptLeaseBeforeSettlementHook = func() error {
		if err := os.Remove(leasePath); err != nil {
			return err
		}
		return os.WriteFile(leasePath, []byte(foreign), 0o600)
	}
	t.Cleanup(func() { adoptLeaseBeforeSettlementHook = previous })
	deleteMarkCause := errors.New("delete-mark-after-slot-replacement")
	previousWindowsFailure := adoptLeaseWindowsFailureHook
	deleteMarkCalled := false
	adoptLeaseWindowsFailureHook = func(stage string) error {
		if stage == "delete-mark" {
			deleteMarkCalled = true
			return deleteMarkCause
		}
		return nil
	}
	t.Cleanup(func() { adoptLeaseWindowsFailureHook = previousWindowsFailure })
	if err := lease.ReleaseAndRemove(); err == nil {
		t.Fatal("identity-swapped lease settled successfully; want lease-release failure")
	} else {
		var leaseFailure *LeaseFailure
		if !errors.As(err, &leaseFailure) || leaseFailure.FailureID != adoptLeaseFailureSlotReplaced || !leaseFailure.RecoveryRequired {
			t.Fatalf("identity swap error=%v, want recovery-required %s", err, adoptLeaseFailureSlotReplaced)
		}
		if !errors.Is(err, deleteMarkCause) || !deleteMarkCalled {
			t.Fatalf("identity swap did not report retained-handle delete-on-close cleanup: called=%v err=%v", deleteMarkCalled, err)
		}
	}
	got, err := os.ReadFile(leasePath)
	if err != nil || !bytes.Equal(got, []byte(foreign)) {
		t.Fatalf("foreign replacement was removed or changed: bytes=%q err=%v", got, err)
	}
}

// TestAdoptExecutedEventRequiresLeaseSettlement proves a transaction cannot
// publish the success-shaped event before the owner has settled its lease.
func TestAdoptExecutedEventRequiresLeaseSettlement(t *testing.T) {
	entry := "event-after-lease-settlement"
	_, _, stateDir := setupAdoptTestEnv(t, entry, `[mcp_servers.event-after-lease-settlement]
command = "go"
args = ["version"]
`)
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9364, Clients: []string{"codex-cli"}})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	previous := adoptLeaseBeforeSettlementHook
	const canary = "token-like-secret-and-machine-path-CONTROL-\\x1b[2J"
	adoptLeaseBeforeSettlementHook = func() error { return errors.New(canary) }
	t.Cleanup(func() { adoptLeaseBeforeSettlementHook = previous })
	var localCLI bytes.Buffer
	err = NewAPI().ExecuteAdopt(plan, &localCLI)
	var stage *AdoptStageError
	if !errors.As(err, &stage) || stage.Stage != "lease-release" {
		t.Fatalf("ExecuteAdopt error=%v, want typed lease-release", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("shared lease stage error leaked canary: %q", err)
	}
	if strings.Contains(localCLI.String(), canary) || localCLI.Len() != 0 {
		t.Fatalf("local CLI success narration leaked or preceded failed settlement: %q", localCLI.String())
	}
	logBytes, readErr := os.ReadFile(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read supervisor event log: %v", readErr)
	}
	if readErr == nil && bytes.Contains(logBytes, []byte(`"event":"adopt-executed"`)) {
		t.Fatalf("adopt-executed was emitted before lease settlement: %s", logBytes)
	}
	if bytes.Contains(logBytes, []byte(canary)) {
		t.Fatalf("lease canary leaked into event channel: %s", logBytes)
	}
}
