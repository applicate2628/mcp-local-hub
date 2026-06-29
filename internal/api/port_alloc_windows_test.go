//go:build windows

package api

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"mcp-local-hub/internal/config"
)

const netshExcludedRangeFixture = `
Protocol tcp Port Exclusion Ranges

Start Port    End Port
----------    --------
      9201        9202

* - Administered port exclusions.
`

func resetWindowsExcludedTCPPortRangesForTest() {
	windowsExcludedTCPPortRangesOnce = sync.Once{}
	windowsExcludedTCPPortRangesCache = nil
	windowsExcludedTCPPortRangesErr = nil
}

func stubWindowsExcludedTCPPortRanges(t *testing.T, output string) {
	t.Helper()
	origQuery := queryWindowsExcludedTCPPortRanges
	origExcluded := excludedTCPPortRanges
	resetWindowsExcludedTCPPortRangesForTest()
	excludedTCPPortRanges = osExcludedTCPPortRanges
	queryWindowsExcludedTCPPortRanges = func() ([]byte, error) {
		return []byte(output), nil
	}
	t.Cleanup(func() {
		queryWindowsExcludedTCPPortRanges = origQuery
		excludedTCPPortRanges = origExcluded
		resetWindowsExcludedTCPPortRangesForTest()
	})
}

func TestAllocatePort_WindowsExcludedRangesSkippedAndReported(t *testing.T) {
	t.Run("skips excluded ports without bind probing them", func(t *testing.T) {
		stubWindowsExcludedTCPPortRanges(t, netshExcludedRangeFixture)
		origAvail := portAvailable
		defer func() { portAvailable = origAvail }()
		probed := map[int]bool{}
		portAvailable = func(port int) bool {
			probed[port] = true
			return true
		}
		reg := NewRegistry(t.TempDir() + "/reg.yaml")
		reg.Put(WorkspaceEntry{WorkspaceKey: "a", Language: "python", Port: 9200})

		got, err := AllocatePort(reg, config.PortPool{Start: 9200, End: 9203})
		if err != nil {
			t.Fatalf("AllocatePort: %v", err)
		}
		if got != 9203 {
			t.Fatalf("AllocatePort = %d, want 9203 after registry port 9200 and Windows-excluded 9201-9202", got)
		}
		for _, p := range []int{9201, 9202} {
			if probed[p] {
				t.Fatalf("portAvailable probed Windows-excluded port %d; exclusions must be skipped before bind probing", p)
			}
		}
		if !probed[9203] {
			t.Fatal("portAvailable did not probe the first non-excluded candidate 9203")
		}
	})

	t.Run("reports OS exclusion capacity when only usable ports are taken", func(t *testing.T) {
		stubWindowsExcludedTCPPortRanges(t, netshExcludedRangeFixture)
		origAvail := portAvailable
		defer func() { portAvailable = origAvail }()
		portAvailable = func(int) bool { return true }
		reg := NewRegistry(t.TempDir() + "/reg.yaml")
		reg.Put(WorkspaceEntry{WorkspaceKey: "a", Language: "python", Port: 9200})
		reg.Put(WorkspaceEntry{WorkspaceKey: "b", Language: "go", Port: 9203})

		_, err := AllocatePort(reg, config.PortPool{Start: 9200, End: 9203})
		if err == nil {
			t.Fatal("expected ErrPortPoolExhausted")
		}
		if !errors.Is(err, ErrPortPoolExhausted) {
			t.Fatalf("error should unwrap to ErrPortPoolExhausted; got: %v", err)
		}
		msg := err.Error()
		for _, want := range []string{"Windows excluded TCP port ranges", "9201-9202", "4 total", "2 OS-excluded", "2 usable"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q missing %q", msg, want)
			}
		}
	})
}
