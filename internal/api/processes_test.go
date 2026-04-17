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
