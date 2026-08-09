//go:build windows

package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"
)

const startWithJobEnvironmentHelper = "MCPHUB_PR591_START_ENV_HELPER"

func TestStartWithJobWindowsEnvironmentHelper(t *testing.T) {
	if os.Getenv(startWithJobEnvironmentHelper) != "probe" {
		return
	}
	path := os.Getenv("MCPHUB_PR591_START_ENV_OUTPUT")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	contents := strings.Join([]string{
		"case=" + os.Getenv("MCPHUB_PR591_CASE"),
		"systemroot=" + os.Getenv("SYSTEMROOT"),
		"pwd=" + os.Getenv("PWD"),
		"wd=" + wd,
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareWindowsCommand_EnvironmentParity(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "nil"},
		{name: "explicit_empty", env: []string{}},
		{name: "mixed_case_last_wins", env: []string{"McpHub_PR591_Case=first", "MCPHUB_PR591_CASE=last", "PWD=explicit"}},
		{name: "explicit_empty_systemroot", env: []string{"SYSTEMROOT="}},
		{name: "leading_equals", env: []string{"=C:=C:\\work", "MCPHUB_PR591_CASE=value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &exec.Cmd{Path: exe, Args: []string{exe}, Env: tc.env}
			_, _, block, err := prepareWindowsCommand(cmd)
			if err != nil {
				t.Fatal(err)
			}
			got := decodeWindowsEnvironmentBlock(t, block)
			want := sortedWindowsEnvironment(cmd.Environ())
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("environment block=%q, want Cmd.Environ normalization=%q", got, want)
			}
		})
	}
}

func TestPrepareWindowsCommand_RejectsNULBeforeSpawn(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Path: exe, Args: []string{exe}, Env: []string{"MCPHUB_PR591_NUL=bad\x00value"}}
	if _, _, _, err := prepareWindowsCommand(cmd); err == nil || err.Error() != "invalid command environment" {
		t.Fatalf("prepareWindowsCommand error=%v, want stable non-echoing error", err)
	}
}

