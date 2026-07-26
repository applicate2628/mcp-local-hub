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
| `cmake_include_graph` | Which CMake files include which, resolved statically, with dangling and unresolved edges distinguished. |
| `vcpkg_discover_root` | Where the vcpkg root is, and by which rule it was decided. |

## Roots and overlays come from you, never from guessing

Build and install roots and the overlay chain are taken from vcpkg's own keys —
`--x-buildtrees-root`, `--x-install-root`, `--overlay-ports`, `--overlay-triplets` — passed as tool
parameters. Discovery order when you do not pass a root:

1. explicit parameter — **terminal**: if it is invalid or unreadable you get
   `unknown(explicit_root_invalid|explicit_root_unreadable)`, never a silent fall-through to some
   other installation you did not ask about;
2. `VCPKG_ROOT`;
3. `PATH`;
4. manifest;
5. machine-layout heuristics — **reported as candidates, never selected**.

There are no hardcoded machine paths. A path that appears in a test fixture is test data.

## Two limits worth knowing

- **`vcpkg_pin_status` cannot report "behind".** `git ls-remote` can prove a ref is at the tip or that
  it cannot be compared; it cannot measure distance. So you get `current` or `unknown(<why>)`. Tags and
  branches are `unknown(named_ref_not_comparable)` with the commit they point at — existence proves the
  remote still has that name, not that it is current.
- **A credential-bearing remote URL is refused, not queried.** Redaction cannot reach a child process's
  `argv`, which is world-readable for its lifetime, so a URL carrying userinfo is rejected outright.
  Use a credential helper.

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
