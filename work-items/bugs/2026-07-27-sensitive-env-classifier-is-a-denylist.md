# `IsSensitiveEnvName` is a DENYLIST, so an unenumerated secret's VALUE is expanded into a generated draft

- **Status:** open
- **Severity:** P2 — see "Severity rationale" below. It is NOT plain operator
  error: on the marketplace path the entry that decides WHICH env var gets
  expanded is untrusted REMOTE content, so this denylist is the only barrier
  between a hostile catalog and the operator's environment.
- **Context:** adjacent-finding
- **Found:** 2026-07-27, sweeping the "enumerate what is unsafe" polarity class
  after closing the PR #591 review-gate findings on the fetch-helper header
  filter and the pinstatus redaction path
- **Amended:** 2026-07-27, after an adversarial review gate (finding F6) showed
  this file's own caller inventory was incomplete — it said "two callers" and
  there are THREE. See "Amendment" at the end.
- **Owner package:** `internal/api` (`import_vscode.go` declares it;
  `adopt_secret_route.go` is the third consumer)
- **Related, already closed:** `internal/api/marketplace_http.go`
  (`allowedMarketplaceHeaders`, commit "make the marketplace fetch header filter
  an allowlist"), `internal/vcpkgmcp/pinstatus/redact.go` (`emitSafeQueryKeys`)
- **Related, still open:** `2026-07-27-pinstatus-argv-refusal-is-a-credential-denylist.md`

## What

`IsSensitiveEnvName` (`internal/api/import_vscode.go:489`) enumerates the
env-var names considered SECRET, across four families:

    exact:     DATABASE_URL, CONNECTION_STRING, DSN, AUTHORIZATION, OAUTH,
               GOOGLE_APPLICATION_CREDENTIALS
    prefix:    AWS_, AZURE_, GCP_, GITHUB_, GOOGLE_, OAUTH_
    suffix:    _TOKEN, _SECRET, _PASSWORD, _PASSWD, _KEY, _API_KEY, _AUTH, _DSN
    substring: TOKEN, SECRET, PASSWORD, PASSWD, CREDENTIAL, BEARER, PRIVATE_KEY

It is not advisory. It GATES data flow at two separate sites, with the SAME
denylist polarity but two different consequences.

**Gate 1 — placeholder expansion** (`import_vscode.go:527`):

```go
if e.SkipSensitiveEnv && IsSensitiveEnvName(envName) {
    // G5 catalog policy: leave the placeholder VERBATIM so
    // the value is never written into the draft YAML.
```

A name that MATCHES keeps its `${env:NAME}` placeholder verbatim. A name that
does NOT match has its VALUE read from the process environment and written into
the generated YAML — which `mcphub marketplace generate` prints to stdout, i.e.
into a terminal, a model transcript, and whatever the caller persists.

**Gate 2 — vault routing** (`adopt_secret_route.go:37`):

```go
if !secretPrefixed && (!IsSensitiveEnvName(key) || !isLiteralAdoptEnvValue(value)) {
    continue   // NOT routed to the vault
}
```

A name that MATCHES (with a literal value) has its value moved into the
encrypted vault and replaced by `secret:<vaultKey>`. A name that does NOT match
is left untouched, so the literal secret is carried straight into
`renderStdioBridgeManifestYAML` (`adopt.go:188` -> `adopt.go:198`) and persisted
in the generated manifest as plaintext.

The default for an unenumerated name is therefore "expand the secret" on one
gate and "leave the secret in plaintext" on the other. Real spellings that miss
every family:

    MY_PAT / AZDO_PAT      personal access token (no _TOKEN suffix)
    JWT / MY_JWT           a bare JSON Web Token
    SESSION_ID / SID       session identifier
    LICENSE / LICENCE      commercial license key (no _KEY suffix)
    ASSERTION              RFC 7523 JWT bearer assertion
    COOKIE                 a raw cookie value
    NPM_AUTHTOKEN          "AUTHTOKEN", not "_TOKEN" as a suffix — but note it
                           DOES match the TOKEN substring, so this one is caught
    REFRESH / ACCESS       OAuth refresh/access values without a suffix

## Severity rationale

P2, and the reason is NOT "the operator misnamed their variable".

On the marketplace path, `marketplace_generate.go:204` constructs the expander
with the comment stating the trust level outright:

```go
SkipSensitiveEnv: true, // catalog is untrusted
```

The catalog is fetched over the network from a registry URL. A catalog ENTRY
therefore chooses which env var name appears in `env:` / `args:`, and hence
which name this classifier is asked about. A hostile or compromised registry can
simply pick a name the denylist does not enumerate — `MY_PAT`, `JWT`,
`LICENSE` — and the expander reads that variable out of the operator's process
environment and prints its value. The denylist is the SOLE barrier on that path
between untrusted remote content and the operator's environment, which is
exactly the property that makes an open-ended enumeration the wrong polarity.

What keeps it at P2 rather than P1, stated so the severity is not overstated
either: the expanded value goes to the operator's own stdout and generated
draft, NOT over the network to the registry — there is no egress channel back
to the attacker in this flow. Exposure is into a terminal, a model transcript,
and whatever the caller persists (which the tool's own docs already name as a
real disclosure surface), and it additionally requires the operator to run
`mcphub marketplace generate` on the hostile entry with that variable set.

The `mcphub adopt` gate has a different shape: the env map comes from the
operator's own client config, not from remote content, so there is no untrusted
chooser. Its failure is that a real secret the operator already has in a client
entry is silently NOT protected — it stays plaintext in the generated manifest
instead of being routed into the encrypted vault, while the operator reasonably
believes adopt's secret-routing handled it.

## Why it was not fixed in this pass

Two reasons, both scope rather than difficulty:

1. **It has THREE callers with DIVERGENT intent**, so inverting the classifier
   is a policy decision, not a mechanical edit:

   | Site | Setting / polarity | What "sensitive" decides there |
   |---|---|---|
   | `marketplace_generate.go:204` | `SkipSensitiveEnv: true` | leave the `${env:NAME}` placeholder verbatim instead of expanding it into the draft. Chooser is UNTRUSTED remote catalog content. |
   | `import_vscode.go:134` | `SkipSensitiveEnv: true` | same gate, but over a local-trusted VS Code file. |
   | `import_vscode.go:404` (G7 default) | `SkipSensitiveEnv: false` | nothing — the classifier is bypassed and every placeholder is deliberately expanded. |
   | `adopt_secret_route.go:37` | called directly, no flag | whether a LITERAL env value is moved into the encrypted vault (`secret:<key>`) or left plaintext in the generated manifest. |

   Inverting the classifier changes what "sensitive" MEANS for all of them at
   once, and the two live gates want opposite-shaped remedies: gate 1 wants
   "expand only an allowlisted name (or never expand)", gate 2 wants "route
   every literal value to the vault unless it is provably not a secret". So the
   right fix is probably to invert each GATE rather than to grow the shared name
   list — a behaviour decision for the owner of the G5 catalog policy and the
   adopt contract, which is why this stays filed rather than fixed.

2. **It is outside the PR #591 change surface.** It was found by sweeping the
   polarity CLASS, not from the review gate's finding list, so it is filed per
   the adjacent-findings protocol rather than fixed opportunistically.

Growing the name list is explicitly NOT the fix: that is instance-fixing an open
class, the pattern `work-items/lessons/2026-07-26-fix-instances-not-classes.md`
exists to prevent, and it is what left this list incomplete in the first place.

## Suggested resolution

Per gate, because they are not the same decision:

- **Gate 1 (G5/vscode expansion).** Invert so the SAFE outcome is the default:
  leave EVERY `${env:NAME}` placeholder verbatim (the operator is already
  instructed to replace them before `manifest create`), or expand only names on
  an explicit allowlist. `IsSensitiveEnvName` then only decides whether to WARN,
  never whether a value is emitted, so its incompleteness stops being a security
  property.
- **Gate 2 (adopt vault routing).** Invert so every LITERAL env value is routed
  into the vault unless it is positively known not to be a secret. The cost of
  over-routing is that a non-secret ends up encrypted (recoverable, and visible
  to the operator as a `secret:` reference); the cost of under-routing is a
  plaintext credential in a persisted manifest.

## Amendment (2026-07-27)

An adversarial review gate over commits `c0b9f67f..b4384bfa` (finding F6) showed
two defects in the version of this file filed at `b4384bfa`:

1. It stated "**It has two callers with OPPOSITE intent**" and named only the
   `PlaceholderExpander` sites. `internal/api/adopt_secret_route.go:37` is a
   third caller with a third intent, reached by `mcphub adopt`. An architect
   acting on the file as written would have fixed the two named callers and
   missed adopt entirely — instance-fixing the very class this file cites
   `work-items/lessons/2026-07-26-fix-instances-not-classes.md` to prevent.
   The caller inventory above is now a table, derived from
   `grep -rn IsSensitiveEnvName --include=*.go .` (4 non-test hits: 1 declaration
   at `import_vscode.go:489`, 3 call sites), so the next reader can re-run the
   one command that produced it.
2. Its severity rationale read as operator error ("requires the operator to have
   the secret in their environment AND a catalog/vscode entry referencing it by
   an unenumerated name"), omitting that `marketplace_generate.go:204` labels the
   catalog untrusted and that a remote entry is what picks the name. The
   "Severity rationale" section above replaces it, and states the bounding facts
   in both directions rather than only the alarming half.
