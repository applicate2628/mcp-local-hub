# Plan

1. Add failing Windows tests for owner-owned broad own-DACL self-heal and foreign-owned broad own-DACL refusal.
2. Implement handle-bound owner verification and DACL tightening in the Windows inode-anchored read helper.
3. Re-run the targeted internal/api tests with pinned caches and temp HOME.
4. Run build, vet, GOOS linux/darwin build, publication-safety scan.
5. Commit and push `feat/cold-read-dacl-selfheal`.
