---
title: "vcpkg-mcp must move into the hub as an in-binary server, not ship as a standalone executable"
status: accepted
date: 2026-07-26
decided-by: lead, on the operator's challenge ("почему vcpkg MCP отдельным бинарником, а не вшит в хаб как остальные?")
supersedes: the `cmd/vcpkg-mcp` placement in 2026-07-25-vcpkg-mcp-tool-contracts.md ("placement B")
---

## The decision

`vcpkg-mcp` moves to the house pattern: `internal/vcpkgmcp` + `servers/vcpkg/manifest.yaml` +
an `mcphub vcpkg` subcommand. The standalone `cmd/vcpkg-mcp` executable is retired.

## Why the standalone shape is wrong — the project already decided this once

`servers/godbolt/manifest.yaml` documents the exact same migration, in the opposite direction,
in its own words:

> "Previously this server was a Python FastMCP process launched per-session through a virtualenv…
> That worked but required a separate venv per machine, spawned a fresh Python process per client…
> The Go rewrite lives inside the mcphub binary — no venv, one hub-managed daemon, shared HTTP
> client pool."

`cmd/vcpkg-mcp` reintroduced precisely the shape that comment describes leaving behind. This is not
a new judgement call; it is a regression against a recorded one.

## What the standalone shape actually cost, measured on the deploy

Not hypothetical — each of these was paid during the 2026-07-26 deploy:

| Cost | Evidence |
|---|---|
| `build.sh` does not build it | had to run `go build ./cmd/vcpkg-mcp` by hand; an npm release would have shipped mcphub WITHOUT it |
| client configs edited by hand, twice | `~/.claude.json` and the codex `config.toml` symlink target, each with a backup |
| not supervised | no Job-Object orphan protection, no restart policy, absent from `mcphub status`, no hub port allocation |
| a process per client, not one shared daemon | transport is `stdio`, not the house `stdio-bridge` |
| a hub upgrade does not update it | separate artifact, separate lifecycle |

## The constraint that makes it decisive: configs must be portable

Operator, same session: *"конфиги должны быть настраиваемые под пользователя, мои — только как
пример, деплоено в общем виде (на разных машинах разные пути)"*.

The hand-written client entry embeds an ABSOLUTE path
(`C:\Users\<user>\.local\bin\vcpkg-mcp.exe`) that is wrong on any other machine. The house pattern
removes the path entirely — the manifest says `command: mcphub` with `base_args`, and
`client_bindings` let `mcphub install` write each client's entry with the locally-correct binary.
Portability is not a nicety here; it is the difference between a deployable server and a
machine-specific hack.

## Related constraint for the TOOLS themselves (already satisfied, keep it that way)

Operator: *"каталоги сборок прописаны в оверлеях, это ключи vcpkg"*. Build and install roots come
from vcpkg's own keys — `--x-buildtrees-root`, `--x-install-root`, `--overlay-ports`,
`--overlay-triplets` — and must be taken as parameters, never discovered by guessing at machine
layout. The measured host paths (`C:\vcpkg`, `R:\b`, `Q:\vcpkg-libs`) are TEST DATA, not defaults.

**CORRECTION (2026-07-26, after the pre-bot commission).** An earlier revision of this section claimed
"the current implementation already honours this". **That claim was wrong**, and it was made from
reading the design intent rather than probing the behaviour. A cross-family review of the branch
returned 27 findings, 8 of them P1, and three land directly on this paragraph:

- `discovery/discovery.go:148` — with no explicit root, no environment, no PATH and no manifest
  evidence, a single hardcoded machine-layout match (`C:\vcpkg\vcpkg.exe`) is **promoted to an
  authoritative root** and returned as `ok`. So heuristics ARE selected, not merely reported.
- `discovery/discovery.go:193` — an explicitly supplied root that is missing or unreadable **silently
  falls through** to a lower-precedence source, returning `ok` for a different installation and
  discarding the rejected explicit candidate. The operator's own parameter is not terminal.
- `patchesapply/varenv.go:62` — triplet variables are **inferred from the triplet NAME**, so a custom
  overlay triplet setting `VCPKG_LIBRARY_LINKAGE static` is modelled as dynamic and static-only patches
  are reported as not applied. That is precisely "guessing at machine layout" in the one place the
  operator named explicitly.

The constraint above stands unchanged; what changes is its status — it is a REQUIREMENT the branch does
not yet meet, not a property it already has. Fixes are in flight. The lesson worth keeping: a design
document asserting that code satisfies a rule is a hypothesis about the code, and stating it in a
decision record does not verify it.

The intended shape remains: root discovery is explicit-param (terminal) → `VCPKG_ROOT` → PATH →
manifest → heuristics reported as CANDIDATES ONLY under a closed `unknown(heuristic_only)` reason, with
`unknown(no_candidates_found)` rather than a guess. There is also an authoritative source for a real
invocation's keys: the operator's wrapper writes a `command:` line into `build_failed.log` carrying the
full overlay chain plus both roots — worth consuming as an optional enrichment, never as a requirement.

## Sequencing

Done BEFORE `feat/vcpkg-mcp` gets a PR. The branch is pushed but unreviewed, so reshaping it now
costs nothing; merging the wrong shape and migrating afterwards would cost a second PR and leave a
churn pair in published history. The four shape-correct branches (#583, #588, #589, #590) went to
review first and are unaffected.

## Residual to clean up after the migration lands

The two hand-written client entries added on 2026-07-26 become redundant and must be removed —
`vcpkg-mcp` in `~/.claude.json` and `[mcp_servers.vcpkg-mcp]` in the codex config — replaced by what
`mcphub install` writes from `client_bindings`.
