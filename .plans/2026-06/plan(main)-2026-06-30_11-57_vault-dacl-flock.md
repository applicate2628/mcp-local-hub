Plan snapshot:

1. Verify reported paths, existing DACL and flock owners. Status: completed.
2. Add failing regression tests for access_denied classification/UI and flock coverage. Status: completed.
3. Implement minimal backend/frontend fixes. Status: completed.
4. Run focused and requested verification commands. Status: completed.
5. Generate assets, safety scan, commit, push. Status: completed.

Execution notes: Fetched first. Codegraph was unavailable until initialized with `codegraph init -i`; after initialization it identified `*api.DACLAllowlistViolation`, `ErrDaclOutsideAllowlist`, `secrets.WithVaultLock`, `classifyVault`, `SecretsRotate`, and `SecretsDelete`. The pushed commit is `6d68e761427693d50cf427f24912b686b20ede37`.
