# Serena publisher telemetry environment verification — 2026-11-18

## Contents

- [1. Result and source binding](#result)
- [2. Reproduced defect and correction](#defect)
- [3. Checks](#checks)
- [4. Evidence and limits](#evidence)

<a id="result"></a>
## 1. Result and source binding

The publisher's child environment now clears `TEST_TELEMETRY_DIR`,
`GO_TELEMETRY_CHILD` and `GO_TELEMETRY_CHILD_UPLOAD`. This is one
telemetry boundary correction, with no new wrapper, dependency or change to the
installed Product.

The parent is `6ca53c83aa4f8ccca9bc0eae3d91f3fff9ede109`. Checks apply to the
following exact source blobs, committed alongside this report; the two files
were not changed after verification.

**Table 1 — verified source bytes.**

| File | Git blob | SHA-256 |
|---|---|---|
| `tools/test-serena-publisher/main_windows.go` | `f31d9f1f0d0e98393fa378d9656efd9226e65f84` | `0c788a30199fc17235860f32a40791c47a748a21c0ef550edd10b3ca2b760646` |
| `tools/test-serena-publisher/main_windows_test.go` | `4e337b0293c2e51f54b085a064bd4a0588ab7e97` | `e6a8ec8823fb2e6aafe0db52d45c061a5030a8ad453948ab1d6cca21e9adc7df` |

<a id="defect"></a>
## 2. Reproduced defect and correction

On installed Windows/AMD64 Go 1.27.4, the original inherited environment allowed
`go telemetry off` to set the mode below private `APPDATA`, while the
following `go version` selected a different directory through
`TEST_TELEMETRY_DIR`. That directory received one `.count` file and
`local/weekends`. This violates the builder's work-root output contract.

Both probe directories were new owned directories under `D:\dev\ChatGPT`.
The outside directory had mode `local`, and
`GO_TELEMETRY_CHILD=2` suppressed the telemetry sidecar in the reproducing
case. No upload, package download, installation or global telemetry change was
performed. After clearing the controls, the sibling retained only its original
`mode` file. Private mode remained `off` in both cases.

The empty `TEST_TELEMETRY_DIR` value is deliberate: the first command and
subsequent Go commands use the same private `APPDATA` mode. A different
nonempty private test directory would preserve the mismatch.

The two `GO_TELEMETRY_CHILD*` keys are related service controls. Go's
`GO_TELEMETRY_CHILD=1` branch runs before ordinary argument handling, and
the upload flag is read inside that branch. Clearing them closes the same
environment boundary. No separate upload or child-mode exploit was executed.

Primary Go 1.27.4 sources, also checked against the installed source tree:

1. [Go main and telemetry-off exception](https://github.com/golang/go/blob/go1.27.4/src/cmd/go/main.go#L98-L109).
2. [TEST_TELEMETRY_DIR input](https://github.com/golang/go/blob/go1.27.4/src/cmd/internal/telemetry/counter/counter.go#L22-L24).
3. [Counter directory selection](https://github.com/golang/go/blob/go1.27.4/src/cmd/vendor/golang.org/x/telemetry/counter/counter.go#L109-L114).
4. [Telemetry child control flow](https://github.com/golang/go/blob/go1.27.4/src/cmd/vendor/golang.org/x/telemetry/start.go).

<a id="checks"></a>
## 3. Checks

- RED: the expanded existing environment test failed on all three inherited
  values against the original implementation. The Go test command exited 1.
- GREEN: all three package root tests and ten subtests ran three times with
  `-race -count=3`: 9 root and 30 nested PASS events, zero FAIL/SKIP.
- `go vet -mod=readonly -p=1 ./tools/test-serena-publisher` exited 0.
- `gofmt` completed, and `git diff --check` reported no whitespace
  errors. Own review and an additional focused source review found no
  remaining blocker in this six-line correction.

Exact commands, output bytes, source hashes and the patch are in the archive.
The tests used the existing offline module cache, `GOPROXY=off`,
`GOSUMDB=off`, `GOVCS=*:off` and `GOTOOLCHAIN=local`.

<a id="evidence"></a>
## 4. Evidence and limits

- [Raw journals and reproducibility scripts](logs.zip), SHA-256
  `86fd4be6bf6fef814dc703f1ae02ccf3a558b528d61e5f825948cf501a35897a`.
- [Per-member byte counts and SHA-256](manifest.json).
- [Checksums of the four adjacent deliverable files](SHA256SUMS).

The archive includes both the direct Go telemetry probe and the actual
`buildEnvironment` regression. The former is a focused native reproduction,
not a complete publisher invocation with a hostile environment.

A separate clean source-issued build and namespace lifetime run at #637 source
`84615d465c54d52588ac17c37174d68096d0da90` passed with these inherited controls
already removed by its runner. That historical source does not contain this
correction. It is not a native build of the updated #635 head.

#632 has advanced from the shared ancestor
`8cead428a6cee57c597e2bd3ed7b3877a6445d30` to
`ff4ba7f85fef35e19b39dced17d9df47bb9237a0`. The current #635 line does not
include that separate empty-PATH-entry fix. No branch integration was performed.

Full mandatory Go gates remain FAIL / unqualified. Positive Serena package
preparation, native cleanup-failure paths and formal exact-head external reviews
are not established by this report. No PR was merged.
