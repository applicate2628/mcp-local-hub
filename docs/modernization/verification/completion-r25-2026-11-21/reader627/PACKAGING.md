# PR627 evidence packaging result

The reviewed packager completed on Wave in `D:\dev\ChatGPT\mcp-r25-reader-20261121\evidence-v1`. A subsequent standalone public verification completed with `integrity: PASS`, using the exact external ZIP/index pins below. No Rust or Product tests were rerun for packaging.

| Artifact | Bytes | SHA-256 |
|---|---:|---|
| `public.zip` | 82,293 | `6038e3b56b74209dcf118117f767ceb180e55d398fcad0aeffe6c678edf1aec3` |
| `public-index.json` | 13,165 | `f64d58dd5a94a64d1196989f4e3dcfd671657433627345bee30aed05213fa8f3` |
| `private-originals.zip` — retain privately on D | 82,286 | `f4188e12167a31c2a8fa93f435543d26f09b86f86e284a88e7d5afcb14c3fc99` |
| `reader627_evidence.py` | 16,484 | `9953cbc4058f02186e531355f92b4b0a690ffc0e6ab335aaa97d8c529060f124` |
| `RUNTIME.md` | 7,032 | `c3a0e7af5368561e510ea183ceb4eafabe2c37c801bfdfd126d65e715d335e7b` |

The package has 31 members. Exactly three have profile-prefix redactions: the frozen input manifest and the format/test receipts. All seven source files, the driver/verifier, thirteen dependency checksum manifests, preparation receipt, and stdout/stderr streams are unchanged. The raw/public index identifies the unavailable original byte hashes without claiming that public redacted bytes reproduce them.

The four local corruption probes all failed for the intended reason: NUL-truncated ZIP name; duplicate plus missing member; stream mutation despite updated outer hashes; and fifteen `ok` lines with a duplicate test name despite consistent receipt/stream hashes. See `packaging-selftest-v1/report.json`. The last probe verifies the exact-name multiset requirement rather than accepting a pass count alone.

The public verifier reports `runtime_authenticated: false`, `excluded_vendor_bytes_verified: false`, and `installed_tools_verified: false`. It proves the integrity and internal consistency of the supplied evidence, not the execution event or omitted bytes. Raw remote readbacks and inventories remain private. The parent agent retains responsibility for independent archive readback/verification and any GitHub publication.
