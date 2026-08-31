# Security

Crucible Analytic sits between a customer's visitors and their traffic
data. The panel holds accounts; the collector and beacon hold records of
who visited what, which is personal data in most jurisdictions this runs
in. This file records how that is defended, what has been checked, and
what is still open.

## Reporting a vulnerability

Open a GitHub issue for anything already public. For anything else,
email **Fırat Coşkun <kettipcimm@gmail.com>** rather than filing
publicly, and give us a reasonable window to ship a fix before
disclosing.

That is a person's address rather than a `security@` alias, and it is
deliberate. This project is one maintainer; an alias would forward to the
same inbox while suggesting a rota that does not exist, and a reporter
who is told to write to a team is entitled to expect one. The person
responsible for this software is named above and is who answers.

## Design, in one page

**Five processes, five database roles.** The collector, beacon,
read-only API and panel each connect as their own role. The panel's role
has **no access at all** to the analytics tables — it reads traffic
numbers over HTTP from the read-only API, exactly as an external panel
would. The two negative facts (the panel cannot read analytics, the API
cannot write) are asserted by startup checks against the live database,
not assumed.

**Nothing the browser loads comes from anywhere else.** The stylesheet,
htmx, every template and every string are compiled into the binary. No
CDN, no npm, no build step. The Content-Security-Policy has neither
`unsafe-inline` nor `unsafe-eval`, and a structural test refuses any
template that would need them.

**Credentials are never stored in a usable form.** Passwords are
argon2id. API tokens, developer links and owner invitations are stored
as SHA-256 — a leaked database or config file hands over nothing that
works.

**Settings with legal weight sit behind a second password**, asked every
time, with no session. Retention periods and IP storage mode are
decisions with consequences outside this software.

## What has been checked

Audited against the OWASP Top 10 (2021), the CWE Top 25, and the ASVS
verification requirements that apply to a self-hosted admin panel.
Every item below was checked against the source, and the fixes carry
tests.

### Fixed

| Finding | Class |
|---|---|
| `pgx/v5` 5.7.6: SQL injection via placeholder confusion with dollar-quoted literals ([GO-2026-5004](https://pkg.go.dev/vuln/GO-2026-5004)), reachable from a live query path | A06, CWE-89 |
| `golang.org/x/text` 0.24.0: infinite loop on invalid input ([GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970)), reachable from the panel's text casing on user-supplied names | A06, CWE-835 |
| Panel: every `POST` read an unbounded request body before authentication. A measured 64 MiB body cost ~128 MiB of heap and was *then* rejected for a missing CSRF token | A05, CWE-770 |
| Panel: a database error's text — constraint names, SQL state, sometimes the query — was rendered into the customer's page when a settings write failed | A04, CWE-209 |
| Panel: the routes reachable without credentials dereferenced the database handle before checking it, so a misconfigured server crashed remotely rather than answering | CWE-476 |
| API: one SQL identifier is interpolated (Postgres has no placeholder for one) and the guard was a comment saying "only pass constants" | CWE-89, latent |
| Panel: an oversized body surfaced as "your CSRF token is stale", sending whoever hit it to reload a page that would fail again | A09 |
| Panel: the 413 above then rendered the *500* page — "the failure was recorded on the server" — because the renderer falls back silently for a status it has no words for. A defect in this audit's own fix, found while documenting it | A09 |

`govulncheck` reports no reachable vulnerabilities, and is part of the
release check below.

### Checked and correct, no change needed

- **SQL injection.** Every query is parameterised. The single
  interpolated identifier is now a closed type with unexported values,
  so a request-derived string cannot reach it and a new one does not
  compile.
- **XSS.** `html/template` throughout, with no `template.HTML`,
  `template.JS`, `template.URL` or `HTMLAttr` anywhere in the tree.
- **Broken access control.** The API resolves the site through one
  choke point that checks the token's grant, so no handler can forget.
  The panel asks for a capability in each handler, and every permission
  test has a pair — the allowed request, and the same one forged with a
  valid CSRF token from a page the actor may see, so it fails on
  authority rather than on a missing token.
- **Open redirect.** The sign-in form's `?next=` is rejected rather than
  repaired, including the two forms that look relative and are not:
  `//host` and `/\host`.
- **SSRF.** Every outbound URL — the bot-data source, the IP range
  downloads, the service health checks — comes from a config file. None
  is derived from a request.
- **CSRF.** Every state-changing route refuses a request with no token,
  asserted by walking all of them.
- **Session management.** Token renewed on login and on entering the
  pending second-factor state (fixation); `HttpOnly`, `SameSite=Lax`,
  `Secure` by default; a 12-hour absolute lifetime as well as an idle
  timeout; server-side storage, so signing out and disabling an account
  take effect immediately rather than at expiry.
- **Timing.** Constant-time comparison for API tokens and CSRF tokens.
  The sign-in form verifies a password even for an address with no
  account, so response time is not a membership oracle.
- **Log injection and secrets in logs.** Logs are JSON, which escapes
  newlines; no credential is logged, and the access log deliberately
  omits query strings.
- **Path traversal.** Static assets are served from an exact map
  lookup, never a filesystem path built from a URL.
- **Header injection.** No request-derived value reaches a response
  header. CORS reflects an origin only after an exact allowlist match,
  sets `Vary: Origin`, and never sets
  `Access-Control-Allow-Credentials`.
- **Supply chain.** The one vendored browser file (htmx) is pinned by
  SHA-256 and asserted by a test.
- **Resource limits.** API `limit` and `offset` are bounded; the
  beacon's ingest body is capped and rate limited; the developer
  password is throttled; login is throttled per account and per address.

### Open, and stated on the pages that are affected

- **Changing a password does not end sessions on other devices.** The
  session store has no user column, so finding them is a table scan
  today. The account page says so rather than implying otherwise.
- **There are no recovery codes.** Somebody who loses their
  authenticator is recovered by an owner or the operator resetting their
  second factor. A sole owner who loses theirs needs shell access. Said
  on the enrolment page and the code form.
- **The panel has no global concurrency limit.** Each sign-in attempt
  costs one argon2id verification, bounded by the throttle counters
  rather than by a queue. The panel binds `127.0.0.1` by default and is
  meant to run behind a proxy that terminates TLS.

## Running the checks

```bash
go test -race ./...                              # unit
go test -tags integration ./...                  # against a real database
CA_BROWSER_TEST=1 go test -tags integration \
    ./internal/panel/...                         # against real Chromium
go vet ./... && go vet -tags "integration loadtest" ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

The browser suite is not decoration: it is what catches a
Content-Security-Policy refusing a resource, and it has found three
defects that every HTTP-level test reported as a healthy 200.

`go vet` is run under every build tag because a test file behind a tag
does not compile in the untagged build, and a suite can rot against an
API that changed months ago with nothing saying a word. That has
happened here once.

## Deployment expectations

This software does not defend against a hostile machine. It assumes:

- The database is not reachable from the internet.
- The panel is behind TLS. `secure_cookies` defaults to true and only
  exists so the panel can be exercised over `http://localhost`.
- The config files are readable only by the service user. They hold the
  database password and the IP hash key.
- The log directory is `0700`. Log lines carry addresses and browser
  details, which is personal data; the setup checks warn when it is not.
