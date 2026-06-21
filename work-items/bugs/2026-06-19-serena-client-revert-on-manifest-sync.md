---
status: mostly-resolved
severity: P1
date: 2026-06-19
slug: serena-client-revert-on-manifest-sync
---

# Serena client entry reverts on manifest sync

## Symptom

Serena client entries can be rewritten from the live GUI router URL back to the legacy per-daemon manifest URL during manifest-driven sync or install planning.

## Decision

Use `SerenaRouterClientURL` as the single owner for serena client URLs across migrate write, install write, scan read/reconcile, and uninstall ownership.

Decision record: [serena-router-client-url-single-owner](../decisions/2026-06-21-serena-router-client-url-single-owner.md).

## Follow-Up

Make the serena manifest router-native and delete the legacy/dynamic split.

## Terms and Abbreviations

- GUI: Graphical user interface.
- MCP: Model Context Protocol.
- URL: Uniform Resource Locator.

_2026-06-21: immediate install/manifest-sync revert to dead 9121/mcp FIXED by #400 (BuildPlanWithOpts consults SerenaRouterClientURL on the install write plane). Verified live: serena on http://127.0.0.1:9125/serena/mcp after redeploy to 43e7619a. Strategic follow-up (router-native manifest) tracked in work-items/decisions/2026-06-21-serena-router-client-url-single-owner.md._
