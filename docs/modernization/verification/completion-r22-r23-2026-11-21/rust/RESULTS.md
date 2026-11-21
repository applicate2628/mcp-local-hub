# R23: independent offline Rust and Go consumer validation

## Contents

1. [Completed checks](#completed-checks)
2. [Preserved failures](#preserved-failures)
3. [Exact sources and dependencies](#exact-sources-and-dependencies)
4. [Public evidence boundary](#public-evidence-boundary)
5. [Remaining limits](#remaining-limits)

## Completed checks

These are independent branch snapshots, with no branch integration. Debug
checks used the existing Windows toolchain and qualified offline dependencies.
Table 1 does not combine parent test events with their leaf cases.

| Table 1. Phase | Actual result | Evidence |
| --- | --- | --- |
| native625-debug | 51 native/unit/integration tests and 2 documentation tests PASS; 53 terminal Rust PASS lines | `runs/native625-debug/receipt.json` |
| native644-debug | 73 native/unit/integration tests and 3 documentation tests PASS; 76 terminal Rust PASS lines | `runs/native644-debug/receipt.json` |
| ffi631-debug, Rust stages | Owned child build PASS; 20 FFI tests PASS; DLL build PASS | `runs/ffi631-debug/receipt.json` |
| ffi631-go-fixtures | 57 root Go tests, 129 terminal test events, and 118 leaf cases PASS; one package PASS; zero FAIL/SKIP | `runs/ffi631-go-fixtures/receipt.json` |

The final Go command was `go test -json -race -count=1 -timeout=120s .`.
It ran from 09:16:37.176991 to 09:17:04.706918 UTC on 21 November 2026;
the child command took 26.9396428 seconds. Rust tests and builds were not
repeated for that retry. Child and dynamic-link library (DLL) hashes were
matched against the original successful build before and after Go execution.
All 48 source files, qualified vendor contents, and tool hashes passed the
final verification. The saved standard output and error streams were also
independently rehashed after completion.

## Preserved failures

`runs/readiness-debug` retains the initial tool readiness failure: the existing
MSYS Go binary required an explicit Go installation root, GOROOT. The successful
`readiness-goroot` phase used the existing `lib/go` tree without installation.

`runs/ffi631-debug` retains the original default Go failure. Its 118 leaf cases
were 114 PASS and 4 FAIL because the runner omitted
`MCPHUB_RX02_IDENTITY_FIXTURES`. The two affected roots had `identity` and
`token-user` subcases; parent group failures account for six FAIL test events.
The failure occurred at the explicit fixture-directory requirement. Product
source was unchanged. The final Go-only phase provided a fresh owned directory.

The initial readiness receipt records runner SHA-256
`17360edf8e18ed3942e72529841b6043abf499c321d97050b1a518bddc332dc4`.
That early source revision was not preserved. Reversing only the known GOROOT
insertion in the preserved `238e0f3f...` runner produced SHA-256
`f55c03cc316c8e35f61d365130aac6b211a1f410c824d803098fa60705c7e638`,
which does not match; it is not presented as a recovered original. Its receipt
and streams remain available. All native and FFI test phases have their actual
runner revisions preserved.

## Exact sources and dependencies

| Table 2. Source snapshot | Exact commit |
| --- | --- |
| #625 native process owner | `287a7772a4be75ed9fb2f9ba1feb68cf035ed907` |
| #644 native members | `1cf5754b81c850a3569be269642b49605895eeb2` |
| #631 Rust foreign function interface (FFI) and Go consumer | `ebfa4ef32b57cfa022acdcfcb4a2745f32b815c6` |
| #627 reader source prepared, not executed | `b827398ef127a1556b7b2bdcdcec7fe03fa6651d` |

The later README-only #631 head
`2eb7c114103ef5c60939cd114110ebe667901166` is distinct from the tested snapshot.
Preparation downloaded and verified 84 public text blobs against immutable Git
blob identifiers and sizes. Source text is recoverable from those commits;
the compact evidence does not duplicate the source trees.

Existing `windows-sys` 0.61.2 and `windows-link` 0.2.1 archives matched the
exact lockfile package hashes. Their archive entries, checksum manifests, and
all 265 vendor files matched. Each test phase used a separate owned Cargo home
with offline source replacement and verified the same read-only vendor.
Go module/proxy/sum-database network use and automatic toolchain downloads
were disabled. Source preparation performed no dependency download or install.

## Public evidence boundary

The selected public text comprises the preparation and validation scripts,
preserved runner revision, source manifest and preparation receipt, the
fixture correction diff and review, this result summary, and receipts plus
standard output/error streams for the six named phases. The explicit selection
manifest records every original selected file's byte count and SHA-256.

The exporter reads only that selection. It validates every selected command
stream against its original receipt, copies into a fresh directory, and records
original and public byte hashes separately. Its sole text projection replaces
the local account directory component in tool paths with `<local-user>`.
It retains the original hashes in metadata; redacted files are not represented
as original bytes. All copied public hashes are independently recalculable.

The exporter never reads runtime directories, target binaries, source copies,
vendor contents, or private identity marker contents. The four private binary
markers (80 bytes total, counted without reading contents) remain in the owned
Windows run directory outside the public evidence. No security identifier
(SID) value is published. Original files and all failures remain in place.

## Remaining limits

The Go race detector checks Go execution in this selected default build. It
does not prove freedom from every race in Rust or Windows. Eight additional
Go test files require explicit fault, legacy, or loader build tags and were
not selected. No release profile, reader627 test, extra workload, installation,
provider call, or system policy change was performed by these phases.

Each direct command process settled through its continuously owned handle.
The wrapper explicitly does not claim a deadline or settlement proof for the
whole descendant process tree. These experiment checks do not establish
installed-product, Windows identity-policy, Unix ARM64, production workload,
or whole-program readiness. Parallel wait/close work was not imported.
