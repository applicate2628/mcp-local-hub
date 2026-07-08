# Plan: mcphub adopt PR4

Date: 2026-07-07
Owner: main Codex conversation

## Scope

Implement `mcphub adopt <entry-name> --client <client>` as an additive CLI/API capability that absorbs direct stdio MCP entries into the hub by composing existing manifest creation, install, client-write, symlink-consent, and supervisor-intent paths.

## Steps

1. Add red tests for dry-run default, execute pipeline, secret routing, vault refusal, name and embedded collision refusal, port allocation, omitted-client reporting, and Codex TOML preservation.
2. Implement API planning and execution with `ExtractManifestFromClient`, bounded `client_bindings`, `ManifestCreate`, and `Install`.
3. Implement dedicated adopted port allocation over 9300-9399 using disk manifests, embedded manifests, supervisor intent, and bind probing.
4. Implement sensitive literal env routing through the existing vault before manifest persistence.
5. Add CLI command and wire existing interactive symlink consent before execution.
6. Run required build, cross-build, vet, focused race tests, and scoped security review.

## Outcome

Status: PASS

The implementation is complete and intentionally uncommitted. Verification passed with the requested command set, and the scoped security review of secret routing plus symlink-consent threading passed.

## Open Items

None for PR4. Out-of-scope GUI, catalog, cross-client different-name removal, and reaper hardening were not implemented.
