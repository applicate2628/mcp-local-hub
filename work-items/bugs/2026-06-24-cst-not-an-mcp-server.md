---
status: open
severity: medium
context: adjacent-finding
---

# `bbl21/CST_MCP` is a CLI toolkit, NOT an MCP server — Tier-1 cst row DROPPED

## Finding (adjacent finding — re-open as a Tier-1 research item)

The desktop-app catalog epic listed **CST Studio Suite 2026** → `bbl21/CST_MCP` as a Tier-1
S1 local-stdio MCP server ("Python3.13/COM, ~113 tools, full geometry→solve→S-params"). While
resolving its actual run command (D-4 protocol "confirm the entrypoint from the repo"), the
repo was verified at tag `v1.0.0` and is **NOT an MCP server**:

- Full `v1.0.0` git-tree scan: ZERO MCP-protocol code — no `mcp.server`, no `FastMCP`, no
  `stdio_server` anywhere in the tree.
- It is a CLI toolkit: inner package `skills/cst-runtime-cli/scripts/` with console-script
  `cst-runtime = "cst_runtime.cli:main"`, invoked as `uv run python -m cst_runtime <subcommand>`
  (health-check, list-tools, inspect-project, run-experiment, …) — a request/response CLI,
  not a long-lived stdio MCP daemon.
- The README explicitly frames it as an AI-agent "skill" / CLI, not a server.

There is no `command`/`args` that launches a stdio MCP server, so there is no valid S1 catalog
row. The repo NAME ("CST_MCP") misled the design, which rested on the name/abstract rather than
a tree scan. The `v1.0.0` reality is authoritative.

## Disposition (architect ruling, binding)

The `cst` row was **DROPPED** from the Tier-1 first-batch catalog PR (final batch = 4 rows:
mathcad, excel, ableton, codex-mcp-server). Including it as a stdio row with a placeholder
command would spawn a non-server that never speaks the MCP handshake — the supervisor would
churn it through backoff → quarantine. That is exactly the false install the
`disabled-until-probe` + readiness gate exist to prevent. Shipping 4 honest rows is correct;
shipping "5 rows, one of which lies about being a server" is broken.

## Follow-up (epic-owner scope, not this PR)

Re-open CST as a Tier-1 research item under the desktop-app epic: either locate a genuine CST
Studio Suite MCP server (one that actually implements the stdio MCP protocol), or scope a
thin build-it wrapper over CST's COM API as a separate work-item. That is a design+research
question, out of scope for the catalog data PR.

## Related (architect's second adjacent finding)

The architect also flagged that 3 of 5 Tier-1 rows had a reality-vs-design conflict because
the design rested on repo names/abstracts rather than a confirmed-entrypoint tree scan
(no `pyproject.toml`/console-script/`FastMCP`/`stdio_server` check). Recommendation: promote
the D-4 "confirm the entrypoint" step to a mandatory pre-design gate for every future catalog
row, so the architect ships shapes, not name-based guesses.
