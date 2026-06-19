package api

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestLauncherGuidance_KnownLaunchersAreActionable(t *testing.T) {
	for _, cmd := range []string{"uvx", "npx", "node", "python", "go", "mcp-language-server", "mcphub"} {
		disp, fix := LauncherGuidance(cmd)
		if disp == "" || fix == "" {
			t.Errorf("LauncherGuidance(%q) = (%q,%q); both must be non-empty", cmd, disp, fix)
		}
		if !strings.Contains(fix, "`") {
			t.Errorf("LauncherGuidance(%q) fix=%q names no concrete command (no backtick) — not actionable", cmd, fix)
		}
	}
}

func TestLauncherGuidance_UnknownLauncherStillActionable(t *testing.T) {
	disp, fix := LauncherGuidance("totally-made-up-launcher-xyz")
	if disp == "" || !strings.Contains(fix, "PATH") {
		t.Errorf("unknown launcher fallback not actionable: disp=%q fix=%q", disp, fix)
	}
}

func TestCheckServerReadiness_MissingLauncherReportedWithFix(t *testing.T) {
	m := &config.ServerManifest{Name: "demo", Command: "definitely-not-on-path-zzz"}
	rep := CheckServerReadiness(m)
	if rep.Ready {
		t.Fatalf("report Ready=true for a missing launcher; want false")
	}
	var found bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "launcher:") {
			found = true
			if r.OK {
				t.Errorf("launcher requirement OK=true for missing launcher")
			}
			if r.Fix == "" {
				t.Errorf("launcher requirement has empty Fix (not actionable)")
			}
		}
	}
	if !found {
		t.Fatalf("no launcher requirement in report: %+v", rep.Requirements)
	}
}

func TestCheckServerReadiness_PresentLauncherReady(t *testing.T) {
	// `go` is on PATH in every Go test environment (same assumption the
	// install-test fake-manifest fixture relies on).
	m := &config.ServerManifest{Name: "demo", Command: "go"}
	rep := CheckServerReadiness(m)
	if !rep.Ready {
		t.Fatalf("report Ready=false for present launcher `go`: %+v", rep.Requirements)
	}
}
