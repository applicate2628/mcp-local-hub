package reversedepgraph

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestRunnerArgvEnvironmentAllowlist(t *testing.T) {
	args := Args{Port: "zlib", VcpkgRoot: t.TempDir(), Triplet: "x64-windows", HostTriplet: "x64-windows", OverlayPorts: []string{t.TempDir()}, OverlayTriplets: []string{t.TempDir()}, ScratchRoot: t.TempDir()}
	command := DependInfoCommand(args, "curl", "dgml", t.TempDir())
	joined := strings.Join(command.Args, "\x00")
	for _, required := range []string{"depend-info", "curl", "--format=dgml", "--triplet=x64-windows", "--host-triplet=x64-windows", "--binarysource=clear", "--x-asset-sources=clear", "--classic"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("argv missing %q: %#v", required, command.Args)
		}
	}
	for _, env := range command.Env {
		upper := strings.ToUpper(env)
		if strings.HasPrefix(upper, "HTTP_PROXY=") || strings.HasPrefix(upper, "VCPKG_BINARY_SOURCES=") {
			t.Fatalf("ambient network/cache env leaked: %q", env)
		}
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(command.Executable), "vcpkg.exe") {
		t.Fatalf("windows executable = %q", command.Executable)
	}
	if err := ValidateArgs(context.Background(), args); err != nil {
		t.Fatalf("valid explicit args rejected: %v", err)
	}
}
