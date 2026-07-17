# Serena Language Projection Fix Plan

Status: verified; awaiting orchestrator-owned commit.

1. Verify the bootstrap writer, survey taxonomy, shipped mcphub LSP names, and installed Serena enum. Completed.
2. Change the existing bootstrap regression test first and observe the tagged test fail on leaked identifiers. Completed.
3. Add one mcphub-to-Serena mapping/filter owner at the YAML write boundary; preserve mcphub LSP registry behavior. Completed.
4. Add enum-value, unknown-omission, diagnostic, mapping, de-duplication, and bootstrap-output assertions. Completed.
5. Run focused tagged tests, `go build ./...`, `go vet ./...`, and the full tagged CLI suite. Completed.
6. Inspect formatting and diff scope; hand off without committing. Completed.

Rollback: remove the two implementation/test file changes before publication; no schema migration, persistent registry mutation, or external contract change was introduced.

## Terms and Abbreviations

- CLI: Command-Line Interface.
- LSP: Language Server Protocol.
- YAML: YAML Ain't Markup Language, the format of Serena's project configuration.

