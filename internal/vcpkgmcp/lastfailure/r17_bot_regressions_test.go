package lastfailure

import (
	"strings"
	"testing"
)

func TestR17WrapperPreservesWindowsPathsOnPOSIXHost(t *testing.T) {
	data := []byte("command: C:\\vcpkg\\vcpkg.exe install zlib --overlay-ports C:\\plain\\ports --x-buildtrees-root C:\\buildtrees\n")
	info, ok, err := parseWrapperContentWithLimitsForGOOS(data, defaultResponseLimits, "linux")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if info.BuildtreesRoot != `C:\buildtrees` {
		t.Fatalf("buildtrees_root=%q, want preserved Windows path", info.BuildtreesRoot)
	}
	if len(info.OverlayPorts) != 1 || info.OverlayPorts[0] != `C:\plain\ports` {
		t.Fatalf("overlay_ports=%q, want preserved Windows path", info.OverlayPorts)
	}
}

func TestR17GNUDriverDiagnosticsRetainLinkerCause(t *testing.T) {
	lines := []string{
		"/usr/bin/ld: object.o: undefined reference to `missing_symbol'",
		"collect2: error: ld returned 1 exit status",
	}
	diagnostics := ScanDiagnostics([]byte(strings.Join(lines, "\n") + "\n"))
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics=%+v, want linker cause and aggregate driver line", diagnostics)
	}
	if diagnostics[0].Tier != TierSpecific || diagnostics[0].Severity != SeverityError || diagnostics[0].Text != lines[0] {
		t.Fatalf("first diagnostic=%+v, want GNU ld cause as specific error", diagnostics[0])
	}
	if diagnostics[1].Tier != TierAggregate || diagnostics[1].Severity != SeverityError || diagnostics[1].Text != lines[1] {
		t.Fatalf("second diagnostic=%+v, want collect2 exit summary as aggregate error", diagnostics[1])
	}
}
