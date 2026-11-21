//go:build linux

package autostart

import (
	"os"
	"strings"
	"testing"
)

// Literal expectations follow systemd.unit(5): %% represents one percent.
// They do not use the test-only firstSystemdToken unquoter, which does not
// implement systemd's separate specifier expansion.
func TestRenderLinuxUnit_PreservesLiteralPercent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		quoted string
	}{
		{"plain", "/opt/bin/mcphub", `"/opt/bin/mcphub"`},
		{"single", "/opt/100%/mcphub", `"/opt/100%%/mcphub"`},
		{"specifier", "/opt/release-%n/mcphub", `"/opt/release-%%n/mcphub"`},
		{"unknown", "/opt/release-%Z/mcphub", `"/opt/release-%%Z/mcphub"`},
		{"double", "/opt/100%%/mcphub", `"/opt/100%%%%/mcphub"`},
		{"mixed", `/home/Дмитрий/My %n "Tools"\bin/mcphub`, `"/home/Дмитрий/My %%n \"Tools\"\\bin/mcphub"`},
	} {
		for _, mode := range []struct {
			name   string
			strict bool
			suffix string
		}{
			{"normal", false, ""},
			{"strict", true, " --strict-mode"},
		} {
			t.Run(tc.name+"-"+mode.name, func(t *testing.T) {
				got := extractExecStartLine(t, renderLinuxUnit(tc.path, mode.strict))
				want := "ExecStart=" + tc.quoted + " supervise" + mode.suffix
				if got != want {
					t.Fatalf("SYSTEMD-PERCENT: ExecStart=%q, want %q", got, want)
				}
			})
		}
	}
}

func TestLinuxBackend_PercentPathRoundTripAndLegacyDrift(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)
	fs.responses["--user is-enabled mcphub-supervisor.service"] = systemctlCall{Stdout: "enabled\n"}
	fs.responses["--user is-active mcphub-supervisor.service"] = systemctlCall{Stdout: "active\n"}
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{MCPHubPath: "/opt/release-%n/mcphub"}
	if err := b.Enable(opts); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	const escaped = `ExecStart="/opt/release-%%n/mcphub" supervise`
	if got := extractExecStartLine(t, string(body)); got != escaped {
		t.Fatalf("SYSTEMD-PERCENT-WRITE: got %q, want %q", got, escaped)
	}
	current, err := b.StatusSnapshot(opts)
	if err != nil || current.State != StateEnabledRunning || current.SpecFingerprint == "" {
		t.Fatalf("SYSTEMD-PERCENT-STATUS: encoded unit not recognized: %+v, %v", current, err)
	}
	const legacy = `ExecStart="/opt/release-%n/mcphub" supervise`
	legacyBody := strings.Replace(string(body), escaped, legacy, 1)
	if err := os.WriteFile(unitPath, []byte(legacyBody), 0o600); err != nil {
		t.Fatal(err)
	}
	drifted, err := b.StatusSnapshot(opts)
	if err != nil || drifted.State != StateDrifted || drifted.SpecFingerprint == "" || drifted.SpecFingerprint == current.SpecFingerprint {
		t.Fatalf("SYSTEMD-PERCENT-DRIFT: legacy unit not distinguished: %+v, %v", drifted, err)
	}
}
