//go:build !windows

package cli

import "mcp-local-hub/internal/process"

// Other platforms keep their existing liveness, identity, and RSS probes.
var defaultStatusPIDObserver func(process.PIDIdentityProof) (process.PIDStatusObservation, error)
