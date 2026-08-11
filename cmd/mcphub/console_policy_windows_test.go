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
		policy        cli.WindowsConsolePolicy
		attachOK      bool
		allocateOK    bool
		wantAttach    int
		wantAllocate  int
		wantAcquired  bool
		wantFailureID string
	}{
		{"disabled calls no APIs", cli.WindowsConsoleDisabled, false, false, 0, 0, false, ""},
		{"explicit attaches to parent", cli.WindowsConsoleDebugExplicit, true, false, 1, 0, true, ""},
		{"explicit allocates without parent", cli.WindowsConsoleDebugExplicit, false, true, 1, 1, true, ""},
		{"explicit dual failure is typed", cli.WindowsConsoleDebugExplicit, false, false, 1, 1, false, WindowsDebugConsoleUnavailableID},
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
				prepare: func() {},
			}

			acquired, err := applyWindowsConsolePolicyWithAPI(tc.policy, api)
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
