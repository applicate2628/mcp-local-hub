# Bug: CST saved-field authority identity is not enforced by the runtime walkers

- id: 2026-08-12-cst-saved-field-authority-identity-not-enforced
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: CST saved-field policy, Windows identity, and workspace transfer
- found-by: security-engineer
- candidate: c7654794c79ee4d8eed5378117e2ce84f7608040
- fix-class: implementation

## Reproduction

1. `WindowsPolicyPlatform._prove` closes the proved policy/root handle before the
   policy is read or later used (`cst_saved_field_policy.py:88-106`). Policy size
   and bytes are then obtained through a new path lookup
   (`cst_saved_field_policy.py:522-528`).
2. `_owner_only_access` reads security by path and accepts any allow entry whose
   trustee is not one of eight string markers; it does not enforce the declared
   owner/SYSTEM/Builtin-Administrators allowlist or evaluate arbitrary security
   identifiers and access masks (`cst_saved_field_policy.py:204-255`).
3. The helper does not use `prove_windows_path` for the project or any manifest
   row. It inventories with `Path.resolve`, `rglob`, `lstat`, and path-based
   `os.open`; on Windows, `os.O_NOFOLLOW` is absent and therefore becomes zero
   (`cst_saved_field_transfer.py:94-184,301-381`).
4. The helper creates an ambient `tempfile.mkdtemp` workspace rather than using an
   injected, local, owner-restricted root (`cst_saved_field_helper.py:235-238`).
5. Vendor payload validation accepts `model/Result/field.sct:secret`; a current-
   session smoke probe returned that exact value from `validate_candidate_records`.

## Expected versus actual

- Expected: one held, no-follow Windows identity owner validates the policy,
  configured roots, every source/destination/vendor path, and the injected trusted
  workspace; only owner, SYSTEM, and Builtin Administrators have effective access.
- Actual: startup checks are detached path observations, runtime source/copy walkers
  bypass that owner, and Windows Alternate Data Stream/device/alias enforcement is
  not wired into vendor or transfer paths.

## Security impact

A policy or source object can be replaced after its detached proof, a non-owner
local trustee may retain access, and bytes outside the authorized default-stream
object model may reach the helper/vendor. This falsifies SEC-C01, SEC-C02, SEC-C04,
SEC-C05, SEC-C07, SEC-C14, and SEC-C18.

## Required correction and falsifying probe

Use one handle-relative, no-follow Windows path/identity implementation for policy
read, root anchoring, complete source-to-destination transfer, vendor payload,
generated header, and registration. Parse effective access-control entries by
security identifier and mask. Inject and validate the trusted workspace root. Run
the full per-role reparse/swap/hard-link/stream/device/mapped-drive/owner/access
matrix and prove zero content/helper/vendor work on every denial.

## Related records

- `2026-08-12-cst-saved-field-windows-ads-alias-escape.md`
- `2026-08-12-cst-saved-field-complete-manifest-copy-drift.md`
