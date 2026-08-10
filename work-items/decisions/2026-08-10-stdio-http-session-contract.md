---
status: accepted
date: 2026-08-10
slug: stdio-http-session-contract
related: 2026-08-10-hfss-cst-mcp-feedback-r1
---

# Give every stdio-bridge HTTP client an honest bounded MCP session

## Context

`StdioHost` exposes one long-lived stdio subprocess to multiple HTTP clients. It currently emits one process-lifetime session ID on every POST and never invalidates it. That contradicts the MCP Streamable HTTP contract and prevents protocol-version validation from being bound to the negotiated session.

## Decision

Keep the shared stdio subprocess, but make the HTTP adapter own a bounded in-memory session registry. Each successful `initialize` creates a fresh cryptographically secure session associated with the negotiated protocol version. Non-initialize POST/GET/DELETE validates that session at the adapter boundary. Missing session is HTTP 400, unknown or terminated session is HTTP 404, and explicit unsupported or session-mismatched protocol version is HTTP 400. DELETE removes exactly that session and leaves the shared subprocess alive.

The registry has a fixed cap and deterministic oldest-session eviction, because abandoned clients must not create unbounded process memory. Session state is adapter-local and ephemeral; daemon restart intentionally invalidates every old session.

## Consequences

- The hub aggregator continues receiving a session ID and requires no compatibility change.
- Existing compliant clients become isolated from one another; a DELETE can no longer leave a reusable token.
- Legacy callers that skipped initialization will now receive a visible HTTP error instead of being silently admitted.
- No persisted state or migration is introduced.
