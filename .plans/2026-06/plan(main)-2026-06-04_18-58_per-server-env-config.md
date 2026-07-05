# Per-server env config and supervisor restart fixes

1. Inspect existing owners for env overlays, supervisor state, status, restart, and GUI settings.
2. Add narrow failing tests for CLI env config, env precedence, stale PID status/cold-start cleanup, liveness sweep events, supervisor restart routing, and GUI env listing.
3. Implement the shared env override path using the existing `daemon-env-overrides.yaml` convention under the resolved state directory.
4. Wire CLI commands, daemon spawn env overlay, GUI API/listing, and a minimal Settings editor.
5. Fix supervisor cold-start liveness reconciliation, runtime stale-running detection, status output, and supervisor-owned restart routing.
6. Apply the memory override to `D:\memory\memory.jsonl` through `mcphub config env set`.
7. Run required build, vet, narrow tests, race tests, frontend checks, GUI generation, and self-review.
