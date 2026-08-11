---
status: accepted
date: 2026-08-10
decided-by: architecture-reviewer
slug: windows-console-debug-opt-in
---

# Make the Windows console a single explicit debug opt-in

Decision id: `DEC-2026-08-10-windows-console-debug-opt-in`

## Context

The installed Windows executable was once promoted with PE subsystem 3
(`WINDOWS_CUI`), and the canonical supervisor/daemon topology multiplied that
mistake into many visible consoles. The corrected PE subsystem 2
(`WINDOWS_GUI`) deployment removed the observed windows, but the composition
root still calls `AttachConsole` by default, several background spawn owners do
not fully suppress console allocation, and canonical setup/upgrade paths do not
reject a CUI candidate before mutation. These facts are established in
`work-items/active/2026-08-10-windows-console-opt-in/research.md:17-26`,
`:59-71`, and `:73-120`.

The operator has set the durable external contract: every ordinary Windows
launch exposes no visible console/window; exactly one explicit debug flag may
request one. There are no other flags, environment variables, configuration
keys, or implicit modes that opt into console behavior.

## Decision

1. The only public Windows console opt-in is the strict startup-prefix flag
   `--debug-console`: it opts in only when it is the exact first argument after
   the executable (`mcphub --debug-console [command ...]`). It has no short
   alias, equals/value form, environment-variable equivalent, config
   equivalent, or implied mode. The token in any later position remains an
   ordinary unchanged Cobra argument or option value and never opts in.
   `--foreground`, `--no-tray`, direct `supervise`, restart, and worker modes do
   not imply console participation.
2. One pure startup-prefix parser is the sole owner of the flag's spelling,
   placement, meaning, and normalization. The Windows composition root passes
   ambient arguments to it once and applies the returned typed process-local
   policy before command execution; Cobra does not register or independently
   parse this startup-only flag. The parser inspects only `argv[1]`. On an exact
   match it removes exactly that element and preserves every remaining argv
   element byte-for-byte and in order; otherwise it returns all argv unchanged.
   With no opt-in the composition root performs no `AttachConsole` or
   `AllocConsole` call. With the opt-in it first attaches to an existing parent
   console and otherwise allocates one; valid inherited redirected standard
   handles are retained.
3. Debug console intent is process-local and is never propagated to a child.
   Every background, helper, supervisor, daemon, scheduler, hidden-worker, and
   external-host spawn exposes no visible console/window and never joins or
   shares its parent's console, even when the parent is running with
   `--debug-console` or under a debugger. Canonical GUI-subsystem mcphub
   descendants remain absent from `GetConsoleProcessList`. A generic external
   CUI child started with `CREATE_NO_WINDOW` may report zero members or exactly
   one member equal to itself; self-only membership is accepted only when a
   debug parent's console list excludes it (or an ordinary parent has a measured
   no-console state), no visible console/window exists, required redirected
   handles remain valid, and process `CreationDate` semantics remain unchanged.
4. The internal `MCPHUB_NO_CONSOLE_ATTACH` marker is removed. Default-off
   policy makes it redundant, and retaining a second ambient console control
   would preserve two competing truths.
5. Every distributable, installable, or canonical Windows `mcphub.exe` is PE
   subsystem 2. One bounded host-neutral PE-admission owner validates build
   outputs and install/upgrade candidates. A subsystem-3 or malformed candidate
   is rejected before any canonical destination, supervisor, scheduler, or
   running fleet state is mutated.
6. No-visible-window and no-parent-console-sharing child creation remains owned
   by the common Windows spawn primitives. Direct `CreateProcess` paths carry
   the same `CREATE_NO_WINDOW` property; bypassing those owners is an admission
   failure. The common owner does not add `DETACHED_PROCESS`: the target A/B
   changed PowerShell/CIM `CreationDate` by the host UTC offset, so forcing zero
   membership would break existing process-identity behavior. A synchronous
   operator-selected console editor is not launched by default: GUI editors
   continue normally, while a console-only editor fails before spawn with a
   diagnostic naming the graphical-editor requirement. The debug flag does not
   become a general-purpose child-console escape hatch.
7. Rollout is availability-preserving: validate and probe the candidate while
   the existing healthy GUI-subsystem fleet remains live, then use the existing
   atomic rename-aside upgrade seam. A post-swap health failure restores the
   retained prior GUI-subsystem binary and restarts its supervisor.

## Consequences

- Ordinary command, GUI, supervisor, daemon, restart, scheduler, npm, and
  worker launches have one fail-closed console policy.
- Existing stdout/stderr redirection and command exit codes remain contracts;
  a GUI-subsystem process can write to valid inherited handles without joining
  a console.
- A headless external CUI child may expose self-only console-list membership as
  an implementation detail; visible windows, parent/shared membership, broken
  redirection, or changed process-identity time remain failures.
- Windows console-based external editors cease to be an implicit exception;
  operators select a graphical editor for `manifest edit` and `secrets edit`.
- Plain `go build` output is not an admitted product artifact. Canonical build
  scripts and documentation produce and verify subsystem 2.
- The decision adds no third-party dependency. The PE check is a narrow bounded
  header reader because the Go standard package explicitly warns that
  `debug/pe` is not hardened for adversarial inputs:
  https://pkg.go.dev/debug/pe.

## Status and promotion

Proposed by the architect. Promotion to `accepted` belongs to a fresh
independent architecture-review gate after it verifies the class-specific child
oracle, the generic `DETACHED_PROCESS` rejection, the single-owner contract,
participant disposition, and executable enforcement probes in the design.
