---
status: proposed
date: 2026-06-23
slug: d2-vendored-source
---

# D-2 — vendored/community-fork provenance + pin descriptor (catalog Tier-0)

## Context

The "install and it works" desktop catalog initiative (Tier-1) wants to list MCP
servers that come from a **vendored or community fork** rather than an upstream
package — e.g. a Mathcad/MATLAB COM-bridge that ships only as a GitHub repo the
operator builds locally. Such a source has no immutable package version; pointing
a catalog row at a moving branch (`main`, `HEAD`) means the fork content can change
underneath the pin between admission and install, defeating any vetting.

Tier-0 adds the SCHEMA SEAM for this provenance (no catalog DATA rows yet) so the
later Tier-1 rows have a validated shape to land in.

## Decision

Add an OPTIONAL `VendoredSource` descriptor on both sides of the catalog/manifest
boundary:

- `config.ServerManifest.VendoredSource *VendoredSource` (yaml `vendored_source`),
  with `{repo, pinned_ref, install_cmd, run_cmd, license_status}`. Metadata-only —
  nothing here spawns a process or writes a client config; `install_cmd`/`run_cmd`
  are human documentation, the real launcher stays `command`/`base_args`.
- `api.MarketplaceEntry.VendoredSource *CatalogVendoredSource` (JSON-tagged mirror)
  for `catalog.json`.

`PinnedRef` is the load-bearing safety field. Enforcement splits across two gates:

- **Gate A — `config.ServerManifest.Validate()`** (pure, host-state-free, runs at
  parse time so it fires at both `ManifestCreate` AND `Install`):
  - A1: `VendoredSource != nil` AND `pinned_ref` empty → REJECT. A vendored source
    MUST be pinned.
  - A2: `pinned_ref` is a well-known MOVING branch name
    (`main|master|head|latest|trunk|develop|dev`, case-insensitive) → REJECT. We do
    NOT attempt full SHA-shape validation (a short SHA or unusual tag is
    legitimate); only the conservative moving-branch set is rejected.
  - A3: `license_status` non-empty and not in `{confirmed,pending,unknown}` →
    REJECT (empty allowed == unknown for gate purposes).
  Placed in `Validate()` because pin-presence and enum-shape are decidable from the
  manifest struct alone, so a hand-authored bad manifest cannot be persisted at all.
- **Gate B — `api.AdmissionCheck()` (advisory)**: a vendored server whose
  `license_status` is not `confirmed` emits an OPTIONAL `vendored-license-unvetted`
  finding (does NOT block install). LICENSE-on-real-repo is a network/gh-API fact
  outside the pre-spawn/mcphub-fixable gate criterion
  (`2026-06-19-admission-check-single-gate.md`); D-4 records it at admission time.

The catalog-side `validateMarketplaceEntry` mirrors A1/A2/A3 as defense-in-depth so
a hostile registry cannot ship an unpinned vendored entry; the manifest Gate A is
the authoritative one post-projection. `generateCommandDraft` projects
`vendored_source` into the drafted (and persisted) hub-install manifest so the
post-install gate can see the pin; `generateRemoteHTTPDraft` does NOT (a vendored
fork is a local-stdio S1 concern, not a remote-URL S2 one).

## Additive guarantee

Both new fields are pointer + `omitempty`. Every existing manifest and every current
catalog entry omits them, decodes to `nil`, re-marshals without the key, and the
Validate helper short-circuits on `nil` — byte-identical behavior on a host with no
desktop rows.

## Consequences

- A community-fork server cannot be persisted or installed without an immutable pin.
- License vetting stays advisory (operator may knowingly install a pending-license
  fork on their own host) and is the D-4 protocol's responsibility, not a schema
  invariant.
- NO new client adapter, NO supervisor/IPC change, NO catalog DATA row in Tier-0.

The `$architecture-reviewer` promotes proposed → accepted.
