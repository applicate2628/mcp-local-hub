//go:build windows

package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"mcp-local-hub/internal/cli"
)

func TestWindowsConsolePolicyApplication(t *testing.T) {
	tests := []struct {
		name          string
		mode          windowsEntrypointMode
		policy        cli.WindowsConsolePolicy
		attachOK      bool
		allocateOK    bool
		wantAttach    int
		wantAllocate  int
		wantAcquired  bool
		wantFailureID string
	}{
		{"GUI calls no APIs", windowsEntrypointGUI, cli.WindowsConsoleDisabled, false, false, 0, 0, false, ""},
		{"background calls no APIs", windowsEntrypointBackground, cli.WindowsConsoleDisabled, false, false, 0, 0, false, ""},
		{"stdio protocol calls no APIs", windowsEntrypointStdioProtocol, cli.WindowsConsoleDisabled, false, false, 0, 0, false, ""},
		{"background ignores debug console prefix", windowsEntrypointBackground, cli.WindowsConsoleDebugExplicit, true, true, 0, 0, false, ""},
		{"stdio protocol ignores debug console prefix", windowsEntrypointStdioProtocol, cli.WindowsConsoleDebugExplicit, true, true, 0, 0, false, ""},
		{"explicit attaches to parent", windowsEntrypointGUI, cli.WindowsConsoleDebugExplicit, true, false, 1, 0, true, ""},
		{"explicit allocates without parent", windowsEntrypointGUI, cli.WindowsConsoleDebugExplicit, false, true, 1, 1, true, ""},
		{"explicit dual failure is typed", windowsEntrypointGUI, cli.WindowsConsoleDebugExplicit, false, false, 1, 1, false, WindowsDebugConsoleUnavailableID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attachCalls, allocateCalls := 0, 0
			api := windowsConsoleAPI{
				attachParent: func() error {
					attachCalls++
					if tc.attachOK {
						return nil
					}
					return errors.New("no parent console")
				},
				allocate: func() error {
					allocateCalls++
					if tc.allocateOK {
						return nil
					}
					return errors.New("allocation denied")
				},
				prepareDebug: func() {},
			}

			acquired, err := applyWindowsConsolePolicyWithAPI(tc.mode, tc.policy, api)
			if attachCalls != tc.wantAttach || allocateCalls != tc.wantAllocate {
				t.Fatalf("calls attach=%d allocate=%d, want attach=%d allocate=%d", attachCalls, allocateCalls, tc.wantAttach, tc.wantAllocate)
			}
			if acquired != tc.wantAcquired {
				t.Fatalf("acquired = %v, want %v", acquired, tc.wantAcquired)
			}
			if tc.wantFailureID == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantFailureID) {
				t.Fatalf("error = %v, want stable id %s", err, tc.wantFailureID)
			}
		})
	}

	t.Run("valid redirected handles are preserved", func(t *testing.T) {
		stdout, err := os.CreateTemp(t.TempDir(), "stdout-*.txt")
		if err != nil {
			t.Fatal(err)
		}
		defer stdout.Close()
		stderr, err := os.CreateTemp(t.TempDir(), "stderr-*.txt")
		if err != nil {
			t.Fatal(err)
		}
		defer stderr.Close()
		stdin, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		originalIn, originalOut, originalErr := stdin, stdout, stderr
		reopenIfInvalid("CONIN$", os.O_RDONLY, &stdin)
		reopenIfInvalid("CONOUT$", os.O_WRONLY, &stdout)
		reopenIfInvalid("CONOUT$", os.O_WRONLY, &stderr)
		if stdin != originalIn || stdout != originalOut || stderr != originalErr {
			t.Fatal("valid redirected standard handle was reopened")
		}
	})
}

