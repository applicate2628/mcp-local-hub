# vcpkg — build-failure triage and port analysis

Read-only diagnostics for a vcpkg tree: what failed, which port definition won, whether a pin is
current, which patches apply, and how CMake files include each other. **No build is ever started by
this server** — a live build invocation is explicitly out of scope.

Runs embedded in the mcphub binary (`mcphub vcpkg`, `transport: stdio-bridge`), so there is no venv,
no npm package, and no second executable to keep updated. Port **9138**.

## The one thing to know before reading any answer

Every tool returns a **tri-state**:

| status | meaning |
|---|---|
| `ok` | the tool established the answer from evidence it actually read |
| `failed` | the tool established that the thing FAILED |
| `unknown(<reason>)` | the tool could not settle the question, and `reason` says why |

`unknown` is a normal, useful answer — not an error. The reasons are a closed set, and each one
distinguishes a **different fact**, because those facts have different remedies:

- `..._not_supplied` — you did not pass something (an overlay chain, a root). Pass it.
- `..._unreadable` — it exists but could not be read (permissions, sharing, I/O). Fix access.
- `..._not_found` / `..._absent` — verified absence. This is a real finding.
- `heuristic_only` / `multiple_candidates` — a guess was available but was NOT selected; every
  candidate is listed so you can confirm one by passing it explicitly.

**A reason describing the tool's own input or its own search is never phrased as a fact about your
build.** "No overlay chain was supplied" and "this build uses no overlays" are different claims; only
the first is something the tool can know when you did not pass overlays.

### Why this is stated so prominently

Because getting it wrong is the failure mode that costs you real time. A confident WRONG NEGATIVE —
"that ref does not exist upstream", "no overlays are in play" — sends you to fix something that is
already correct. Several such defects were found and closed before this shipped, by an operator
driving the tools against a live tree and checking the answers rather than relaying them. If you ever
see a definite verdict you can disprove in ten seconds, that is a bug worth reporting, not a quirk.

## Tools

| Tool | Answers |
|---|---|
| `vcpkg_last_failure` | What failed in the last build — first real diagnostic, the failing phase, the reproducible command, and every log path it read. Handles nested builds (vcpkg → make → NMAKE → cmake → clang-cl/lld-link), where the real error sits thousands of lines under content-free wrapper noise. |
| `vcpkg_port_resolution` | Which port definition actually wins across your overlay chain and the builtin registry, and why. |
| `vcpkg_pin_status` | Whether a pinned ref is current. Reports `current` or an honest `unknown` — never a fabricated "behind". Expands `${VERSION}` / `${PORT}` from `vcpkg.json` so the common `"v${VERSION}"` idiom resolves. |
| `vcpkg_patches_apply` | Which declared patches apply for a triplet, from static analysis of the portfile's guards. Resolves patch paths rather than assuming they are port-dir-relative. |
| `vcpkg_cmake_trace` | Reads an existing CMake trace. Bounded — it will not materialize an arbitrarily large trace. |
| `cmake_include_graph` | Which CMake files include which, resolved statically, with dangling, allowed OPTIONAL absence, and unresolved edges distinguished. |
| `vcpkg_discover_root` | Where the vcpkg root is, and by which rule it was decided. |

## Roots and overlays come from you, never from guessing

Build and install roots and the overlay chain are taken from vcpkg's own keys —
`--x-buildtrees-root`, `--x-install-root`, `--overlay-ports`, `--overlay-triplets` — passed as tool
parameters. Discovery order when you do not pass a root:

1. explicit parameter — **terminal**: if it is invalid or unreadable you get
   `unknown(explicit_root_relative|explicit_root_invalid|explicit_root_unreadable)`, never a silent
   fall-through to some other installation you did not ask about; a relative explicit root returns
   `unknown(explicit_root_relative)` before any filesystem, environment, PATH, manifest, or heuristic
   probe;
2. `VCPKG_ROOT` — must be absolute; a relative value is terminal
   `unknown(env_root_relative)` and is never bound to the hub daemon's working directory;
3. `PATH`;
4. manifest;
5. machine-layout heuristics — **reported as candidates, never selected**.

There are no hardcoded machine paths. A path that appears in a test fixture is test data.

Patch analysis admits the complete triplet-root chain before touching the filesystem. More than the
published overlay maximum returns `failed(too_many_overlay_triplet_roots)`. Every nonblank relative
overlay root returns `failed(relative_overlay_triplet_root)`, and a relative `vcpkg_root` returns
`failed(relative_vcpkg_root)`. All three failures occur before filesystem access.
`var_overrides` is likewise admitted before filesystem access: its entry count,
per-name bytes, per-value bytes, and aggregate bytes are bounded. Exceeding any
of those ceilings returns `failed(var_overrides_limit_exceeded)`.

## Two limits worth knowing

- **Every successful result has a 256-KiB exact JSON ceiling.** Aggregate-heavy
  tools perform bounded admission before full JSON materialization; an ordinary
  result under the ceiling is unchanged. If a complete result exceeds it, the owning
  tool returns its causal core and an additive `result_projection` object with
  `complete: false` and closed omission metadata; it never slices JSON or
  presents a retained prefix as complete.

- **`vcpkg_pin_status` cannot report "behind".** `git ls-remote` can prove a ref is at the tip or that
  it cannot be compared; it cannot measure distance. So you get `current` or `unknown(<why>)`. Tags and
  branches are `unknown(named_ref_not_comparable)` with the commit they point at — existence proves the
  remote still has that name, not that it is current.
