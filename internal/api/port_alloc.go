package api

import (
	"errors"
	"fmt"

	"mcp-local-hub/internal/config"
)

// ErrPortPoolExhausted signals every port in the manifest's port_pool is
// already allocated in the registry.
var ErrPortPoolExhausted = errors.New("port pool exhausted")

// AllocatePort returns the lowest free port in pool that is not currently
// assigned to any workspace entry in reg. First-free (not round-robin) so
// hole-filling is predictable and user-visible ports stay dense.
//
// This function does NOT acquire the registry lock — callers must hold it
// before calling AllocatePort if they intend to persist the allocation.
func AllocatePort(reg *Registry, pool config.PortPool) (int, error) {
	if pool.Start <= 0 || pool.End < pool.Start {
		return 0, fmt.Errorf("invalid port pool {start=%d,end=%d}", pool.Start, pool.End)
	}
	taken := reg.AllocatedPorts()
	for p := pool.Start; p <= pool.End; p++ {
		if !taken[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("%w: pool {%d..%d} fully allocated (%d entries)",
		ErrPortPoolExhausted, pool.Start, pool.End, len(taken))
}
