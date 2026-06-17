package api

// hotswap_eligibility.go — Slice 3 of the zero-downtime hot-swap design
// (work-items/decisions/2026-06-16-hot-swap-zero-downtime-config.md).
//
// A daemon is "hot-swap eligible" when it can be restarted WITHOUT dropping the
// client's connection — i.e. the client talks to a STABLE front (a proxy's
// external port, or the hub aggregator) and the upstream child restart happens
// invisibly behind it. A daemon the client connects to DIRECTLY by port (a
// generic global stdio/HTTP daemon) is NOT eligible: a restart drops the
// client's socket, and (for stdio-wrapped globals) the port is assigned at
// spawn so it can even change.
//
// This is the Stability Council's "instrument-before-act" slice: it is a PURE
// FUNCTION whose verdict is surfaced as an OBSERVATION-ONLY field on DriftEntry
// (additive, omitempty) for operator visibility via the reconcile/status
// surface. It NEVER feeds the supervisor Action — no spawn/kill/transition
// decision reads it. Shipping it first lets an operator SEE which daemons would
// benefit from the gate-ON migration (Slice 4) before any behavior changes.

// HotSwapEligibility is the observation-only verdict for one daemon. Eligible
// reports whether the daemon can be hot-swapped; Reason is a short
// human-readable explanation for the reconcile/status surface.
type HotSwapEligibility struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

// Reason strings — exported as constants so tests and any future consumer pin
// the exact wording without copy-pasting it.
const (
	HotSwapReasonProxyFronted = "proxy-fronted: client holds a stable external port; the upstream child restarts invisibly behind the proxy"
	HotSwapReasonDirectPort   = "direct-port global daemon (gate-OFF): the client connects straight to the daemon port, so a restart drops the connection; needs gate-ON migration behind the hub to become hot-swappable"
)

// ClassifyHotSwapEligibility returns the observation-only hot-swap verdict for a
// supervisor daemon descriptor. PURE: depends only on the descriptor, performs
// no I/O, and never mutates anything.
//
// Signal: a non-nil RuntimeSpec marks a MATERIALIZED proxy daemon (serena
// dynamic-pool, workspace-scoped LSP) — the proxy binds the client-facing
// ExternalPort and execs the upstream child on a separate internal port, so the
// upstream restart is invisible to the client. A nil RuntimeSpec is a
// generic global daemon the supervisor spawns via `mcphub daemon --server`
// (memory/time/sequential-thinking/...) which the client targets directly by
// port — not hot-swappable until migrated behind the hub (Slice 4).
//
// See SupervisorDaemon.RuntimeSpec: "nil for legacy/global daemons that the
// supervisor spawns via the generic `mcphub daemon --server --daemon` path."
func ClassifyHotSwapEligibility(d SupervisorDaemon) HotSwapEligibility {
	if d.RuntimeSpec != nil {
		return HotSwapEligibility{Eligible: true, Reason: HotSwapReasonProxyFronted}
	}
	return HotSwapEligibility{Eligible: false, Reason: HotSwapReasonDirectPort}
}
