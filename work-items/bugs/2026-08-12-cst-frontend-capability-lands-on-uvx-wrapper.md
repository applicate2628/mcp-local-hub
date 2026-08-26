# Bug: frontend launch capability lands on `uvx` wrapper

- id: 2026-08-12-cst-frontend-capability-lands-on-uvx-wrapper
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: CST frontend enrollment / StdioHost launch capability
- found-by: security-reviewer
- fix-class: design-decision

## Reproduction

1. Read `servers/cst/manifest.yaml:5-10`: the configured command is `uvx`, and `mcphub-cst-mcp` is its tool argument.
2. Read `internal/daemon/host.go:283-295,335-369`: `StdioHost` creates the direct child from that command and applies the inherited launch-capability handle list to it.
3. Compare the design's “child alone” and exact frontend PID/parent binding at `work-items/active/2026-08-11-cst-saved-field-sampler/design.md:48-66,465-482` with Astral's documented `uvx` tool invocation: <https://docs.astral.sh/uv/guides/tools/>.

## Expected

Only the exact authenticated frontend process receives and can read the one-use launch capability, with complete process-identity, replay, cancel, close and zeroization settlement.

## Actual

The direct capability recipient is `uvx`. The design has no wrapper-to-frontend transfer or identity/lifecycle contract. Without propagation the frontend cannot enroll; with propagation `uvx` is an unmodelled capability principal.

## Required design correction

Make the authenticated frontend the direct recipient, or explicitly secure and bind the launcher-to-frontend transfer and topology. Re-review Claims 5, 7, 16 and 18.

## Terms and Abbreviations

- **CST** — Computer Simulation Technology Studio Suite.
