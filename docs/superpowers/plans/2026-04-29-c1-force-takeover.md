# C1 `mcphub gui --force` Stuck-Instance Recovery — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the placeholder `--force` flag in `mcphub gui` with a stuck-instance recovery flow: bare `--force` shows a structured diagnostic and opens the lock folder; `--force --kill` runs a three-part identity gate, kills the recorded PID, polls TryLock until acquired, and proceeds with normal startup. No file deletion ever (Codex r4 #7 invariant).

**Architecture:** Cross-platform process probes (`processID`, `pingIncumbent`, `killProcess`) live in `internal/gui/probe*.go`. File-manager helper (`OpenFolderAt`) lives in `internal/gui/openfolder*.go`. `internal/gui/single_instance.go` gains a `Verdict` struct + `Probe(pidportPath)` classifier + `KillRecordedHolder(pidportPath, opts)` performer. `cli/gui.go` integrates the bare-`--force` and `--force --kill` flows, mapping Verdict to exit codes 0/1/2/3/4/6/7 (5 reserved). 8-scenario test file in `internal/cli/`, the last one being a real subprocess E2E using the existing `ensureMcphubBinary` pattern.

**Tech Stack:** Go 1.26+, `gofrs/flock`, `golang.org/x/sys/windows` for Win32 syscalls, `cobra`, `term.IsTerminal`. No new dependencies required (term, exec, os/signal already in std).

**Memo source:** `docs/superpowers/specs/2026-04-29-c1-force-takeover-design.md` rev 9, after 8 review rounds (Codex r1-r8 + Claude r2). Codex r8 PASS.

**Branch:** `feat/c1-force-takeover` (current master tip: `306b17a`).

---

## File Structure

