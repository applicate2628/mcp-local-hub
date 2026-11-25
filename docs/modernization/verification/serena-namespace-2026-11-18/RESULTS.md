# Clean-source Serena namespace lifetime verification — 2026-11-18

## Contents

- [1. Result and exact source](#result)
- [2. Publisher build](#publisher)
- [3. Native test](#native)
- [4. Review and limits](#limits)
- [5. Evidence](#evidence)

<a id="result"></a>
## 1. Result and exact source

A source-issued Windows qualification image was built from the clean, unchanged
#637 source `84615d465c54d52588ac17c37174d68096d0da90` and executed directly.
`TestSerenaQualificationNamespaceLifetime` and all four subtests passed,
with exit 0 and no skipped tests.

This is a clean-source result for that exact commit, distinct from earlier
combined-source receipts. The enclosing evidence commit changes documentation
only; it is not substituted for the measured source commit.

**Table 1 — source and image binding.**

| Item | Observed value |
|---|---|
| Platform | Comet; Windows build 22631; Go 1.27.4; Windows/AMD64 |
| Selected source files | 3227 |
| Source snapshot SHA-256 | `6d88b415276c271ffb85ca415669e5eccdd37fb76cf9dfd2d7a04db029cf2f17` |
| Source epoch | `c882ff60ee204e41f79764cf53312ef9e0e4b7483a3df7aca0513017a3d31cd9` |
| Qualification image SHA-256 | `c67713e56639a5b0ff611ee43d02901d89914c3f135d20956913733bf919643e` |
| Portable Executable role | `serena-qualification` |
| Ordinary Product admission | Refused |
| Source checkout before / after | Exact commit, clean |

<a id="publisher"></a>
## 2. Publisher build

The builder used only the installed compiler, Git and existing module cache.
Its version-resource generator was served by the existing local-file proxy;
there was no network/direct fallback and no package installation.

All four owned build stages passed: private telemetry-off, Go version,
version-resource generation and compile. Each stage recorded exit 0, known root
wait, complete native process settlement, complete input/output settlement and
no failure. The final publisher receipt confirms that retained input handles
were closed. The generated resource was moved into the builder's private trash
before the source closure was checked again.

The receipt intentionally says `tests_executed=false`: the builder does not
run tests. Test execution is recorded separately in the outer runner journal.
The image digest was checked before and after that execution.

The build took 257.795 seconds at the outer runner; that duration includes
source preparation and owned stages. It is not a performance benchmark.

<a id="native"></a>
## 3. Native test

The runner atomically created a private directory with a protected Windows
discretionary access control list (DACL), allowing the current user, SYSTEM and
Administrators, with child inheritance. It checked ancestors for reparse points.
All temporary and profile locations were descendants of the owned D-only task
tree. No installed Product, `uv`, Python package or Serena package was
required or launched by this test.

The exact executable arguments were:

```text
-test.v -test.count=1 -test.parallel=1 -test.timeout=3m -test.run=^TestSerenaQualificationNamespaceLifetime$
```

**Table 2 — observed test results.**

| Test | Result |
|---|---|
| `TestSerenaQualificationNamespaceLifetime` | PASS |
| `MCPHUB_STATE_DIR_OVERRIDE` | PASS |
| `MCPHUB_SERENA_QUALIFICATION_RUN_ROOT` | PASS |
| `in-memory_state_root` | PASS |
| `canceled_request` | PASS |

The test exercises real native directory acquisition and identity, private
directory policy, truthful observation fields, refusal of overlapping scopes,
environment/in-memory binding drift, nonrenewal of an existing observation's
deadline, request cancellation, repeated close, reopening the same native
directory and rejection of the old observation.

The native test process exited 0; stdout contains the exact parent and four
subtests plus the final PASS marker. Stderr is empty. The outer wait completed in
3.793 seconds. Its finite process wait is not represented as a fabricated
native Job receipt; the four publisher stages have their own actual Job/process
settlement evidence.

<a id="limits"></a>
## 4. Review and limits

Own source review and independent focused reviews found no new demonstrated
defect in the #636 namespace or #637 fixture changes. The fixture's UTF-16
working-directory budget uses the actual longest Hub-selected qualification
path. Manifest seeding changes only command/base arguments and preserves the
other declarations. Scope cleanup precedes restoration of its bindings.

The migration file preserves 386 `testing.T` failure/assertion calls:
49 Fatal, 200 Fatalf, 21 Error and 116 Errorf in both the base and head versions.
The previous “392 assertion calls” wording included six `fmt.Errorf`
error-construction calls; this reporting metric is corrected without changing
test bodies.

This lifetime test does not execute the fifteen positive prepared migrations,
the short-CWD package child path, or native package cleanup. It cancels a request
context, not the owning scope context, and compares live deadlines without
waiting for expiry. Actual native close failures are not experimentally covered.

The exact measured source still lacks #632's later empty-PATH-entry change,
the new #635 telemetry correction and independent #643 cleanup work. The runner
cleared telemetry controls before invoking this older builder; this prevents
the reproduced environment issue for this run without integrating any branches.

Full mandatory Go gates remain FAIL / unqualified. This report is not whole-stack
acceptance, an installed Product update, a signature or memory attestation.
No PR was merged and no branch was combined with another.

<a id="evidence"></a>
## 5. Evidence

- [Raw logs, source snapshot, owner receipts and runner](logs.zip), SHA-256
  `89951300f1df94eb73f8a04c8f834af327106120f33b478bafb52f65f3824f3f`.
- [Per-member byte counts and SHA-256](manifest.json).
- [Checksums of the four adjacent deliverable files](SHA256SUMS).

The full 3227-file source snapshot is included. Compiler caches and the generated
test executable are not committed; the exact source, compiler version,
commands and image digest are retained. The image is reproducible as a new
source-issued build, with a new build date and consequently its own digest.

Only the current-user security identifier in the outer runner's directory-policy
receipt is replaced by `<CURRENT_USER_SID>` in the public copy. Its original
byte count and SHA-256 are recorded in the manifest. Native publisher receipts,
source snapshot and stdout/stderr are unmodified.
