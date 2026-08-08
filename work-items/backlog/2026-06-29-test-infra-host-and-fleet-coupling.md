---
status: candidate
context: backlog
date: 2026-06-29
---

# Test-infra gaps: go-test suite couples to the dev host's port config + the live fleet

A release-quality operability audit (codex, 2026-06-29, on a dev host with a live
35-daemon fleet + a Windows TCP port-exclusion 9206-9305 + a broadened R:\Temp)
found that `go test ./...` is NOT clean on such a host. **None are product
regressions** (build/vet/frontend GREEN; the session's catalog/LSP-reg/pixel-perfect
work is gap-clean; the fleet runs 35/0). They are TEST-ENV/TEST-QUALITY couplings
that make the suite non-reproducible on a developer machine. Filed so the tests can
be made host- and fleet-independent.

| Test(s) | Cause | Fix direction |
|---|---|---|
| `TestRegister_DefaultAllLanguages`, `TestMigrateLegacy_*` | The LSP port pool `9200..9299` has only 6 usable ports on this host (Windows excludes `9206..9305`, confirmed via `netsh int ipv4 show excludedportrange`); registering all 9 languages exhausts it. Also collides with the live fleet's daemons on 9200/9201/9204/9205. | Use an isolated/ephemeral port pool in these tests (don't bind the real 9200-9299); OR an exclusion-aware fallback pool. Separately consider whether the PRODUCT default pool is too small on Windows-port-excluded hosts (a real but pre-existing, host-dependent edge — `servers/mcp-language-server/manifest.yaml:11`, `internal/api/port_alloc.go:40`). |
| `TestRestart_ServerDaemon_DoesNotKillSiblingPort`, `TestRestart_Server_DoesNotKillSiblingDaemon` | `run=[]` — the restart/scheduler tests collide with the real Task-Scheduler tasks of the live fleet (the documented "full test sweep affects the real scheduler" hazard). | Stub/isolate the scheduler in these tests so they don't read the host's real tasks. |
| `TestUnregister_MixedCanonicalAndLegacyKeysRemovesBoth`, `TestWorkspaceUnregister_BackendAllRemoves...` | On Windows the fixture's "canonical" and "legacy" paths normalize to the SAME workspace key, so the mixed-key compat path isn't exercised. | Build a Windows fixture that produces genuinely distinct old/new keys, or retire the legacy-key compat if obsolete (`internal/api/workspace_path.go:142`). |
| `TestWorkspaceList_TabularOutput`, `TestWorkspacePrune_IdleAddsCandidates` | The CLI table truncates the (pathologically long `t.TempDir()`) workspace path before the default `*` marker / idle identity, so the assertion can't see them. | Preserve the unique leaf/suffix or move marker/identity to non-truncated columns; keep full path in JSON (`internal/cli/workspace_cmd.go:645`). |
| `TestE2E_LazyRegisterFullLifecycle` | Production state-dir resolution refuses the env fallback, so the E2E can't use a redirected safe profile path. | Add a test-only state-root seam/tag for this E2E (don't weaken production fail-closed) (`internal/e2e/lazy_register_test.go:325`). |

Verified non-issues (the session's work): `/api/lsp/register` route present; catalog
`install_probe`/`docs_only` consumers all wired; no missing dark/system-dark CSS token.