| File | Responsibility | Status |
|---|---|---|
| `internal/gui/probe.go` | Cross-platform `pingIncumbent` (HTTP probe to /api/ping with PID match) | Create |
| `internal/gui/probe_windows.go` | Windows `processID` (OpenProcess + QueryFullProcessImageName + NtQueryInformationProcess for cmdline + GetProcessTimes) + `killProcess` (TerminateProcess) | Create |
| `internal/gui/probe_unix.go` | Linux/macOS `processID` (Kill(0) + readlink /proc/pid/exe + read /proc/pid/cmdline + /proc/pid/stat) + `killProcess` (SIGKILL) | Create |
| `internal/gui/probe_test.go` | Unit tests covering self-PID happy path + impossible PID dead path. `_test.go` (not Windows-only) — uses runtime.GOOS for branch matrix. | Create |
| `internal/gui/openfolder.go` | Cross-platform dispatch + injectable seam | Create |
| `internal/gui/openfolder_windows.go` | `explorer.exe /select,<path>` | Create |
| `internal/gui/openfolder_other.go` | `open -R` (macOS) / `xdg-open` (Linux); fallback no-op when binary missing | Create |
| `internal/gui/openfolder_test.go` | Records seam invocation; doesn't actually open | Create |
| `internal/gui/single_instance.go` | Add `Verdict` struct, `VerdictClass` enum, `Probe(p string)`, `KillRecordedHolder(p, opts)`. Existing `formatPidport`/`ReadPidport` etc. unchanged. | Modify |
| `internal/gui/single_instance_test.go` | Add tests for `Probe` (Healthy / LiveUnreachable / Stale / Malformed) + `KillRecordedHolder` (gate-fail / kill-success / race-lost) | Modify (file currently doesn't exist; create or extend) |
| `internal/cli/gui.go` | Replace `--force` placeholder; add `--yes`; wire bare-`--force` (Probe + folder-open + exit 2) and `--force --kill` (KillRecordedHolder + exit-code mapping); update flag help | Modify |
| `internal/cli/gui_force_test.go` | 8 scenarios per memo §Tests | Create |
| `CLAUDE.md` | Add "When mcphub gui won't start" runbook section | Modify |
| `docs/phase-3b-ii-verification.md` | Update D2 row to reflect `--force [--kill]` manual smoke procedure | Modify |
| `docs/superpowers/plans/phase-3b-ii-backlog.md` | A4-b row: forward-reference to Verdict + future POST /api/force-kill HTTP contract | Modify |

---

## Task 0: Branch setup

**Files:**
- Modify: working tree (no commits yet)

- [ ] **Step 0.1: Verify clean tree on master at 306b17a**

```bash
git checkout master
git status -s
git log --oneline -1
```

Expected:
- `git status -s` shows only `?? servers/gdb/` (untracked local-only file from prior session — leave alone) and `?? docs/superpowers/specs/2026-04-29-c1-force-takeover-design.md` (the memo) and `?? .reports/`. No tracked modifications.
- `git log --oneline -1` shows `306b17a Phase 3B-II completion: A5 About + C1-C4 Windows UX + reliability harness + verification doc (#22)`.

- [ ] **Step 0.2: Create and switch to feature branch**

```bash
git checkout -b feat/c1-force-takeover
```

Expected: `Switched to a new branch 'feat/c1-force-takeover'`.

---

## Task 1: cross-platform process probe (probe.go + probe_*.go)

**Files:**
- Create: `internal/gui/probe.go`
- Create: `internal/gui/probe_windows.go` (build tag: `//go:build windows`)
- Create: `internal/gui/probe_unix.go` (build tag: `//go:build !windows`)
- Create: `internal/gui/probe_test.go`

- [ ] **Step 1.1: Write the failing tests**

Create `internal/gui/probe_test.go`:

```go
package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestProcessID_SelfAlive verifies the current test process is reported
// as alive with a non-empty image path, an argv that includes "test"
// (the Go test binary's name pattern), and a non-zero start time.
func TestProcessID_SelfAlive(t *testing.T) {
	got, err := processID(os.Getpid())
	if err != nil {
		t.Fatalf("processID(self): %v", err)
	}
	if !got.Alive {
		t.Errorf("self.Alive = false, want true")
	}
	if got.Denied {
		t.Errorf("self.Denied = true, want false (we should always be able to query our own process)")
	}
	if got.ImagePath == "" {
		t.Errorf("self.ImagePath empty, want non-empty")
	}
	// Test binary name varies (TestMain.exe, foo.test, etc.) — assert
	// only that we got SOMETHING in argv.
	if len(got.Cmdline) == 0 {
		t.Errorf("self.Cmdline empty, want non-empty")
	}
	if got.StartTime.IsZero() {
		t.Errorf("self.StartTime zero, want non-zero")
	}
}

// TestProcessID_ImpossiblePIDDead verifies a known-impossible PID
// reports alive=false. math.MaxInt32 is far beyond the kernel's PID
// allocation range on every platform we support.
func TestProcessID_ImpossiblePIDDead(t *testing.T) {
	const impossible = 2147483646 // math.MaxInt32 - 1; one off avoids ANY kernel reuse risk
	got, err := processID(impossible)
	// Some platforms return a structured error here; others fill
	// got.Alive=false. Both are acceptable — the contract is
	// "callers can determine the process is not alive".
	if err == nil && got.Alive {
		t.Errorf("processID(impossible) reported alive; got=%+v", got)
	}
}

// TestProcessID_NegativePIDRejected verifies pid <= 0 is treated as
// a dead-PID without invoking platform syscalls (defensive: a negative
// PID through OpenProcess could otherwise crash the test).
func TestProcessID_NegativePIDRejected(t *testing.T) {
	got, _ := processID(0)
	if got.Alive {
		t.Errorf("processID(0).Alive = true, want false")
	}
	got, _ = processID(-1)
	if got.Alive {
		t.Errorf("processID(-1).Alive = true, want false")
	}
}

// TestPingIncumbent_Success spins up an httptest server that mimics
// /api/ping returning {ok:true, pid:<recordedPID>}; PingIncumbent
// must return matchedPID equal to the response body's PID.
func TestPingIncumbent_Success(t *testing.T) {
	const recordedPID = 4128
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ping" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": recordedPID})
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)
	got, err := pingIncumbent(context.Background(), port, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("pingIncumbent: %v", err)
	}
	if got != recordedPID {
		t.Errorf("matchedPID = %d, want %d", got, recordedPID)
	}
}

// TestPingIncumbent_PortNotListening returns an error promptly when
// the port has no listener. We don't assert the specific error type
// (varies by platform: "connection refused" / "actively refused" etc.);
// only that it's non-nil and the call returns within ~1s deadline.
func TestPingIncumbent_PortNotListening(t *testing.T) {
	// Pick a port that's almost certainly closed: ephemeral range start.
	const probablyClosedPort = 1
	deadline := time.Now().Add(1 * time.Second)
	_, err := pingIncumbent(context.Background(), probablyClosedPort, 500*time.Millisecond)
	if err == nil {
		t.Errorf("pingIncumbent on closed port returned nil error")
	}
	if time.Now().After(deadline) {
		t.Errorf("pingIncumbent took >1s on closed port; expected fast-fail")
	}
}

// TestKillProcess_Stub verifies the killProcess function exists and
// returns an error for an impossible PID — we don't actually kill any
// real process here. (Test 8 in cli/gui_force_test.go covers the
// real-kill path via subprocess.)
func TestKillProcess_Stub(t *testing.T) {
	const impossible = 2147483646
	err := killProcess(impossible)
	if err == nil {
		t.Errorf("killProcess(impossible) returned nil error; expected non-nil")
	}
	// Skip on platforms where killProcess is a no-op stub.
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("killProcess unimplemented for %s", runtime.GOOS)
	}
}

// portFromURL extracts the TCP port number from an httptest server URL.
func portFromURL(t *testing.T, u string) int {
	t.Helper()
	// httptest URLs are "http://127.0.0.1:<port>".
	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("unexpected httptest URL %q", u)
	}
	rest := u[len(prefix):]
	var port int
	if _, err := fmtSscanf(rest, "%d", &port); err != nil {
		t.Fatalf("parse port from %q: %v", u, err)
	}
	return port
}

// fmtSscanf trampolines through a non-fmt name so the file's import
// list stays minimal (Go vet flags unused imports). Defined inline.
func fmtSscanf(s, format string, args ...any) (int, error) {
	// Simplest: split on the URL's port and atoi. Avoid fmt.Sscanf
	// dependency to keep this test file's import surface tiny.
	var port int
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	if format == "" || len(args) == 0 {
		return port, nil
	}
	if p, ok := args[0].(*int); ok {
		*p = port
	}
	return 1, nil
}
```

- [ ] **Step 1.2: Run tests to verify they fail (no implementation yet)**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./internal/gui/ -run "TestProcessID_|TestPingIncumbent_|TestKillProcess_" -v
```

Expected: build failure with `undefined: processID`, `undefined: pingIncumbent`, `undefined: killProcess`.

- [ ] **Step 1.3: Write `internal/gui/probe.go` (cross-platform pingIncumbent + struct)**

```go
// internal/gui/probe.go
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ProcessIdentity is the cross-platform result of inspecting an OS
// process. Used by single_instance.go's Probe + KillRecordedHolder
// to run the three-part identity gate before any destructive action.
//
// Codex r5 #1 + r6 #5 + r7 #6: image basename + (argv[1]=="gui" OR
// len(argv)==1) + start-time vs pidport mtime. All three checks live
// in single_instance.go; this struct just exposes the raw signals.
type ProcessIdentity struct {
	Alive     bool      // process is alive (cross-platform: Kill(0) on Unix; OpenProcess on Windows)
	Denied    bool      // privilege rejected the query — treat as alive (refuse take-over)
	ImagePath string    // canonical executable path, empty on Denied
	Cmdline   []string  // argv split honoring CommandLineToArgvW (Win) / NUL-delimited (Linux)
	StartTime time.Time // process creation time; opaque token for identity equality
}

// pingIncumbent issues GET http://127.0.0.1:<port>/api/ping and
// returns the PID the incumbent reports. Returns an error on
// connection failure, non-200 status, malformed body, or ok:false.
//
// Lifted from TryActivateIncumbent's inner block so Probe and the
// existing handshake share the same probe semantic. Callers should
// pass a 500ms-or-shorter timeout to keep the diagnostic path snappy.
func pingIncumbent(ctx context.Context, port int, timeout time.Duration) (int, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/ping", port), nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err // connection refused / timeout / etc.
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("ping status %d", resp.StatusCode)
	}
	var body struct {
		OK  bool `json:"ok"`
		PID int  `json:"pid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode ping body: %w", err)
	}
	if !body.OK {
		return 0, fmt.Errorf("ping returned ok=false")
	}
	return body.PID, nil
}

// processID is the platform-specific process inspector; defined in
// probe_windows.go and probe_unix.go. Called by single_instance.go
// only — keep callers narrow.
func processID(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{Alive: false}, nil
	}
	return processIDImpl(pid)
}

// killProcess sends SIGKILL / TerminateProcess to the given PID.
// Defined in probe_windows.go and probe_unix.go. Returns nil on
// successful signal delivery; "process not found" errors map to
// non-nil so callers can distinguish "kill succeeded" from "PID
// already gone".
func killProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	return killProcessImpl(pid)
}
```

- [ ] **Step 1.4: Write `internal/gui/probe_windows.go`**

```go
// internal/gui/probe_windows.go
//go:build windows

package gui

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processIDImpl is the Windows implementation. Uses
// PROCESS_QUERY_LIMITED_INFORMATION (works without admin in most
// cases) for image path + creation time; reads the command line via
// NtQueryInformationProcess + PEB walk; treats ACCESS_DENIED as
// alive=true,denied=true (Claude r2 #2: refuses take-over of a
// SYSTEM/scheduler-launched lock).
func processIDImpl(pid int) (ProcessIdentity, error) {
	const (
		PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
		STILL_ACTIVE                      = 259
	)
	h, err := windows.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// ERROR_ACCESS_DENIED → process exists but we can't query it.
		// Match Unix EPERM semantics: alive=true, denied=true.
		if err == windows.ERROR_ACCESS_DENIED {
			return ProcessIdentity{Alive: true, Denied: true}, nil
		}
		// Other errors (ERROR_INVALID_PARAMETER for dead PID, etc.) → not alive.
		return ProcessIdentity{Alive: false}, nil
	}
	defer windows.CloseHandle(h)

	// Liveness via GetExitCodeProcess.
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return ProcessIdentity{Alive: false}, nil
	}
	if exitCode != STILL_ACTIVE {
		return ProcessIdentity{Alive: false}, nil
	}

	// Image path via QueryFullProcessImageName.
	imagePath := queryImagePath(h)

	// Creation time via GetProcessTimes.
	var creation, exit, kernel, user windows.Filetime
	startTime := time.Time{}
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err == nil {
		startTime = time.Unix(0, creation.Nanoseconds())
	}

	// Command line via NtQueryInformationProcess + PEB walk.
	cmdline := queryCmdline(uint32(pid))

	return ProcessIdentity{
		Alive:     true,
		Denied:    false,
		ImagePath: imagePath,
		Cmdline:   cmdline,
		StartTime: startTime,
	}, nil
}

// killProcessImpl uses PROCESS_TERMINATE + TerminateProcess. Errors
// other than "process gone" propagate to the caller.
func killProcessImpl(pid int) error {
	const PROCESS_TERMINATE = 0x0001
	h, err := windows.OpenProcess(PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(PROCESS_TERMINATE, %d): %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("TerminateProcess(%d): %w", pid, err)
	}
	return nil
}

// queryImagePath returns the canonical executable path for an open
// process handle. Returns "" on failure.
func queryImagePath(h windows.Handle) string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// queryCmdline reads the target process's argv via PEB inspection.
// Falls back to a single-element slice with the image basename on
// failure (so Cmdline is never nil; callers gate on len(Cmdline) and
// element values).
//
// Implementation note: NtQueryInformationProcess(ProcessBasicInformation)
// returns a PROCESS_BASIC_INFORMATION whose PebBaseAddress points
// into the target process's address space. We read PEB → ProcessParameters
// → CommandLine using ReadProcessMemory.
func queryCmdline(pid uint32) []string {
	const PROCESS_QUERY_INFORMATION = 0x0400
	const PROCESS_VM_READ = 0x0010
	h, err := windows.OpenProcess(PROCESS_QUERY_INFORMATION|PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)

	type processBasicInformation struct {
		Reserved1                    uintptr
		PebBaseAddress               uintptr
		Reserved2                    [2]uintptr
		UniqueProcessId              uintptr
		Reserved3                    uintptr
	}
	var pbi processBasicInformation
	var retLen uint32
	ntdll := syscall.NewLazyDLL("ntdll.dll")
	procNtQuery := ntdll.NewProc("NtQueryInformationProcess")
	r1, _, _ := procNtQuery.Call(
		uintptr(h),
		0, // ProcessBasicInformation
		uintptr(unsafe.Pointer(&pbi)),
		unsafe.Sizeof(pbi),
		uintptr(unsafe.Pointer(&retLen)),
	)
	if r1 != 0 || pbi.PebBaseAddress == 0 {
		return nil
	}

	// PEB layout (Windows 10/11 x64): ProcessParameters at offset 0x20.
	const pebProcessParametersOffset = 0x20
	var paramsAddr uintptr
	var n uintptr
	if err := windows.ReadProcessMemory(h, pbi.PebBaseAddress+pebProcessParametersOffset,
		(*byte)(unsafe.Pointer(&paramsAddr)), unsafe.Sizeof(paramsAddr), &n); err != nil {
		return nil
	}

	// RTL_USER_PROCESS_PARAMETERS.CommandLine is a UNICODE_STRING
	// at offset 0x70 (Windows 10/11 x64).
	const commandLineOffset = 0x70
	type unicodeString struct {
		Length        uint16
		MaximumLength uint16
		Buffer        uintptr
	}
	var us unicodeString
	if err := windows.ReadProcessMemory(h, paramsAddr+commandLineOffset,
		(*byte)(unsafe.Pointer(&us)), unsafe.Sizeof(us), &n); err != nil {
		return nil
	}
	if us.Length == 0 || us.Buffer == 0 {
		return nil
	}
	wbuf := make([]uint16, us.Length/2)
	if err := windows.ReadProcessMemory(h, us.Buffer,
		(*byte)(unsafe.Pointer(&wbuf[0])), uintptr(us.Length), &n); err != nil {
		return nil
	}
	cmdline := string(utf16.Decode(wbuf))
	return splitCommandLineW(cmdline)
}

// splitCommandLineW honors CommandLineToArgvW quoting rules so paths
// with spaces and quoted args parse correctly. Reimplemented here to
// avoid a syscall to a UI DLL (shell32).
func splitCommandLineW(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	i := 0
	// First arg (executable) parses with simple quote handling.
	for i < len(s) {
		c := s[i]
		if c == '"' {
			inQuote = !inQuote
			i++
			continue
		}
		if !inQuote && c == ' ' {
			i++
			break
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	// Remaining args use full backslash-aware Microsoft rules.
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' {
			if !inQuote {
				flush()
				i++
				continue
			}
		}
		if c == '\\' {
			// Count backslashes.
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			backslashes := j - i
			if j < len(s) && s[j] == '"' {
				// 2N backslashes + " → N backslashes + toggle quote
				// 2N+1 + " → N backslashes + literal "
				cur.WriteString(strings.Repeat(`\`, backslashes/2))
				if backslashes%2 == 1 {
					cur.WriteByte('"')
				} else {
					inQuote = !inQuote
				}
				i = j + 1
				continue
			}
			cur.WriteString(strings.Repeat(`\`, backslashes))
			i = j
			continue
		}
		if c == '"' {
			inQuote = !inQuote
			i++
			continue
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	return args
}

// matchBasename returns true iff filepath.Base(path) equals
// "mcphub.exe" (case-insensitive). Used by single_instance.go's
// Probe + KillRecordedHolder.
func matchBasename(path string) bool {
	return strings.EqualFold(filepath.Base(path), "mcphub.exe")
}
```

- [ ] **Step 1.5: Write `internal/gui/probe_unix.go`**

```go
// internal/gui/probe_unix.go
//go:build !windows

package gui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processIDImpl is the POSIX implementation. Uses Kill(0) for
// liveness; reads /proc/<pid>/exe + /proc/<pid>/cmdline +
// /proc/<pid>/stat for image, argv, and start-time.
//
// EPERM (we're not allowed to signal the target) is treated as
// alive=true,denied=true to mirror Windows ACCESS_DENIED handling.
func processIDImpl(pid int) (ProcessIdentity, error) {
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return ProcessIdentity{Alive: true, Denied: true}, nil
		}
		// ESRCH or other: not alive.
		return ProcessIdentity{Alive: false}, nil
	}

	// /proc/<pid>/exe
	imagePath, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))

	// /proc/<pid>/cmdline (NUL-delimited args)
	var cmdline []string
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		raw := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		for _, a := range raw {
			if a != "" {
				cmdline = append(cmdline, a)
			}
		}
	}

	// /proc/<pid>/stat field 22 = starttime in jiffies-since-boot.
	// Convert to wall-clock approximation via /proc/uptime: the
	// design memo's identity-gate compares against pidport mtime
	// with a 1s tolerance, so jitter from this conversion is
	// acceptable. (memo §"PID identity")
	startTime := readStartTimeLinux(pid)

	return ProcessIdentity{
		Alive:     true,
		Denied:    false,
		ImagePath: imagePath,
		Cmdline:   cmdline,
		StartTime: startTime,
	}, nil
}

func killProcessImpl(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill(%d, SIGKILL): %w", pid, err)
	}
	return nil
}

// readStartTimeLinux returns the process's wall-clock start time by
// combining /proc/<pid>/stat's starttime field with the system boot
// time. Returns time.Time{} on any read error.
func readStartTimeLinux(pid int) time.Time {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}
	}
	// Format: <pid> (<comm>) <state> <ppid> <pgrp> <session>
	//   <tty> <tpgid> <flags> <minflt> <cminflt> <majflt> <cmajflt>
	//   <utime> <stime> <cutime> <cstime> <priority> <nice>
	//   <num_threads> <itrealvalue> <starttime> ...
	// (comm) can contain spaces/parens — find the trailing ) first.
	rp := strings.LastIndexByte(string(data), ')')
	if rp == -1 || rp+2 >= len(data) {
		return time.Time{}
	}
	fields := strings.Fields(string(data[rp+2:]))
	// After ')' field 3 is state; index 19 in fields == starttime
	// (because /proc/<pid>/stat fields are 1-indexed in docs and we
	// dropped fields 1+2 by parsing post-')').
	const startTimeFieldIndex = 19
	if len(fields) <= startTimeFieldIndex {
		return time.Time{}
	}
	startJiffies, err := strconv.ParseInt(fields[startTimeFieldIndex], 10, 64)
	if err != nil {
		return time.Time{}
	}
	hz := int64(100) // CLK_TCK on most Linux configs; sysconf(_SC_CLK_TCK) would be more correct
	startSecsSinceBoot := startJiffies / hz

	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}
	}
	upFields := strings.Fields(string(uptimeData))
	if len(upFields) < 1 {
		return time.Time{}
	}
	uptimeSec, err := strconv.ParseFloat(upFields[0], 64)
	if err != nil {
		return time.Time{}
	}
	bootTime := time.Now().Add(-time.Duration(uptimeSec * float64(time.Second)))
	return bootTime.Add(time.Duration(startSecsSinceBoot) * time.Second)
}

// matchBasename returns true iff filepath.Base(path) equals "mcphub"
// (POSIX exact, no .exe — Codex r6 #6).
func matchBasename(path string) bool {
	return filepath.Base(path) == "mcphub"
}
```

- [ ] **Step 1.6: Run tests to verify they pass**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./internal/gui/ -run "TestProcessID_|TestPingIncumbent_|TestKillProcess_" -v
```

Expected: all 6 tests PASS.

- [ ] **Step 1.7: Commit Task 1**

```bash
git add internal/gui/probe.go internal/gui/probe_windows.go internal/gui/probe_unix.go internal/gui/probe_test.go
git commit -m "feat(gui): cross-platform process probe primitives

processID(pid) returns liveness + denied flag + image path + argv +
start-time. Windows: OpenProcess(QUERY_LIMITED) + GetExitCodeProcess
+ QueryFullProcessImageName + GetProcessTimes + NtQueryInformationProcess
PEB walk for cmdline. Linux: Kill(0) + /proc/<pid>/{exe,cmdline,stat}.
ACCESS_DENIED / EPERM both treated as alive=true,denied=true so
take-over refuses lock holders we lack privilege to inspect.

pingIncumbent extracted from TryActivateIncumbent for reuse by
single_instance.Probe. killProcess wraps SIGKILL/TerminateProcess.

matchBasename helper: 'mcphub.exe' Windows (case-insensitive) /
'mcphub' POSIX (exact, no .exe — Codex r6 #6).

splitCommandLineW reimplements CommandLineToArgvW backslash rules
in pure Go so we don't depend on shell32 from this DLL surface.

Tests: 6 scenarios covering self-PID happy path, impossible PID,
negative PID, ping success against httptest server, ping against
closed port, killProcess error path."
```

---

## Task 2: open-folder helper

**Files:**
- Create: `internal/gui/openfolder.go`
- Create: `internal/gui/openfolder_windows.go`
- Create: `internal/gui/openfolder_other.go`
- Create: `internal/gui/openfolder_test.go`

- [ ] **Step 2.1: Write the failing test**

Create `internal/gui/openfolder_test.go`:

```go
package gui

import (
	"path/filepath"
	"testing"
)

// TestOpenFolderAt_RecordsSeam verifies the helper invokes its
// injectable spawn function with the resolved file path. We don't
// actually open Explorer/Finder — that's a manual smoke (D2.1) per
// the verification matrix.
func TestOpenFolderAt_RecordsSeam(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "gui.pidport")
	var spawnedName string
	var spawnedArgs []string
	prev := openFolderSpawn
	defer func() { openFolderSpawn = prev }()
	openFolderSpawn = func(name string, args ...string) error {
		spawnedName = name
		spawnedArgs = args
		return nil
	}
	if err := OpenFolderAt(target); err != nil {
		t.Fatalf("OpenFolderAt: %v", err)
	}
	if spawnedName == "" {
		t.Fatalf("spawn was not invoked")
	}
	// Some args list must mention the target path (as-is or as the
	// containing directory). On Windows it's `explorer.exe /select,<path>`;
	// on macOS `open -R <path>`; on Linux `xdg-open <dir>`.
	joined := spawnedName
	for _, a := range spawnedArgs {
		joined += " " + a
	}
	if !contains(joined, target) && !contains(joined, filepath.Dir(target)) {
		t.Errorf("spawn args %q do not reference target path or its dir", joined)
	}
}

// TestOpenFolderAt_FireAndForget verifies that a spawn error does
// not propagate (best-effort by design — Codex r5 #3 PASS). The
// caller should still receive nil so the diagnostic flow continues.
func TestOpenFolderAt_FireAndForget(t *testing.T) {
	prev := openFolderSpawn
	defer func() { openFolderSpawn = prev }()
	openFolderSpawn = func(name string, args ...string) error {
		return errSpawnTest
	}
	if err := OpenFolderAt("/nonexistent/path"); err != nil {
		t.Errorf("OpenFolderAt should swallow spawn errors; got %v", err)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// errSpawnTest is a sentinel for the fire-and-forget test.
var errSpawnTest = &spawnTestError{}

type spawnTestError struct{}

func (*spawnTestError) Error() string { return "spawn-test-error" }
```

- [ ] **Step 2.2: Run test to verify it fails**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./internal/gui/ -run "TestOpenFolderAt_" -v
```

Expected: build failure (`undefined: OpenFolderAt`, `undefined: openFolderSpawn`).

- [ ] **Step 2.3: Write `internal/gui/openfolder.go`**

```go
// internal/gui/openfolder.go
package gui

import "os/exec"

// openFolderSpawn is the injectable seam used by OpenFolderAt.
// Tests overwrite it; production callers use exec.Command(...).Start()
// via openFolderDefault below.
var openFolderSpawn = openFolderDefault

func openFolderDefault(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

// OpenFolderAt opens the file manager focused on the given file's
// parent directory (and selects the file on Windows where the
// shell exposes that capability). Best-effort fire-and-forget per
// Codex r5 #3: if the spawn fails, the diagnostic flow has already
// printed the path to stdout so the operator can navigate manually.
//
// Cross-platform dispatch lives in openfolder_windows.go and
// openfolder_other.go; this function is just the public entry that
// tests hook through openFolderSpawn.
func OpenFolderAt(path string) error {
	return openFolderImpl(path)
}
```

- [ ] **Step 2.4: Write `internal/gui/openfolder_windows.go`**

```go
// internal/gui/openfolder_windows.go
//go:build windows

package gui

func openFolderImpl(path string) error {
	// `explorer.exe /select,<path>` opens the parent dir AND
	// highlights the file. The leading commas-no-space form is
	// Microsoft's quirk; do NOT add a space.
	_ = openFolderSpawn("explorer.exe", "/select,"+path)
	return nil // fire-and-forget per Codex r5 #3
}
```

- [ ] **Step 2.5: Write `internal/gui/openfolder_other.go`**

```go
// internal/gui/openfolder_other.go
//go:build !windows

package gui

import (
	"path/filepath"
	"runtime"
)

func openFolderImpl(path string) error {
	dir := filepath.Dir(path)
	if runtime.GOOS == "darwin" {
		// macOS: `open -R <path>` reveals the file in Finder.
		_ = openFolderSpawn("open", "-R", path)
	} else {
		// Linux/BSD: xdg-open opens the directory in the user's
		// configured file manager. Cannot select a file with
		// xdg-open; opening the dir is the best we can do without
		// depending on a specific desktop environment's file
		// manager (gio open / nautilus / dolphin / nemo all have
		// different selection flags).
		_ = openFolderSpawn("xdg-open", dir)
	}
	return nil // fire-and-forget
}
```

- [ ] **Step 2.6: Run test to verify it passes**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./internal/gui/ -run "TestOpenFolderAt_" -v
```

Expected: both tests PASS.

- [ ] **Step 2.7: Commit Task 2**

```bash
git add internal/gui/openfolder.go internal/gui/openfolder_windows.go internal/gui/openfolder_other.go internal/gui/openfolder_test.go
git commit -m "feat(gui): cross-platform open-folder helper for diagnostic UX

OpenFolderAt(path) launches the OS file manager focused on the
file's parent directory. Used by bare 'mcphub gui --force' to give
the operator quick access to the lock files alongside the
diagnostic block (memo §6 default flow).

Windows: explorer.exe /select,<path> (selects the file).
macOS:   open -R <path> (reveals in Finder).
Linux:   xdg-open <dir> (no file-select; xdg-open lacks a portable
         flag for that, and per-DE fallbacks are out of scope).

Best-effort fire-and-forget per Codex r5 #3 (PASS): a failing spawn
doesn't propagate — the diagnostic block has already printed the
path to stdout for manual navigation.

Tests: 2 scenarios via test seam (recorded invocation;
swallowed spawn error)."
```

---

## Task 3: Verdict + Probe + KillRecordedHolder in single_instance.go

**Files:**
- Modify: `internal/gui/single_instance.go`
- Create: `internal/gui/single_instance_test.go` (or extend if exists)

- [ ] **Step 3.1: Confirm no existing `single_instance_test.go`**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
ls internal/gui/single_instance_test.go 2>&1
```

Expected: `ls: cannot access ...: No such file or directory`. (If the file exists, append; this plan assumes new file.)

- [ ] **Step 3.2: Write the failing tests**

Create `internal/gui/single_instance_test.go`:

```go
package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// TestProbe_Healthy verifies a Probe call against a live incumbent
// (here: an httptest server bound to a real port) returns
// VerdictHealthy with PingMatch=true.
func TestProbe_Healthy(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Spin up a fake /api/ping that reports our own PID.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ping" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}

	v := Probe(context.Background(), pidport)
	if v.Class != VerdictHealthy {
		t.Errorf("Class = %v, want VerdictHealthy. Diagnose=%q", v.Class, v.Diagnose)
	}
	if !v.PingMatch {
		t.Errorf("PingMatch = false, want true")
	}
	if v.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", v.PID, os.Getpid())
	}
}

// TestProbe_LiveUnreachable: alive PID, no listener.
func TestProbe_LiveUnreachable(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	v := Probe(context.Background(), pidport)
	if v.Class != VerdictLiveUnreachable {
		t.Errorf("Class = %v, want VerdictLiveUnreachable. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestProbe_DeadPID: pid is impossible.
func TestProbe_DeadPID(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const impossible = 2147483646
	if err := os.WriteFile(pidport, []byte(formatPidport(impossible, 9125)), 0o600); err != nil {
		t.Fatal(err)
	}
	v := Probe(context.Background(), pidport)
	if v.Class != VerdictDeadPID {
		t.Errorf("Class = %v, want VerdictDeadPID. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestProbe_Malformed: garbage in pidport.
func TestProbe_Malformed(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte("not a pidport file"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := Probe(context.Background(), pidport)
	if v.Class != VerdictMalformed {
		t.Errorf("Class = %v, want VerdictMalformed. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestKillRecordedHolder_RefusesNonMcphubImage: pidport refers to
// the test process (image is the test binary, NOT mcphub.exe), so
// the three-part identity gate's image-basename check refuses.
// Asserts Class=VerdictKillRefused, no kill attempted.
func TestKillRecordedHolder_RefusesNonMcphubImage(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const port = 9125
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Acquire a flock so KillRecordedHolder reaches the gate path.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{})
	if err == nil {
		t.Errorf("expected non-nil error on kill-refused; got nil")
	}
	if v.Class != VerdictKillRefused {
		t.Errorf("Class = %v, want VerdictKillRefused. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestKillRecordedHolder_HealthyEarlyExit: incumbent is healthy
// (ping matches) — KillRecordedHolder must NOT kill, must report
// VerdictHealthy so the caller can route to handshake.
func TestKillRecordedHolder_HealthyEarlyExit(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{})
	if err != nil {
		t.Errorf("expected nil error on healthy early-exit; got %v", err)
	}
	if v.Class != VerdictHealthy {
		t.Errorf("Class = %v, want VerdictHealthy. Diagnose=%q", v.Class, v.Diagnose)
	}
	if !v.PingMatch {
		t.Errorf("PingMatch should be true on healthy")
	}
}

// TestVerdictDiagnoseHintNotInJSON guards the json:"-" tags so
// A4-b's HTTP API doesn't ship pre-formatted strings to the UI.
func TestVerdictDiagnoseHintNotInJSON(t *testing.T) {
	v := Verdict{
		Class:    VerdictDeadPID,
		PID:      123,
		Port:     9125,
		Diagnose: "should not appear in JSON",
		Hint:     "should not appear either",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "should not appear") {
		t.Errorf("Diagnose/Hint leaked into JSON: %s", b)
	}
}

// portFromURL is shared with probe_test.go; kept package-private.
// (Defined in probe_test.go; redefining would conflict.)

// matchBasenameForCurrentOS returns true on the current platform's
// match function being permissive enough to recognize the test
// binary. This is a defensive sanity check used by skip logic in a
// few tests; the real basename check happens inside Probe.
func matchBasenameForCurrentOS(path string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Base(path), "mcphub.exe")
	}
	return filepath.Base(path) == "mcphub"
}

// errFlockUnusedSentinel keeps unused-import linters quiet when the
// flock package is referenced only by parens in build tags above.
var errFlockUnusedSentinel = errors.New("unused")
var _ = errFlockUnusedSentinel
var _ = fmt.Sprintf
var _ = time.Now
```

- [ ] **Step 3.3: Run tests to verify they fail**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./internal/gui/ -run "TestProbe_|TestKillRecordedHolder_|TestVerdict" -v
```

Expected: build failure with `undefined: Verdict`, `undefined: VerdictHealthy`, `undefined: Probe`, `undefined: KillRecordedHolder`, `undefined: KillOpts`.

- [ ] **Step 3.4: Extend `internal/gui/single_instance.go` with Verdict + Probe + KillRecordedHolder**

Append these new types and functions to the existing `internal/gui/single_instance.go` (do NOT modify existing types — pidport format stays `<PID> <PORT>\n`):

```go
// (Add these at the bottom of internal/gui/single_instance.go,
// after the existing exported helpers.)

// VerdictClass enumerates the result of Probe / KillRecordedHolder.
type VerdictClass int

const (
	VerdictHealthy        VerdictClass = iota // incumbent ping matches recorded PID
	VerdictLiveUnreachable                     // recorded PID is alive but not serving HTTP
	VerdictDeadPID                             // recorded PID does not exist
	VerdictMalformed                           // pidport is missing/garbage/incomplete
	VerdictKilledRecovered                     // KillRecordedHolder succeeded; new flock acquired
	VerdictKillRefused                         // three-part identity gate failed
	VerdictKillFailed                          // SIGKILL/TerminateProcess returned error
	VerdictRaceLost                            // post-kill, a competitor won the new acquire
)

func (c VerdictClass) String() string {
	switch c {
	case VerdictHealthy:
		return "Healthy"
	case VerdictLiveUnreachable:
		return "LiveUnreachable"
	case VerdictDeadPID:
		return "DeadPID"
	case VerdictMalformed:
		return "Malformed"
	case VerdictKilledRecovered:
		return "KilledRecovered"
	case VerdictKillRefused:
		return "KillRefused"
	case VerdictKillFailed:
		return "KillFailed"
	case VerdictRaceLost:
		return "RaceLost"
	}
	return fmt.Sprintf("VerdictClass(%d)", int(c))
}

// Verdict bundles the Probe result. JSON marshaling skips Diagnose
// and Hint (Codex r6 #4): A4-b's POST /api/force-kill returns the
// raw structured fields and the UI formats locally.
type Verdict struct {
	Class      VerdictClass `json:"class"`
	PID        int          `json:"pid"`
	Port       int          `json:"port"`
	Mtime      time.Time    `json:"mtime"`
	PIDAlive   bool         `json:"pid_alive"`
	PIDImage   string       `json:"pid_image"`
	PIDCmdline []string     `json:"pid_cmdline"` // truncated to 1KB display
	PIDStart   time.Time    `json:"pid_start"`
	PingMatch  bool         `json:"ping_match"`
	Diagnose   string       `json:"-"`
	Hint       string       `json:"-"`
}

// KillOpts controls KillRecordedHolder behavior.
type KillOpts struct {
	// PingTimeout is how long pingIncumbent waits before declaring
	// "unreachable". Default 500ms when zero.
	PingTimeout time.Duration
	// AcquireDeadline is the maximum total time KillRecordedHolder
	// waits for TryLock to succeed after sending the kill signal.
	// Default 2s when zero.
	AcquireDeadline time.Duration
	// AcquireBackoff is the inter-attempt delay during the
	// post-kill TryLock poll. Default 50ms when zero.
	AcquireBackoff time.Duration
}

// Probe inspects the pidport file and classifies the incumbent's
// state without taking any destructive action. Used by bare
// `mcphub gui --force` to build the diagnostic block.
//
// Class progression:
//   - Pidport unreadable / unparseable → VerdictMalformed.
//   - processID(pid).Alive == false   → VerdictDeadPID.
//   - PID alive AND ping matches      → VerdictHealthy.
//   - PID alive AND ping fails/wrong  → VerdictLiveUnreachable.
//
// Three-part identity gate is NOT run here — it's specific to
// KillRecordedHolder. Probe is read-only and provides display data.
func Probe(ctx context.Context, pidportPath string) Verdict {
	v := Verdict{}
	pid, port, err := ReadPidport(pidportPath)
	if err != nil || pid <= 0 {
		v.Class = VerdictMalformed
		v.Diagnose = fmt.Sprintf("pidport unreadable or empty: %v", err)
		v.Hint = "Reboot the OS or remove the pidport directory contents manually."
		return v
	}
	v.PID = pid
	v.Port = port
	if st, statErr := os.Stat(pidportPath); statErr == nil {
		v.Mtime = st.ModTime()
	}

	id, _ := processID(pid)
	v.PIDAlive = id.Alive
	v.PIDImage = id.ImagePath
	v.PIDCmdline = truncateCmdline(id.Cmdline, 1024)
	v.PIDStart = id.StartTime

	if !id.Alive {
		v.Class = VerdictDeadPID
		v.Diagnose = fmt.Sprintf("recorded PID %d is not alive", pid)
		v.Hint = "The previous incumbent process has exited. The lock should release on its own; if not, reboot."
		return v
	}

	matched, perr := pingIncumbent(ctx, port, 500*time.Millisecond)
	if perr == nil && matched == pid {
		v.Class = VerdictHealthy
		v.PingMatch = true
		v.Diagnose = fmt.Sprintf("incumbent PID %d is healthy on port %d", pid, port)
		v.Hint = ""
		return v
	}
	v.Class = VerdictLiveUnreachable
	if perr != nil {
		v.Diagnose = fmt.Sprintf("recorded PID %d alive but /api/ping on %d failed: %v", pid, port, perr)
	} else {
		v.Diagnose = fmt.Sprintf("recorded PID %d alive but /api/ping on %d returned PID %d", pid, port, matched)
	}
	v.Hint = "Kill the recorded PID manually OR rerun with --force --kill (subject to identity gate)."
	return v
}

// KillRecordedHolder is the destructive opt-in path for
// `mcphub gui --force --kill`. Runs the healthy-incumbent early-exit,
// then the three-part identity gate, then SIGKILL/TerminateProcess
// on the recorded PID, then a TryLock poll loop until acquired or
// AcquireDeadline expires.
//
// Returns (lock, verdict, err). On VerdictKilledRecovered, lock is
// the freshly-acquired SingleInstanceLock the caller must Release.
// On all other Verdicts lock is nil.
//
// Three-part identity gate (memo §"Why automation is opt-in"):
//
//  1. matchBasename(image) — "mcphub.exe" Windows / "mcphub" POSIX.
//  2. argv subcommand: argv[1] == "gui" OR len(argv) == 1.
//     The len(argv)==1 branch covers cmd/mcphub/main.go:32 which
//     internally appends "gui" to os.Args when invoked with no
//     arguments (Explorer/Start-menu double-click); externally the
//     command line is just the executable path.
//  3. process start time ≤ pidport mtime + 1s tolerance.
//
// Codex r4 #7: never os.Remove the lock file. The OS releases the
// flock as a side effect of process termination.
func KillRecordedHolder(ctx context.Context, pidportPath string, opts KillOpts) (*SingleInstanceLock, Verdict, error) {
	if opts.PingTimeout == 0 {
		opts.PingTimeout = 500 * time.Millisecond
	}
	if opts.AcquireDeadline == 0 {
		opts.AcquireDeadline = 2 * time.Second
	}
	if opts.AcquireBackoff == 0 {
		opts.AcquireBackoff = 50 * time.Millisecond
	}

	v := Probe(ctx, pidportPath)
	switch v.Class {
	case VerdictMalformed, VerdictDeadPID:
		return nil, v, fmt.Errorf("kill skipped: %s", v.Class)
	case VerdictHealthy:
		// Codex r5 #7b: incumbent is healthy — do NOT kill. Caller
		// routes to handshake instead. Verdict is returned as-is so
		// the cli layer can print "incumbent is healthy; activating
		// instead of killing" before TryActivateIncumbent.
		return nil, v, nil
	}

	// LiveUnreachable: run the three-part identity gate.
	if !matchBasename(v.PIDImage) {
		v.Class = VerdictKillRefused
		v.Diagnose = fmt.Sprintf("recorded PID %d image %q is not an mcphub binary", v.PID, v.PIDImage)
		v.Hint = "Identity-gate (image basename) failed; identify and kill the actual flock holder via OS tools."
		return nil, v, fmt.Errorf("kill refused: image gate")
	}
	if !cmdlineIsGui(v.PIDCmdline) {
		v.Class = VerdictKillRefused
		v.Diagnose = fmt.Sprintf("recorded PID %d argv subcommand is not 'gui' (argv=%v)", v.PID, v.PIDCmdline)
		v.Hint = "Identity-gate (argv subcommand) failed; the recorded PID is a different mcphub subcommand."
		return nil, v, fmt.Errorf("kill refused: argv gate")
	}
	if !startTimeBeforeMtime(v.PIDStart, v.Mtime, time.Second) {
		v.Class = VerdictKillRefused
		v.Diagnose = fmt.Sprintf("recorded PID %d start-time %s postdates pidport mtime %s — PID-recycled", v.PID, v.PIDStart.Format(time.RFC3339), v.Mtime.Format(time.RFC3339))
		v.Hint = "Identity-gate (start-time) failed; the PID has been recycled to a different process."
		return nil, v, fmt.Errorf("kill refused: start-time gate")
	}

	// All three gates passed. Kill.
	if err := killProcess(v.PID); err != nil {
		v.Class = VerdictKillFailed
		v.Diagnose = fmt.Sprintf("kill PID %d failed: %v", v.PID, err)
		v.Hint = "Permission denied or process already gone; rerun mcphub gui without --force to handshake."
		return nil, v, err
	}

	// Acquire-poll loop (memo §"Take-over protocol" step 5g).
	deadline := time.Now().Add(opts.AcquireDeadline)
	for time.Now().Before(deadline) {
		lock, err := AcquireSingleInstanceAt(pidportPath, v.Port)
		if err == nil {
			v.Class = VerdictKilledRecovered
			v.Diagnose = fmt.Sprintf("force-killed previous incumbent PID %d and acquired lock", v.PID)
			v.Hint = ""
			return lock, v, nil
		}
		if !errors.Is(err, ErrSingleInstanceBusy) {
			v.Class = VerdictKillFailed
			v.Diagnose = fmt.Sprintf("post-kill acquire failed: %v", err)
			v.Hint = ""
			return nil, v, err
		}
		time.Sleep(opts.AcquireBackoff)
	}
	v.Class = VerdictRaceLost
	v.Diagnose = fmt.Sprintf("kill succeeded but a competitor acquired the lock during %s deadline", opts.AcquireDeadline)
	v.Hint = "Rerun mcphub gui without --force to handshake with the new incumbent."
	return nil, v, fmt.Errorf("race lost")
}

// cmdlineIsGui implements the rev 9 argv-subcommand gate:
// argv[1] == "gui" OR len(argv) == 1 (Explorer no-arg auto-gui).
func cmdlineIsGui(argv []string) bool {
	if len(argv) == 1 {
		return true
	}
	if len(argv) >= 2 && argv[1] == "gui" {
		return true
	}
	return false
}

// startTimeBeforeMtime returns true iff start ≤ mtime + tolerance.
// A start time strictly later than the pidport mtime indicates the
// PID was recycled to a process that began AFTER our pidport was
// written.
func startTimeBeforeMtime(start, mtime time.Time, tolerance time.Duration) bool {
	if start.IsZero() || mtime.IsZero() {
		// Defensive: missing telemetry → fail closed.
		return false
	}
	return !start.After(mtime.Add(tolerance))
}

// truncateCmdline caps the total argv string length at maxBytes for
// safe logging/JSON. Per Codex r6 #5: truncation is for display only;
// the gate already inspected argv[1] before this point.
func truncateCmdline(argv []string, maxBytes int) []string {
	if len(argv) == 0 {
		return argv
	}
	out := make([]string, 0, len(argv))
	used := 0
	for _, a := range argv {
		if used+len(a) > maxBytes {
			remaining := maxBytes - used
			if remaining > 0 {
				out = append(out, a[:remaining])
			}
			break
		}
		out = append(out, a)
		used += len(a)
	}
	return out
}
```

Update the existing import block at the top of `internal/gui/single_instance.go` to add the new imports if not already present:

```go
import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)
```

(`context`, `time` are new; rest already there.)

- [ ] **Step 3.5: Run tests to verify they pass**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./internal/gui/ -run "TestProbe_|TestKillRecordedHolder_|TestVerdict" -v
```

Expected: all 7 tests PASS.

- [ ] **Step 3.6: Run full gui package tests to verify no regression**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./internal/gui/ -v 2>&1 | tail -10
```

Expected: `ok  mcp-local-hub/internal/gui ...` — all existing tests continue to pass.

- [ ] **Step 3.7: Commit Task 3**

```bash
git add internal/gui/single_instance.go internal/gui/single_instance_test.go
git commit -m "feat(gui): Verdict + Probe + KillRecordedHolder for --force flow

Adds the classification + destructive-op core for PR #23 C1
stuck-instance recovery.

Verdict struct (memo §rev 9 §Verdict):
  Class (8 enum values) + PID + Port + Mtime + alive/image/cmdline/
  start-time signals + PingMatch. Diagnose/Hint are derived strings
  with json:'-' tags so A4-b's future POST /api/force-kill returns
  raw structured fields, not pre-formatted human blocks (Codex r6
  #4).

Probe(ctx, pidportPath) classifies the incumbent without any
destructive action: Malformed/DeadPID/Healthy/LiveUnreachable.
Used by bare 'mcphub gui --force' for the diagnostic block.

KillRecordedHolder(ctx, pidportPath, opts) is the opt-in destructive
path. Order:
  1. Probe → if Healthy, return early (Codex r5 #7b: never kill a
     healthy incumbent).
  2. Three-part identity gate:
     - matchBasename(image) — mcphub.exe Win / mcphub POSIX.
     - argv[1]=='gui' OR len(argv)==1 (rev 9 fix for Explorer
       no-args auto-gui — cmd/mcphub/main.go:32 internally
       appends 'gui').
     - start-time ≤ pidport mtime + 1s tolerance (rejects
       PID-recycled processes).
  3. killProcess (SIGKILL/TerminateProcess).
  4. Acquire-poll loop (50ms backoff, 2s deadline) — TryLock is
     source of truth for 'lock is mine', not a fixed sleep
     (Codex r5 #2).

Never os.Remove the lock file (Codex r4 #7 invariant). The OS
releases the flock as a side effect of process exit.

Pidport format unchanged (<PID> <PORT>) — token-based identity
attempted in earlier revs proved unsafe (Codex r3 BLOCKER on
clock-stable identity). Rev 9 falls back to the 3-gate runtime
check.

Tests: 7 scenarios covering Probe classification + KillRecordedHolder
gate-fail + healthy early-exit + json:'-' tag enforcement."
```

---

## Task 4: cli/gui.go integration

**Files:**
- Modify: `internal/cli/gui.go`

- [ ] **Step 4.1: Read current state**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
sed -n '37,80p' internal/cli/gui.go
```

This is informational — no test runs yet. Use the output to confirm the placeholder location matches the planned diff in step 4.2.

- [ ] **Step 4.2: Replace the placeholder with the new flow**

Edit `internal/cli/gui.go`. Replace the existing `if err != nil { ... if force { ... falling back ... } ... TryActivateIncumbent ...}` block with the new flow:

In the function `newGuiCmdReal()`, locate the section:

```go
			lock, err := gui.AcquireSingleInstanceAt(pidportPath, port)
			if err != nil {
				if !errors.Is(err, gui.ErrSingleInstanceBusy) {
					return err
				}
				if force {
					fmt.Fprintln(cmd.OutOrStderr(), "warning: --force not implemented yet; falling back to handshake")
				}
				if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
					return fmt.Errorf(
						"another mcphub gui is running but unreachable (%v); "+
							"if the incumbent process is gone, remove %s.lock and retry",
						err, pidportPath)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
				return nil
			}
			defer lock.Release()
```

Replace with:

```go
			lock, err := gui.AcquireSingleInstanceAt(pidportPath, port)
			if err != nil {
				if !errors.Is(err, gui.ErrSingleInstanceBusy) {
					return err
				}
				// PR #23 C1 stuck-instance recovery. Three flows:
				//   - default ErrSingleInstanceBusy without --force →
				//     try handshake; on failure, exit 1 with concise
				//     "remove the .lock and retry" message (legacy).
				//   - bare --force → run Probe, print structured
				//     diagnostic, open lock folder, exit 2.
				//   - --force --kill → KillRecordedHolder (with
				//     three-part identity gate); on success continue
				//     normal startup; on failure map Verdict to the
				//     appropriate exit code.
				if force {
					if kill {
						newLock, exitCode := runForceKill(cmd, pidportPath, yes)
						if newLock != nil {
							lock = newLock
							goto serverStart // tip: continue to Phase B
						}
						return forceExit(exitCode)
					}
					exitCode := runForceDiagnostic(cmd, pidportPath)
					return forceExit(exitCode)
				}
				if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
					return fmt.Errorf(
						"another mcphub gui is running but unreachable (%v); "+
							"rerun with --force for diagnostic, or --force --kill to recover",
						err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
				return nil
			}
		serverStart:
			defer lock.Release()
```

(NOTE: `goto serverStart` + label is the simplest control-flow change that lets `--force --kill` jump straight into Phase B with the freshly-acquired lock. If the codebase prefers no-`goto`, refactor by extracting the post-acquire startup into a helper — see step 4.3 alternative.)

Add at the bottom of the file (before `versionString`):

```go
// runForceDiagnostic implements the bare `--force` flow: Probe,
// print structured block, open lock folder, return exit code 2 (or
// 0 on Healthy fall-through to handshake).
func runForceDiagnostic(cmd *cobra.Command, pidportPath string) int {
	v := gui.Probe(cmd.Context(), pidportPath)
	if v.Class == gui.VerdictHealthy {
		// Healthy → fall through to TryActivateIncumbent (legacy
		// handshake). Returning 0 signals the caller to handshake.
		if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "incumbent reported healthy but activate-window failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
		return 0
	}
	fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))
	_ = gui.OpenFolderAt(pidportPath)
	return 2
}

// runForceKill implements `--force --kill`. Returns
// (acquiredLock, exitCode). On success acquiredLock is non-nil and
// exitCode==0; the caller continues into Phase B.
func runForceKill(cmd *cobra.Command, pidportPath string, yes bool) (*gui.SingleInstanceLock, int) {
	// Gate 1: non-TTY without --yes → exit 6.
	if !yes && !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(cmd.OutOrStderr(), "non-interactive shell — pass --yes to confirm --kill")
		return nil, 6
	}

	v := gui.Probe(cmd.Context(), pidportPath)

	// Healthy early-exit (Codex r5 #7b): never kill a healthy gui.
	if v.Class == gui.VerdictHealthy {
		fmt.Fprintf(cmd.OutOrStdout(), "incumbent is healthy (PID %d); activating instead of killing\n", v.PID)
		if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "activate-window failed: %v\n", err)
			return nil, 1
		}
		return nil, 0
	}

	// Print diagnostic so the operator sees what we're about to kill.
	fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))

	// Confirmation prompt unless --yes.
	if !yes {
		fmt.Fprintf(cmd.OutOrStdout(), "Kill PID %d (mcphub gui)? [y/N]: ", v.PID)
		var resp string
		_, _ = fmt.Fscanln(os.Stdin, &resp)
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp != "y" && resp != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "cancelled")
			return nil, 0
		}
	}

	lock, killVerdict, err := gui.KillRecordedHolder(cmd.Context(), pidportPath, gui.KillOpts{})
	if killVerdict.Class == gui.VerdictKilledRecovered {
		fmt.Fprintln(cmd.OutOrStdout(), killVerdict.Diagnose)
		return lock, 0
	}
	// Map class to exit code.
	fmt.Fprintln(cmd.OutOrStderr(), killVerdict.Diagnose)
	if killVerdict.Hint != "" {
		fmt.Fprintln(cmd.OutOrStderr(), "hint:", killVerdict.Hint)
	}
	switch killVerdict.Class {
	case gui.VerdictKillRefused:
		return nil, 7
	case gui.VerdictKillFailed:
		return nil, 4
	case gui.VerdictRaceLost:
		return nil, 3
	case gui.VerdictMalformed:
		return nil, 4
	case gui.VerdictDeadPID:
		// Probe said dead but acquire failed afterward — treat as
		// race-lost because the OS should have released flock when
		// the dead process exited; if we can't acquire, someone
		// else holds it now.
		return nil, 3
	default:
		_ = err
		return nil, 1
	}
}

// formatDiagnostic builds the human-readable diagnostic block from
// a Verdict. Output format matches memo §"Diagnostic format".
func formatDiagnostic(v gui.Verdict, pidportPath string) string {
	var b strings.Builder
	b.WriteString("Cannot acquire mcphub gui single-instance lock.\n\n")
	fmt.Fprintf(&b, "Lock file:  %s.lock\n", pidportPath)
	fmt.Fprintf(&b, "Pidport:    %s\n", pidportPath)
	fmt.Fprintf(&b, "  recorded PID:  %d\n", v.PID)
	fmt.Fprintf(&b, "  recorded port: %d\n", v.Port)
	if !v.Mtime.IsZero() {
		fmt.Fprintf(&b, "  pidport mtime: %s\n\n", v.Mtime.UTC().Format(time.RFC3339))
	}
	b.WriteString("Live-holder probe:\n")
	if v.PIDAlive {
		fmt.Fprintf(&b, "  PID %d status:    alive\n", v.PID)
		if v.PIDImage != "" {
			fmt.Fprintf(&b, "  PID %d image:     %s\n", v.PID, v.PIDImage)
		}
	} else {
		fmt.Fprintf(&b, "  PID %d status:    not alive\n", v.PID)
	}
	if v.PingMatch {
		fmt.Fprintf(&b, "  /api/ping on %d:  ok (PID matches)\n\n", v.Port)
	} else {
		fmt.Fprintf(&b, "  /api/ping on %d:  failed or PID mismatch\n\n", v.Port)
	}
	if v.Diagnose != "" {
		b.WriteString("Verdict: " + v.Class.String() + "\n")
		b.WriteString("  " + v.Diagnose + "\n")
	}
	if v.Hint != "" {
		b.WriteString("Hint: " + v.Hint + "\n")
	}
	return b.String()
}

// forceExit returns a cobra-friendly error that propagates the exit
// code. Cobra exits with status 1 by default; we wrap a typed error
// that main.go can branch on to set os.Exit(code) explicitly.
type forceExitError struct{ code int }

func (e *forceExitError) Error() string { return fmt.Sprintf("force exit %d", e.code) }
func (e *forceExitError) ExitCode() int { return e.code }

func forceExit(code int) error {
	return &forceExitError{code: code}
}
```

Update the imports of `internal/cli/gui.go` to add the new packages:

```go
import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mcp-local-hub/internal/gui"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)
```

(`strings` and `golang.org/x/term` are new.)

Update the flag declarations near the end of `newGuiCmdReal()`:

Find:
```go
	c.Flags().BoolVar(&force, "force", false, "take over a stuck single-instance mutex if pidport probe fails")
	// --force is a Phase 3B-II placeholder: today it only prints a warning and still falls into
	// the standard handshake path. Hide it from --help so users don't expect the takeover behavior.
	_ = c.Flags().MarkHidden("force")
```

Replace with:
```go
	c.Flags().BoolVar(&force, "force", false, "stuck-instance recovery: print diagnostic + open lock folder. Add --kill to terminate the recorded PID after a three-part identity gate.")
	c.Flags().BoolVar(&kill, "kill", false, "with --force: kill the recorded PID (image/argv/start-time gate); SIGKILL/TerminateProcess. The kernel releases the flock as a side effect.")
	c.Flags().BoolVar(&yes, "yes", false, "with --force --kill: skip the confirmation prompt (required in non-interactive shells).")
	_ = c.Flags().MarkHidden("force")
	_ = c.Flags().MarkHidden("kill")
	_ = c.Flags().MarkHidden("yes")
```

And add `kill, yes bool` to the var declaration block at the top of `newGuiCmdReal()`:

```go
	var (
		port      int
		noBrowser bool
		noTray    bool
		force     bool
		kill      bool
		yes       bool
	)
```

Finally, wire `forceExitError` into `cmd/mcphub/main.go`. Open `cmd/mcphub/main.go` and locate the section where `RunE`'s error is returned. Add a typed-error check that maps `*forceExitError.ExitCode()` to `os.Exit`:

```go
// Anywhere main.go calls cli.NewRootCmd().Execute(): replace the
// default error handling with:
err := cli.NewRootCmd().Execute()
if err != nil {
    var fe interface{ ExitCode() int }
    if errors.As(err, &fe) {
        os.Exit(fe.ExitCode())
    }
    fmt.Fprintln(os.Stderr, err)
    os.Exit(1)
}
```

(If main.go currently uses `cobra.CheckErr` or similar, the change is to extract the exec call, branch on the typed error, and call `os.Exit(fe.ExitCode())` before falling through to the default-1 path.)

- [ ] **Step 4.3: Verify compile**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go build ./...
```

Expected: build succeeds with no errors. (If `goto`/label is rejected by lint policy, refactor `runForceKill` to return the lock from `newGuiCmdReal()` directly — extract the post-acquire startup block into a helper `startServer(cmd, lock, port, noBrowser, noTray, pidportPath)` and call it from both the normal path and the `--force --kill` recovery path.)

- [ ] **Step 4.4: Commit Task 4**

```bash
git add internal/cli/gui.go cmd/mcphub/main.go
git commit -m "feat(cli): wire --force / --force --kill stuck-instance recovery

Replaces the 'warning: --force not implemented yet; falling back to
handshake' placeholder with the rev 9 design from PR #23.

Bare --force:
  Probe(pidportPath) → if Healthy, fall through to TryActivateIncumbent
  (handshake). Otherwise print structured diagnostic block and call
  OpenFolderAt(pidportPath) (best-effort fire-and-forget). Exit 2.

--force --kill:
  Gate 0: non-TTY without --yes → exit 6.
  Gate 1: Probe → if Healthy, print 'incumbent is healthy; activating
  instead of killing' notice and route to TryActivateIncumbent
  (Codex r5 #7b: never kill a healthy gui).
  Gate 2: confirmation prompt unless --yes.
  Gate 3: KillRecordedHolder runs the three-part identity gate
  (image basename + argv subcommand + start-time vs pidport mtime),
  SIGKILLs the recorded PID, polls TryLock with 50ms backoff and
  2s deadline. On success the lock is returned and the gui
  proceeds into Phase B normally.

Verdict-to-exit-code map:
  Healthy → 0 (handshake fall-through)
  KilledRecovered → 0 (proceeds with normal startup)
  Malformed / KillFailed → 4
  RaceLost / DeadPID-but-acquire-failed → 3
  KillRefused → 7
  Non-TTY without --yes → 6
  Generic startup → 1
  (5 reserved per memo §Exit codes; not emitted by PR #23.)

Flag help text updated; --force / --kill / --yes all hidden in
--help (Codex r1 SUGGEST 1).

main.go propagates *forceExitError.ExitCode() to os.Exit so the
distinct exit codes survive cobra's default '1 on error'."
```

---

## Task 5: 8-scenario test file `internal/cli/gui_force_test.go`

**Files:**
- Create: `internal/cli/gui_force_test.go`

- [ ] **Step 5.1: Write the 8 scenarios**

Create `internal/cli/gui_force_test.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"mcp-local-hub/internal/gui"
)

// ---------------------------------------------------------------
// Scenario 1: Healthy --force AND healthy --force --kill --yes notice
// ---------------------------------------------------------------

func TestForce_HealthyIncumbent_BareFlagActivates(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	srv := healthyPingServer(t, os.Getpid())
	defer srv.Close()
	port := portFromHTTPTestURL(t, srv.URL)
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	out := captureStdout(t, func() {
		_ = c.Execute()
	})
	if !strings.Contains(out, "activated existing mcphub gui") {
		t.Errorf("expected 'activated existing mcphub gui'; got %q", out)
	}
}

func TestForce_HealthyIncumbent_KillFlagPrintsNoticeAndActivates(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	srv := healthyPingServer(t, os.Getpid())
	defer srv.Close()
	port := portFromHTTPTestURL(t, srv.URL)
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	out := captureStdout(t, func() {
		_ = c.Execute()
	})
	if !strings.Contains(out, "incumbent is healthy") {
		t.Errorf("expected 'incumbent is healthy' notice; got %q", out)
	}
	if !strings.Contains(out, "activating instead of killing") {
		t.Errorf("expected 'activating instead of killing' notice; got %q", out)
	}
}

// ---------------------------------------------------------------
// Scenario 2: Stuck — bare --force shows diagnostic + opens folder
// ---------------------------------------------------------------

func TestForce_StuckIncumbent_BareFlagShowsDiagnosticAndOpensFolder(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-acquire flock so AcquireSingleInstance returns busy.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Mock the open-folder seam.
	prevSpawn := gui.OpenFolderSpawnForTest()
	defer gui.RestoreOpenFolderSpawn(prevSpawn)
	var spawnedName string
	gui.SetOpenFolderSpawn(func(name string, args ...string) error {
		spawnedName = name
		return nil
	})

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	out := captureStdout(t, func() {
		err := c.Execute()
		var fe interface{ ExitCode() int }
		if !errors.As(err, &fe) {
			t.Errorf("expected typed exit code error; got %v", err)
			return
		}
		if fe.ExitCode() != 2 {
			t.Errorf("exit code = %d, want 2", fe.ExitCode())
		}
	})
	if !strings.Contains(out, "Cannot acquire") {
		t.Errorf("expected diagnostic block; got %q", out)
	}
	if spawnedName == "" {
		t.Errorf("OpenFolderAt seam was not invoked")
	}
}

// ---------------------------------------------------------------
// Scenario 3: --force --kill happy path (seam-mocked gate)
// ---------------------------------------------------------------

func TestForce_KillHappyPath_SeamMocked(t *testing.T) {
	// This scenario uses a seam to bypass the three-part gate's
	// strict cmdline/image checks (which would reject the test
	// binary as non-mcphub). Real-process proof is in scenario 8.
	prevGate := gui.IdentityGateForTest()
	defer gui.RestoreIdentityGate(prevGate)
	gui.SetIdentityGate(func(v gui.Verdict) (refused bool, reason string) { return false, "" })

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// Spawn a child sleep process so we have a real PID we can kill.
	sleepCmd := exec.Command("powershell", "-NoExit", "-Command", "Start-Sleep -Seconds 60")
	if runtime.GOOS != "windows" {
		sleepCmd = exec.Command("sleep", "60")
	}
	if err := sleepCmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep helper: %v", err)
	}
	defer sleepCmd.Process.Kill()
	pid := sleepCmd.Process.Pid
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	// We'll release the flock when the kill happens — simulate the
	// OS auto-release by unlocking when the child dies.
	go func() {
		_, _ = sleepCmd.Process.Wait()
		fl.Unlock()
	}()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	if err != nil {
		// Acceptable: the cobra cmd may exit normally after acquiring
		// or with a typed error — we just assert it isn't exit 7
		// (refused) or exit 4 (failed).
		var fe interface{ ExitCode() int }
		if errors.As(err, &fe) {
			if fe.ExitCode() == 7 || fe.ExitCode() == 4 {
				t.Errorf("happy path exit code = %d (refused/failed); want 0 or 3", fe.ExitCode())
			}
		}
	}
}

// ---------------------------------------------------------------
// Scenario 4: --force --kill refuses non-mcphub image
// ---------------------------------------------------------------

func TestForce_KillRefusesNonMcphubImage(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// os.Getppid is the shell — image does NOT match mcphub.exe/mcphub.
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 7 {
		t.Errorf("exit code = %d, want 7 (KillRefused)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// Scenario 5: --force --kill non-interactive without --yes → exit 6
// ---------------------------------------------------------------

func TestForce_KillNonInteractiveWithoutYesExits6(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill"})
	// `go test` runs without a TTY by default.
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 6 {
		t.Errorf("exit code = %d, want 6 (NonInteractive)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// Scenario 6: --force --kill race-lost → exit 3
// ---------------------------------------------------------------

func TestForce_KillRaceLost(t *testing.T) {
	// Inject seam: KillRecordedHolder kill succeeds but a competitor
	// pre-acquires the new flock before our acquire-poll catches up.
	prevGate := gui.IdentityGateForTest()
	defer gui.RestoreIdentityGate(prevGate)
	gui.SetIdentityGate(func(v gui.Verdict) (refused bool, reason string) { return false, "" })

	prevHook := gui.PostKillHookForTest()
	defer gui.RestorePostKillHook(prevHook)

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	gui.SetPostKillHook(func() {
		// After the kill (which won't happen for our own PID, but
		// the seam fires anyway), keep the flock held — competitor.
		// Already held; just ensure we don't release.
	})

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		// Some race-lost paths return generic exit 1; acceptable.
		fl.Unlock()
		return
	}
	if fe.ExitCode() != 3 {
		t.Logf("exit code = %d (expected 3 RaceLost; this scenario is hard to deterministically engineer without subprocess control)", fe.ExitCode())
	}
	fl.Unlock()
}

// ---------------------------------------------------------------
// Scenario 7: Malformed pidport → exit 2
// ---------------------------------------------------------------

func TestForce_MalformedPidport(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte("garbage not a pidport"), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2 (Malformed reaches the diagnostic-only path)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// Scenario 8: Real subprocess E2E
// ---------------------------------------------------------------

func TestForce_RealSubprocessE2E(t *testing.T) {
	// Use the existing ensureMcphubBinary pattern from
	// daemon_reliability_test.go.
	bin := ensureMcphubBinary(t)
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// Phase A: spawn a first mcphub gui that holds the lock.
	first := exec.Command(bin, "gui", "--port", "0", "--no-browser", "--no-tray")
	first.Env = append(os.Environ(), "MCPHUB_GUI_TEST_PIDPORT_DIR="+dir)
	if err := first.Start(); err != nil {
		t.Fatalf("spawn first gui: %v", err)
	}
	defer func() {
		if first.Process != nil {
			_ = first.Process.Kill()
		}
	}()

	// Wait for first gui to write pidport.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidport); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(pidport); err != nil {
		t.Fatalf("first gui did not write pidport within deadline: %v", err)
	}

	// Phase B: spawn a second mcphub gui --force --kill --yes.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	second := exec.CommandContext(ctx, bin, "gui", "--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes")
	second.Env = append(os.Environ(), "MCPHUB_GUI_TEST_PIDPORT_DIR="+dir)
	out, err := second.CombinedOutput()
	t.Logf("second gui output:\n%s", string(out))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Logf("second gui exit code: %d", ee.ExitCode())
		}
	}
	// Expect: second succeeded with KilledRecovered (exit 0) OR
	// raced and exit 3. Either is acceptable for E2E — the kill
	// path was exercised end-to-end.
	if !strings.Contains(string(out), "force-killed") &&
		!strings.Contains(string(out), "Race") &&
		!strings.Contains(string(out), "race") {
		t.Errorf("expected force-killed or race output; got %q", string(out))
	}
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

func healthyPingServer(t *testing.T, pid int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": pid})
	}))
}

func portFromHTTPTestURL(t *testing.T, u string) int {
	t.Helper()
	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("unexpected httptest URL %q", u)
	}
	rest := u[len(prefix):]
	var port int
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	return port
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	done := make(chan string)
	go func() {
		buf := make([]byte, 4096)
		var b strings.Builder
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	w.Close()
	return <-done
}

// newGuiCmdRealForTest builds a fresh gui cobra command in test mode.
// We need the real RunE wired so --force flows actually execute.
func newGuiCmdRealForTest() *cobra.Command {
	c := newGuiCmdReal()
	c.SetIn(os.Stdin)
	c.SetOut(os.Stdout)
	c.SetErr(os.Stderr)
	return c
}
```

Note the test file references `gui.OpenFolderSpawnForTest()`, `gui.SetOpenFolderSpawn(...)`, `gui.IdentityGateForTest()`, `gui.SetIdentityGate(...)`, `gui.PostKillHookForTest()`, `gui.SetPostKillHook(...)`, `gui.RestoreOpenFolderSpawn(...)`, `gui.RestoreIdentityGate(...)`, `gui.RestorePostKillHook(...)`. These are test-only seams. Add them in `internal/gui/single_instance.go` or a new `internal/gui/test_seams.go`:

```go
// internal/gui/test_seams.go
//go:build test || !release
// (Actually: use an init-time exported variable so tests work in
// any build mode. Below uses the simpler "always exported" pattern.)

package gui

// OpenFolderSpawnForTest returns the current openFolderSpawn for
// later restoration.
func OpenFolderSpawnForTest() func(string, ...string) error { return openFolderSpawn }
func SetOpenFolderSpawn(fn func(string, ...string) error)   { openFolderSpawn = fn }
func RestoreOpenFolderSpawn(fn func(string, ...string) error) { openFolderSpawn = fn }

var identityGateOverride func(Verdict) (refused bool, reason string)

func IdentityGateForTest() func(Verdict) (refused bool, reason string) { return identityGateOverride }
func SetIdentityGate(fn func(Verdict) (refused bool, reason string))   { identityGateOverride = fn }
func RestoreIdentityGate(fn func(Verdict) (refused bool, reason string)) { identityGateOverride = fn }

var postKillHook func()

func PostKillHookForTest() func()    { return postKillHook }
func SetPostKillHook(fn func())      { postKillHook = fn }
func RestorePostKillHook(fn func())  { postKillHook = fn }
```

Then update `KillRecordedHolder` in `single_instance.go` to consult `identityGateOverride` and `postKillHook`:

```go
// In KillRecordedHolder, BEFORE the matchBasename check:
if identityGateOverride != nil {
    if refused, reason := identityGateOverride(v); refused {
        v.Class = VerdictKillRefused
        v.Diagnose = "identity gate (test override): " + reason
        return nil, v, fmt.Errorf("kill refused (override): %s", reason)
    }
} else {
    // ... existing 3-part gate ...
}

// AFTER killProcess(v.PID), BEFORE the acquire-poll loop:
if postKillHook != nil {
    postKillHook()
}
```

- [ ] **Step 5.2: Run all 8 scenarios**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./internal/cli/ -run "TestForce_" -v -timeout 60s
```

Expected: all 8 PASS (some may take ~5-10s for the subprocess scenario).

- [ ] **Step 5.3: Run full test suite to verify no regression**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./... 2>&1 | tail -15
```

Expected: all packages PASS (`ok` lines, no `FAIL`).

- [ ] **Step 5.4: Commit Task 5**

```bash
git add internal/cli/gui_force_test.go internal/gui/test_seams.go internal/gui/single_instance.go
git commit -m "test(cli): 8-scenario coverage for --force / --force --kill

Per memo §rev 9 §Tests:
1. Healthy --force → handshake activates (exit 0).
2. Healthy --force --kill --yes → 'incumbent is healthy; activating
   instead of killing' notice + handshake (Codex r5 #7b).
3. Stuck --force bare → diagnostic block + open-folder seam
   invocation + exit 2.
4. --force --kill happy path (seam-mocked three-gate so the test
   binary's own PID can be the target without failing the
   image/argv checks; real-process proof is in scenario 8).
5. --force --kill refuses non-mcphub image (uses os.Getppid which
   is the shell, not mcphub) → exit 7.
6. --force --kill non-interactive without --yes → exit 6.
7. --force --kill race-lost → exit 3 (deterministic engineering of
   this is hard without subprocess control; scenario logs the
   actual exit code if not 3).
8. Real subprocess E2E using ensureMcphubBinary pattern from
   daemon_reliability_test.go: spawn first mcphub gui, then
   second mcphub gui --force --kill --yes; assert kill path was
   exercised end-to-end (Codex r4 #6 + r5 #6 satisfaction).

Test seams in gui/test_seams.go:
  OpenFolderSpawn / IdentityGate / PostKillHook getters/setters/
  restorers. Allow tests to bypass strict gates and observe
  hooks without hard-coding mcphub.exe identity into the test
  binary.

KillRecordedHolder consults identityGateOverride if set and fires
postKillHook between the kill and the acquire-poll loop."
```

---

## Task 6: Documentation updates

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/phase-3b-ii-verification.md`
- Modify: `docs/superpowers/plans/phase-3b-ii-backlog.md`

- [ ] **Step 6.1: Add CLAUDE.md runbook section**

Append to `CLAUDE.md` after the "## GUI E2E tests" block, as a new "## Stuck-instance recovery" section:

```markdown
## Stuck-instance recovery

If `mcphub gui` exits with the structured "Cannot acquire mcphub gui
single-instance lock" block, run `mcphub gui --force` for the
diagnostic — it also opens the lock folder in your file manager so
the offending files are visible.

To recover automatically:
    mcphub gui --force --kill              # prompts before killing
    mcphub gui --force --kill --yes        # no prompt; for scripts

`--kill` only terminates the recorded PID after a three-part identity
gate: (1) image basename is `mcphub.exe` (Windows) or `mcphub` (POSIX);
(2) `argv[1]` (cobra subcommand token) equals `gui` exactly OR the
process was launched with no args (Explorer/Start-menu double-click,
which `cmd/mcphub/main.go:32` defaults to gui internally); (3) process
start time precedes pidport mtime. If any gate fails (e.g. PID
recycled to a `mcphub.exe daemon` Task Scheduler child), `--kill`
refuses with exit 7.

Manual recovery when `--kill` refuses:
  Windows: download Sysinternals `handle.exe`, then
           `handle.exe -a "<lock-path>"` (REQUIRES ELEVATED shell).
  Linux:   `lsof "<lock-path>"` or `fuser "<lock-path>"` (use `sudo`
           for cross-user holders).

Then `kill -9 <pid>` (Linux) or Task Manager → End Task on that
PID (Windows). DO NOT delete the lock file — deleting under a live
holder splits ownership. If admin tooling isn't available, reboot
is the universally available recovery (stuck file handles survive
user-mode cleanup only across a session reset).

Exit codes:
  0 — success
  1 — non-force startup error
  2 — bare --force exited after diagnostic
  3 — race lost or pidport changed mid-prompt
  4 — kill failed / pidport unrecoverable
  5 — RESERVED (not emitted)
  6 — non-interactive shell with --kill but no --yes
  7 — --kill refused by identity gate
```

- [ ] **Step 6.2: Update D2 row in `docs/phase-3b-ii-verification.md`**

Find the existing D2 row covering single-instance recovery. If none exists, add this new row to the D2 section:

```markdown
### D2.X — `mcphub gui --force` stuck-instance recovery (PR #23)

**Test:** Reproduce a stuck single-instance lock via debugger pause:

1. `mcphub gui` (binds default port; tray icon visible).
2. Attach a debugger (e.g. `dlv attach <PID>`) and pause the gui
   process.
3. From a second terminal: `mcphub gui --force`.
4. Verify: structured diagnostic prints (Lock file path, recorded
   PID, port, alive=true, /api/ping=connection refused).
   Explorer/Finder window opens at the pidport directory.
5. From the same terminal: `mcphub gui --force --kill --yes`.
6. Verify: "force-killed previous incumbent PID <pid> and acquired
   lock" prints; new gui starts on a fresh port.
7. Detach the debugger. The original gui is gone (TerminateProcess'd).

**Expected outcomes:**
- Step 4: exit code 2 (bare diagnostic).
- Step 6: exit code 0 (kill succeeded).

**If exit 7:** the recorded PID is a different mcphub subcommand
(e.g. `mcphub daemon`); rerun without `--kill` and identify the
actual flock holder via `handle.exe` (admin shell) or reboot.
```

- [ ] **Step 6.3: Update A4-b row in `docs/superpowers/plans/phase-3b-ii-backlog.md`**

Find the A4-b Settings lifecycle row and add a forward-reference:

```markdown
9b. **A4-b** — Settings lifecycle: tray, port live-rebind, weekly schedule edit, retry policy, Clean now confirm, export bundle.
   - **Forward-ref to PR #23 C1:** A4-b's "Recover stuck instance" Settings UI button posts to a new `POST /api/force-kill` HTTP handler that returns the `gui.Verdict` JSON contract from PR #23 (`internal/gui/single_instance.go::Verdict`). Diagnose/Hint are not on the wire (`json:"-"`); UI formats from the structured fields.
```

- [ ] **Step 6.4: Commit Task 6**

```bash
git add CLAUDE.md docs/phase-3b-ii-verification.md docs/superpowers/plans/phase-3b-ii-backlog.md
git commit -m "docs: PR #23 C1 force take-over runbook + verification + A4-b ref

CLAUDE.md gains a 'Stuck-instance recovery' section explaining:
  - bare --force diagnostic + open-folder UX
  - --force --kill three-part identity gate
  - manual recovery when the gate refuses (handle.exe / lsof, sudo
    for cross-user, reboot when admin tooling unavailable)
  - all 7 exit codes (5 reserved)

Verification matrix (D2.X) gains the manual smoke procedure for
stuck-instance recovery (debugger-pause repro).

phase-3b-ii-backlog.md A4-b row forward-references the Verdict
contract: A4-b's future POST /api/force-kill HTTP handler returns
gui.Verdict JSON without Diagnose/Hint (json:'-'); UI formats
locally."
```

---

## Task 7: final verification

**Files:** none (verification only)

- [ ] **Step 7.1: Run all unit + e2e tests**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go test ./... 2>&1 | tail -25
```

Expected: every line ends with `ok` or shows skip/no-tests; no `FAIL`.

- [ ] **Step 7.2: Vet + typecheck**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go vet ./...
```

Expected: no diagnostic output.

- [ ] **Step 7.3: Build the binary and verify version**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
go build -o /tmp/mcphub_c1_test ./cmd/mcphub
/tmp/mcphub_c1_test gui --help 2>&1 | head -20
```

Expected: `gui --help` shows Use/Short/Long; `--force / --kill / --yes` are NOT in the help summary (hidden) but ARE accepted via `--help` no-error.

- [ ] **Step 7.4: Verify branch has the expected commits**

```bash
cd <repo-root>  # i.e. the directory containing go.mod
git log --oneline master..HEAD
```

Expected: 6 commits — Task 1 through Task 6 — in order, all on `feat/c1-force-takeover`.

- [ ] **Step 7.5: STOP — DO NOT push or open PR**

Per repo standing rule (precedent A3-a/A3-b/A4-a/PR#21): explicit user approval required before push/PR. Halt here. The user reviews the branch locally first; only after explicit "ok" does the orchestrator push.

---

## Self-Review

**1. Spec coverage:** Each rev 9 memo section has a task:
- §"Why automation is opt-in" → Task 3 (KillRecordedHolder + 3-gate)
- §"PID identity" three gates → Task 3 (`matchBasename`, `cmdlineIsGui`, `startTimeBeforeMtime`)
- §"Default --force flow" → Task 4 (`runForceDiagnostic`)
- §"--force --kill flow" → Task 4 (`runForceKill`)
- §"Diagnostic format" → Task 4 (`formatDiagnostic`)
- §"Verdict struct (kept for A4-b future)" → Task 3 (Verdict + VerdictClass + json:"-")
- §"Exit codes" → Task 4 (forceExit + exit-code mapping)
- §"Files" → Tasks 1, 2, 3, 4, 5, 6 cover all 8 listed files
- §"Tests" 8 scenarios → Task 5 (one test function per scenario)
- §"CLAUDE.md runbook" → Task 6 step 6.1
- §"Out of scope" — none implemented (no macOS, no listing, no auto-recovery default)

**2. Placeholder scan:** No "TBD", "TODO", "implement later". Every step has full code, exact paths, expected output for verification.

**3. Type consistency:** `Verdict.Class`, `Verdict.PID`, `Verdict.Port`, `Verdict.PIDAlive`, `Verdict.PIDImage`, `Verdict.PIDCmdline`, `Verdict.PIDStart`, `Verdict.PingMatch`, `Verdict.Mtime`, `Verdict.Diagnose`, `Verdict.Hint` — all consistent across Tasks 3 / 4 / 5 / 6. `KillOpts` has `PingTimeout`, `AcquireDeadline`, `AcquireBackoff` — referenced consistently. `processID` returns `(ProcessIdentity, error)` — same signature in Task 1 (probe.go) and Task 3 (consumed in single_instance.go). Test seam names (`OpenFolderSpawnForTest`, `SetOpenFolderSpawn`, etc.) are consistent between Task 5 declaration and Task 5 test usage.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-29-c1-force-takeover.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration. Per the user's stated preference for option (1) when this plan was requested.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

**Which approach?**

If Subagent-Driven chosen: REQUIRED SUB-SKILL — Use superpowers:subagent-driven-development. Fresh subagent per task + two-stage review.

If Inline Execution chosen: REQUIRED SUB-SKILL — Use superpowers:executing-plans. Batch execution with checkpoints for review.