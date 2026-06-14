package gui

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// supervisorTestImagePath returns an mcphub binary path whose basename
// satisfies the per-OS matchBasename (mcphub.exe on Windows, mcphub on
// POSIX) so the image gate passes on every test runner.
func supervisorTestImagePath() string {
	if runtime.GOOS == "windows" {
		return `C:\Program Files\mcphub\mcphub.exe`
	}
	return "/usr/local/bin/mcphub"
}

// foreignImagePath returns a non-mcphub binary path so the image gate
// (matchBasename) refuses regardless of OS.
func foreignImagePath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32\notepad.exe`
	}
	return "/usr/bin/python3"
}

// These tests cover the kill-target identity gate added to
// killSupervisorProcess (POST /api/supervisor/restart). The gate proves
// the recorded supervisor.lock PID is still the live mcphub supervisor
// before the force-kill fires, so a recycled / reused PID is never
// killed. The probe is injected via the processIDOverride seam and the
// kill is observed via the killSupervisorPIDFn seam, so NO real process
// is ever signalled.

// swapSupervisorKillProbe installs a processIDOverride for the test
// scope and restores the prior value on cleanup.
func swapSupervisorKillProbe(t *testing.T, fn func(pid int) (ProcessIdentity, error)) {
	t.Helper()
	orig := processIDOverride
	t.Cleanup(func() { processIDOverride = orig })
	processIDOverride = fn
}

// captureSupervisorKill installs a kill spy for the test scope. The
// returned pointer records the PID the kill was invoked with (0 if the
// kill never fired — i.e. the gate refused).
func captureSupervisorKill(t *testing.T) *int {
	t.Helper()
	killed := 0
	orig := killSupervisorPIDFn
	t.Cleanup(func() { killSupervisorPIDFn = orig })
	killSupervisorPIDFn = func(pid int) error {
		killed = pid
		return nil
	}
	return &killed
}

func TestKillSupervisorProcess_RealSupervisor_Killed(t *testing.T) {
	// The recorded PID is a live mcphub supervisor whose process started
	// BEFORE the sidecar started_at → the gate must authorize the kill.
	started := time.Now().UTC()
	swapSupervisorKillProbe(t, func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{
			Alive:     true,
			ImagePath: supervisorTestImagePath(),
			Cmdline:   []string{supervisorTestImagePath(), "supervise"},
			StartTime: started.Add(-5 * time.Second),
		}, nil
	})
	killed := captureSupervisorKill(t)

	if err := killSupervisorProcess(4321, started.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("killSupervisorProcess on a real supervisor PID returned error: %v", err)
	}
	if *killed != 4321 {
		t.Fatalf("expected PID 4321 to be killed, got killed=%d", *killed)
	}
}

func TestKillSupervisorProcess_RecycledPID_Refused(t *testing.T) {
	// A crashed supervisor's PID was REUSED by an unrelated process: the
	// image basename + argv match by coincidence is impossible here —
	// the recycled process began AFTER the sidecar was written. Gate 3
	// (start-time precedes started_at) must refuse, and NOTHING is killed.
	started := time.Now().UTC()
	swapSupervisorKillProbe(t, func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{
			Alive:     true,
			ImagePath: supervisorTestImagePath(),
			Cmdline:   []string{supervisorTestImagePath(), "supervise"},
			// Started AFTER the sidecar → PID-recycled.
			StartTime: started.Add(10 * time.Second),
		}, nil
	})
	killed := captureSupervisorKill(t)

	err := killSupervisorProcess(4321, started.Format(time.RFC3339Nano))
	if err == nil {
		t.Fatalf("expected refusal error for a recycled PID, got nil")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected a 'refused' refusal error, got: %v", err)
	}
	if *killed != 0 {
		t.Fatalf("recycled PID must NOT be killed, but kill fired on PID %d", *killed)
	}
}

func TestKillSupervisorProcess_ForeignSubcommand_Refused(t *testing.T) {
	// PID reused by another mcphub subcommand (e.g. `mcphub status`) →
	// Gate 2 (argv[1] == "supervise") refuses; nothing killed.
	started := time.Now().UTC()
	swapSupervisorKillProbe(t, func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{
			Alive:     true,
			ImagePath: supervisorTestImagePath(),
			Cmdline:   []string{supervisorTestImagePath(), "status"},
			StartTime: started.Add(-5 * time.Second),
		}, nil
	})
	killed := captureSupervisorKill(t)

	err := killSupervisorProcess(4321, started.Format(time.RFC3339Nano))
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected refusal for a non-supervise subcommand, got: %v", err)
	}
	if *killed != 0 {
		t.Fatalf("foreign-subcommand PID must NOT be killed, but kill fired on PID %d", *killed)
	}
}

func TestKillSupervisorProcess_ForeignImage_Refused(t *testing.T) {
	// PID reused by a non-mcphub binary → Gate 1 (image basename)
	// refuses; nothing killed.
	started := time.Now().UTC()
	swapSupervisorKillProbe(t, func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{
			Alive:     true,
			ImagePath: foreignImagePath(),
			Cmdline:   []string{foreignImagePath(), "supervise"},
			StartTime: started.Add(-5 * time.Second),
		}, nil
	})
	killed := captureSupervisorKill(t)

	err := killSupervisorProcess(4321, started.Format(time.RFC3339Nano))
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected refusal for a foreign image, got: %v", err)
	}
	if *killed != 0 {
		t.Fatalf("foreign-image PID must NOT be killed, but kill fired on PID %d", *killed)
	}
}

func TestKillSupervisorProcess_DeadPID_Refused(t *testing.T) {
	// Recorded PID is already gone (Alive=false) → benign refusal,
	// nothing killed.
	started := time.Now().UTC()
	swapSupervisorKillProbe(t, func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{Alive: false}, nil
	})
	killed := captureSupervisorKill(t)

	err := killSupervisorProcess(4321, started.Format(time.RFC3339Nano))
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected refusal for a dead PID, got: %v", err)
	}
	if *killed != 0 {
		t.Fatalf("dead PID must NOT be killed, but kill fired on PID %d", *killed)
	}
}

func TestKillSupervisorProcess_ProbeError_Propagated_NotKilled(t *testing.T) {
	// A transient identity-probe failure means we CANNOT prove the PID is
	// the supervisor → propagate as an error and kill NOTHING (never
	// guess).
	started := time.Now().UTC()
	swapSupervisorKillProbe(t, func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{}, errors.New("simulated transient identity probe stall")
	})
	killed := captureSupervisorKill(t)

	err := killSupervisorProcess(4321, started.Format(time.RFC3339Nano))
	if err == nil {
		t.Fatalf("expected a propagated probe error, got nil")
	}
	if !strings.Contains(err.Error(), "could not verify") {
		t.Fatalf("expected a 'could not verify' probe error, got: %v", err)
	}
	if *killed != 0 {
		t.Fatalf("an unprovable target must NOT be killed, but kill fired on PID %d", *killed)
	}
}

func TestKillSupervisorProcess_MissingStartedAt_Refused(t *testing.T) {
	// A missing/unparseable started_at cannot anchor Gate 3 → fail closed
	// (refuse) even when image + argv match, since a recycled PID can't
	// be ruled out.
	swapSupervisorKillProbe(t, func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{
			Alive:     true,
			ImagePath: supervisorTestImagePath(),
			Cmdline:   []string{supervisorTestImagePath(), "supervise"},
			StartTime: time.Now().UTC().Add(-5 * time.Second),
		}, nil
	})
	killed := captureSupervisorKill(t)

	err := killSupervisorProcess(4321, "")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected refusal when started_at is missing, got: %v", err)
	}
	if *killed != 0 {
		t.Fatalf("missing-started_at target must NOT be killed, but kill fired on PID %d", *killed)
	}
}

func TestKillSupervisorProcess_NonPositivePID_NoProbe(t *testing.T) {
	// PID <= 0 is rejected up front and never reaches the probe or kill.
	probed := false
	swapSupervisorKillProbe(t, func(pid int) (ProcessIdentity, error) {
		probed = true
		return ProcessIdentity{Alive: true}, nil
	})
	killed := captureSupervisorKill(t)

	if err := killSupervisorProcess(0, time.Now().UTC().Format(time.RFC3339Nano)); err == nil {
		t.Fatalf("expected error for PID 0, got nil")
	}
	if probed {
		t.Fatalf("PID 0 must short-circuit before the identity probe")
	}
	if *killed != 0 {
		t.Fatalf("PID 0 must never be killed, but kill fired on PID %d", *killed)
	}
}

// cmdlineIsSupervise unit coverage — argv[1] must be exactly "supervise".
func TestCmdlineIsSupervise(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"mcphub", "supervise"}, true},
		{[]string{"mcphub", "supervise", "--strict-mode"}, true},
		{[]string{"mcphub", "status"}, false},
		{[]string{"mcphub", "supervise-foo"}, false}, // substring, not token
		{[]string{"mcphub"}, false},                  // no argv[1]
		{nil, false},
	}
	for _, tc := range cases {
		if got := cmdlineIsSupervise(tc.argv); got != tc.want {
			t.Errorf("cmdlineIsSupervise(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}
