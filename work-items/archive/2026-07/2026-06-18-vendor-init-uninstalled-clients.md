# Finding: vendor Initialize unavailable for not-yet-installed clients on a clean install

Date: 2026-06-18 (user: "инициализация не везде доступна при чистой установке")
Status: RESOLVED (2026-06-21) — secure-parent-create stack shipped, then the F1/F2/F3 follow-up
(branch fix/g17-followups) closed the wave-2 visibility merge-blocker + stale security comments + POSIX
divergence doc. Commission: security-reviewer PASS + sonnet + opus, all APPROVE. The 5-step design below was
implemented; F1 (DETECTED_PRESENCE_STATES omitted missing-init-creatable → wave-2 clients got no Initialize
button) was the gap that made it not actually work for the opt-in client set until the follow-up.
Epic: 2026-06-17-gui-quality-initiative

## Symptom (user-confirmed)
On a clean install, the Servers-matrix **Initialize** affordance is NOT shown for
every client column — only for clients whose config **parent directory already
exists**. A client that is not installed yet (e.g. `~/.cursor/` absent) cannot be
pre-configured through the GUI.

## Root cause (VERIFIED — this is security-by-design, not an oversight)
- `classifyMissingClientConfig` (internal/api/scan.go:302-328): config file missing
  AND parent dir exists+regular → `missing-init-possible` (button shown); parent dir
  ABSENT → `missing` (no button); parent is a symlink → `missing-init-blocked-symlink`.
- The Servers matrix only renders Initialize for `missing-init-possible`.
- The init endpoint `realClientInitializer.Init` (internal/gui/init_client_config.go:103-123)
  REQUIRES the parent to pre-exist (`errParentMissing` → 412) and is NOT a directory-creator.
- `MkdirAll` was DELIBERATELY REMOVED from `EnsureClientConfigStub` (PR #208 deep-sec
  Lane A round 2/3, init_client_config.go:128) so the init pipeline can never silently
  (re)create a parent — closing a symlink-TOCTOU / parent-planting window. The hardened
  `SecureCreateClientConfigIfMissing` pipeline refuses to follow a symlinked parent.

So "not available everywhere on clean install" = the deliberate consequence of: only
offer Initialize when there is a real, pre-existing, non-symlink parent dir to write into.

## The gap to close
The operator legitimately wants to pre-configure mcphub for a client they are ABOUT to
install (or one that creates its config dir lazily). Today that requires the parent dir
to already exist.

## Fix design (security-sensitive — needs codex/security review)
Offer Initialize for the `missing` (no-parent) case too, by creating the parent dir
SECURELY as part of the init flow:
1. **Backend:** extend the hardened create pipeline (internal/api/secure_create_client_config.go)
   with a handle-relative, symlink-refusing parent-dir create: `mkdir` the parent (and any
   missing ancestors under the user home) with owner-only mode, where each component create
   either succeeds atomically or fails if the path already exists as a non-dir / symlink
   (POSIX `mkdir`+O_NOFOLLOW semantics; Windows `CreateDirectoryW` fails if exists, + reparse
   refusal). NEVER `MkdirAll` blindly — create component-by-component with the same DACL/gate
   posture as the file create. Respect `MCPHUB_REQUIRE_SINGLE_USER_HOME` strict gate.
2. **Classifier:** make `classifyMissingClientConfig` return a NEW state for "no parent dir but
   creatable" (e.g. `missing-init-creatable`) — distinct from `missing-init-possible` so the
   UX can DIFFERENTIATE (the GUI can show Initialize with a "will create <dir>" tooltip), and
   keep `missing-init-blocked-symlink` suppressed.
3. **Endpoint:** allow Init when parent is absent → secure-create the parent then the stub;
   keep the 412 for symlink/non-dir parent.
4. **Frontend:** render Initialize for the new state too, with a tooltip naming the dir it will
   create (so the operator knows a config dir is being made for a not-yet-installed client).
5. **Tests:** clean-tmpHome where the parent dir is absent → Initialize available → click →
   parent dir + stub created with owner-only DACL; symlinked parent still suppressed/refused;
   strict-mode honored.

## Why a review loop
Creating directories/files for not-yet-installed clients re-opens exactly the surface PR #208
hardened. The secure-parent-create must be symlink/TOCTOU-safe and strict-mode-correct. Run a
codex (xhigh) + security review on the implemented diff before merge.

## Tracking
GUI initiative item G17 (added to work-items/ROADMAP.md). Implement after Wave A (G5/G7/G8)
integrates, as a dedicated security-reviewed lane.
