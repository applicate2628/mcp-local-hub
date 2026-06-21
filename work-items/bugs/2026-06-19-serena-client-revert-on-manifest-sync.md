---
status: active
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
