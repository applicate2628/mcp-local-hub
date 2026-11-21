# R22 retained evidence, 21 November 2026

## Contents

- [1. Result and scope](#1-result-and-scope)
- [2. Reading the evidence](#2-reading-the-evidence)
- [3. Qualification limits](#3-qualification-limits)

## 1. Result and scope

This package preserves the fixed completed R22 selection: diagnostics, LSP (Language Server Protocol) benchmark fixture, pending RAM (random-access memory), CMake producer and consumer, Relay retry v1 and v2, scheduler rollback, and one native raw-export roundtrip. Linux phases are excluded. Packaging ran no tests, builds, scheduler operations, or network commands.

All retained command/observation stdout and stderr bytes were checked against their original SHA-256 (Secure Hash Algorithm, 256-bit) and byte-count receipts; Go terminal events were reconstructed from those bytes. Source candidates, frozen overlays and source snapshots were checked independently of a wrapper's final exit. There are 36 retained phases: 35 recorded complete and one historical Relay v1 candidate recorded failed. Successful negative controls intentionally retain nonzero command exits. See Table 1 in [RESULTS.md](RESULTS.md).

Eighteen final candidate files match GitHub blob identifiers at the published commits in execution-summary.json. The evidence consists of executions on pinned original sources with explicit Go overlays, which temporarily substitute candidate files without editing the source checkout. It does not claim that the complete published branch heads were rerun. Relay v1 is retained as failed history and is not a second published candidate.

## 2. Reading the evidence

- [RESULTS.md](RESULTS.md): bounded results, negative evidence and Table 1.
- [execution-summary.json](execution-summary.json): exact retained commands, exits, test and package events, source pins, observations and separate original/public stream hashes. Private literal values in commands are replaced by angle-bracket placeholders.
- [MANIFEST.json](MANIFEST.json): original and public SHA-256 plus byte counts for each retained member; hashes of generated files. Original hash fields in retained receipts remain original claims and are not rewritten to public hashes.
- [archive-metadata.json](archive-metadata.json): ZIP archive size/hash and manifest identity. The ZIP (compressed archive) contains the same README, RESULTS, summary and manifest plus selected retained evidence, source candidates, base source files, scripts and CMake artifacts.

Paths, account/host names, native task identity and the private Windows SID (security identifier) are replaced using exact known literals. All public member hashes were checked after redaction. Unchanged public streams reproduce their original hashes; changed public streams reproduce only their separately recorded public hashes. Raw native XML (Extensible Markup Language), private native manifest/final records and the harness executable are verified locally and excluded from the archive. Their original hashes are retained in the summary. Redacted scripts/source files are audit records and are not advertised as runnable fixtures.

## 3. Qualification limits

Native evidence records two acknowledged registrations, two acknowledged deletion calls, two absence checks and equality of all four actual raw exports. It concerns a disabled, hidden fixture with no triggers and no start-on-demand; it does not qualify Run, RunOwned, Stop, cancellation, owner changes or production Delete scenarios. Its source is ba17867125b3728fb734e90b383639a3ff9175ee, preceding the later scheduler rollback commit. Private raw-export bytes cannot be independently reconstructed from this public package.

Only direct-child waits were bounded. Whole-process-tree deadlines and settlement were not established. The original CMake preparation plan requests stronger process-tree handling than the retained execution achieved; both the plan and the execution limitation are preserved. One-iteration benchmark outputs establish that the selected workloads executed; they establish no latency, throughput, memory, percentile or concurrency budget acceptance.

This package establishes the stated R22 evidence, not overall program readiness. Earlier broad failures and NOT_RUN gates remain unless separately resolved. No external APPROVED review is claimed.
