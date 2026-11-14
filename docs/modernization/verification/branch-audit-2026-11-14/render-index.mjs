import {readFileSync,writeFileSync} from 'node:fs';
import {join,resolve} from 'node:path';
import assert from 'node:assert/strict';
const dir=resolve(process.argv[2]);
const branches=JSON.parse(readFileSync(join(dir,'branches-before.json'),'utf8'));
const prs=JSON.parse(readFileSync(join(dir,'open-pr-analysis-before.json'),'utf8'));
const decision={default:'Leave master unchanged','open-pr':'Own PR: synchronize/qualify; relay630 may be superseded by629','protected-active-session':'READ ONLY: parallel session','contained-in-master':'Historical contained ref; retain, no automatic deletion','merged-pr-retained-ref':'Merged PR exact head, different ancestry; retain history','closed-pr-not-ancestry-proven':'Closed but inclusion not proven; preserve for separate residue review','unassociated-not-proven':'No associated PR; preserve unique work'};
let text='# Branch inventory — pinned before snapshot\n\n[Audit and decisions](RESULTS.md) · [Machine-readable records](branches-before.json)\n\n## Contents\n\n1. [Open pull requests](#open-pull-requests)\n2. [All remote branches](#all-remote-branches)\n\n## Open pull requests\n\n**Table 1. Own delta relative to each actual base, not accumulated stack size.**\n\n| PR | Base | Own files | Behind accepted master | Responsibility |\n|---|---|---:|---:|---|\n';
for(const p of prs.sort((a,b)=>a.number-b.number))text+=`| [#${p.number}](https://github.com/applicate2628/mcp-local-hub/pull/${p.number}) | \`${p.base}\` | ${p.files.length} | ${p.behind_master} | ${p.protected_active_session?'Protected parallel session':p.number===630?'Combine in629 after verified containment':'Own isolated review target'} |\n`;
text+='\n## All remote branches\n\n**Table 2. All93 refs from refs-before.tsv. Behind/ahead are exact graph counts, not unimplemented-feature counts.**\n\n| Branch | Snapshot head | Last associated PR | Ahead / behind master | Disposition |\n|---|---|---:|---:|---|\n';
for(const b of branches.sort((a,b)=>a.name.localeCompare(b.name)))text+=`| \`${b.name}\` | \`${b.sha.slice(0,12)}\` | ${b.latest_pr?'#'+b.latest_pr:'—'} | ${b.ahead} / ${b.behind} | ${decision[b.state]} |\n`;
assert.equal(branches.length,93);assert.equal(prs.length,19);writeFileSync(join(dir,'BRANCHES.md'),text,'utf8');
// Preserve original roadmap bytes exactly; add only a small ASCII checkpoint.
const plan=resolve(dir,'../../plans/rust-independent-execution-plan-2026-11-12.md');
const bytes=readFileSync(plan);
const prefix=Buffer.from('> Current checkpoint (2026-11-14): [all-branch audit, convergence and reserved parallel work](../verification/branch-audit-2026-11-14/RESULTS.md). The original November 12 baseline below is historical; completed work and current integration boundaries are recorded in the linked report and existing PRs.\n\n','utf8');
assert(!bytes.includes(Buffer.from('Current checkpoint (2026-11-14):')),'do not duplicate checkpoint');
writeFileSync(plan,Buffer.concat([prefix,bytes]));
console.log('93 branch rows,19 PR rows; original roadmap bytes preserved after new checkpoint.');
