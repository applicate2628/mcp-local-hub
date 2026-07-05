# Backlog: parallelize the serial capability probe

Found: four-axis audit 2026-06-19 (AXIS-3 performance).
Priority: low (the path is TTL-cached and off the per-request path).

## What

`computeCapabilitiesSection` (internal/api/health.go ~711) iterates
`probes.Items` and calls `realCapabilityRow` **serially** per daemon. Each
`realCapabilityRow` does upstream round-trips (initialize + tools/list +
prompts/list + resources/list). With N reachable daemons that is N serial
network sweeps.

## Why it is low priority

The section is **TTL-cached** (`capabilitiesTTLMs`), so the serial cost is
paid only on a cache-miss/refresh, not on every `/api/status`. Behavior is
correct; only the refresh latency is affected.

## Fix sketch

Mirror the hub aggregator's bounded-parallel pattern
(`hub_mcp_aggregator.go` `FanOutConcurrency` semaphore + `sync.WaitGroup`):
fan the per-daemon `fn(...)` calls out under a buffered-channel semaphore,
collect rows, preserve the existing ordering + per-daemon error rows. ~15
LOC, contained to the loop. Test via `HealthCapabilityFn` seam (already
injectable). Verify the section JSON is order-stable for the GUI.
