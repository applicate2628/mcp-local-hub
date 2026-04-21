package api

import (
	"errors"
	"fmt"
	"net"

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

// AllocatePort returns the lowest free port in pool that is BOTH not
// recorded in the registry AND currently unbound at the OS level. First-free
// (not round-robin) so hole-filling is predictable and user-visible ports
// stay dense.
//
// Without the OS-level bind check, an unrelated local process occupying
// e.g. 9200 would still have that port returned; Register would write
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
	for p := pool.Start; p <= pool.End; p++ {
		if taken[p] {
			continue
		}
		if !portAvailable(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("%w: pool {%d..%d} fully allocated or occupied by other processes (%d registry entries)",
		ErrPortPoolExhausted, pool.Start, pool.End, len(taken))
}