func TestWindowsEntrypointConsolePolicyRepairsOnlyInvalidCLIOutputs(t *testing.T) {
	t.Run("valid inherited outputs need no console API", func(t *testing.T) {
		attachCalls, allocateCalls, prepareCalls, inspections := 0, 0, 0, 0
		api := windowsConsoleAPI{
			attachParent: func() error { attachCalls++; return nil },
			allocate:     func() error { allocateCalls++; return nil },
			prepareCLI:   func() { prepareCalls++ },
			outputsValid: func() (bool, bool) { inspections++; return true, true },
		}
		acquired, err := applyWindowsConsolePolicyWithAPI(windowsEntrypointCLI, cli.WindowsConsoleDisabled, api)
		if err != nil || acquired || attachCalls != 0 || allocateCalls != 0 || prepareCalls != 0 || inspections != 1 {
			t.Fatalf("valid outputs result=%v/%v calls=%d/%d/%d/%d, want false/nil/0/0/0/1", acquired, err, attachCalls, allocateCalls, prepareCalls, inspections)
		}
	})

	t.Run("invalid output attaches once and never allocates", func(t *testing.T) {
		attachCalls, allocateCalls, prepareCalls, inspections := 0, 0, 0, 0
		api := windowsConsoleAPI{
			attachParent: func() error { attachCalls++; return nil },
			allocate:     func() error { allocateCalls++; return nil },
			prepareCLI:   func() { prepareCalls++ },
			outputsValid: func() (bool, bool) {
				inspections++
				return inspections > 1, true
			},
		}
		acquired, err := applyWindowsConsolePolicyWithAPI(windowsEntrypointCLI, cli.WindowsConsoleDisabled, api)
		if err != nil || acquired || attachCalls != 1 || allocateCalls != 0 || prepareCalls != 1 || inspections != 2 {
			t.Fatalf("repair result=%v/%v calls attach/allocate/prepare/inspect=%d/%d/%d/%d, want false/nil/1/0/1/2", acquired, err, attachCalls, allocateCalls, prepareCalls, inspections)
		}
	})

	for _, tc := range []struct {
		name          string
		attachOK      bool
		secondStdout  bool
		wantPrepare   int
		wantFailureID string
	}{
		{name: "attach failure is typed", attachOK: false, secondStdout: false, wantPrepare: 0, wantFailureID: WindowsCLIStdioUnavailableID},
		{name: "repair still invalid is typed", attachOK: true, secondStdout: false, wantPrepare: 1, wantFailureID: WindowsCLIStdioUnavailableID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attachCalls, allocateCalls, prepareCalls, inspections := 0, 0, 0, 0
			api := windowsConsoleAPI{
				attachParent: func() error {
					attachCalls++
					if tc.attachOK {
						return nil
					}
					return errors.New("no parent console")
				},
				allocate:   func() error { allocateCalls++; return nil },
				prepareCLI: func() { prepareCalls++ },
				outputsValid: func() (bool, bool) {
					inspections++
					if inspections == 1 {
						return false, true
					}
					return tc.secondStdout, true
				},
			}
			acquired, err := applyWindowsConsolePolicyWithAPI(windowsEntrypointCLI, cli.WindowsConsoleDisabled, api)
			if acquired || err == nil || !strings.Contains(err.Error(), tc.wantFailureID) ||
				attachCalls != 1 || allocateCalls != 0 || prepareCalls != tc.wantPrepare {
				t.Fatalf("failure result=%v/%v calls attach/allocate/prepare=%d/%d/%d, want false/%s/1/0/%d", acquired, err, attachCalls, allocateCalls, prepareCalls, tc.wantFailureID, tc.wantPrepare)
			}
		})
	}
}

func TestWindowsOptedOutBareUsesCLIOutputAcquisition(t *testing.T) {
	t.Setenv(NoAutoGUIEnv, "1")
	mode := classifyWindowsEntrypointMode([]string{"mcphub"})
	attachCalls, allocateCalls, prepareCalls, inspections := 0, 0, 0, 0
	api := windowsConsoleAPI{
		attachParent: func() error { attachCalls++; return nil },
		allocate:     func() error { allocateCalls++; return nil },
		prepareCLI:   func() { prepareCalls++ },
		outputsValid: func() (bool, bool) {
			inspections++
			return inspections > 1, true
		},
	}
	acquired, err := applyWindowsConsolePolicyWithAPI(mode, cli.WindowsConsoleDisabled, api)
	if mode != windowsEntrypointCLI || err != nil || acquired ||
		attachCalls != 1 || allocateCalls != 0 || prepareCalls != 1 || inspections != 2 {
		t.Fatalf("opt-out mode/result=%v/%v/%v calls=%d/%d/%d/%d, want CLI/false/nil/1/0/1/2", mode, acquired, err, attachCalls, allocateCalls, prepareCalls, inspections)
	}
}
