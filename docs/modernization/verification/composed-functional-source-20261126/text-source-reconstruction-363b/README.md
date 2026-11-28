# Exact composed Source from text deltas

## Contents

1. [Contract and inputs](#1-contract-and-inputs)
2. [Reconstruct the complete snapshots](#2-reconstruct-the-complete-snapshots)
3. [Optional intermediate and original commit objects](#3-optional-intermediate-and-original-commit-objects)
4. [Complete inventory and verification evidence](#4-complete-inventory-and-verification-evidence)
5. [Historical results and limits](#5-historical-results-and-limits)

## 1. Contract and inputs

This text capsule preserves the exact composed Source6d and Source52 snapshots. It reuses the public e66 object graph, the two unchanged full-index text patches and the three original raw commit payloads. The additional 524-byte status patch records the intermediate 5ed status change. No incremental bundle, encoded bundle or substitute binary transport is included.

Table 1. Exact Git identities, using the repository's SHA-1 object format.

| Snapshot | Commit | Complete tree |
|---|---|---|
| Public e66 base | `e66f5c6fab7da2a2eff16ff1f0891991ca1a839c` | `4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c` |
| Intermediate5ed | `5ed87ff759405efa739229b7307b5c2f9579c8fc` | `75c703052e518b8f286f3aa4f079f630de951441` |
| Source6d | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` | `277dad42f9e9080d66955fa7469fbc68bc8618e4` |
| Source52 | `52a2d9ef57b7c372b9fa149adf062ad7e503b22d` | `51258ebd9ad3a654418b6ecb9163947734ef0314` |

First obtain the exact public e66 snapshot using the [existing composition instructions](../README.md#4-восстановление-source) and [public P02 bootstrap](../p02-capsule/README.md). Their earlier public recovery proof remains separate. This capsule does not require private ancestors or a new persistent environment. The older incomplete P02 parent history remains incomplete; original commit headers are not rewritten to conceal that limit.

Check artifact byte counts and SHA-256 pins in the two `source.json` manifests and [evidence.json](evidence.json) before use. SHA-256 binds the supplied bytes; Git SHA-1 identities in Table 1 bind typed Git objects. These are separate hash domains. Preserve the patch bytes, including any carriage returns in historical line-ending changes.

## 2. Reconstruct the complete snapshots

The commands below use POSIX shell notation. `repo` is the absolute path to the disposable bare Git database reconstructed by the public base instructions. `capsule` is the absolute path to the enclosing `composed-functional-source-20261126` directory. `work` is a new, caller-owned temporary directory. On Windows, keep that directory in the authorized workspace and use the equivalent literal Git arguments in the selected shell. The commands write only the disposable object database and fresh indexes; no existing Source checkout or publication branch needs to be reset.

Use a shell scope without inherited `GIT_DIR`, `GIT_COMMON_DIR`, `GIT_WORK_TREE`, `GIT_OBJECT_DIRECTORY` or private alternate overrides. The following block assumes those three path variables have been set. Stop on every nonzero exit or identity mismatch.

```sh
set -eu
test ! -e "$work"
mkdir "$work"
export GIT_INDEX_FILE="$work/source.index"
export GIT_OPTIONAL_LOCKS=0 GIT_TERMINAL_PROMPT=0
export GIT_NO_REPLACE_OBJECTS=1 GIT_NO_LAZY_FETCH=1

git -C "$repo" -c core.autocrlf=false read-tree 4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c
test "$(git -C "$repo" write-tree)" = 4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c
git -C "$repo" ls-files --stage -z > "$work/index-e66.z"

git -C "$repo" -c core.autocrlf=false apply --cached --binary --whitespace=nowarn "$capsule/source6d-363b/e66-to-source6d.patch"
test "$(git -C "$repo" write-tree)" = 277dad42f9e9080d66955fa7469fbc68bc8618e4
git -C "$repo" ls-files --stage -z > "$work/index-6d.z"
cp "$GIT_INDEX_FILE" "$work/source6d.index"

git -C "$repo" -c core.autocrlf=false apply --cached --binary --whitespace=nowarn "$capsule/source52-363b/source6d-to-source52.patch"
test "$(git -C "$repo" write-tree)" = 51258ebd9ad3a654418b6ecb9163947734ef0314
git -C "$repo" ls-files --stage -z > "$work/index-52.z"
cp "$GIT_INDEX_FILE" "$work/source52.index"

test "$(git -C "$repo" hash-object -t commit "$capsule/source6d-363b/commit-5ed.raw")" = 5ed87ff759405efa739229b7307b5c2f9579c8fc
test "$(git -C "$repo" hash-object -t commit "$capsule/source6d-363b/commit-6d.raw")" = 6d4fa7b8f9b86079020af125b030a26350cb5bb0
test "$(git -C "$repo" hash-object -t commit "$capsule/source52-363b/commit-52.raw")" = 52a2d9ef57b7c372b9fa149adf062ad7e503b22d
unset GIT_INDEX_FILE
```

There is one public base read-tree operand. Each target is computed by applying exact text changes to the received index. `--whitespace=nowarn` prevents rewriting historical Source whitespace; it is not a publication-scanner exemption. No `--3way`, network fetch, target-commit checkout or ancestry walk supplies missing changes.

These commands specify the same base, two patches and tree checks that were received natively. The documented shell block itself was not launched as an additional test run; the actual native execution used the retained command owner described in section 4.

## 3. Optional intermediate and original commit objects

The snapshot route above does not need the intermediate 5ed tree to materialize either final tree. To also restore the exact three commit objects, the intermediate can be reconstructed from the saved verified 6d index. Reverse the original status patch and restore the two exact e66 test blobs:

```sh
set -eu
test ! -e "$work/source5ed.index"
cp "$work/source6d.index" "$work/source5ed.index"
export GIT_INDEX_FILE="$work/source5ed.index"
git -C "$repo" -c core.autocrlf=false apply --cached --reverse --whitespace=nowarn "$capsule/source6d-363b/status5ed-to6d.patch"
git -C "$repo" update-index --cacheinfo 100644,5d739d3e0b5fcc4ca880b432d5a3c8c5e4a2d18f,internal/cli/supervise_status_process_test.go
git -C "$repo" update-index --cacheinfo 100644,14abb540847502c65473ed2a75446e34a73d16d3,internal/cli/supervisor_controller_orphan_reap_test.go
test "$(git -C "$repo" write-tree)" = 75c703052e518b8f286f3aa4f079f630de951441
unset GIT_INDEX_FILE

test "$(git -C "$repo" hash-object -w -t commit "$capsule/source6d-363b/commit-5ed.raw")" = 5ed87ff759405efa739229b7307b5c2f9579c8fc
test "$(git -C "$repo" hash-object -w -t commit "$capsule/source6d-363b/commit-6d.raw")" = 6d4fa7b8f9b86079020af125b030a26350cb5bb0
test "$(git -C "$repo" hash-object -w -t commit "$capsule/source52-363b/commit-52.raw")" = 52a2d9ef57b7c372b9fa149adf062ad7e503b22d
```

The intermediate status blob must be `018b4d9ad0648f0c54b3d2d1835cf87eb465cb90`, 23149 bytes, SHA-256 `f40e3a5c0059c773efba429a9ebab095067d9c23f15754f86e6c7f8cc8622e99`. Its forward/reverse text transformation was checked offline. The native run separately encoded the complete 5ed tree model from the received 6d index and exact three replacements. It **did not** reread the intermediate blob or execute this optional native patch/commit restoration. Those scopes remain distinct.

Writing the supplied raw commits preserves their existing headers byte-for-byte. It adds no invented ancestor and gives no authority to update a publication ref. A later fresh checkout or test invocation is a separate operation with its own environment and ownership requirements.

## 4. Complete inventory and verification evidence

[e66-tree-files.tsv](e66-tree-files.tsv) contains one header and all 3867 e66 entries, with columns `mode`, `blob_oid` and repository-relative `path`. All modes are 100644. This is a textual rendering of the original NUL-delimited native index, whose SHA-256 is recorded in [evidence.json](evidence.json).

To derive the complete Source6d manifest, require every `base_blob` in [Source6d source.json](../source6d-363b/source.json) to match its e66 entry, then replace exactly those nine entries with `source_blob`, keeping mode 100644. To derive Source52, additionally require and replace the single `parent_blob` from [Source52 source.json](../source52-363b/source.json). Every other path/mode/blob remains inherited. The small older `source*-files.txt` files are selected changed-path observations, not complete tree listings.

The actual native reconstruction completed with 14 original commands and 28 original readers CLOSED. Exact receipt SHA-256: `f059068dcfefe637e8048bfa6f8bc7b77157c8818b2a038a74cb38d4334a90fd`. All original Jobs/processes were reaped, active-zero and handles closed; all stdout/stderr workers reached EOF, joined and closed their captures. The same parent and outer process objects were received. No command deadline was extended and no original invocation was retried.

Each received index had 3867 entries and 274 directory objects. Native `write-tree`, the native independent encoder and two independent local receivers agreed on every tree object's Git ID, byte length and SHA-256. Complete dictionaries matched exactly nine replacements and then one. All three raw commit identities, tree and parent fields matched their original bytes. All 17 observed pins matched in all 13 step censuses. The sanitized evidence file records these facts without republishing raw machine paths or captures.

## 5. Historical results and limits

The earlier bundle export verification/head outputs remain in both Source directories as historical textual evidence. Their original binary payloads and rejected commit 58e were preserved outside this replacement. Historical A/B reports can therefore still mention that earlier export; the active reconstruction contract is the text route above.

The original #664 publication attempt at `58e6d54ecfcbbe00a6ba14869e6bd66da4de5b74` remains FAIL/CLOSED, with `PGG-SCAN-FINDING` at range-coverage and original receipt `820676d027233591c2f1074a7cea396669f51ba06cd0c4439c4045a616b1ca02`. This text representation starts from the already-public parent `bd7d32c044a84ae467e9f23106fc026d7c2dadec`, retaining both intervening upstream README updates unchanged; it does not remove binaries in a child that would leave them in outgoing history. A successful reconstruction does not itself establish publication approval. The ordinary scanner must inspect the complete new outgoing range.

This result proves complete Git tree identity and exact text delta application. It does not create a complete checkout, reread every unchanged blob, repeat the public cold-bootstrap proof, establish complete old ancestry or run a Go build/test suite. Unchanged bytes inherit the established public e66 snapshot graph. The independently checked ten changed final postimages remain bound by their exact patch/blob identities.

Historical FAIL, HOLD, UNKNOWN and V1 results retain their original scopes. Later scoped A acceptance is reported in the existing [#660](https://github.com/applicate2628/mcp-local-hub/pull/660) and [#669](https://github.com/applicate2628/mcp-local-hub/pull/669) records; those results do not establish full B or native Scheduler qualification. This source representation makes no additional runtime or release-readiness claim.
