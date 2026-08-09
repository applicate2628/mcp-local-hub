package lastfailure

import (
	"io"
	"os"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

type noWrapperIOFS struct {
	openCalls int
}

func (f *noWrapperIOFS) Stat(string) (os.FileInfo, error)  { return nil, os.ErrNotExist }
func (f *noWrapperIOFS) OpenDir(string) (DirReader, error) { return nil, os.ErrNotExist }
func (f *noWrapperIOFS) Open(string) (io.ReadCloser, error) {
	f.openCalls++
	return nil, os.ErrNotExist
}

func TestLastFailureRejectsRelativeWrapperBeforeIO(t *testing.T) {
	fsys := &noWrapperIOFS{}
	deps := testDeps()
	deps.FS = fsys
	res := LastFailure(Args{BuildFailedLog: "relative/build_failed.log"}, deps)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonRelativeRoot {
		t.Fatalf("result = status=%v reason=%v, want unknown/%s", res.Status, res.Reason, ReasonRelativeRoot)
	}
	if fsys.openCalls != 0 {
		t.Fatalf("relative wrapper caused %d filesystem opens; admission must reject before I/O", fsys.openCalls)
	}
}

func TestSplitRecordedCommandLineUsesPOSIXQuoting(t *testing.T) {
	command := "vcpkg install demo --x-buildtrees-root '/tmp/build trees' --triplet x64-linux"
	argv, err := splitRecordedCommandLine(command, "linux")
	if err != nil {
		t.Fatalf("splitRecordedCommandLine: %v", err)
	}
	if got := commandFlagValue(argv, "--x-buildtrees-root"); got != "/tmp/build trees" {
		t.Fatalf("buildtrees root = %q, want POSIX single-quoted path", got)
	}
	if _, err := splitRecordedCommandLine("vcpkg install 'unterminated", "linux"); err == nil {
		t.Fatal("unterminated POSIX quote was accepted")
	}
}

func TestWrapperMetadataUsesInjectedCommandPlatform(t *testing.T) {
	data := []byte("command: vcpkg install demo --x-buildtrees-root '/tmp/build trees' --triplet x64-linux\n")
	info, ok, err := parseWrapperContentWithLimitsForGOOS(data, defaultResponseLimits, "linux")
	if err != nil || !ok {
		t.Fatalf("parse wrapper: ok=%v err=%v", ok, err)
	}
	if info.BuildtreesRoot != "/tmp/build trees" || info.Triplet != "x64-linux" {
		t.Fatalf("wrapper metadata = %+v, want POSIX-quoted root and triplet", info)
	}
}

func TestRedactResultCommandsCoversEveryWireCommand(t *testing.T) {
	const marker = "opaque-marker-7f3a"
	r := Result{
		ExactCommand: "vcpkg install demo --token=" + marker + " --overlay-ports=https://user:" + marker + "@host/repo",
		BuildCommand: "cmake --password " + marker,
	}
	r.Evidence.Commands = []string{
		"vcpkg install demo --access-token " + marker,
		"git fetch https://host/repo?token=" + marker,
	}
	redacted := redactResultCommands(r)
	for name, command := range map[string]string{
		"exact_command": redacted.ExactCommand,
		"build_command": redacted.BuildCommand,
		"evidence[0]":   redacted.Evidence.Commands[0],
		"evidence[1]":   redacted.Evidence.Commands[1],
	} {
		if strings.Contains(command, marker) {
			t.Fatalf("%s leaked credential: %q", name, command)
		}
		if !strings.Contains(command, "REDACTED") {
			t.Fatalf("%s did not make redaction visible: %q", name, command)
		}
	}
}
