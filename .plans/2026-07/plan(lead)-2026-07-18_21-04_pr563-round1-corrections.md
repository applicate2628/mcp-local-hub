# PR #563 round-1 correction plan snapshot

Canonical plan: `work-items/active/2026-07-16-productization-gui-solidify/item3-unitB-plan.md` §10.

Summary: TDD-correct the port-change navigation race, compose the same-port child bind window as
`Quiesce + Bind`, and distinguish valid actual TCP ports from persisted GUI-setting ports. Then regenerate
the frontend bundle and run the exact human-required frontend, build, vet, formatting, and tagged GUI/CLI
checks. No commit, process sweep/kill, real GUI spawn, Graphify, `claude` CLI, or spawn-test environment flag.

