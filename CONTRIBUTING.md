# Contributing

Maintained by **Fırat Coşkun** (kettipcimm@gmail.com), who is who to ask
when something below is unclear or looks wrong. Security reports go to
the same address and not to a public issue — `SECURITY.md` says why.

## Before anything else: the licence agreement

Contributions require agreeing to `CLA.md`. Read it — particularly the
first section, which says plainly why it exists, including the reason
most projects leave out: it keeps the project's future licensing
decisions possible.

To sign, add a row to `CLA-SIGNATURES.md` in your first pull request and
say in the pull request that you have read and agree. Once is enough.

If you would rather your work could never appear in a version offered
under commercial terms, please do not sign. That is a reasonable position
and an unsigned pull request will simply be declined rather than argued
with.

## What this project asks of a change

More than most, and it is better to know before you start than after.

**A change is not finished when it works.** It is finished when:

1. It has tests, and `go test -race ./...` is clean.
2. It has been verified against the real dependency — a real PostgreSQL,
   a real headless browser, real concurrent load — and not against a
   fake. `internal/testdb` exists because a suite whose fixture was more
   privileged than production hid three real bugs; its package comment
   lists them.
3. Every check it adds has been mutation-tested: break the thing on
   purpose, watch the test go red, put it back. A test that stays green
   when the code is broken is worse than no test, because it is believed.
4. The reasoning is written down — in the code where a reader would ask
   "why like this", and in `NOTES.md` when the answer took measuring.

**Comments say why, not what.** The code already says what. A comment
that describes a hole that does not exist teaches readers to disbelieve
the ones about holes that do.

**Measure, do not assume.** Almost every defect this project has found
was found by running something rather than by reading it. When a comment
in this codebase states a number, that number was measured.

## Running the tests

The unit suite needs nothing:

```
go test -race ./...
```

The integration suite needs a PostgreSQL with the schema applied and the
five roles created, which `release/install.sh` sets up. Each suite
connects as the role its production code uses:

```
export CA_SUPERUSER_DSN="postgres://postgres@localhost:5432/analytics"
go test -tags integration ./...
```

`go test` caches results, and its cache cannot see a change you made to
the database. Use `-count=1` whenever the thing you changed is database
state.

## The gate

What CI runs, and what a pull request has to pass:

```
gofmt -l .
go build ./... && go vet ./...
go test -count=1 -race ./...
CA_BROWSER_TEST=1 go test -tags integration -race -count=1 ./...
for tag in loadtest network release e2e docker; do go vet -tags "$tag" ./...; done
go test -tags release -count=1 ./release/
gosec    ... | go run ./internal/sast/cmd/sastdiff    -report gosec.json
deadcode ... | go run ./internal/sast/cmd/deadcodediff -report deadcode.txt
```

**Copy the integration line exactly.** It used to be written here without
`-race` and without the browser flag, while CI ran both — so a change
could be gated locally against a weaker command than the one that
decides, and "the gate is green" meant two different things depending on
who said it.

The last two compare against committed baselines rather than failing on
every finding. Both have their own file explaining what to do when they
go red; neither is a list to silence something in.

## Things that will not be accepted

From `PLAN.md`'s permanent rules, and they are not negotiable:

- A "run SQL" box, a free-text configuration field, a shell command, or a
  restart that takes a command string.
- Any operation whose parameter is code, a query, a file path, or a
  hostname the deployment will connect to.
- A panel that can reach the analytics tables. The panel's database role
  holds no privilege on them, deliberately, and no feature is worth
  changing that.
- A test fixture more privileged than production.

## Releases

`VERSIONING.md` has the scheme and, more usefully, when a version is cut:
**on a completed phase, not on a commit**. `CHANGELOG.md` is the record,
and every entry has to answer the question a release note is actually
read for - whether the person installing it has to do anything.

Two numbers, and conflating them is the easy mistake: the build version
(`v0.9.0+L3`) is what people say out loud, and the schema version
(`internal/schemaver.Version`, an integer) is what the upgrade machinery
in L1-L3 acts on. They move independently.

## Documents

`PLAN.md`, `NOTES.md`, `SOZLUK.md`, `KURULUM.md`, `VERSIONING.md` and
`CHANGELOG.md` are Turkish;
`README.md`, the code and its comments are English. Several tests in
`internal/docs` check that the Turkish files are not corrupted and that
every document reference resolves, so a link to a file that does not
exist fails the build rather than the reader.