- **A credential-bearing or unclassified value-bearing remote URL is refused, not queried.** Redaction
  cannot reach a child process's `argv`, which is world-readable for its lifetime. Positive credential
  evidence returns `unknown(remote_url_credential_bearing)`; any other non-empty query value returns
  `unknown(remote_url_query_unclassified)`. Empty or valueless query segments remain admissible. Use a
  credential helper.
- **A scheme-less relative local Git remote is refused, not daemon-relative.** It returns
  `unknown(remote_url_relative)` before a child process starts. Absolute local paths and
  host-qualified URL/SCP remotes retain their existing behavior.
- **Pin-status remote work is bounded as one batch.** Duplicate approved remotes share one
  call-scoped snapshot, at most four remote queries run across the process, and one 60-second
  deadline covers the whole batch including slot wait time.
- **A failed remote lifecycle has a typed causal field.** A per-port remote
  failure retains the established `status` and `reason` and additionally emits
  `failure.id` with a fixed safe `failure.detail`. It never publishes raw Git
  stderr, process error text, remote URLs, or credentials in that field.
- **Caller-supplied filesystem targets are absolute-path contracts.** A relative `vcpkg_pin_status.port_dirs`
  entry returns `failed(relative_port_dir)` before filesystem or network access; a relative
  `vcpkg_cmake_trace.trace_path` returns `failed(relative_trace_path)` before filesystem access. The hub never
  binds either value to its own working directory.
- **Overlay batches are admitted before inspection.** `vcpkg_port_resolution`
  publishes its package-owned maximum in the input schema; requests beyond it
  return `failed(too_many_overlay_roots)` without probing the filesystem.
- **Triplet input is complete-or-unknown.** `vcpkg_patches_apply` reads a
  selected triplet through the same bounded CMake-input admission as its
  portfile. An over-limit triplet returns
  `unknown(triplet_file_size_limit_exceeded)`; no prefix establishes facts.
- **Patch reachability and orphan identity fail closed.** A definitely active
  `return()` stops later extraction; a conditionally active one returns
  `unknown(patches_execution_uncertain)`. If a declared patch path retains an
  unresolved variable, orphan inventory is suppressed and reported as
  `unknown(orphan_scan_incomplete)` with
  `orphan_scan_stop_cause: unresolved_patch_identity`.
- **Projected CMake traces remain auditable.** Oversized trace results retain a
  bounded trace-path identity and enumerate exact omissions for `include_chain`,
  `records`, `executed_lines`, and `files_in_trace`.
- **Projected include graphs retain causal identity.** Oversized graph results
  keep the histogram plus a bounded prefix of `unscanned_files` and evidence
  paths. `result_projection.omissions` enumerates reduced `edges`, `files`,
  coverage holes, and evidence collections instead of silently dropping them.
- **Coverage-hole retention is bounded.** `cmake_include_graph` caps both the
  number and retained bytes of `unscanned_files`; `coverage_cap_truncated`,
  `dropped_coverage_holes`, and `retained_coverage_bytes` make that reduction
  explicit.
- **CMake trace parsing is json-v1 only.** An explicit header with another major returns
  `unknown(unsupported_trace_version)` and no partial records.
- **`vcpkg_last_failure` bounds work before it builds the response.** One call examines at most 1024
  port-directory entries, admits at most 64 relevant logs, reads at most 32 MiB per log and 256 MiB
  total, retains bounded diagnostic rank cells, and emits at most 256 KiB of inner result JSON. The
  shared server admits two such scans at once. A limit, cancellation, or saturation returns the normal
  tri-state reasons `artifact_limit_exceeded`, `metadata_limit_exceeded`, `resource_cancelled`, or
  `resource_busy`; it never turns partial evidence into `ok` or `failed`.
- **Bounded-output metadata is explicit.** `resources.completeness` names every evidence class,
  `resources.omitted` reports exact drops or lower-bound sentinel counts, and
  `resources.high_water` records the retained producer maxima. `diagnostics_dropped_exact=false` means
  the returned diagnostic-drop count is only a lower bound because not every evidence byte reached EOF.
  `resources.high_water.log_buffer_bytes` is the maximum scanner-owned read-plus-line buffer capacity
  for the call; it is not Go heap usage or process Working Set.
  A `failed` result always retains `first_error`, the same entry at `diagnostics[0]`,
  `diagnostic_log`, that path at `log_paths[0]`, and the exit code when it was known.

## Reporting a defect

Include **which endpoint answered** — the port, and what `initialize` returned in `serverInfo`. Right
after an upgrade two implementations can briefly be reachable at once, and "the fix did not work" and
"I tested the old binary" produce byte-identical symptoms. The endpoint identity is the only thing that
tells them apart.

Also note that a client session started before this server was registered will never see it —
`listChanged` only notifies already-connected clients, so a new server needs a client restart. That is
a client-side property, not a fault here.

## Contracts and decisions

`work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md` (per-tool contracts and invariants),
`2026-07-25-vcpkg-ground-truth-measured.md` (what was measured on a real tree), and
`2026-07-26-vcpkg-mcp-must-follow-the-in-hub-server-pattern.md` (why this is in the hub binary rather
than a standalone executable — including a retraction of an earlier claim that the implementation
already satisfied the no-guessing rule; it did not, and the gap is recorded there).