func TestStartWithJob_ChildSeesNormalizedEnvironment(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	output := filepath.Join(dir, "environment.txt")
	cmd := &exec.Cmd{
		Path: exe,
		Args: []string{exe, "-test.run=^TestStartWithJobWindowsEnvironmentHelper$"},
		Dir:  dir,
		Env: []string{
			startWithJobEnvironmentHelper + "=probe",
			"MCPHUB_PR591_START_ENV_OUTPUT=" + output,
			"McpHub_PR591_Case=first",
			"MCPHUB_PR591_CASE=last",
			"PWD=explicit-pwd",
		},
	}
	if _, err := StartWithJob(job, cmd); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(output)
		if readErr == nil {
			got := string(body)
			for _, want := range []string{"case=last", "systemroot=", "pwd=explicit-pwd", "wd=" + dir} {
				if !strings.Contains(got, want) {
					t.Fatalf("child environment=%q, missing %q", got, want)
				}
			}
			if strings.Contains(got, "systemroot=\n") {
				t.Fatalf("child environment=%q, missing Go-owned SYSTEMROOT", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child did not write normalized environment probe")
}

func decodeWindowsEnvironmentBlock(t *testing.T, ptr *uint16) []string {
	t.Helper()
	if ptr == nil {
		t.Fatal("nil environment block")
	}
	words := unsafe.Slice(ptr, 1<<20)
	var out []string
	start := 0
	for i, word := range words {
		if word != 0 {
			continue
		}
		if i == start {
			return out
		}
		out = append(out, string(utf16.Decode(words[start:i])))
		start = i + 1
	}
	t.Fatal("environment block lacks final double NUL")
	return nil
}

func sortedWindowsEnvironment(env []string) []string {
	out := append([]string(nil), env...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && testWindowsEnvironmentLess(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func testWindowsEnvironmentLess(left, right string) bool {
	for i := 0; ; i++ {
		var l, r byte
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if l == '=' || r == '=' || i == len(left) || i == len(right) {
			return l < r
		}
		if l >= 'a' && l <= 'z' {
			l -= 'a' - 'A'
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		if l != r {
			return l < r
		}
	}
}

// TestStartWithJob_AssignsAtCreate proves that StartWithJob spawns a
// child process that is *already* a member of the supervisor's Job
// Object at create time — closing the v0.4.x Start-then-Assign race
// documented at internal/process/jobobject_windows.go:65-71.
//
// The child is `cmd.exe /c exit 0` — short-lived so the test ends
// quickly, but long-enough to be observable in IsProcessInJob before
// the kernel reaps the process handle.
func TestStartWithJob_AssignsAtCreate(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	// Spawn a short-lived child. We use ping -n 2 (≈1s) instead of
	// `cmd.exe /c exit 0` so the process handle is still openable
	// for the IsProcessInJob probe — `exit 0` races the kernel reaper.
	cmd := exec.Command("ping.exe", "-n", "2", "127.0.0.1")
	pid, err := StartWithJob(job, cmd)
	if err != nil {
		t.Fatalf("StartWithJob: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("invalid pid: %d", pid)
	}
	defer func() {
		// Defensive: kill the child so we don't leak it if the test
		// fails before the natural exit.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Verify the child is associated with the Job Object.
	if !job.HasMember(pid) {
		t.Fatalf("child PID %d not in Job Object", pid)
	}

	// Wait for child exit (best-effort; ping should finish in ~1s).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid, t) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Child still running after 5s — odd for a 1s ping but not a
	// failure of the assign-at-create contract; the test goal was
	// the HasMember check, which already passed.
}

// TestStartWithJob_NilJob verifies the misuse guard.
func TestStartWithJob_NilJob(t *testing.T) {
	cmd := exec.Command("ping.exe", "-n", "1", "127.0.0.1")
	if _, err := StartWithJob(nil, cmd); err == nil {
		t.Error("StartWithJob(nil job) returned nil error; want error")
	}
}

// TestStartWithJob_NilCmd verifies the misuse guard.
func TestStartWithJob_NilCmd(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()
	if _, err := StartWithJob(job, nil); err == nil {
		t.Error("StartWithJob(nil cmd) returned nil error; want error")
	}
}

// TestStartWithJob_NormalizesEmptyArgs verifies the nil-Args path:
// callers may construct a Cmd directly with only Path set (per os/exec
// docs), in which case StartWithJob must normalize Args before slicing
// rather than panic on nil[1:].
func TestStartWithJob_NormalizesEmptyArgs(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	// Construct cmd without exec.Command() — empty Args.
	cmd := &exec.Cmd{Path: `C:\Windows\System32\ping.exe`, Args: nil}
	pid, err := StartWithJob(job, cmd)
	if err != nil {
		t.Fatalf("StartWithJob with nil Args should not panic: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("expected valid pid, got %d", pid)
	}
	// Cleanup: kill the spawned ping.
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// TestJob_HandleReturnsUnderlyingHandle exercises Job.Handle() — used
// by StartWithJob to thread the job handle through the attribute list.
func TestJob_HandleReturnsUnderlyingHandle(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()
	if job.Handle() == 0 {
		t.Error("Handle() returned 0; want non-zero")
	}
	// nil-receiver safety.
	var nilJob *Job
	if nilJob.Handle() != 0 {
		t.Error("nil receiver Handle() returned non-zero")
	}
}

// TestJob_HasMemberNilSafe exercises Job.HasMember nil-receiver and
// invalid-pid paths so the helper can be called defensively without
// crashing.
func TestJob_HasMemberNilSafe(t *testing.T) {
	var nilJob *Job
	if nilJob.HasMember(1) {
		t.Error("nil receiver HasMember returned true")
	}
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()
	// PID 0 is the system idle process — OpenProcess will fail; the
	// helper must return false rather than panic.
	if job.HasMember(0) {
		t.Error("HasMember(0) returned true; want false")
	}
}
