# Exact P02 snapshot capsule

This capsule preserves the immutable source snapshot at commit
`b6f82b3f531bb3eec197049167cdc5971b44e2b0`, root tree
`645ddcd88647b8c628ec966f037762886bd56b31`. It reuses the objects in explicitly
listed public snapshots and two small Git packs. It does not replay the 38
integration stages or copy a source checkout.

## Contents

- [Object coverage](#object-coverage)
- [Public inputs](#public-inputs)
- [Reproduce the restoration](#reproduce-the-restoration)
- [Proof and limits](#proof-and-limits)

## Object coverage

Table 1 partitions the **4,107 unique objects** needed for the exact P02 snapshot.
The three sets are disjoint. Counts include its commit and root tree, and deduplicate
objects used by more than one path.

| Table 1. Object source | Unique P02 objects | Transport bytes |
|---|---:|---:|
| 43 declared public commit snapshots | 3,974 | Reused from an existing local repository |
| Original `p02-private-objects.pack` | 131 | 1,110,609 |
| Additional `p02-additional-objects.pack` | 2 | 20,814 |
| Exact P02 snapshot | **4,107** | |

The target contains one commit, 273 unique tree objects and 3,833 unique blobs.
The two additional blobs in Table 2 are the complete missing set relative to
the declared public snapshots and the unchanged original pack. No other P02
objects were included in the additional pack.

| Table 2. Additional object | P02 path | Uncompressed bytes |
|---|---|---:|
| `dc709de845f45906a6b4cd0541a86916cea17329` | `internal/cli/windows_product_pair_test.go` | 44,094 |
| `f51fe7e38b8fd2803a9949dc9c33af0ce3b8cf61` | `internal/api/register_supervisor.go` | 46,401 |

SHA-256 of the original pack:
`730e0e54b338685b6e7e214d6a365060abcfe988d87673fa64ec0b1fadccb4f0`.
SHA-256 of the additional pack:
`48a5ef42c6ebac5e2a886be3cdd0b3464ec71c9299fbc990afa3f635ca4cf6be`.
Both packs are self-contained. The manifest also records object-list digests,
raw blob digests and lengths; `.gitattributes` preserves the pack and raw commit
bytes during transport.

## Public inputs

[manifest.json](manifest.json) lists **all 43 full commit IDs**, their root trees,
snapshot object counts, object-list digests and canonical commit URLs. No local
branch name or mutable `HEAD` is an input.

The first 39 are exactly `required_input_commits` from
`.plans/2026-11/integration-audit-20261125/composition.json` at public PR #664
commit `0c255490461fed0a3ee3d43acdd5216e2e064a53`. The earlier transfer's
36 missing-public-commit list is a subset; the other three are explicitly
included here rather than assumed to exist implicitly. Table 3 lists the four
additional public snapshots.

| Table 3. Additional public snapshot | Exact commit |
|---|---|
| #637 | `123858ef8292aceb86d8ca7ef71d4e4dea1fec91` |
| #659 | `6f59746beae329d9f3f6e4f5af4d503d822d2e4f` |
| #671 | `ff8128a5f717fae75b263de55c382933a0bc9121` |
| #664 | `0c255490461fed0a3ee3d43acdd5216e2e064a53` |

#669 `ec2248643d875390c41b519b85424a5ca0ce90d1` is already among the 39.
The checked #660 `e6f1ce53`, #627 `d9107679`, #631 `62c853e8` and #644
`ba14bd24` snapshots add no missing P02 objects to this union; their full IDs
are retained in the manifest as considered, unnecessary inputs. This is a
fixed object-coverage list, not a declaration of the newest functional Source.

## Reproduce the restoration

Requirements: Python 3, Git supporting `ls-tree --format`, and an existing local
repository containing the 43 pinned public snapshot commits and their tree/blob
objects. The source repository may contain additional objects: the script reads
only objects selected by the declared public snapshots. The capsule's private
objects are read from its packs, not taken from that repository.

```sh
python /path/to/p02-capsule/restore.py \
  --repository /path/to/existing/repository \
  --work-dir /path/to/new-owned-restoration-directory
```

Choose a new directory outside the input repository, its shared Git directory
and this capsule. The script refuses an existing output directory. All inputs
are read-only. It disables Git transport protocols, lazy fetch and replace refs,
and removes inherited `GIT_*` configuration overrides. Missing public inputs
cause a failure; the script does not fetch them.

The output is `snapshot.git`, a new bare object database, plus a compact
`verification.json` and scratch object-list/error files. No source checkout,
new commit, branch or tag is created. The bare database has **4,490 objects**:
the 4,107 P02 objects and 383 additional, explicitly declared public tree nodes.
These tree nodes permit graph enumeration without shipping a redundant list of
every P02 object. No unrelated public blobs or public commit history are copied.

## Proof and limits

The reproducer first verifies all public commit headers, root trees, object sets
and their union. It imports only the 625 declared public tree objects and the two
capsule packs into the empty database. After enumerating the target tree, it
imports exactly the remaining target blobs whose IDs belong to the public
union. Every pack's actual object set is checked. The final database must equal
the declared target plus the public tree nodes; no alternate object database is
allowed. Finally, the script reads and recomputes the Git object hash of every
one of the 4,107 target objects and verifies the exact raw commit bytes.

The original raw commit is also included as `p02.commit` (276 bytes). Its parent
header remains `afdf474930da60080e9aa132a3a373fcf2009bab`; that parent is
deliberately absent from the restored database. **This restores the exact
snapshot, not its complete history.** An ancestry walk or a whole-history
connectivity check is outside this capsule's claim. No synthetic parent,
rewritten commit or shallow-history substitution is used.

The accompanying [verification.json](verification.json) records the actual
fresh-database restoration result and binds the manifest and script digests.
This is a source-object preservation proof. It is not a build, functional test,
native run or product qualification. Subsequent Source commits, the separate
A+B patch, their execution results and publication in the existing PR remain
separate; this capsule neither embeds nor changes that patch.
