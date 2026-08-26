# Bug: Python global-site startup precedes worker capability revocation

- id: 2026-08-12-cst-worker-capability-prebootstrap-site-execution
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: CST saved-field worker bootstrap / inherited capabilities
- found-by: security-reviewer
- fix-class: design-decision

## Reproduction

1. Read the exact worker launch at `work-items/decisions/2026-08-12-cst-saved-field-authority-containment.md:67-88` and `work-items/active/2026-08-11-cst-saved-field-sampler/design.md:1108-1118`.
2. Observe that it uses `-I -s -E -m ...` but not `-S`.
3. Compare the promised first-instruction handle clear at `design.md:1175-1202` with CPython 3.13's documented automatic `site` import, executable `.pth` processing, and `sitecustomize` import: <https://docs.python.org/3.13/library/site.html> and <https://docs.python.org/3.13/using/cmdline.html>.

## Expected

No extensible or ambient code executes while any inherited worker handle is still inheritable; the first process instruction revokes and reads back all required flags.

## Actual

Global-site `.pth` and `sitecustomize` code can execute before the `-m` worker module. It can use/retain capabilities or spawn an inheriting descendant before the claimed revocation and before receipts can observe it.

## Required design correction

Define a pinned non-extensible bootstrap that revokes inheritance before global-site/package/plugin/import execution and preserves exact package resolution. Re-review Claims 12-14.

## Terms and Abbreviations

- **CST** — Computer Simulation Technology Studio Suite.
