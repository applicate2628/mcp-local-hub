# Hub Initial-Bind Adversarial Token Rotation Plan

Date: 2026-07-18
Role: `$backend-engineer`
Status: completed

## Goal

Implement accepted Option B without reverting Phase C: rotate the hub InstanceID only when the failed initial-bind port owner is foreign or unverifiable, while preserving benign same-port recovery and honest exhaustion health.

## Completed Steps

- [x] Verify repository authority, branch state, accepted decision, Phase C recovery, owner classifier, warning emitter, and existing reconcile wiring.
- [x] Add tagged regression tests and observe the expected RED failure before production changes.
- [x] Add the bounded port-owner probe, reuse `daemonrecovery.ClassifyPortOwner`, and gate one `api.RotateHubInstanceID` call before retry.
- [x] Verify foreign/unverifiable rotation, verified-own/no-holder preservation, needs-reconcile publication, exhaustion-to-down, and credential-warning preservation.
- [x] Run `go build ./...`, `go vet ./...`, touched-file `gofmt -l`, and `go test -tags=test_state_path_env -count=1 -timeout 15m ./internal/api/ ./internal/gui/`.

## Constraints Preserved

- Phase C remains enabled.
- Gated restart-v3 machinery remains untouched.
- No `MCPHUB_GUI_SPAWN_TESTS` setting was introduced.
- No commit was created.

## Terms and Abbreviations

- API: Application Programming Interface.
- GUI: Graphical User Interface.
- RED: A test failure caused by the intentionally missing implementation.
