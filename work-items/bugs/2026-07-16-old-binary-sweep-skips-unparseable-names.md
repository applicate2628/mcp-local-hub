# BUG: the `.old-*` binary sweep silently skips every name it cannot parse — accumulation is NOT bounded

Status: open
Filed: 2026-07-16
Severity: P3 (disk-only, no correctness impact — but it defeats a documented guarantee and grew to 182 MB)

## Symptom (measured on the live host, 2026-07-16)

```
total mcphub.exe.old-* : 308 MB across 12 files
  parseable (canonical 20060102T150405Z) :  5 files, 126 MB  -> swept correctly
  UNPARSEABLE                            :  7 files, 182 MB  -> never swept, oldest from 2026-07-09
```

The unparseable set: `old-1131846675`, `old-2026-07-12T17-10-50Z`, `old-20260713-100911`,
`old-20260715T214628`, `old-20260716T010728` (canonical shape but missing the trailing `Z`), and two
created the same day by a manual deploy using a PowerShell `yyyyMMddHHmmss` format.

## Root cause

`internal/api/binary_rename_aside.go:SweepOldBinaries` globs `mcphub.exe.old-*` / `mcphub.old-*`, then:

```go
createdAt, ok := generatedRenameAsideTime(m)
if !ok {
    continue          // <-- skipped FOREVER
}
```

`parseRenameAsideTimestamp` accepts only `renameAsideTimestampLayout` (`20060102T150405Z`),
`time.RFC3339Nano`, or `time.RFC3339`. Anything else is skipped on EVERY pass, so a single
foreign-format aside is immortal. Both the 7-day retention AND the `renameAsideMaxKeep = 5` cap operate
only on the parseable subset, so the count cap cannot bound the directory either.

CLAUDE.md documents the opposite guarantee under "Cold-restart upgrade flow":

> "`os.Remove` failures (file still mapped, AV scan, ACL flip) logged warn + retried on next pass.
> **Bounded accumulation**; no admin rights required…"

That holds only for names the parser recognizes.

## Who writes the foreign names

The production Go path (`binary_rename_aside_{windows,posix}.go`) always writes the canonical layout, so
this is not a self-inflicted format drift in the shipped code. The offenders are MANUAL deploys: the
documented deploy protocol ("Redeploy always after merge: build.sh + rename-aside + FULL supervisor
restart") is performed by hand, and each session invents its own timestamp format. Today's two came from
`Get-Date -Format 'yyyyMMddHHmmss'`.

## Proposed fix (two parts)

1. **Make the sweep robust** — an aside that matches the glob but whose name does not parse should fall
   back to the file's **mtime** for the age/rank decision instead of being skipped. Anything matching
   `mcphub*.old-*` in the install dir IS an aside by construction; refusing to reap it because a human
   typed a different timestamp is the wrong failure mode. (Keep skipping directories, as today.)
   Add a test with a foreign-format name asserting it is still reaped by age and still counted against
   `renameAsideMaxKeep`.
2. **Stop hand-rolling the timestamp** — the manual deploy step should either call the shipped
   rename-aside code path or use the canonical `20060102T150405Z` (UTC). Worth pinning the exact command
   in CLAUDE.md's redeploy section so future sessions do not invent a third format.

## Immediate remediation applied

The 7 unparseable asides were removed by hand (182 MB reclaimed). The 5 canonical ones were left to the
sweeper (they are within its retention/cap rules).
