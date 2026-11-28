# Exact Source `3ebe335a` capsule

This capsule reconstructs the exact private Go Source used by the C stdio-consumer experiment: commit **`3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`**, tree **`67c13b8fc1317b4fd60e3135a3eb8a376fe01817`**. Reconstruction was verified from the publicly available tree at `62c853e8a9e9b2817f5fcaf15018450b76798690`, using a fresh index and Git object directory with only the public base object store as an alternate.

The capsule carries a full-index patch and exact commit/inventory metadata. It does not carry private ancestor objects. All 3,508 target files are recoverable from the public base and patch.

## Contents

1. [Exact identities and payload](#1-exact-identities-and-payload)
2. [Reconstruction recipe](#2-reconstruction-recipe)
3. [Recorded verification](#3-recorded-verification)
4. [Disclosure review scope](#4-disclosure-review-scope)
5. [Experiment applicability](#5-experiment-applicability)

## 1. Exact identities and payload

**Table 1. Content identities.** [manifest.json](manifest.json) binds the capsule files; [source-files.json](source-files.json) contains every target path, Git mode, blob ID and byte count.

| Role | Identity or size |
| --- | --- |
| Public base commit | `62c853e8a9e9b2817f5fcaf15018450b76798690` |
| Public base tree | `32b55d2e5708462a2bf4cddb4a5cc7143ddf59e4` |
| Target commit | `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4` |
| Target tree | `67c13b8fc1317b4fd60e3135a3eb8a376fe01817` |
| Target inventory | 3,508 files; 57,144,526 bytes; every entry mode `100644` |
| Full-index patch | 3,471,196 bytes; SHA-256 `10af6f1b1b179c04299c13024ca75b4dc868fb945574a3e941b34ac343d7c03e` |
| Raw target commit | 276 bytes; SHA-256 `a951fc2799e12e93e118ea1251438fb8ca908c86786f0bf65a4e522e04bfe537` |

The delta has 424 paths: 211 modified, 207 added and 6 deleted. Its text statistics are 57,655 added and 4,369 removed lines; the selected delta has no binary paths in Git's `--numstat` result. Its 418 added/modified target files total 8,716,087 bytes. That postimage size is different from the patch size. The other 3,090 target files are inherited unchanged from the public base. [changed-files.json](changed-files.json) records all old/new blob and mode identities.

The raw target commit names parent `c3b8c802be190c335b84ff7323a14d9eb63518b6`. The retained donor history is shallow there, so an available common ancestor with the public base was not established. A tree patch does not require common ancestry. Among the four examined public anchors (`62c853…`, `25508f…`, `0e8bec…`, `75dbc6…`), the selected base produced the smallest measured delta.

The patch contains 1,043 carriage-return bytes that must be preserved. The scoped `.gitattributes` disables text normalization for this capsule. Do not copy the patch through a text viewer, reformat it, or convert its line endings. Its six deletions affect the reconstructed target tree; the patch itself remains a data file in the publication repository.

## 2. Reconstruction recipe

First run the included metadata verifier from the capsule directory:

```text
python -I -S -B verify_capsule.py
```

It hashes the capsule members, verifies the raw commit identity, and rebuilds the target tree identity from the complete inventory. It does not execute Source or perform Git reconstruction. The next commands reconstruct the actual file contents.

The following PowerShell example uses a fresh directory under the existing D work area. Set `$capsuleDir` to this capsule's actual directory. Each nonzero Git exit stops the recipe. Only the public base commit is fetched; no private ancestor is needed.

```powershell
$capsuleDir = (Resolve-Path '.').Path
$rebuildDir = 'D:\dev\ChatGPT\source-3ebe-reconstruction'
if (Test-Path -LiteralPath $rebuildDir) { throw 'Choose a fresh reconstruction directory' }

git -c core.autocrlf=false init $rebuildDir
if ($LASTEXITCODE -ne 0) { throw 'git init failed' }

git -C $rebuildDir -c core.autocrlf=false fetch --depth=1 https://github.com/applicate2628/mcp-local-hub.git 62c853e8a9e9b2817f5fcaf15018450b76798690
if ($LASTEXITCODE -ne 0) { throw 'Public base fetch failed' }

git -C $rebuildDir -c core.autocrlf=false checkout --detach FETCH_HEAD
if ($LASTEXITCODE -ne 0) { throw 'Public base checkout failed' }

git -C $rebuildDir -c core.autocrlf=false apply --index --binary --whitespace=nowarn (Join-Path $capsuleDir 'source.patch')
if ($LASTEXITCODE -ne 0) { throw 'Exact patch application failed' }

$reconstructedTree = git -C $rebuildDir write-tree
if ($LASTEXITCODE -ne 0 -or $reconstructedTree.Trim() -ne '67c13b8fc1317b4fd60e3135a3eb8a376fe01817') {
    throw 'Reconstructed tree differs'
}
```

The resulting index represents the exact target Source, while Git HEAD still identifies the public base. The directory also contains the materialized files. Git blob identities in the inventory are the authority for exact file content; local checkout filters and line-ending settings must preserve those bytes. The exact raw target commit is supplied as data; there is no need to create a private commit ref or import its parents. Do not use `--3way`, filter-based rewriting or line-ending fixes to make an unexpected failure pass.

The recorded native verification used a bare repository and `git apply --cached --binary`, so it verified the same target contents without creating a second full Source worktree. The materializing PowerShell recipe above is an instruction for the reader; it was not separately executed as another native test during this work.

## 3. Recorded verification

[export-provenance.json](export-provenance.json) records the exact Git `2.56.1.windows.1` export. All three original commands and six original readers closed; the parent was reaped with its captures closed. The receipt SHA-256 is `dc8be0f3b8c56918eb5333f6c3844f0be58f0cbe8470c961812557460b877c0b`.

[reconstruction.json](reconstruction.json) records the successful independent reconstruction. All eight original commands exited 0; all eight native owners and sixteen original readers closed; the parent was reaped with closed captures. The receipt SHA-256 is `23d34a1b2b0d5ee1626674ad86ef40d44bcb10bb36e42efa9d1aa4ac67603ce8`.

The resulting tree matched Table 1. The complete Git file/mode/blob/size census matched the original 374,784-byte inventory, SHA-256 `5ce9324e7beb695da0bcffceb83748bdeaf457ba933442f8af2d0746307f9e27`. The raw 276-byte commit also hashed to the target commit and named the reconstructed tree. The verifier used only the public object alternate, created no commit refs, and imported no private ancestry. Its own and the outer parent's closure were received after the verifier returned.

A normalized text transfer was rejected because it changed the patch bytes. The subsequent ZIP/base64 transfer was checked against the native ZIP hash and every capsule member. The accepted local patch retained all 1,043 carriage returns and the exact hash in Table 1. The rejected transfer is preserved separately as operational evidence; it is not an alternative Source.

## 4. Disclosure review scope

[disclosure-review.json](disclosure-review.json) distinguishes the checks performed. A heuristic byte-pattern scan covered all seven files of the exact staged Source capsule, totaling 4,241,675 bytes, including every added, removed and context line in the patch. Ten candidates were manually inspected with their context: two synthetic author/committer addresses (`chatgpt@local.invalid`) and eight Go module/version or documentation URL strings containing `@`. They were not credential findings. No pattern for a credential, private key, user-profile path or private IPv4 address matched that payload.

This is a complete byte-pattern scan of that payload plus manual review of the ten candidates. It is not a full manual review of all 418 changed postimages, a proof that no possible secret encoding exists, or a new code audit. Newly generated capsule metadata is separately checked at final package sealing. The repository's native publication gate is separate from this Source-content and disclosure review.

## 5. Experiment applicability

This capsule identifies **private Go Source `3ebe`**. The consumer overlay remains the separately published [test input](../continuation-363b/testdata/zz_rust_adoption_stdio_consumer_windows_test.go), SHA-256 `c7218f274e9ef00906eff79975ca439f48974ef0fd9593f7198fc27a65a7b6c8`; it is not one of the target tree's 3,508 files.

Rust native build `efa73c0e23f7af16b8aa2ccb047b745a58ba4f71`, its DLL, and reviewed Source `a217c7799a0214967bf86211542e508162c680b5` plus its recorded native delta remain separate evidence in the [full a217 review](../continuation-363b/full-a217-review/README.md). The 64-file a217 static review is distinct from this capsule's disclosure scan and from the full Go Source tree.

Exact Source reconstruction does not expand runtime qualification, establish new performance claims, or justify adoption from two timing observations per implementation. The earlier C failures, UNKNOWN/V1 history and the frozen 29-file continuation packet remain unchanged.
