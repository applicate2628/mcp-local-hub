// Exact, bounded resolution of the one observed master/relay composition conflict.
// Run ONLY in the private relay integration worktree, before committing the merge.
import {execFileSync} from 'node:child_process';
import {writeFileSync} from 'node:fs';
import {join} from 'node:path';
import assert from 'node:assert/strict';
const repo=process.argv[2];assert(repo);
const master='17bc78fc602efa204a74596409c939a3bf1ddc42';
const path='cmd/mcphub/main.go';
const blob=execFileSync('git',['-C',repo,'rev-parse',`${master}:${path}`],{encoding:'utf8'}).trim();
assert.equal(blob,'10e7aaf4c614bc9f3bad5c1df404ac7471181f1a');
const original=execFileSync('git',['-C',repo,'show',`${master}:${path}`]).toString('utf8');
const before='if err := cli.NewRootCmdWithWindowsCanonicalEntryBinding(entryBinding, entryBindingErr).Execute(); err != nil {';
const after=`root := cli.NewRootCmdWithWindowsCanonicalEntryBinding(entryBinding, entryBindingErr)
\t// Only the executable bootstrap owns this process's inherited stdin.
\t// Keep canonical-entry admission and borrowed library inputs unchanged.
\tif os.Stdin != nil {
\t\tif err := cli.BindProcessRelayInput(root, os.Stdin); err != nil {
\t\t\tfmt.Fprintln(os.Stderr, "error:", err)
\t\t\tos.Exit(1)
\t\t}
\t}
\tif err := root.Execute(); err != nil {`;
assert.equal(original.split(before).length,2);
const result=original.replace(before,after);
assert.equal(result.replace(after,before),original,'unrelated master bytes must stay exact');
writeFileSync(join(repo,path),result,'utf8');
console.log('Exact master canonical-entry binding preserved; explicit process stdin grant composed once.');
