---
severity: medium
context: adjacent-finding
---

- **status:** fixed
- **fixed-by:** PR #494 (`c1e59d99`) - embedded manifest-name create refusal and warning surfacing.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `TRIAGE-2026-07-09.md` for code/test evidence.

# Embed-first install silently shadows a same-named DISK manifest for any embedded server name

> **FIXED 2026-07-03** by `work-items/decisions/2026-07-03-embed-vs-disk-manifest-precedence.md`
> (Option B — embed always wins; the collision is made LOUD at the WRITE gate).
> Read precedence is UNCHANGED (embed-first stays); the fix stops the *silent*
> part. Implemented on branch `feat/embed-name-create-refusal`:
>
> - `isEmbeddedManifestName` predicate (`internal/api/manifest_source.go`,
>   override-symmetric) + exported `IsEmbeddedManifestName` wrapper.
> - `ManifestCreateIn` now HARD-REFUSES a disk manifest under a shipped/embedded
>   name (wrapping `ErrManifestNameEmbedded`), BEFORE the disk-stat guard — so a
>   re-created shipped server can no longer be written to disk and then ignored.
> - Warn surface for PRE-EXISTING collisions (install + scan) via
>   `embeddedDiskShadowWarning`; the user's file is NEVER deleted.
> - GUI mirrors: readiness Save-&-Install dry-run reports the refusal, and the
>   marketplace one-click maps the refusal to a friendly 400 (no opaque 500).

## Finding (adjacent finding surfaced during D2 r3 — NOT fixed here)

`loadManifestYAMLEmbedFirst` (`internal/api/manifest_source.go:81`) returns the EMBEDDED
(shipped) manifest YAML whenever the requested server name is present in the binary's embed
set, before ever consulting the on-disk dev-checkout path (`:84-86`). The install read path
goes through this helper, so for ANY embedded server name a user-authored disk manifest with
the SAME name is silently ignored at install time:

- A user re-adds (or hand-creates) a shipped server such as `wolfram` and edits its
  `command`/`args` in the GUI.
- `ManifestCreateIn` (`internal/api/manifest.go:444`, disk-stat-only guard `:449`) happily
  writes the edited manifest to disk (it gates only on a pre-existing DISK manifest, not on
  embed membership).
- Install reads embed-first → installs the UNEDITED shipped manifest while the UI reports the
  save/install succeeded.

This is NOT D2-specific. It affects any user who creates or edits a disk manifest under a name
that collides with a shipped/embedded server. There is currently no override mechanism that
lets a user disk manifest take precedence over the shipped one at install.

## What D2 r3 does about it (UX-layer mitigation only)

D2 r3 (FINDING 2) adds an honest, in-form notice in `AddServer.tsx`: when a catalog-match
Re-add prefills CREATE mode with a shipped name, a `shipped-server-notice` warns the operator
that installing re-installs the shipped manifest and that edits to command/args here won't
change what's installed — rename the server to save a customized copy. The notice is live only
while the form name is still the shipped name (honest on rename). This makes the symptom
visible but does NOT change the install read precedence.

## Why not fixed here (deeper behavior deferred)

Per the backend-engineer adjacent-findings protocol, the deeper behavior is outside the
approved D2 r3 change surface and has broad blast radius:

- `loadManifestYAMLEmbedFirst` is the shared read owner consumed by install, uninstall, scan,
  status_enrich, secrets_scan, migrate — changing its precedence is cross-cutting.
- "Should a user's disk manifest override a shipped manifest of the same name at install?" is a
  product/architecture decision (it trades the embed-as-source-of-truth invariant against
  user customization), and needs a `work-items/decisions/` registry item, not an implementer
  judgment call inside a 2×P2 UX-nicety fix.

PROTECTED in D2 r3 (0-diff): `loadManifestYAMLEmbedFirst` + `embeddedManifestNames`
(`manifest_source.go`), `ManifestCreateIn`/`ManifestCreate` (`manifest.go:444`), the install
read path (`install.go`).

## Suggested next step (for whoever picks this up)

Open a decision-registry item: decide whether shipped servers should support a user override
(e.g. a disk manifest under a reserved override dir, or an explicit `--prefer-disk` install
flag, or a per-server "customize" flow that forces a distinct name). If override is desired,
add a single owner for the precedence rule rather than branching at each consumer.
