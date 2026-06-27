---
status: open
context: adjacent-finding
created: 2026-06-27
---

# D2 follow-up — full-fidelity restore of arbitrary NON-catalog object-members (deferred)

## Summary

Full-fidelity restore of an arbitrary NON-catalog cursor/vscode object-member on
cold re-enable would need a secret-bearing per-project **stash-on-disable**
(option 3a): on disable, before the member is hard-deleted from the client
config, mcphub would have to persist the member's full value (command, args, and
its literal `env`/`headers` — which can include resolved secrets) to a
per-project on-disk store, then replay it on re-add.

This is **disproportionate** and is therefore **deferred**:

- It re-introduces exactly the secret-bearing store that P3a r2 deliberately
  removed (the warm value-replay path was dead because the `/api/projects`
  aggregate NILs every `ClientPresence.raw` via `sanitizeScanResult` /
  `stripClientEntryRaw` — the strip-Raw security posture that keeps
  secret-bearing config off the wire).
- A new secret-bearing per-project stash is a NEW attack surface (a file on disk
  holding resolved secrets, with its own DACL/encryption/lifetime/cleanup
  obligations) for a convenience that the catalog-by-name path already covers for
  every shipped server.

## What v1 (this PR) does instead

D2 v1 is **secret-safe by construction** — it does NOT stash any value:

- The Re-add CTA carries the disabled server's NAME
  (`#/add-server?readd=<name>`).
- **Catalog-known** name → Add-server pre-fills from the SHIPPED manifest via
  the embed-ONLY `/api/catalog/manifest` endpoint (D2 r2 — NOT `/api/manifest/get`,
  which is disk-only and would echo a hand-planted disk literal; see PR #439 r2),
  whose `env` carries `secret:`/`${env:}` placeholders, never a resolved literal
  (e.g. `servers/wolfram/manifest.yaml`:
  `WOLFRAM_LLM_APP_ID: "secret:wolfram_app_id"`). Command + args come along.
- **Non-catalog** name → Add-server seeds ONLY the name (blank
  command/args/env) plus an honest banner telling the operator to re-enter its
  command/args/secrets manually (via the existing AddSecretModal / `secret:<key>`
  refs).

The no-literal-secret-echo invariant holds because mcphub never held the deleted
member's value to echo (the member was hard-deleted on disable) and the Re-add
flow never reaches for `/api/extract-manifest` (the dead post-delete path that
would carry the client env verbatim).

## When to revisit

Only if operators report friction re-entering command/args for NON-catalog
(third-party / hand-rolled) object-members frequently enough to justify the
secret-bearing stash's added attack surface and lifecycle cost. Until then, the
manual re-entry for non-catalog members is the accepted tradeoff.
