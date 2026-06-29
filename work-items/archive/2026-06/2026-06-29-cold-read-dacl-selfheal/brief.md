# Cold-read DACL self-heal

## Scope

- Change only the shared Windows hardened state-file read helper and local tests under `internal/api`.
- Self-heal only the existing file-DACL WRITE/DAC/DELETE refusal class.
- Verify the owner from the held file handle with `GetSecurityInfo(OWNER_SECURITY_INFORMATION)`.
- Tighten only if the owner is the current process user SID.
- Reuse the owner-only protected DACL installed by the hardened write path.
- Re-verify after tightening and retry the read once.
- Emit `warn` event `state-file-dacl-self-healed` with path and offending SID.

## Out Of Scope

- POSIX read path.
- Write path behavior.
- Symlink, reparse-point, irregular-file, parent-DACL, and foreign-owner refusals.
- Pull request creation.

## Risks And Owners

- Security boundary: security-engineer constraints; human security-reviewer gates before merge.
- Implementation: backend-engineer.
- Verification: targeted Go tests, build, vet, cross-OS build, publication-safety scan.
