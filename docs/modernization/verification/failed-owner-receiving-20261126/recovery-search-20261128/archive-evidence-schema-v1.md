# B archive recovery evidence

These are data-only search records. They do not qualify Product, TestSource, receiver behavior, or native product tests.

## Files

- archive-selection-v1.json: actual 266 archive paths plus exact counts and hashes binding the broader preliminary D selection.
- transport-inputs-v1.json and transport-inputs-v2.json: container identities, decoded shapes, byte candidates and exact-target outcomes.
- archive-content-v1.json and archive-content-v2.json: complete member/nested-value candidate metadata, archive hashes, bounds, exceptions and queue state.
- archive-recovery-summary-v2.json: bounded result and independently received command closure. The early dependent launcher remains terminal/closure UNKNOWN.
- archive-transformation-manifest-v1.json: original/public byte lengths and SHA256, transformation rules and root-token mapping fingerprints.

## Interpretation

An empty remaining queue means the selected queue was consumed; it does not remove parse errors or oversized-member omissions. Candidate counts include duplicate or nested values and are not unique-file counts. Exact SHA256 matches are distinct from a prefix9315 hint. No matches were found. Recorded wrapper PASS indicates completion of the data-reading operation, not full search coverage or product acceptance.

All candidate byte lengths, SHA256 values, match flags, errors and queue fields are retained. Absolute machine roots are replaced by logical root tokens; original-prefix fingerprints and transformation counts are recorded. The preliminary D inventory is explicitly projected to the actual chosen set and omission counts. No source payload or archive-member contents are published in these JSON files.

## Terms and Abbreviations

- Receiver: input-receiving and validation code being sought.
- SHA256: cryptographic byte digest.
- JSON: structured data encoding.
- XZ: compression format used by saved transfer envelopes.
- UNKNOWN: no qualified terminal/closure outcome was available.
- CLOSED: independently received native owner and reader completion within the stated run.

## Budget checkpoint semantics

The compressed_total value is a stopping threshold checked before the next archive. In v1, the already admitted archive raised compressed_bytes_read to 268496235 B, which is 60779 B above the 268435456 B threshold. The seconds threshold is checked before archives and while advancing ZIP/TAR members; it does not interrupt an ongoing read or decode. Per-archive, per-member and nested-decode limits are separate. Original report values and outcomes are unchanged. This explanatory paragraph was added during root review; the preparation-package schema hash in structural-verification.json identifies the earlier text, while artifact-bindings.json binds this final file.
