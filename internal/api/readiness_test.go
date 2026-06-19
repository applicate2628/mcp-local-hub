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
	// `go` is on PATH in every Go test environment. Assert the LAUNCHER
	// requirement is OK; overall Ready also depends on the canonical mcphub
	// binary + free ports, which are env-dependent in a test sandbox.
	m := &config.ServerManifest{Name: "demo", Command: "go"}
	rep := CheckServerReadiness(m)
	var launcherOK, found bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "launcher:") {
			found, launcherOK = true, r.OK
		}
	}
	if !found || !launcherOK {
		t.Fatalf("launcher requirement not OK for present `go`: %+v", rep.Requirements)
	}
}

func TestCheckServerReadinessByName_EmbeddedServer(t *testing.T) {
	// "memory" is an embedded manifest (command: npx). Resolves without
	// state/network. Its readiness must include a launcher requirement.
	rep, err := CheckServerReadinessByName("memory")
	if err != nil {
		t.Fatalf("CheckServerReadinessByName(memory): %v", err)
	}
	if rep.Server != "memory" {
		t.Errorf("report Server=%q, want memory", rep.Server)
	}
	var hasLauncher bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "launcher:") {
			hasLauncher = true
		}
	}
	if !hasLauncher {
		t.Errorf("memory readiness has no launcher requirement: %+v", rep.Requirements)
	}
}

func TestCheckServerReadinessByName_UnknownServerErrors(t *testing.T) {
	if _, err := CheckServerReadinessByName("no-such-server-zzz"); err == nil {
		t.Fatal("CheckServerReadinessByName(unknown) returned nil error; want resolve error")
	}
}

func TestAllServerReadiness_CoversEmbeddedServers(t *testing.T) {
	reports := AllServerReadiness()
	if len(reports) < 5 {
		t.Fatalf("AllServerReadiness returned %d reports; want >= 5 embedded servers", len(reports))
	}
	for _, rep := range reports {
		if rep.Server == "" {
			t.Errorf("report with empty Server name: %+v", rep)
		}
	}
}

func TestCheckServerReadiness_UnsetSecretIsOptionalNotBlocking(t *testing.T) {
	m := &config.ServerManifest{
		Name:    "demo",
		Command: "go", // on PATH in every Go test env
		Env:     map[string]string{"DEMO_KEY": "secret:demo_unset_key_zzz"},
	}
	rep := CheckServerReadiness(m)
	// The unset secret must be ADVISORY (Optional) so it does not block
	// readiness. (Overall Ready also depends on env-dependent mcphub/ports, so
	// assert the secret requirement's Optional flag — the real claim — not the
	// aggregate.)
	var secretReq *ReadinessRequirement
	for i := range rep.Requirements {
		if strings.HasPrefix(rep.Requirements[i].Name, "secret:") {
			secretReq = &rep.Requirements[i]
		}
	}
	if secretReq == nil {
		t.Fatalf("no per-key secret requirement in report: %+v", rep.Requirements)
	}
	if !secretReq.Optional {
		t.Errorf("secret requirement Optional=false; want true (advisory, not a blocker)")
	}
	if secretReq.OK {
		t.Errorf("unset secret OK=true; want false so the GUI prompts to fill it")
	}
	if secretReq.Fix == "" {
		t.Errorf("unset secret has no Fix guidance for the inline prompt")
	}
}

func TestCheckServerReadiness_RequiredBinariesSurfaced(t *testing.T) {
	m := &config.ServerManifest{
		Name:             "demo",
		Command:          "mcphub",
		RequiredBinaries: []string{"definitely-absent-binary-zzz"},
	}
	rep := CheckServerReadiness(m)
	var found bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "binary:") {
			found = true
			if r.OK {
				t.Errorf("absent required binary reported OK=true")
			}
			if r.Fix == "" {
				t.Errorf("absent binary has no Fix guidance")
			}
		}
	}
	if !found {
		t.Fatalf("no binary requirement for declared required_binaries: %+v", rep.Requirements)
	}
}

func TestCheckServerReadiness_RemoteHTTPSecretIsBlocking(t *testing.T) {
	rm := &config.ServerManifest{
		Name:      "remote-demo",
		Transport: config.TransportRemoteHTTP,
		URL:       "https://example.com/mcp",
		Headers:   map[string]string{"Authorization": "Bearer ${secret:demo_remote_token_zzz}"},
	}
	rep := CheckServerReadiness(rm)
	var found bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "secret (remote):") {
			found = true
			if r.Optional {
				t.Errorf("remote-http secret marked Optional; remote secrets are install-blocking")
			}
			if r.OK {
				t.Errorf("unset remote-http secret reported OK=true")
			}
		}
	}
	if !found {
		t.Fatalf("no remote secret requirement for ${secret:} in headers: %+v", rep.Requirements)
	}
}
