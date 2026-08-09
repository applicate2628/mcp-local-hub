//go:build windows && amd64

// Tests for splitCommandLineW + matchBasename. Tagged windows&&amd64
// to match probe_windows.go's tag — the implementation it tests is
// amd64-only because of the embedded PEB struct offsets.

package gui

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRetainedProcessID_WindowsSelfPinsWaitableHandle(t *testing.T) {
	identity, err := retainedProcessIDImpl(os.Getpid())
	if err != nil {
		t.Fatalf("retainedProcessIDImpl(self): %v", err)
	}
	if !identity.Alive || identity.Denied || identity.Handle == 0 {
		t.Fatalf("retained self identity = %+v, want live permitted retained handle", identity)
	}
	if err := identity.Close(); err != nil {
		t.Fatalf("close retained self identity: %v", err)
	}
	if err := identity.Close(); err != nil {
		t.Fatalf("second close retained self identity: %v", err)
	}
}

// TestSplitCommandLineW_TabSeparatorBetweenExeAndArgs locks in the
// Codex iter-4 P2 #1 fix: tabs must be honored as argv separators in
// the first-token loop, not just in the remaining-args loop. Pre-fix,
// `mcphub.exe<TAB>daemon` returned a single argv element and the
// no-arg GUI branch of cmdlineIsGui passed for a non-GUI subcommand.
func TestSplitCommandLineW_TabSeparatorBetweenExeAndArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "tab between exe and subcommand",
			in:   "mcphub.exe\tdaemon",
			want: []string{"mcphub.exe", "daemon"},
		},
		{
			name: "tab between exe and gui subcommand",
			in:   "mcphub.exe\tgui",
			want: []string{"mcphub.exe", "gui"},
		},
		{
			name: "space still works",
			in:   "mcphub.exe daemon",
			want: []string{"mcphub.exe", "daemon"},
		},
		{
			name: "no separator (Explorer no-arg launch)",
			in:   "mcphub.exe",
			want: []string{"mcphub.exe"},
		},
		{
			name: "quoted exe path with tab to next arg",
			in:   `"C:\Program Files\mcphub\mcphub.exe"` + "\tdaemon",
			want: []string{`C:\Program Files\mcphub\mcphub.exe`, "daemon"},
		},
		{
			name: "tab inside quoted argv[0] is preserved",
			in:   `"weird` + "\t" + `name.exe"` + "\tdaemon",
			want: []string{"weird\tname.exe", "daemon"},
		},
		// Codex bot review on PR #23 P3: empty quoted argv tokens
		// must be preserved (CommandLineToArgvW behavior). Without
		// this, len(argv)==1 misclassifies `mcphub.exe ""` as the
		// no-arg auto-gui case in cmdlineIsGui.
		{
			name: "empty quoted arg preserved",
			in:   `mcphub.exe ""`,
			want: []string{"mcphub.exe", ""},
		},
		{
			name: "two empty quoted args preserved",
			in:   `mcphub.exe "" ""`,
			want: []string{"mcphub.exe", "", ""},
		},
		{
			name: "empty quoted argv[0] preserved",
			in:   `"" daemon`,
			want: []string{"", "daemon"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCommandLineW(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitCommandLineW(%q) = %#v; want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Residual 1(a) review fix: classifyOpenProcessError must carry ambiguous
// platform errors as Indeterminate, never coerce them to Alive:false — only
// ERROR_INVALID_PARAMETER (verified empirically this session: OpenProcess on
// a real just-exited PID on this host returns exactly this error) may claim
// definitive death.
// ---------------------------------------------------------------------------

// TestClassifyOpenProcessError_InvalidParameterIsDefinitiveDead is the ONLY
// case that may report Alive:false.
func TestClassifyOpenProcessError_InvalidParameterIsDefinitiveDead(t *testing.T) {
	got, err := classifyOpenProcessError(windows.ERROR_INVALID_PARAMETER)
	if err != nil {
		t.Fatalf("err = %v, want nil (a definitive dead verdict reports no error)", err)
	}
	if got.Alive {
		t.Errorf("Alive = true, want false")
	}
	if got.Indeterminate {
		t.Errorf("Indeterminate = true, want false (this is the ONE definitive-dead case)")
	}
}

// TestClassifyOpenProcessError_AccessDeniedIsAliveDenied pins the pre-existing
// EPERM-mirroring behavior: access denied means the process EXISTS but we
// cannot query it — never Indeterminate, never dead.
func TestClassifyOpenProcessError_AccessDeniedIsAliveDenied(t *testing.T) {
	got, err := classifyOpenProcessError(windows.ERROR_ACCESS_DENIED)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !got.Alive || !got.Denied {
		t.Errorf("got = %+v, want Alive:true Denied:true", got)
	}
	if got.Indeterminate {
		t.Errorf("Indeterminate = true, want false")
	}
}

// TestClassifyOpenProcessError_EveryOtherErrorIsIndeterminate reproduces the
// residual 1(a) danger directly: BEFORE this fix, "Other errors ... → not
// alive" collapsed every non-access-denied OpenProcess failure into
// Alive:false — which single_instance.go's probeOnce turns into
// VerdictDeadPID, the ONLY class that authorizes a destructive
// relaunch/kill. A transient handle-exhaustion or any unrecognized future
// error must never reach that class.
//
// MUTATION: revert classifyOpenProcessError to return ProcessIdentity{Alive:
// false} for any error that is not ERROR_ACCESS_DENIED — every case below
// then reports Alive:true/Indeterminate:false-shaped wrongness and this
// test's "want Indeterminate:true, Alive:false" assertions fail.
func TestClassifyOpenProcessError_EveryOtherErrorIsIndeterminate(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"not enough quota", windows.ERROR_NOT_ENOUGH_QUOTA},
		{"not enough memory", windows.ERROR_NOT_ENOUGH_MEMORY},
		{"generic invalid handle", windows.ERROR_INVALID_HANDLE},
		{"unrecognized synthetic error", errors.New("injected ambiguous platform failure")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyOpenProcessError(tc.err)
			if got.Alive {
				t.Errorf("Alive = true, want false (an ambiguous error must never claim liveness either)")
			}
			if !got.Indeterminate {
				t.Errorf("Indeterminate = false, want true — this error must NOT be treated as proof of death")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("returned err = %v, want the original classified error preserved (%v)", err, tc.err)
			}
		})
	}
}
