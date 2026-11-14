// Read-only analysis of pinned Git/GitHub snapshots. No fetch/push/checkout/delete.
import {readFileSync, writeFileSync, existsSync} from 'node:fs';
import {execFileSync, spawnSync} from 'node:child_process';
import {resolve, join} from 'node:path';
import {createHash} from 'node:crypto';
import assert from 'node:assert/strict';
const [repoArg, dirArg, suffix='before']=process.argv.slice(2);
if(!repoArg||!dirArg||!/^[a-z0-9-]+$/i.test(suffix))throw Error('repo, snapshot directory and safe suffix required');
const repo=resolve(repoArg),dir=resolve(dirArg);
const git=(...args)=>execFileSync('git',['-C',repo,...args],{encoding:'utf8',maxBuffer:32*1024*1024,windowsHide:true}).trim();
const ancestor=(a,b)=>{const r=spawnSync('git',['-C',repo,'merge-base','--is-ancestor',a,b],{encoding:'utf8',windowsHide:true});if(![0,1].includes(r.status))throw Error(r.stderr);return r.status===0;};
const save=(name,value)=>writeFileSync(join(dir,`${name}-${suffix}.json`),JSON.stringify(value,null,2)+'\n');
const refs=readFileSync(join(dir,`refs-${suffix}.tsv`),'utf8').trim().split(/\r?\n/).map(line=>{const m=/^([0-9a-f]{40})\s+refs\/heads\/(.+)$/.exec(line);assert(m,'invalid ref snapshot');return {sha:m[1],name:m[2]};});
assert.equal(new Set(refs.map(r=>r.name)).size,refs.length);
assert.equal(git('rev-parse','--is-shallow-repository'),'false','ancestry requires complete fetched history');
const pulls=JSON.parse(readFileSync(join(dir,`pulls-${suffix}.json`),'utf8'));
const open=pulls.filter(p=>p.state==='open');
const master=refs.find(r=>r.name==='master');assert(master);
const byName=new Map(refs.map(r=>[r.name,r]));
const branches=refs.map(r=>{
 const related=pulls.filter(p=>p.head_ref===r.name&&p.head_repo==='applicate2628/mcp-local-hub').sort((a,b)=>b.number-a.number);
 const contained=ancestor(r.sha,master.sha);
 const [behind,ahead]=git('rev-list','--left-right','--count',`${master.sha}...${r.sha}`).split(/\s+/).map(Number);
 const active=related.some(p=>p.state==='open'&&p.number>=632)||r.name==='verify/serena-postmerge-20261114';
 const merged=related.some(p=>p.merged_at&&p.head_sha===r.sha);
 const state=r.name==='master'?'default':active?'protected-active-session':related.some(p=>p.state==='open')?'open-pr':contained?'contained-in-master':merged?'merged-pr-retained-ref':related.length?'closed-pr-not-ancestry-proven':'unassociated-not-proven';
 return {...r,state,ahead,behind,tree:git('rev-parse',`${r.sha}^{tree}`),contained_in_master:contained,latest_pr:related[0]?.number??null,related_prs:related.map(p=>({number:p.number,state:p.state,merged_at:p.merged_at,exact_head:p.head_sha===r.sha})),last_commit:git('show','-s','--format=%cI',r.sha)};
});
const prs=open.map(p=>{
 const base=byName.get(p.base_ref);const head=byName.get(p.head_ref);
 assert(base,`missing base branch ${p.base_ref}`);assert(head,`missing head branch ${p.head_ref}`);
 assert.equal(head.sha,p.head_sha,`head changed during snapshot #${p.number}`);
 const mb=git('merge-base',base.sha,p.head_sha);
 const files=git('diff','--name-only',mb,p.head_sha).split('\n').filter(Boolean);
 const raw=git('diff','--raw','--no-abbrev',mb,p.head_sha);
 const fingerprint=createHash('sha256').update(raw).digest('hex');
 const [behind,ahead]=git('rev-list','--left-right','--count',`${master.sha}...${p.head_sha}`).split(/\s+/).map(Number);
 return {number:p.number,title:p.title,head:p.head_ref,sha:p.head_sha,base:p.base_ref,base_sha:base.sha,merge_base:mb,base_is_ancestor:ancestor(base.sha,p.head_sha),ahead_master:ahead,behind_master:behind,files,change_fingerprint:fingerprint,protected_active_session:p.number>=632};
});
const pairs=[];
for(let i=0;i<prs.length;i++)for(let j=i+1;j<prs.length;j++){
 const a=prs[i],b=prs[j];const common=a.files.filter(f=>b.files.includes(f));
 const aInB=ancestor(a.sha,b.sha),bInA=ancestor(b.sha,a.sha);
 if(common.length||aInB||bInA||a.change_fingerprint===b.change_fingerprint)pairs.push({a:a.number,b:b.number,a_ancestor_b:aInB,b_ancestor_a:bInA,same_head:a.sha===b.sha,same_change_fingerprint:a.change_fingerprint===b.change_fingerprint,overlapping_files:common});
}
const integrations=[];
for(const p of prs.filter(p=>p.number<=631)){
 const r=spawnSync('git',['-C',repo,'merge-tree','--write-tree',master.sha,p.sha],{encoding:'utf8',windowsHide:true,maxBuffer:32*1024*1024});
 if(![0,1].includes(r.status))throw Error(r.stderr);
 integrations.push({pr:p.number,master:master.sha,head:p.sha,clean_merge:r.status===0,tree:r.stdout.split(/\r?\n/)[0],diagnostics:r.stdout.split(/\r?\n/).slice(1).filter(Boolean)});
}
const counts=Object.fromEntries([...new Set(branches.map(b=>b.state))].map(k=>[k,branches.filter(b=>b.state===k).length]));
const summary={snapshot:suffix,master:master.sha,branches:refs.length,open_prs:prs.length,all_pr_records:pulls.length,branch_classes:counts,exact_duplicate_open_pairs:pairs.filter(p=>p.same_head||p.same_change_fingerprint).length};
save('branches',branches);save('open-pr-analysis',prs);save('overlaps',pairs);save('merge-predictions',integrations);save('summary',summary);
console.log(JSON.stringify(summary,null,2));
console.log('Open PRs: '+prs.map(p=>`#${p.number}:${p.files.length} files, behind=${p.behind_master}, base=${p.base}`).join('\n'));
console.log('Overlaps: '+JSON.stringify(pairs));
console.log('Private merge predictions: '+JSON.stringify(integrations.map(x=>({pr:x.pr,clean:x.clean_merge}))));
