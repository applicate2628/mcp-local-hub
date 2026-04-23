# B1 + B2 — Pending design decisions

Autonomous work paused pending human decision on two architecture-level API choices. Each has a defensible default but is substantial enough that picking the wrong option means a meaningful rewrite. Per Daisy's overnight instruction:

> Останавливаюсь и жду твоего утра если: Найдён A/B/C-style architecture-level выбор (как было со scheduler seam)

## Context

D1 merged successfully (PR #2, commit `8c20892`). Next in sequence per backlog: B1 + B2 combined (both backend-only, no UI). Investigation of existing code (`internal/api/migrate.go`, `internal/clients/clients.go`) surfaced the decisions below.

---

## Decision 1 — B1 Demigrate: how to extract pre-migrate state?

`api.MigrateFrom` rewrites stdio entries to hub-HTTP entries and creates a timestamped backup before each rewrite. Demigrate needs to reverse this.

### A. Whole-file restore via `adapter.Restore(backupPath)`

Use the existing `Restore()` method to roll back the client config file to its most recent pre-migrate state.

- **Pros:** Trivial implementation — 10-20 LOC, no new client-interface methods. Matches how `Restore()` is already documented.
- **Cons:** Destroys unrelated user changes since the migrate. Example: user migrates `serena`, then manually adds `mcp-playwright` via CLI, then clicks "Demigrate serena" in GUI — `mcp-playwright` silently disappears because the backup predates it.
- **Surface:** Just calls the existing `Restore()` method.

### B. Per-entry rollback (recommended)

Add a new `Client` interface method `GetEntryFromBackup(backupPath, name string) (*RawEntry, bool, error)` that reads an arbitrary backup file and extracts one entry's raw shape (including stdio command/args/env fields that `GetEntry` currently strips). Demigrate reads the most recent backup, extracts the server's pre-migrate entry, and calls either `AddEntry` (if backup had an entry) or `RemoveEntry` (if migrate added it from scratch).

- **Pros:** Preserves unrelated user changes. Correct UX.
- **Cons:** ~60-80 LOC new interface method + 4 adapter implementations (JSON, TOML, Claude, Antigravity) + tests. Introduces a new typed shape for "raw pre-migrate entry" that must round-trip stdio fields.
- **Surface:** New `Client.GetEntryFromBackup` method. New `RawEntry` type (or reuse extended `MCPEntry`).

### C. Pre-migrate snapshot side-channel

Store the pre-migrate entry in a hub-side sidecar file (`~/.mcp-local-hub/migrate-snapshots/<client>/<server>.json`) at migrate time. Demigrate reads the sidecar and writes it back.

- **Pros:** Decouples from existing backup format. Per-entry precision by construction.
- **Cons:** New storage surface + migration discipline (what if sidecar is missing? user deletes it?). More moving parts than B.
- **Surface:** New storage dir + `api.readMigrateSnapshot` / `writeMigrateSnapshot` helpers + lifecycle glue.

### My lean: **B**. Correct UX matters for a visible feature in the GUI; `A` would produce a subtle data-loss bug the first time someone migrates + customizes + demigrates in that order.

---

## Decision 2 — B2 ExtractManifestFromClient: how to expose stdio fields?

`api.ExtractManifestFromClient(client, server)` reads a stdio entry from a client config and returns a draft manifest YAML. The existing `MCPEntry` struct only carries `URL` / `RelayServer` / `RelayDaemon` — it has no Command/Args/Env fields. Need to expose those somewhere.

### A. Expand `MCPEntry` with optional stdio fields

```go
type MCPEntry struct {
  Name         string
  URL          string
  Command      string            // NEW: stdio command (npx, uvx, ...)
  Args         []string          // NEW
  Env          map[string]string // NEW
  RelayServer  string
  RelayDaemon  string
  RelayExePath string
}
```

- **Pros:** One struct covers both stdio and hub-HTTP entries. Existing callers unchanged (zero values are zero values).
- **Cons:** Struct bloats — callers now have to know which fields are meaningful for which transport. Inconsistent API (Url OR Command+Args+Env, never both). Mild "god struct" smell.
- **Surface:** `MCPEntry` gains fields, all 4 adapter `GetEntry` implementations populate them, tests updated.

### B. New typed method `GetStdioEntry(name) (*StdioEntry, error)`

```go
type StdioEntry struct {
  Name    string
  Command string
  Args    []string
  Env     map[string]string
}
```

Separate method from `GetEntry`. Returns `(nil, nil)` if the entry is hub-HTTP-migrated or absent.

- **Pros:** Clean separation of transport types. Caller sees one-or-the-other.
- **Cons:** Two nearly-parallel methods (`GetEntry` + `GetStdioEntry`). Two struct types where one might suffice.
- **Surface:** New interface method, new type, 4 adapter implementations, tests.

### C. Generic raw-entry method `GetRawEntry(name) (map[string]any, error)`

```go
// Returns the raw key/value map for the entry as stored in the config file,
// without any transport-specific parsing. Caller does the field extraction.
func (c *client) GetRawEntry(name string) (map[string]any, error)
```

- **Pros:** Smallest interface surface. Caller pattern-matches the shape it needs.
- **Cons:** Type-unsafe — every caller re-implements the stdio-field extraction. Easy to diverge between callers.
- **Surface:** One new method, but downstream code becomes stringly-typed.

### My lean: **B** — enough typing to give callers confidence, explicit separation between hub and stdio entries. A nudges us toward a god-struct, C loses type safety.

---

## What I need from you

For each decision, pick A / B / C (or propose D). I'll then write the B1+B2 combined plan, run it through Codex CLI verify, execute via subagent-driven-development, open the PR, run the review cycles, and merge.

If both decisions come back as my leans (both B), I can start immediately without further questions. If either differs, I'll adapt the plan accordingly.

---

## Status snapshot

- **D1 merged** — PR #2 closed, master at `8c20892`.
- **Feature branch deleted**.
- **Next branch to create**: `feat/phase-3b-ii-b1-b2-reverse-migrate-and-extract` (or whatever you prefer).
- **Estimated scope** post-decisions:
  - B1 = ~150-200 LOC (Go) + ~100 LOC tests depending on option.
  - B2 = ~150 LOC + ~100 LOC tests depending on option.
  - Combined plan: ~8-10 tasks.

**I'm idle until you respond.** No branches created, no code written, no PRs opened. This status document is the only artifact from this attempt.
