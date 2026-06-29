package api

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"mcp-local-hub/internal/config"
)

// ErrPortPoolExhausted signals every port in the manifest's port_pool is
// already allocated in the registry.
var ErrPortPoolExhausted = errors.New("port pool exhausted")

// portAvailable is the test seam for the OS-level bind check in AllocatePort.
// Production implementation attempts a 127.0.0.1 TCP listen; tests swap it to
// simulate a port already held by an unrelated process.
var portAvailable = func(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

var excludedTCPPortRanges = osExcludedTCPPortRanges

type tcpPortRange struct {
	start int
	end   int
}

func (r tcpPortRange) contains(port int) bool {
	return port >= r.start && port <= r.end
}

func portInRanges(port int, ranges []tcpPortRange) bool {
	for _, r := range ranges {
		if r.contains(port) {
			return true
		}
	}
	return false
}

func countTakenPortsInPool(taken map[int]bool, pool config.PortPool) int {
	count := 0
	for port := range taken {
		if port >= pool.Start && port <= pool.End {
			count++
		}
	}
	return count
}

func mergeTCPPortRanges(ranges []tcpPortRange) []tcpPortRange {
	if len(ranges) == 0 {
		return nil
	}
	merged := append([]tcpPortRange(nil), ranges...)
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].start == merged[j].start {
			return merged[i].end < merged[j].end
		}
		return merged[i].start < merged[j].start
	})
	out := merged[:0]
	for _, r := range merged {
		if r.end < r.start {
			continue
		}
		if len(out) == 0 || r.start > out[len(out)-1].end+1 {
			out = append(out, r)
			continue
		}
		if r.end > out[len(out)-1].end {
			out[len(out)-1].end = r.end
		}
	}
	return out
}

func excludedRangesInPool(pool config.PortPool, ranges []tcpPortRange) []tcpPortRange {
	var clipped []tcpPortRange
	for _, r := range ranges {
		if r.end < pool.Start || r.start > pool.End {
			continue
		}
		start := r.start
		if start < pool.Start {
			start = pool.Start
		}
		end := r.end
		if end > pool.End {
			end = pool.End
		}
		clipped = append(clipped, tcpPortRange{start: start, end: end})
	}
	return mergeTCPPortRanges(clipped)
}

func countPortsInRanges(ranges []tcpPortRange) int {
	count := 0
	for _, r := range ranges {
		count += r.end - r.start + 1
	}
	return count
}

func formatTCPPortRanges(ranges []tcpPortRange) string {
	if len(ranges) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		if r.start == r.end {
			parts = append(parts, fmt.Sprintf("%d", r.start))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d-%d", r.start, r.end))
	}
	return strings.Join(parts, ", ")
}

// AllocatePort returns the lowest free port in pool that is BOTH not
// recorded in the registry AND currently unbound at the OS level. First-free
// (not round-robin) so hole-filling is predictable and user-visible ports
// stay dense.
//
// Without the OS-level bind check, an unrelated local process occupying
// e.g. 9400 would still have that port returned; Register would write
// scheduler/client state and report success, but the proxy subprocess
// would immediately fail to bind and exit — producing a broken
// registration that looks successful.
//
// This function does NOT acquire the registry lock — callers must hold it
// before calling AllocatePort if they intend to persist the allocation.
func AllocatePort(reg *Registry, pool config.PortPool) (int, error) {
	if pool.Start <= 0 || pool.End < pool.Start {
		return 0, fmt.Errorf("invalid port pool {start=%d,end=%d}", pool.Start, pool.End)
	}
	taken := reg.AllocatedPorts()
	excluded, excludedErr := excludedTCPPortRanges()
	excludedInPool := excludedRangesInPool(pool, excluded)
	occupiedByProcess := 0
	for p := pool.Start; p <= pool.End; p++ {
		if taken[p] {
			continue
		}
		if portInRanges(p, excludedInPool) {
			continue
		}
		if !portAvailable(p) {
			occupiedByProcess++
			continue
		}
		return p, nil
	}
	total := pool.End - pool.Start + 1
	excludedCount := countPortsInRanges(excludedInPool)
	usable := total - excludedCount
	registryCount := countTakenPortsInPool(taken, pool)
	msg := fmt.Sprintf("pool {%d..%d} exhausted (%d total, %d OS-excluded, %d usable, %d registry entries in pool, %d occupied by other processes)",
		pool.Start, pool.End, total, excludedCount, usable, registryCount, occupiedByProcess)
	if len(excludedInPool) > 0 {
		msg += fmt.Sprintf("; Windows excluded TCP port ranges in pool: %s", formatTCPPortRanges(excludedInPool))
	}
	if excludedErr != nil {
		msg += fmt.Sprintf("; OS excluded TCP port range query failed: %v", excludedErr)
	}
	return 0, fmt.Errorf("%w: %s", ErrPortPoolExhausted, msg)
}
