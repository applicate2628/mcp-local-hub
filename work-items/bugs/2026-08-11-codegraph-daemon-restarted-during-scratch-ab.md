# Bug: CodeGraph daemon restarted during in-repository scratch A/B

- id: 2026-08-11-codegraph-daemon-restarted-during-scratch-ab
- context: 2026-08-11-windows-console-opt-in-r2
- status: open
- severity: medium
- area: CodeGraph watcher and scratch isolation
- found-by: qa-engineer

The QA lane issued no install, restart, deployment, or Windows product command. It created two source snapshots only below the ignored `/.scratch/` boundary and invoked the connected CodeGraph read tool as required.

Observed: CodeGraph then reported snapshot-contained source paths as pending edits. The installed `codegraph` daemon PID 85776 exited with code 1 at `2026-08-11T12:12:46Z`; the live supervisor restarted installed PID 99560 at `12:12:49Z`. The supervisor stayed up, all 47 observed `mcphub.exe` processes remained at the installed path, and no scratch/test `mcphub.exe` existed.

Expected: ignored in-repository scratch snapshots do not enter the live CodeGraph watch/index surface and do not destabilize the daemon.

Actual: live-fleet bytes were not changed and no explicit fleet mutation occurred, but the fleet was observably not untouched. The snapshot/CodeGraph interaction is the prime suspect and remains `ASSUMPTION (UNVERIFIED)` until reproduced with a minimal ignored snapshot while tracing the watcher. Evidence is in `.scratch/windows-console-contract/qa-final-r2-20260811-143303/live-fleet-readonly-{snapshot,final}.txt` and the supervisor event rows quoted in the canonical QA report.
