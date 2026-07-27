# `IsSensitiveEnvName` is a DENYLIST, so an unenumerated secret's VALUE is expanded into a generated draft

- **Status:** open
- **Context:** adjacent-finding
- **Severity:** P2 (credential-value exposure into tool output; requires the
  operator to have the secret in their environment AND a catalog/vscode entry
  referencing it by an unenumerated name)
- **Found:** 2026-07-27, sweeping the "enumerate what is unsafe" polarity class
  after closing the PR #591 review-gate findings on the fetch-helper header
  filter and the pinstatus redaction path
- **Owner package:** `internal/api` (`import_vscode.go`)
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

It is not advisory. At `import_vscode.go:527` it GATES a data flow:

```go
if e.SkipSensitiveEnv && IsSensitiveEnvName(envName) {
    // G5 catalog policy: leave the placeholder VERBATIM so
    // the value is never written into the draft YAML.
```

A name that MATCHES keeps its `${env:NAME}` placeholder verbatim. A name that
does NOT match has its VALUE read from the process environment and written into
the generated YAML — which `mcphub marketplace generate` prints to stdout, i.e.
into a terminal, a model transcript, and whatever the caller persists.

The default for an unenumerated name is therefore "expand the secret". Real
spellings that miss every family:

    MY_PAT / AZDO_PAT      personal access token (no _TOKEN suffix)
    JWT / MY_JWT           a bare JSON Web Token
    SESSION_ID / SID       session identifier
    LICENSE / LICENCE      commercial license key (no _KEY suffix)
    ASSERTION              RFC 7523 JWT bearer assertion
    COOKIE                 a raw cookie value
    NPM_AUTHTOKEN          "AUTHTOKEN", not "_TOKEN" as a suffix — but note it
                           DOES match the TOKEN substring, so this one is caught
    REFRESH / ACCESS       OAuth refresh/access values without a suffix

## Why it was not fixed in this pass

Two reasons, both scope rather than difficulty:

1. **It has two callers with OPPOSITE intent.** `SkipSensitiveEnv: true`
   (marketplace generate `marketplace_generate.go:204`, vscode import
   `import_vscode.go:134`) wants secrets left verbatim. G7 callers leave it
   `false` and deliberately expand every placeholder (`import_vscode.go:404`).
   Inverting the classifier changes what "sensitive" MEANS for both, so the
   right fix is probably to invert the GATE (expand only allowlisted names, or
   never expand at all under the G5 policy) rather than to grow the name list —
   and that is a behaviour decision for the owner of the G5 catalog policy.

2. **It is outside the PR #591 change surface.** It was found by sweeping the
   polarity CLASS, not from the review gate's finding list, so it is filed per
   the adjacent-findings protocol rather than fixed opportunistically.

Growing the name list is explicitly NOT the fix: that is instance-fixing an open
class, the pattern `work-items/lessons/2026-07-26-fix-instances-not-classes.md`
exists to prevent, and it is what left this list incomplete in the first place.

## Suggested resolution

Under the G5/vscode policy, invert the gate so the SAFE outcome is the default:
leave EVERY `${env:NAME}` placeholder verbatim (the operator is already
instructed to replace them before `manifest create`), or expand only names on an
explicit allowlist. `IsSensitiveEnvName` then only decides whether to WARN,
never whether a value is emitted, so its incompleteness stops being a security
property.
