# Per-run artifact identities after a normal rebuild

A final publication readback initially compared the currently existing ordinary
DLLs with `native-cleanup-final/normal-artifact-hashes.json` and stopped with
`ordinary artifact changed`. That comparison incorrectly crossed build epochs.
It did not reveal a source-code delta: the ordinary Rust source, manifests and
locks still match the recorded source identities.

The protected-close qualification recorded its ordinary-artifact hashes before
and after that isolated run. The later complete ordinary regression runner then
explicitly rebuilt both DLLs; its build logs record actual compilation at
2026-11-14T15:25:14.9631942Z (debug command) and
2026-11-14T15:25:15.3671070Z (release command). The old per-run hash cannot be used
as a byte-identity guarantee for subsequent builds.

| Profile | Protected-close-run ordinary SHA256 | Post-rebuild readback SHA256 | Current artifact last-write UTC |
|---|---|---|---|
| debug | `027c8879508554f5c094e980683c24214635c39b0ac39d38d8eabf825ec70662` | `e8908fe2b3c0db3a08c01bd63a3eaa505ba02fe76c34816ec3b4e849402280e6` | 2026-11-14T15:25:15.2950633Z |
| release | `93aed1b8d782dad75f5ad5605695401f49920dfc28566488c488957e8ee0ca71` | `8185d3d7e9109e47d947e0ff1d582b3c744c335768dcf4452659a637f96e94e1` | 2026-11-14T15:25:15.7082377Z |

The precise internal cause of differing binary bytes was not investigated;
reproducible/binary-identical builds are NOT claimed. Both observations are
retained, without editing the historical hashes to match the later files.

A new, committed [artifact verification script](verify-artifacts.ps1) tests the
already-built current ordinary DLLs without rebuilding them. Each current hash
was stable before/after its absence-of-qualification-export check and complete
normal Go suite with the race detector. All four commands passed. The normal
runtime stayed unchanged; no mutation/recovery hook was found in the artifacts.

[Post-rebuild steps](../runs/native-cleanup-artifact-readback/steps.json),
[current artifact hashes](../runs/native-cleanup-artifact-readback/artifact-hashes.json),
[current source hashes](../runs/native-cleanup-artifact-readback/source-hashes.json),
and [completion marker](../runs/native-cleanup-artifact-readback/COMPLETED.txt)
are durable evidence of these exact latest checks. Earlier ordinary full runs
retain their own recorded configurations; this is an additional race x1 check,
not a replacement for the earlier debug/release race x3 evidence.
