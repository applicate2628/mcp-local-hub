# Bug: Windows ADS and alias paths escape the CST bundle manifest model

- id: 2026-08-12-cst-saved-field-windows-ads-alias-escape
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: CST saved-field Windows path grammar and manifest identity
- found-by: security-reviewer
- fix-class: design-decision

## Reproduction

Design-level counterexample against `design.md` SHA-256
`AAD6F5D5395EB1E3ADF289BE898878A59F584AA3193B671B3A71F40BDA7667F4`:

1. Supply a policy project component or vendor payload component using a Windows
   named-stream form such as `field.sct:hidden:$DATA`, or an equivalent Win32 alias
   using reserved device/trailing-dot/trailing-space semantics.
2. `project_relative` does not define a closed Windows component grammar
   (`design.md:247-252`), and a vendor payload is only required to be a clean
   contained POSIX-relative string (`design.md:417-419`).
3. The canonical manifest enumerates ordinary regular files
   (`design.md:261-266`); named Alternate Data Streams (ADS) are distinct byte
   streams and are not represented by that ordinary default-stream row.
4. The declared path test matrix covers Universal Naming Convention (UNC), mapped
   drives, reparses, and swaps but not ADS or Windows alias forms
   (`design.md:962-966`).

Microsoft documents that a path can use `filename:stream:$DATA` to address a named
stream and that the last path component may denote a stream rather than a file:

- <https://learn.microsoft.com/en-us/windows/win32/fileio/file-streams>
- <https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file>
- <https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/ffb795f3-027d-4a3c-997d-3085f2332f6f>

## Expected versus actual

- Expected: policy, manifest, source/destination walkers, vendor candidate paths,
  generated headers, and registration use one canonical Windows file-object model;
  no byte stream omitted from the manifest can reach CST.
- Actual design: alternate streams and Win32 alias forms are neither explicitly
  rejected nor included in the authorization manifest.

## Security impact

An untrusted bundle/vendor record can select bytes outside the operator-authorized
ordinary file manifest while remaining below the same directory spelling. This
falsifies security claims `SEC-C02`, `SEC-C07`, and `SEC-C14`.

## Required correction

Return to `$security-engineer` and `$architect`. Define a shared closed Windows
component grammar and file-object model. At minimum decide whether named streams are
categorically rejected—including files that carry non-default streams—or explicitly
enumerated and authorized. Also cover DOS/NT device forms, reserved names, colon and
wildcards, separators, trailing-dot/space aliases, case/Unicode normalization, and
identity round-trip through held directory handles.

## Falsifying probe

For project, mesh, selected payload, clean payload, generated header, and registration
roles, inject ADS syntax, reserved DOS names, trailing dot/space, case/Normalization
Form C aliases, and device/extended prefixes. Every case must fail before CST and
before any byte omitted from the canonical manifest is read.

## Provenance

- Finding: `SR-02` in `security-review.md`.
- Security review SHA-256:
  `9EEE85250CF927BE9D89D5A6E48A8E8437F23417EEBD6D8A9C4B4165EABBE316`.
- Upstream security constraints SHA-256:
  `E3FF52C6F35D617BDA3E774838C4E88441C5195CBB32A11E113ECE33EF17715C`.
