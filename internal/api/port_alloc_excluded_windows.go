//go:build windows

package api

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"mcp-local-hub/internal/process"
)

var (
	windowsExcludedTCPPortRangesOnce  sync.Once
	windowsExcludedTCPPortRangesCache []tcpPortRange
	windowsExcludedTCPPortRangesErr   error
	queryWindowsExcludedTCPPortRanges = func() ([]byte, error) {
		cmd := newExcludedPortNetshCommand("netsh", "int", "ipv4", "show", "excludedportrange", "protocol=tcp")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("netsh int ipv4 show excludedportrange protocol=tcp: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
)

func newExcludedPortNetshCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	process.NoConsole(cmd)
	return cmd
}

func osExcludedTCPPortRanges() ([]tcpPortRange, error) {
	windowsExcludedTCPPortRangesOnce.Do(func() {
		out, err := queryWindowsExcludedTCPPortRanges()
		if err != nil {
			windowsExcludedTCPPortRangesErr = err
			return
		}
		ranges, err := parseWindowsExcludedTCPPortRanges(out)
		if err != nil {
			windowsExcludedTCPPortRangesErr = err
			return
		}
		windowsExcludedTCPPortRangesCache = ranges
	})
	return append([]tcpPortRange(nil), windowsExcludedTCPPortRangesCache...), windowsExcludedTCPPortRangesErr
}

func parseWindowsExcludedTCPPortRanges(output []byte) ([]tcpPortRange, error) {
	var ranges []tcpPortRange
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		start, startErr := strconv.Atoi(fields[0])
		end, endErr := strconv.Atoi(fields[1])
		if startErr != nil || endErr != nil {
			continue
		}
		if start < 1 || end < start || end > 65535 {
			return nil, fmt.Errorf("parse Windows excluded TCP port range %q: invalid port bounds", strings.TrimSpace(line))
		}
		ranges = append(ranges, tcpPortRange{start: start, end: end})
	}
	return mergeTCPPortRanges(ranges), nil
}
