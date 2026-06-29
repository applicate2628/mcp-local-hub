# Cold-read DACL self-heal

Admission source: direct human request on 2026-06-29.

## Goal

Implement a Windows-only, owner-verified self-heal for stale hub-mcp state files whose own DACL grants a non-allowlisted SID tampering-capable rights, so hardened cold reads recover user-owned legacy files without weakening symlink, reparse, irregular-file, or foreign-owned refusals.
