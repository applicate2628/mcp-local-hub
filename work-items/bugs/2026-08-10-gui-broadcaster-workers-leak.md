# Bug: Full GUI suite leaves nine broadcaster persistence workers

- id: 2026-08-10-gui-broadcaster-workers-leak
- context: adjacent-finding
- status: open
- severity: high
- area: internal/gui test broadcaster ownership
- found-by: qa-engineer

The candidate, its identical diagnostic rerun, and immutable `HEAD` all end
with `TEST_BROADCASTER_LIFECYCLE_LEAK drainPersist=9 runDropReporter=0`.
This is pre-existing and not caused by A-D, but the canonical GUI gate remains
red until every test-created broadcaster has an explicit close owner.
