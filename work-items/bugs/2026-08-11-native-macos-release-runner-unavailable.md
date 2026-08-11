# Bug: Required native macOS release runner is unavailable

- id: 2026-08-11-native-macos-release-runner-unavailable
- context: 2026-08-11-windows-console-opt-in-r2
- status: open
- severity: high
- area: release verification infrastructure
- found-by: qa-engineer

Fresh direct probes found zero configured GitHub Actions runners, no local SSH configuration, and no macOS runner binding. Repository workflows contain Darwin cross-build commands only; cross-compilation is not native execution.

Expected: an authorized native macOS target executes the console/release gate, or the operator explicitly narrows the supported release platform.

Actual: native macOS remains `BLOCKED:dependency`. Raw probes are `.scratch/windows-console-contract/qa-final-r2-20260811-143303/macos-gh-auth.txt`, `macos-runners.json`, and `macos-workflow-probe.txt`.
