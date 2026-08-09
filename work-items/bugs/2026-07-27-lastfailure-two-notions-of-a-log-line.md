# `lastfailure`'s phase-log scanners disagree about where a LINE ends (CR is a separator for one, not the other)

- **Status:** fixed
- **Severity:** P3 (an interrupt marker is found on a carriage-return-overwritten
  capture but a diagnostic on the same capture is not; no wrong verdict is
  manufactured, but evidence is silently lost)
- **Context:** adjacent-finding
- **Found:** 2026-07-27, while sweeping every line scanner in `internal/vcpkgmcp/lastfailure`
  to decide the scope of the log-normalization fix (adversarial-gate finding F4)
- **Owner package:** `internal/vcpkgmcp/lastfailure` (`diagnostics.go`)

## What

Three scanners read the SAME input class — a captured build-tool stream from a
buildtrees phase log. After the F4 fix they share one line NORMALIZER
(`normalizeLogLine`), but they still disagree about what a LINE IS:

| Scanner | Splits on | Source |
|---|---|---|
| `DetectInterrupted` | `\n` **and** `\r` | `diagnostics.go` (`bytes.IndexAny(content, "\r\n")`) |
| `scanDiagnostics` | `\n` only, then `TrimRight(line, "\r")` | `diagnostics.go` (`bufio.NewScanner`) |
| `findRunBuildCommandLine` | `\n` only, then `TrimRight(line, "\r")` | `lastfailure.go` |

`DetectInterrupted`'s doc states the reason for its choice explicitly:

> Lines are split on '\n' AND '\r' so a CRLF log, and a capture that retained a
> terminal's carriage-return overwrites, both decompose correctly.

That reason applies verbatim to the other two, and they do not follow it. On a
capture that retained carriage-return overwrites — a progress meter rewriting one
display line, which is exactly what `ninja`'s default non-verbose output does —
a phase log can carry

    [3/9] Building a.cpp\r[4/9] Building b.cpp\ra.cpp:3:5: error: undefined\n

`DetectInterrupted` sees three lines here. `scanDiagnostics` sees ONE, whose text
begins `[3/9] Building a.cpp` — so `gccClangDiagRE`'s `^` anchor does not reach
the diagnostic and the error is invisible. The tool then answers
`unknown(no_diagnostic_found)` (or reports a later phase's error instead) for a
build that plainly failed.

## Why it was not fixed in the F4 pass

Scope, not difficulty. F4 was a decision about ESCAPE BYTES within a line; this
is a decision about line BOUNDARIES, and changing `scanDiagnostics`' splitting
has its own blast radius: every existing fixture and every real log whose lines
contain a bare `\r` would decompose differently, which changes which diagnostics
are extracted and therefore which verdict is issued. That needs its own measured
pass over real logs (how often does a bare `\r` actually appear mid-line in the
618-file scout corpus?), not an opportunistic edit inside an unrelated fix.

It is also not certain the right answer is "make the other two match": splitting
diagnostics on `\r` could split a diagnostic MESSAGE that legitimately contains
one. The measurement decides it.

## Suggested resolution

Give the package ONE owner for "split this log content into lines", used by all
three scanners, in the same way `normalizeLogLine` is now the one owner for
"clean this line". Decide its `\r` rule ONCE, from a measurement over the scout
corpus, and state the decision in that owner's doc — rather than leaving two
different answers in one file with only one of them explained.

## Terminal evidence

Closed: 2026-08-01T17:39:21Z

- **Disposition:** fixed; pending lifecycle-owner archive.
- **Owner result:** the shared streaming log parser now preserves the three
  consumers' line semantics at one boundary.
- **Verification:** `TestScanPhaseLogStream_PreservesThreeConsumerLineSemantics`,
  `TestDetectInterrupted_RealProducerLinesAreStillDetected`,
  `TestScanDiagnostics_MatchesColourizedOutput`, and
  `TestFindRunBuildCommandLine_SurvivesTerminalDisplayBytes` passed in the
  bounded `internal/vcpkgmcp/lastfailure` test run.
