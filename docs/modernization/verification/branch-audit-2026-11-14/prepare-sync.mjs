// Prepare local, non-rewriting synchronization commits for OWNED dormant roots.
// Does not checkout, touch other sessions, push, close PRs or delete branches.
import {execFileSync} from 'node:child_process';
import {readFileSync,writeFileSync} from 'node:fs';
import {join} from 'node:path';
import assert from 'node:assert/strict';
const [repo,dir]=process.argv.slice(2);assert(repo&&dir);
const git=(...args)=>execFileSync('git',['-C',repo,...args],{encoding:'utf8',maxBuffer:16*1024*1024,windowsHide:true}).trim();
const master='17bc78fc602efa204a74596409c939a3bf1ddc42';
const records=JSON.parse(readFileSync(join(dir,'open-pr-analysis-before.json'),'utf8'));
const checked=git('worktree','list','--porcelain');
const receipt=[];
for(const id of [624,625,626,627,628]){
 const p=records.find(x=>x.number===id);assert(p);
 assert(!checked.includes(`branch refs/heads/${p.head}\n`)&&!checked.endsWith(`branch refs/heads/${p.head}`),'branch already checked out');
 assert.equal(git('rev-parse',`refs/remotes/origin/${p.head}`),p.sha);
 const output=git('merge-tree','--write-tree',p.sha,master);
 const tree=output.split('\n')[0];assert(/^[a-f0-9]{40}$/.test(tree));
 // Exact raw diff equality proves every original patch's before/after blobs and
 // file modes are preserved on the new base. It does not replace runtime tests.
 const before=git('diff','--raw','--no-abbrev',p.merge_base,p.sha);
 const after=git('diff','--raw','--no-abbrev',master,tree);
 assert.equal(after,before,`unexpected patch change #${id}`);
 const sha=git('commit-tree',tree,'-p',p.sha,'-p',master,'-m',`merge: align PR #${id} with accepted master without changing its scoped patch`);
 // A private local integration ref, not the remote shared branch. Remote updates
 // are a separate explicitly verified step after actual scoped qualification.
 git('update-ref',`refs/heads/convergence/pr${id}-20261114`,sha,'0000000000000000000000000000000000000000');
 receipt.push({pr:id,branch:p.head,old:p.sha,master,tree,new:sha,patch_identity_preserved:true,local_ref:`convergence/pr${id}-20261114`});
 console.log(`#${id}: ${p.sha.slice(0,8)} -> ${sha.slice(0,8)}, scoped patch exact`);
}
writeFileSync(join(dir,'prepared-sync.json'),JSON.stringify(receipt,null,2)+'\n');
