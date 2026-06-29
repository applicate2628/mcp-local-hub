# Closure

Closed: 2026-06-29

Outcome: IMPLEMENTED — Windows hardened state-file reads now self-heal only the existing file-DACL write/DAC/delete refusal class when the file owner read from the held handle is the current process user. The heal applies the protected owner-only DACL on the held handle, re-verifies it, emits `state-file-dacl-self-healed`, and continues the handle-bound read. Foreign-owned files, symlinks/reparse points, irregular files, parent-DACL refusals, strict mode, and the write path remain outside the heal boundary.

Archive location: `work-items/archive/2026-06/2026-06-29-cold-read-dacl-selfheal/`.

Residual risk: the foreign-owner file integration test is privilege-gated because this non-elevated Windows token cannot assign a different allowlisted owner to a temp file. The exact owner predicate is covered by `TestStateFileDACLSelfHealOwnerCheckRequiresCurrentUser`; elevated runs exercise `TestReadStateFileInodeAnchored_FileDACLWriteBroadenedForeignOwnerDoesNotSelfHeal`.

Follow-up: human security-reviewer gate before merge.
