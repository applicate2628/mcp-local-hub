# LSP router dependency ownership — 22 November 2026

## Contents

1. [Change and contract](#1-change-and-contract)
2. [Observed verification](#2-observed-verification)
3. [Architecture gate and limits](#3-architecture-gate-and-limits)

## 1. Change and contract

The Language Server Protocol (LSP) router's three graphical-interface handlers
used a process-wide mutable factory. The production factory constructed a fresh
application programming interface (API) object for every request, bypassing the
API already owned by `Server`. The test helper replaced that factory for every
server: configuring a second server redirected the first server's requests.

The existing narrow interface is now a `Server` field, bound directly to `s.api`
in `NewServer`. Each test configures only its own server. No wrapper, public
configuration parameter, dependency, or persistent routing namespace was added.
Method/origin/body/client validation, error redaction, JSON responses, partial
success and operator events retain their existing behavior. Empty routing
options still resolve through the existing canonical routing authority.

The source parent is [d39b99d](https://github.com/applicate2628/mcp-local-hub/commit/d39b99db427eee026af12f8120c24477a2d9612a),
tree `8b6d3891e3b5790b53fc24fa9cb62a52397b4aa6`. Qualification materialized this
exact tree, then the four candidate files. The documentation added with this
report was not part of the executable input. Exact file inventories, commands,
tool hashes, original logs and direct-child receipts are in
[raw-evidence.zip](raw-evidence.zip); [results.json](results.json) records the
independent exact-event verification and physical scan comparison.

## 2. Observed verification

**Table 1 — Windows/amd64, existing Go 1.27.4, race detector enabled.** Counts
separate test/subtest events from the package event; count caching was disabled.

| Check | Observed result |
|---|---|
| Original production + original helper + same new two-server regression | First-server leaf FAIL; second-server leaf PASS; root and package FAIL; Go exit 1 |
| Candidate: three existing route tests + new regression | Four roots, two leaves: 6 test/subtest PASS + package PASS; no FAIL/SKIP |
| `gofmt` | No byte change; subsequent check empty, exit 0 |
| `go build ./...` | Exit 0 |
| `go vet ./...` | Exit 0 |
| Physical architecture scan | 2663 → 2661 findings; exactly two removed fingerprints, zero additions |

The negative control deterministically exercises both status reads and the two
mutations; no scheduling race is needed. Tests use an unbound server, private
client/state paths and fake API methods. They do not start a listener, native
scheduler, child process, or installed Product. The broadcaster is closed by the
existing fixture. The entire JSON terminal-event multisets were compared with
the admitted plans, including the absence of unexpected tests and skips.

Root own review and an independent source review covered the dependency binding,
all consumers, zero-server call sites, HTTP compatibility, authority ownership,
fixture isolation, complexity, and the discriminating control. No blocking
finding remained in this bounded correction. External review of the new Git
head remains pending; this pull request stays Draft.

## 3. Architecture gate and limits

**Table 2 — exact architecture accounting.**

| Quantity | Before | After |
|---|---:|---:|
| Physical findings | 2663 | 2661 |
| Baseline entries | 2403 | 2401 |
| New findings | 388 | 388 |
| Stale baseline entries | 128 | 128 |
| Grown metrics | 42 | 42 |
| Worker findings within New | 72 | 72 |

Removed fingerprints are the `mutable_global` and `api_construction` findings
for `lspRouterControlAPIFactory`. Only their two baseline blocks were removed;
all 2401 remaining blocks and the header are byte-identical. Policy limits and
worker contracts were not relaxed. The existing `server.go` file-size metric
increased from 1263 to 1265 when the field and its construction binding were
added; that already-grown finding remains visible.

The complete architecture gate is still **FAILED**: verify exit 1,
`New=388`, `Stale=128`, `Grown=42`, `workers=72`, `expired=0`, `unowned=0`.
An expected gate failure is not an acceptance PASS. Previous scan evidence is
the [manifest-services report](https://github.com/applicate2628/mcp-local-hub/blob/d39b99db427eee026af12f8120c24477a2d9612a/docs/modernization/verification/manifest-services-2026-11-22/README.md).

The regression proves instance-specific dependency binding, not separate
persistent settings or concurrency safety of all test infrastructure. Full
default/tagged Go suites and native installation/lifecycle qualification were
not run. The runner waited its original direct-child handles; no timeout
occurred, but it does not establish whole-process-tree containment. Dependency
downloads, system scheduler mutations, installations, manual CI and external
quota review were not performed.
