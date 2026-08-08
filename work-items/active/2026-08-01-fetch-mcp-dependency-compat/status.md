# Fetch MCP dependency compatibility

Status: active
Opened: 2026-08-01
Template: quick-fix

Task: Restore the shipped fetch MCP daemon by pinning the official fetch server and a compatible stable MCP Python SDK in the canonical manifest.

Current step: Build and deploy the verified no-client-write install seam through the canonical production lifecycle.

Last result: The explicit `--no-client-config` seam and pinned fetch manifest pass focused API/CLI normal and race tests, `go vet`, diff/gofmt checks, and a live `uvx --with mcp==1.28.1 mcp-server-fetch@2026.7.10 --help` import smoke. The installed production binary still carries the old embedded manifest, so fetch remains in its pre-existing restart loop until rebuild and canonical redeploy.

Next action: Produce a Windows production build, redeploy through the canonical supervisor handoff, install only fetch with `--no-client-config`, then prove daemon liveness and MCP `tools/list`.

Rollback: Restore the prior manifest arguments and rebuild/reinstall the prior canonical binary; do not edit the uv cache.

Oracle: `fetch/default` remains running on port 9133, MCP `tools/list` succeeds, and no new fetch crash/quarantine events appear.
