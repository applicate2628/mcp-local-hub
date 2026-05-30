package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
)

func TestGuiCmd_HelpIncludesFlags(t *testing.T) {
	cmd := newGuiCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// PR #214 closes QA-r6-Gap-4 by asserting --strict-mode appears in
	// the user-visible help. The flag was added by PR #212 r1
	// (gui_supervisor_owner.go) but the help-text test never updated;
	// a future regression that drops the cobra registration would
	// have gone unnoticed without this assertion.
	for _, want := range []string{"--port", "--no-browser", "--no-tray", "--strict-mode"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--help missing %q; got %q", want, buf.String())
		}
	}
	// --force is intentionally hidden (Phase 3B-II placeholder); --help must NOT advertise it.
	if strings.Contains(buf.String(), "--force") {
		t.Errorf("--help unexpectedly advertises --force; should be hidden until take-over is implemented")
	}
}

// TestGuiCmd_ForceFlagStillParseable confirms `--force` is hidden but
// remains a valid flag (parseable, not removed). Hiding via MarkHidden
// keeps the wiring in place for Phase 3B-II without breaking any
// scripted callers that already pass --force.
func TestGuiCmd_ForceFlagStillParseable(t *testing.T) {
	cmd := newGuiCmdReal()
	if cmd.Flags().Lookup("force") == nil {
		t.Fatal("--force flag should still be defined (just hidden)")
	}
	if !cmd.Flags().Lookup("force").Hidden {
		t.Error("--force should be marked hidden")
	}
}

// TestResolveGuiPort pins bug-bash A5 (#18/#19/#20) closure: effective
// port follows --port flag when explicitly passed, otherwise reads
// `gui_server.port` settings, otherwise 0 (auto-pick). Pre-fix, the
// persisted setting was cosmetic — startup ignored it.
func TestResolveGuiPort(t *testing.T) {
	cases := []struct {
		name         string
		flagChanged  bool
		flagValue    int
		settingValue string
		want         int
	}{
		// 1. Flag explicitly set wins.
		{"explicit --port 9200 wins over setting 9125", true, 9200, "9125", 9200},
		{"explicit --port 0 wins (operator wants ephemeral)", true, 0, "9125", 0},
		// 2. Flag not set + valid setting → use setting.
		{"no flag, valid setting 9125", false, 0, "9125", 9125},
		{"no flag, setting 1024 (min)", false, 0, "1024", 1024},
		{"no flag, setting 65535 (max)", false, 0, "65535", 65535},
		{"no flag, setting with whitespace", false, 0, " 9125 ", 9125},
		// 3. Flag not set + invalid/empty setting → 0 (auto-pick).
		{"no flag, empty setting", false, 0, "", 0},
		{"no flag, non-numeric setting", false, 0, "abc", 0},
		{"no flag, below privileged-port boundary", false, 0, "80", 0},
		{"no flag, above 65535", false, 0, "70000", 0},
		{"no flag, negative setting", false, 0, "-1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGuiPort(tc.flagChanged, tc.flagValue, tc.settingValue)
			if got != tc.want {
				t.Errorf("resolveGuiPort(flagChanged=%v, flagValue=%d, setting=%q) = %d, want %d",
					tc.flagChanged, tc.flagValue, tc.settingValue, got, tc.want)
			}
		})
	}
}

func TestRunSessionCleanupTicker_ExpiresOldSessions(t *testing.T) {
	oldNow := time.Now().Add(-2 * time.Hour)
	sessions := serena_routing.NewSessionRouterWithClock(func() time.Time { return oldNow })
	sessions.BindSession("s1", &api.WorkspaceEntry{WorkspacePath: "alpha"})
	sessions.BindSession("s2", &api.WorkspaceEntry{WorkspacePath: "beta"})
	if got := sessions.Len(); got != 2 {
		t.Fatalf("Len before cleanup = %d, want 2", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// nil Server: this test exercises only the sticky-router sweep;
		// SweepSerenaSessions is covered by the gui-package unit test.
		runSessionCleanupTicker(ctx, nil, sessions, 10*time.Millisecond, time.Hour)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runSessionCleanupTicker did not exit after ctx cancel")
		}
	})

	deadline := time.After(time.Second)
	for sessions.Len() != 0 {
		select {
		case <-deadline:
			t.Fatalf("Len after cleanup ticker = %d, want 0", sessions.Len())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
