---
title: extractPathParam off-by-one for language_id, instruction_set, opcode in internal/godbolt/handlers.go
severity: medium
found-by: backend-engineer
found-in-phase: godbolt perf-expansion Task 4 (resource://popularArguments)
affected-surface: internal/godbolt/handlers.go:extractPathParam (~line 388 after Task 4 changes)
context: adjacent-finding
status: closed
---

## Summary

`extractPathParam(uri, paramName)` decodes a resource URI such as
`resource://compilers/cpp` by `strings.Split(uri, "/")` and indexing into
the resulting slice. The function's `switch` claims:

- `language_id` lives at `parts[2]` for `resource://compilers/cpp`
- `instruction_set` lives at `parts[2]` for `resource://asm/x86/mov`
- `opcode` lives at `parts[3]` for `resource://asm/x86/mov`

Those positions are wrong. `strings.Split("resource://compilers/cpp", "/")`
yields `["resource:", "", "compilers", "cpp"]` (the `//` after the scheme
becomes an empty element between two slashes), so the actual positions
are:

- `language_id` should be `parts[3]`, not `parts[2]` (currently returns
  `"compilers"` for `resource://compilers/cpp`)
- `instruction_set` should be `parts[3]`, not `parts[2]` (currently
  returns `"asm"` for `resource://asm/x86/mov`)
- `opcode` should be `parts[4]`, not `parts[3]` (currently returns
  `"x86"` for `resource://asm/x86/mov`)

Discovered while implementing the Task 4 `compiler_id` case for
`resource://popularArguments/{compiler_id}`: copying the brief's
`paramPosition = 2` produced `gotURL = "/api/popularArguments/popularArguments"`
instead of `"/api/popularArguments/gcc-13.2"`. The brief inherited the
same off-by-one comment style from the existing handlers, so the test
caught it as a real implementation bug. Task 4 was patched in-scope to
position 3 (verified by `TestGetPopularArguments`) and an explanatory
comment now flags the latent issue, but the three pre-existing cases
were intentionally left untouched per scope discipline.

## Why this matters

`getCompilers`, `getLibraries`, and `getInstructionInfo` will all hit
godbolt with the wrong path:

- `resource://compilers/cpp` -> GET `/api/compilers/compilers` (404)
- `resource://libraries/cpp` -> GET `/api/libraries/libraries` (404)
- `resource://asm/x86/mov`   -> GET `/api/asm/asm/x86`         (404)

These resources have no unit tests exercising real path values (only the
new Task 4 test calls a real URL through `httptest`), so the bug stayed
latent. Anyone who actually reads one of these resources today gets an
upstream error or empty content; this is silently broken behavior.

## Suggested fix

Bump each existing case to its correct position:

```go
case paramName == "language_id":
    paramPosition = 3
case paramName == "instruction_set":
    paramPosition = 3
case paramName == "opcode":
    paramPosition = 4
```

And add direct unit tests for `getCompilers`, `getLibraries`, and
`getInstructionInfo` mirroring the `TestGetPopularArguments` shape, so
the next regression is caught immediately.

Alternatively, replace the positional decoder with `net/url`-based
parsing or a precomputed template-aware matcher; positional indexing on
`strings.Split` is fragile against future template additions and the
empty-segment quirk that caused this bug.
