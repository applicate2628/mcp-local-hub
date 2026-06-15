package api

import "fmt"

// globalDaemonBandStart / globalDaemonBandEnd is the hub daemon port band a
// single-daemon global server installed via the marketplace one-click flow
// draws from. It follows the repo port-map convention recorded in
// serena_dynamic_pool.go: globals historically 9121–9149, serena dynamic
// pool 9150–9199, and 9200–9299 for hub-allocated single-daemon globals
// (the same band servers/mcp-language-server/manifest.yaml declares and
// configs/ports.yaml reserves under workspace_scoped). Named constants —
// not a bare 9200 literal — keep this aligned with the §8a live-band guard
// convention (a port value that reaches a kill/listen sink must trace back
// to a band declaration, never a magic number sprinkled at the call site).
//
// NEVER 9125: that is the GUI's own port and is deliberately OUTSIDE this
// band, so a marketplace install can never collide with the GUI listener.
const (
	globalDaemonBandStart = 9200
	globalDaemonBandEnd   = 9299
)

// PortInGlobalDaemonBand reports whether port falls inside the hub
// single-daemon global band [globalDaemonBandStart, globalDaemonBandEnd].
// Used to validate an operator-supplied ?port override before honoring it:
// an out-of-band override is refused so a one-click install cannot be
// steered onto the GUI port or an unrelated daemon's band.
func PortInGlobalDaemonBand(port int) bool {
	return port >= globalDaemonBandStart && port <= globalDaemonBandEnd
}

// AllocateSingleGlobalPort returns the lowest free port in the hub
// single-daemon global band [globalDaemonBandStart, globalDaemonBandEnd]
// that is BOTH not in `taken` AND currently unbound at the OS level.
//
// `taken` is the set of ports already owned by installed global manifests'
// daemons (the caller scans installed manifests and passes their declared
// daemon ports). First-free (not round-robin) so hole-filling is
// predictable and user-visible ports stay dense — the same posture as
// AllocatePort for the workspace pool. A nil/empty `taken` means no
// installed daemon claims a band port yet.
//
// The OS-level bind probe (portAvailable) is the same shared test seam
// AllocatePort uses; without it a port that the registry/manifest set
// reports free but an unrelated local process holds would still be
// returned, producing a daemon that immediately fails to bind. Returns a
// wrapped ErrPortPoolExhausted when every band port is taken or OS-bound,
// so the caller can distinguish exhaustion from other failures.
func AllocateSingleGlobalPort(taken map[int]bool) (int, error) {
	for p := globalDaemonBandStart; p <= globalDaemonBandEnd; p++ {
		if taken[p] {
			continue
		}
		if !portAvailable(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("%w: hub global band {%d..%d} fully claimed by installed daemons or occupied by other processes",
		ErrPortPoolExhausted, globalDaemonBandStart, globalDaemonBandEnd)
}
