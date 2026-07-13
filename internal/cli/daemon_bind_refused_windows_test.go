//go:build windows

package cli

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

// forceExitFace mirrors cmd/mcphub/main.go's combined matcher: an error must
// satisfy BOTH ExitCode() and IsMcphubForceExit() for main.go to route it to
// os.Exit(code) rather than collapse to cobra's exit 1.
type forceExitFace interface {
	ExitCode() int
	IsMcphubForceExit() bool
}

func TestBindRefusedExit_WSAEACCES_MapsToExitBindRefused(t *testing.T) {
	// WSAEACCES (10013): the port is held by an established socket a
	// SO_EXCLUSIVEADDRUSE bind cannot share — the AdGuard-steals-9205 class.
	err := bindRefusedExit(fmt.Errorf("bind proxy: %w", windows.WSAEACCES))
	var fe forceExitFace
	if !errors.As(err, &fe) {
		t.Fatalf("WSAEACCES did not map to a force-exit error; got %v", err)
	}
	if fe.ExitCode() != exitBindRefused {
		t.Errorf("exit code = %d, want exitBindRefused (%d)", fe.ExitCode(), exitBindRefused)
	}
	// The concrete Winsock cause is preserved for the per-workspace log.
	if !errors.Is(err, windows.WSAEACCES) {
		t.Errorf("wrapped error lost the WSAEACCES cause")
	}
}

func TestBindRefusedExit_WSAEADDRINUSE_MapsToExitBindRefused(t *testing.T) {
	// WSAEADDRINUSE (10048): the port already has a listener.
	err := bindRefusedExit(fmt.Errorf("http server: %w", windows.WSAEADDRINUSE))
	var fe forceExitFace
	if !errors.As(err, &fe) {
		t.Fatalf("WSAEADDRINUSE did not map to a force-exit error; got %v", err)
	}
	if fe.ExitCode() != exitBindRefused {
		t.Errorf("exit code = %d, want exitBindRefused (%d)", fe.ExitCode(), exitBindRefused)
	}
}

func TestBindRefusedExit_OtherError_PassesThrough(t *testing.T) {
	// A non-bind failure must keep cobra's exit-1 behavior (NOT a force-exit
	// error) so the supervisor treats it as a genuine crash, not a self-heal.
	orig := errors.New("manifest load failed")
	got := bindRefusedExit(orig)
	var fe forceExitFace
	if errors.As(got, &fe) {
		t.Fatalf("a non-bind error must NOT map to a force-exit error; got exit %d", fe.ExitCode())
	}
	if got != orig {
		t.Errorf("bindRefusedExit mutated a non-bind error: got %v, want the original", got)
	}
}

func TestBindRefusedExit_Nil_PassesThrough(t *testing.T) {
	if got := bindRefusedExit(nil); got != nil {
		t.Errorf("bindRefusedExit(nil) = %v, want nil", got)
	}
}

// exitBindRefused must be distinct from cobra's default error exit (1) and the
// Go runtime's panic/flag exit (2), so the supervisor can tell a stolen-port
// bind refusal apart from a genuine crash on the daemon-proxy process.
func TestExitBindRefused_DistinctFromGenericExits(t *testing.T) {
	if exitBindRefused == 1 || exitBindRefused == 2 {
		t.Fatalf("exitBindRefused = %d collides with a generic exit code (1=cobra, 2=runtime)", exitBindRefused)
	}
}
