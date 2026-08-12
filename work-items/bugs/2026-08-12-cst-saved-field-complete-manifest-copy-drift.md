# Bug: CST workspace copy is not bound to the complete authorized bundle manifest

- id: 2026-08-12-cst-saved-field-complete-manifest-copy-drift
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: CST saved-field authority manifest and workspace-copy boundary
- found-by: security-reviewer
- fix-class: design-decision

## Reproduction

Design-level counterexample against `design.md` SHA-256
`AAD6F5D5395EB1E3ADF289BE898878A59F584AA3193B671B3A71F40BDA7667F4`:

1. Admit a source bundle whose complete canonical file-list manifest matches the
   operator policy (`design.md:261-280`).
2. After that check and before its copy, replace or change one ancillary regular
   bundle file that is not the project, `Result/3d.slim`, or selected `.sct`.
3. The declared copy/postflight flow verifies only project and mesh at copy time
   (`design.md:467-470`), the selected field later (`design.md:482-484`), and those
   same three monitored roles at settlement (`design.md:372-373,548-558`).
4. The copied project may therefore consult ancillary bytes that do not belong to
   the authorized complete manifest while all named success checks still pass.

## Expected versus actual

- Expected: every path, size, file identity, and byte hash presented to CST in the
  disposable bundle is exactly represented by the single operator-authorized
  complete manifest before any CST/vendor creation.
- Actual design: the full source manifest is checked before copy, but no complete
  destination-manifest equality proof binds all copied ancillary bytes to it.

## Security impact

This is a multi-file authorization time-of-check-to-time-of-use gap. It can place
unapproved ancillary bytes inside the write-capable CST workspace despite exact-
bundle policy admission. It falsifies security claims `SEC-C04`, `SEC-C05`, and
`SEC-C14`.

## Required correction

Return to `$security-engineer` and `$architect`. Define one coherent snapshot
guarantee that binds every vendor-consumed workspace byte to the authorized manifest
and state whether settlement proves unchanged source state or unchanged authorized
copy state. The decision is cross-module and security-sensitive; an implementation-
local check is insufficient.

## Falsifying probe

For every non-project/non-mesh/non-selected manifest entry, inject mutation at each
enumerate/open/read/copy boundary and inject one add/remove operation. Assert that no
CST session is created unless the complete destination manifest—cardinality, path,
size, identity, and SHA-256—equals the policy manifest exactly.

## Provenance

- Finding: `SR-01` in `security-review.md`.
- Security review SHA-256:
  `9EEE85250CF927BE9D89D5A6E48A8E8437F23417EEBD6D8A9C4B4165EABBE316`.
- Upstream security constraints SHA-256:
  `E3FF52C6F35D617BDA3E774838C4E88441C5195CBB32A11E113ECE33EF17715C`.
