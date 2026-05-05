# Contributing to mcp-local-hub

`mcp-local-hub` is an early **preview-stage** project. Contributions are welcome, but please understand that the API surface, manifest schema, GUI routes, and HTTP endpoints may change in incompatible ways before a 1.0 tag.

## Before you start

1. Read the [README](README.md) preview-status warning.
2. Skim [docs/phase-3b-ii-verification.md](docs/phase-3b-ii-verification.md) — the manual smoke matrix is the closest thing to a release-acceptance gate.
3. Check [docs/superpowers/plans/phase-3b-ii-backlog.md](docs/superpowers/plans/phase-3b-ii-backlog.md) for active sequencing and explicitly-rejected proposals (so you don't reinvent something already reviewed and declined).

## Filing an issue

- **Bug reports** — include OS (`mcphub --version` output is fine), client(s) involved, steps to reproduce, and the relevant logs from `%LOCALAPPDATA%\mcp-local-hub\logs\` (Windows) or `$XDG_DATA_HOME/mcp-local-hub/logs/` (Linux/macOS). Redact secrets before pasting.
- **Feature requests** — describe the user goal, not the proposed implementation. Many requests overlap with backlog items already scoped under different names; the maintainers will cross-reference.
- **Security issues** — DO NOT open a public issue. See [SECURITY.md](SECURITY.md).

## Pull requests

### Scope

- One PR = one logical change. Bug fixes do not need surrounding cleanup; features do not need bundled bug fixes.
- Bot-generated PRs (Codex security scans, Dependabot, etc.) are batched into umbrella PRs by the maintainers — please do not file duplicates of issues already covered by an open umbrella.
- Substantial UI or API changes require a design memo under `docs/superpowers/specs/` before code review starts. Open a draft PR with the memo first; the maintainers will guide the next step.

### Review

- Every PR runs `go test ./...` locally before push (CI is `workflow_dispatch`-only to conserve build minutes; the maintainers trigger CI manually for non-trivial PRs).
- The Codex bot may add automated security review comments on PR open. Treat them as input, not gates — the maintainers decide which findings to act on.
- For PRs that touch security-sensitive surfaces (auth, manifest parsing, subprocess execution, HTTP endpoints, Windows scheduler XML), expect a `codex exec -c model_reasoning_effort=xhigh` second-opinion review before merge.
- **Readiness matrix gate (`README.md` Feature & readiness matrix):** if the PR introduces a new user-visible capability, lands the live-smoke evidence that promotes a row from `🚧 Roadmap` → `⚠ Preview` → `✅ Stable`, or removes a documented surface, the PR must include the matrix update in the same commit. Reviewers explicitly verify this row was touched when applicable.

### Style

- Go: `gofmt -d ./...` (or `goimports`) clean and `go vet ./...` clean. `staticcheck ./...` is encouraged when available but not strictly required to pass — install via `go install honnef.co/go/tools/cmd/staticcheck@latest`. (`go vet` does NOT run `staticcheck`; treat them as two separate tools.) No new linter rules added without discussion.
- Frontend: `cd internal/gui/frontend && npm run typecheck && npm test -- --run` must pass. The embedded bundle (`internal/gui/assets/{index.html,app.js,style.css}`) must be regenerated via `go generate ./internal/gui/...` before commit if frontend source changed.
- Commits: one focused commit per logical change, message starts with a conventional-commits prefix (`fix:`, `feat:`, `chore:`, `docs:`, `security:`, `refactor:`, `test:`).

## Local development

See [README.md](README.md) for the build instructions and [CLAUDE.md](CLAUDE.md) for the GUI frontend dev-loop and E2E test workflow.

`go test ./... -count=1 -timeout 240s` is the canonical full-suite check. On Windows, the `internal/gui/e2e/` Playwright suite is the integration-level gate; it is currently Windows-only because the scheduler backend is Windows-first.

## Security-sensitive contributions

Changes to any of the following surfaces require explicit reviewer approval and a security-impact note in the PR description:

- `internal/gui/csrf.go`, `internal/gui/server.go` — DNS-rebinding and same-origin gates.
- `internal/daemon/loopback_guard.go`, `internal/daemon/host.go`, `internal/daemon/lazy_proxy.go` — daemon HTTP admission.
- `internal/api/manifest.go`, `internal/api/install.go` — manifest name validation, install/uninstall scope.
- `internal/perftools/path_guard.go` — subprocess path traversal.
- `internal/secrets/` — encrypted vault.
- `internal/gui/single_instance.go`, `internal/gui/probe*.go` — `--force --kill` identity gate.

If you are unsure whether your change touches one of these surfaces, ask before opening the PR.

## License

Contributions are accepted under the [Mozilla Public License 2.0](LICENSE) (the project license). By submitting a PR you affirm that you have the right to license your contribution under MPL-2.0.

For commercial licensing inquiries, see the README.

## Terms and Abbreviations

- `Codex bot` — OpenAI Codex Cloud's automated PR review agent; comments on PR open with security findings.
- `codex exec -c model_reasoning_effort=xhigh` — invocation of OpenAI Codex CLI in "extra-high reasoning" mode used by maintainers for security-sensitive review.
- `CSRF` — Cross-Site Request Forgery; an attack class the GUI's same-origin gate defends against.
- `DNS-rebinding` — an attack that binds an attacker hostname to `127.0.0.1`; the GUI's Host-allowlist defends against it.
- `E2E` — end-to-end test; here, the Playwright suite under `internal/gui/e2e/`.
- `MPL-2.0` — Mozilla Public License 2.0; the project's open-source license.
- `MCP` — Model Context Protocol.
- `staticcheck` — third-party Go static analyzer (`honnef.co/go/tools/cmd/staticcheck`). Separate tool from `go vet`; install explicitly to use.
- `xhigh` — short for "extra-high reasoning effort", a Codex CLI mode used for second-opinion review.
