---
status: open
context: adjacent-finding
---

# `parseJSONCBytes` should defensively copy its input (hujson.Standardize mutates in place)

Filed during PR #433 (per-project-GUI P3a, write phase, round 2) as bot arch
note N1 (deferred — touches a hot shared helper on the protected golden scan
path). Documented as a CAUTION on the helper's doc comment in this PR; the proper
single-owner fix is deferred here.

## Summary

`internal/clients/jsonc.go` `parseJSONCBytes(data []byte)` calls
`hujson.Standardize(data)`, which MUTATES its input slice IN PLACE — it overwrites
comment bytes with spaces while standardizing JSONC into strict JSON. So the
`data` slice the caller passed is clobbered after the call returns.

## Why it is not a live bug today

No current caller depends on `data` staying intact after `parseJSONCBytes`. The
read-back path (`ProjectObjectMemberPresent`, the adapters' scan/read) parses to
INSPECT and then discards the slice; the comment-preserving WRITE path
(`applyJSONCObjectMemberPath`) does a FRESH `os.ReadFile` + `hujson.Parse` on the
original bytes, never reusing a slice that was already handed to
`parseJSONCBytes`. So the in-place mutation is invisible in every present call
site.

## The risk to flag

A future caller that (a) reads on-disk bytes once, (b) parses them with
`parseJSONCBytes` to inspect, then (c) later Pack()s / comment-preserve-writes
the SAME slice would silently lose the operator's comments — the bytes were
already overwritten by the earlier Standardize. The hazard is non-obvious
(`parseJSONCBytes` reads like a pure read), so the trap is easy to fall into.

## What a proper fix would do (deferred)

- Make `parseJSONCBytes` defensively copy its input before
  `hujson.Standardize` (e.g. `std, err := hujson.Standardize(append([]byte(nil),
  data...))`), so the helper is genuinely read-only on its argument and no caller
  can be surprised.
- Why deferred from P3a: `parseJSONCBytes` is a hot shared helper on the scan
  path, which P3a holds at 0-diff (golden-pinned). A per-call extra allocation on
  every JSONC parse is a (small) perf change on a protected surface that warrants
  its own change + a perf sanity check, separate from the P3a write phase.

## Pointers

- `internal/clients/jsonc.go` — `parseJSONCBytes` (the in-place-mutating helper;
  the CAUTION doc comment added in PR #433 names this same hazard).
- `github.com/tailscale/hujson` `Standardize` — the in-place mutation source.
