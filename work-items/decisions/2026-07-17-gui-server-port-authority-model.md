---
status: proposed
---

# Decision: GUI-server port authority is valid persisted intent reconciled to the bound listener at (re)start

Date: 2026-07-17

Owner: `$architect`; promotion `proposed -> accepted` belongs to the architecture-reviewer.

Relates:

- `work-items/active/2026-07-16-productization-gui-solidify/item3-restart-recon.md`
- `work-items/active/2026-07-16-productization-gui-solidify/item3-restart-design.md`
- roadmap item 3

## Context

The GUI has three different port representations:

- `resolveGuiPort` currently chooses explicit `--port`, else a persisted value in `[1024,65535]`, else `0`
  (`internal/cli/gui.go:47-55`).
- The bound listener is the runtime fact (`internal/gui/server.go:980-985`).
- `gui.pidport` is a derived rendezvous cache rewritten after bind (`internal/cli/gui.go:680-700`).

A self-restart currently reuses `os.Args[1:]` (`internal/gui/gui_self_restart.go:175-184`), so an inherited
`--port` can defeat a setting the restart is meant to apply. Shipping Windows autostart does not add a port
flag: `superviseArgs` returns `gui` with optional `--strict-mode`
(`internal/autostart/windows.go:63-68`). Only the long flag `port` is registered
(`internal/cli/gui.go:358`).

## Decision

1. A **valid** persisted `gui_server.port` is authoritative desired intent.
2. The bound GUI listener is authoritative actual runtime state.
3. `gui.pidport` remains a derived cache, never an intent source.
4. Manual launch precedence remains `explicit --port -> valid persisted -> 0`.
5. During a v2 self-restart only, valid persisted intent wins over inherited `--port` occurrences so the
   bound child reconciles to the setting the operator requested.

This is desired/actual reconciliation, not an inversion of runtime truth.

## Unset and invalid persisted values

Persisted state has one typed classification owned beneath `resolveGuiPort`:

- **Unset:** empty/whitespace or setting absent.
- **Valid:** parses as an integer in `[1024,65535]`.
- **Invalid:** set but unparsable or outside `[1024,65535]`, including persisted `0`.

Unset and invalid both mean **no valid persisted intent** for precedence. Therefore a self-restart preserves
an inherited explicit `--port`, including `--port 0`. Invalid is not silent: the CLI emits a visible warning
with the rejected persisted value and the fallback source. If invalid and no explicit flag exists, resolution
degrades to `0` (OS-assigned ephemeral) with that warning.

The validity/range predicate exists exactly once in the typed helper used by `resolveGuiPort`. The child-argv
builder consumes the returned classification and must not reproduce the range check.

## Parser-aware self-restart argv reconstruction

The builder uses the actual GUI `pflag.FlagSet` metadata, respects the `--` terminator, and removes recognized
effective flag spans rather than stripping matching strings.

When persisted intent is valid, remove **every** recognized pre-terminator occurrence so an earlier repeated
flag cannot survive and override it. When persisted is unset or invalid, preserve the original effective
occurrences and their normal last-wins parser behavior.

| Input shape | Valid persisted | Unset or invalid persisted |
| --- | --- | --- |
| `--port N` | Remove both tokens | Preserve both |
| `--port=N` | Remove token | Preserve token |
| `--port 0` | Remove both tokens | Preserve explicit zero |
| Repeated/mixed `--port` forms | Remove all recognized occurrences | Preserve all; normal last occurrence wins |
| `-port` | Reject as unregistered shorthand; do not normalize | Same rejection |
| `-- --port N` | Preserve; tokens after terminator are positional | Preserve |
| No port flag | No argv change | No argv change |

Malformed effective forms such as a missing `--port` value remain parser errors; self-restart does not repair
or reinterpret them.

## Warning and observability contract

- Invalid persisted value: stable warning/event `gui-port-persisted-invalid` with redacted-safe raw value,
  fallback source (`explicit-flag` or `ephemeral`), and no claim that the invalid setting took effect.
- Valid persisted value that removes inherited flags: restart progress records source `persisted`.
- Unset fallback: source `explicit-flag` or `ephemeral` without warning.

## Scope and non-goals

- The hub-aggregate port remains a separate authority owned by `hub-mcp.endpoint.json` and the existing bind
  transaction.
- Manual foreground `--port` behavior does not change.
- The accepted numeric persisted range does not change.
- This decision does not define listener handoff, lock reservation, or browser navigation; those are local to
  item 3 DESIGN v2.

## Rejected alternatives

- **Treat invalid persisted as set and drop explicit `--port`:** rejected because it loses a valid explicit
  operator choice, then silently degrades to ephemeral.
- **Strip `--port` unconditionally:** rejected because unset/invalid development sessions lose their explicit
  port, including intentional `0` semantics.
- **Raw string removal:** rejected because it mishandles `--`, repeats, `--port=N`, and tokens that merely
  resemble a flag.
- **Make pidport authoritative:** rejected because pidport is written after bind and cannot express desired
  intent.

## Enforcement probe

`TestRestartV2_PortArgvMatrix` is a table test whose rows are every argv shape above crossed with persisted
`unset`, `valid`, `invalid-parse`, `invalid-low`, and `invalid-high`. It must additionally assert:

- manual launch precedence is unchanged;
- the invalid branch preserves explicit `--port 0` and emits `gui-port-persisted-invalid`;
- a valid persisted value removes every effective occurrence before `--`;
- tokens after `--` are byte-preserved;
- `-port` and missing-value forms are rejected;
- the argv builder consumes the typed resolution and contains no independent `[1024,65535]` predicate.

## Terms and Abbreviations

- **Actual state:** the port the listener successfully bound.
- **Desired intent:** a valid persisted operator choice.
- **Effective flag:** a token span the registered command parser interprets as `--port` before `--`.
- **GUI:** Graphical User Interface.
- **pflag:** The flag parser used by Cobra.
