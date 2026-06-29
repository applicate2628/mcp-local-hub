---
status: accepted
date: 2026-06-29
---

# Decision: ROADMAP §B sections 2 (open bugs) + 3 (deferred) gap-triage closeout (pre-npm)

Before an npm release the user asked to close ROADMAP-remainder sections 2 (open bugs/test-infra) and 3 (deferred-by-verdict). A 3-codex parallel gap-triage (xhigh, file-based; `gap-sec2-bugs`, `gap-sec3-deferred-r2`, `gap-sec1-catalog`) re-verified every item against HEAD. Result: almost all were already stale/superseded or genuinely precondition-gated. This decision records the closeout so the items don't reappear.

## Section 2 — open bugs / test-infra
| Item | Verdict | Evidence |
|---|---|---|
| api-tests-flock-contention | CLOSE — superseded | `internal/api/main_test.go:30,70-80` fences the package to temp state |
| tests-leak-state (gui) | CLOSE — superseded | `internal/gui/main_test.go:11-34,70-82` redirects state/LOCALAPPDATA/XDG/override |
| cli-supervise-statedir gui-side | CLOSE — superseded | prod resolver ignores the env seam (`statedir_seam_test.go:8-31`); seam only in TestMain |
| §5 manual `/api/supervisor/restart` flagless-retry | CLOSE — already exists | `supervisor_restart_spawn_windows.go:24-36,65-82` already retries flagless on ERROR_ACCESS_DENIED |
| api-surfaces-status-restart-cleanup-race | CLOSE — fixed | fn-pointers snapshotted before goroutine (`api_surfaces.go:115-120,154-160`); -race passes |
| TRIAGE-2026-05-28 ledger | CLOSE — stale ledger | rows already closed/moved; archive the ledger, don't count as a release bug |
| **single_instance R:\Temp DACL flake** | **FIX-NOW** | the ONLY real fix — GUI pidport test fixtures used plain `t.TempDir()`; on a broadened-DACL volume the hardened-read gate (correctly) refuses. Fix = `apitest.HardenedTempDir(t)` in the test fixtures (product path unchanged, security gate intact). Branch `fix/single-instance-test-hardened-tempdir`. |
| tools-list-live-membership-symmetry | CLOSE — superseded | live-binding filter + fresh hidden-set (`hub_mcp_aggregator.go:318-386,546-561`); tools/call revalidates (`:862-891`) |
| gui-self-restart-gate-on-port-drift | CLOSE — not-a-bug | bind uses persisted port (`hub_mcp_bind.go:149-182`), fresh only on port==0; same-port-retry invariant (`hub_listener.go:606-611`) |
| parallelize-capability-probe | KEEP-DEFERRED | TTL-cached, behavior-correct, not release-blocking; bounded-fan-out is a future perf slice |

## Section 3 — deferred-by-verdict (NET: zero code; all close-as-decision)
| Item | Verdict + revival trigger |
|---|---|
| §11.3 strict-job metrics + auto-remediation | CLOSE — no metrics-consumer contract + opaque Job-Object syscall errors (can't classify transient/permanent → backoff would churn host-policy failures). Revive when a Prometheus/OTel/expvar consumer lands AND Job-failure classification is reliable. |
| E2 daemon-intent.json deletion | CLOSE — live stop truth is sub-block-only (`api_surfaces.go:199`); legacy reads are migration-only; deleting strands pre-0.4.8 in-place-upgrade pending stops. Remove in 0.5.0/1.0 once the supported-upgrade floor excludes pre-0.4.8. |
| §3 serena+LSP event-driven teardown | CLOSE — **serena half ALREADY SHIPPED**: HEAD publishes `daemon-backend-lost` (`poller.go:270`) + wires the serena subscriber/ticker (`gui.go:497`). The LSP-backend-loss invalidation is a separate gated design (LSP only has TTL/manual unbind at `session_router.go:103`). Revive on a captured LSP zombie-session repro or an accepted LSP backend-loss design. |
| `file:` secret prefix | CLOSE — needs a real `config.local.yaml` owner (schema/path/permissions/reload/spawn-wiring/docs/tests); resolver supports a local map but prod passes nil (`daemon.go:178`), readiness marks `file:` fatal (`readiness.go:957`), UI hides it (`SecretPicker.tsx:20`). Revive on an accepted local-config design + security posture. |
| FieldRenderer-unify | CLOSE — low-value/risky rewrite; `SectionGuiServer`/`SectionDaemons` own bespoke dirty/save semantics. Revive only with a tested settings-row design-system pass. |
| SVG sprite | CLOSE — net-negative infra (~5 scattered inline SVGs). Revive if duplicated icon paths grow or an icon system is adopted. |
| inline matrix RAM/Uptime columns | CLOSE — subsumed by ServerRowDrawer (per-daemon RAM/Uptime). Revive only with a UX spec for aggregating N-daemon metrics into one cell. |
| event-bus extra SSE types | CLOSE — install/scan are synchronous handlers, no frontend consumer; `api/events.go` is a documented dead stub. Revive on a streaming Store-install orchestrator or a real consumer. |

## Net
Sec-1: catalog VERIFIED operationally sound (codex: all 33 entries GenerateDraftManifest→ParseManifest PASS, packages resolve, 0 broken). Sec-2: 1 code fix (single_instance test) + the rest closed. Sec-3: zero code, all close-as-decision (serena teardown already shipped). The pre-npm gap-closeout is the single_instance test fix + the provenance-strengthening + this decision record.
