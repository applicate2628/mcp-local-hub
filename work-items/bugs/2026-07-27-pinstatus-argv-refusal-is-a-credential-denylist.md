# pinstatus argv-refusal is still a credential DENYLIST, so unenumerated token spellings reach `git ls-remote`'s argv

- **Status:** open
- **Context:** adjacent-finding
- **Severity:** P2 (real credential-exposure channel; bounded to a portfile that
  embeds a secret in a fetch-URL query, which is 0 of 124 real fetch URLs measured)
- **Found:** 2026-07-27, while closing the PR #591 review-gate finding
  "redaction denylist inversion" (`internal/vcpkgmcp/pinstatus/redact.go`)
- **Owner package:** `internal/vcpkgmcp/pinstatus`

## What

`redact.go` holds two predicates over the same query string, answering two
different questions:

- **Emission** — may this value be PRINTED into an MCP result? As of
  2026-07-27 this is an ALLOWLIST (`emitSafeQueryKeys`, currently empty), so an
  unrecognized parameter's value is redacted. Fixed.
- **Refusal** — does this URL EMBED A CREDENTIAL, such that handing it to
  `git ls-remote` would put the secret in a child process's argv? This is still
  a DENYLIST (`argvSecretQueryKeys`: token, secret, password, passwd, pwd, key,
  auth, credential, sig, signature), matched as a substring of the key.

So these real spellings are redacted on emission but are STILL passed to the
child's command line, where it is readable by every local account for the
child's lifetime (`pinstatus.go:218` → `defaultRemoteRefs` →
`exec.CommandContext("git", "ls-remote", remote)`):

    ?code=       OAuth 2.0 authorization code
    ?jwt=        a bare JSON Web Token
    ?assertion=  RFC 7523 JWT bearer assertion
    ?pat=        Azure DevOps personal access token
    ?session= / ?sid=   session identifier
    ?ticket=     CAS / Kerberos service ticket
    ?refresh=    OAuth refresh token

None contains "token", "secret", "key" or "auth" as a substring.

## Why it was not fixed in place

The fail-closed fix is to refuse EVERY query-bearing remote URL: a git remote
URL has no legitimate query (measured 2026-07-27 against `C:\vcpkg`: of 2856
portfiles, 124 `URL:`/`GITLAB_URL:` fetch-URL lines, **0** carry a query string
and **0** carry userinfo), so an unclassifiable parameter is something we cannot
prove safe to put in argv.

But the refusal's verdict is a CLOSED WIRE ENUM value whose contract
(`types.go:177`) is *"the portfile's remote URL embeds a credential"*. Returning
that for `?depth=1` would assert a fact the tool never observed — a conclusion,
not an observation, which is precisely the fabricated-verdict class this server
exists to eliminate, and it violates the tool's own stated VOCABULARY RULE
(`vcpkgserver/tools.go:108`).

Closing it therefore requires either a renamed or an additional per-port
`Reason` (something on the order of `remote_url_query_unclassifiable`), which
changes a documented closed enum surfaced in `vcpkg_pin_status`'s tool
description and in `servers/vcpkg/README.md`. That is an architect decision, not
an implementer's, so the emission fix was landed and this was filed instead of
silently widening the denylist by a few more names — extending the list is
instance-fixing an open class, which is the pattern the repo already has a
lesson about (`work-items/lessons/2026-07-26-fix-instances-not-classes.md`).

## Reproduction

```go
hasEmbeddedCredential("https://host/repo.git?jwt=eyJhbGciOi...")  // == false
```

The query is then executed and the JWT appears in the `git ls-remote` command
line.

## Suggested resolution

1. Architect decides the reason name for "query parameter present that this tool
   cannot classify".
2. `hasEmbeddedCredential` splits into `hasEmbeddedCredential` (userinfo, and
   positively-identified secret keys → `remote_url_credential_bearing`) and a
   fail-closed `hasUnclassifiableQuery` (any remaining query parameter with a
   value → the new reason).
3. `argvSecretQueryKeys` then only decides WHICH of the two reasons is reported,
   never whether the URL is queried, so its incompleteness stops being a
   security property.
