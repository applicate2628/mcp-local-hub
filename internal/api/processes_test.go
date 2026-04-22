package api

import (
	"strings"
	"testing"
)

// TestCountProcessesHandlesEmptyInput verifies the parser returns (0, nil)
// on blank wmic output — zero processes matching, no error.
func TestCountProcessesHandlesEmptyInput(t *testing.T) {
	got, err := parseWmicCount(strings.NewReader(""), []string{"memory", "server-memory"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// TestCountProcessesMatchesSubstrings verifies a line containing any of the
// patterns counts once; a line containing multiple patterns still counts once.
func TestCountProcessesMatchesSubstrings(t *testing.T) {
	wmicCsv := `Node,CommandLine,ProcessId,WorkingSetSize
HOST,"npx -y @modelcontextprotocol/server-memory",1234,41000000
HOST,"node server-memory/dist/index.js",5678,40000000
HOST,"some-other-process",9999,1000000
`
	got, err := parseWmicCount(strings.NewReader(wmicCsv), []string{"server-memory"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("got %d, want 2 (both lines mention server-memory)", got)
	}
}

func TestNetstatLinePIDForLoopbackPort_ExactPortMatch(t *testing.T) {
	line := "  TCP    127.0.0.1:9121         0.0.0.0:0              LISTENING       1234"
	pid, ok := netstatLinePIDForLoopbackPort(line, 9121)
	if !ok {
		t.Fatalf("expected exact match, got no match")
	}
	if pid != 1234 {
		t.Fatalf("expected pid 1234, got %d", pid)
	}
}

func TestNetstatLinePIDForLoopbackPort_DoesNotMatchPortPrefix(t *testing.T) {
	line := "  TCP    127.0.0.1:91210        0.0.0.0:0              LISTENING       4242"
	if pid, ok := netstatLinePIDForLoopbackPort(line, 9121); ok {
		t.Fatalf("expected no match for prefix port, got pid %d", pid)
	}
}

func TestNetstatLinePIDForLoopbackPort_DoesNotMatchNonLoopback(t *testing.T) {
	line := "  TCP    0.0.0.0:9121           0.0.0.0:0              LISTENING       777"
	if pid, ok := netstatLinePIDForLoopbackPort(line, 9121); ok {
		t.Fatalf("expected no match for non-loopback, got pid %d", pid)
	}
}
