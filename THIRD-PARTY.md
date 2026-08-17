# Third-party code and data

Crucible Analytic is MIT-licensed (see `LICENSE`). This file records
what else is inside a build, and under what terms, so that anyone who
clones, forks or redistributes this repository knows exactly what they
are carrying.

## The rule about data

**This repository redistributes no third-party dataset.**

That is a deliberate constraint, not a description of how things happen
to be. A permissively licensed repository that also carries somebody
else's data under unstated terms passes that uncertainty to everyone who
clones it: the licence file says "help yourself", and the data says
nothing at all.

So the pattern is always the same — **ship the mechanism, not the
data**. Every dataset this software can use is retrieved by the
deployment, onto the deployment's own machine, under the source's own
terms. Nothing here is fetched at build time, and nothing is committed.

| Dataset | Used for | How it arrives | Source's licence |
|---|---|---|---|
| Known-bot JA4 fingerprints | Labelling automation tools | `collector -update-bot-data` (see `internal/botdata`) | The source's own; check before relying on it commercially |
| IP → country ranges | Country attribution | `asn_lookup.enabled = true`, or a local CSV | [PDDL 1.0](https://opendatacommons.org/licenses/pddl/1.0/) — public domain, no attribution required |
| IP → ASN ranges | Network attribution | as above | PDDL 1.0 |

Both range datasets come from [ipinfo.io's free
downloads](https://ipinfo.io/products/free-ip-database), which are PDDL
— effectively public domain. They are still fetched rather than
committed, because they are large, they age, and a stale copy in a
repository is a copy somebody eventually trusts.

**Not fetching anything is a supported state.** With no known-bot file
the bot-fingerprint signal is simply absent and every other signal still
works; with `asn_lookup.enabled = false` (the default) there is no
country or ASN attribution. Each service says so at startup, and the
setup wizard reports it, so nothing goes quietly missing.

## Vendored code

One third-party file is committed, because it is served to browsers and
this project has no build step:

| File | Version | Licence |
|---|---|---|
| `internal/panel/ui/static/htmx.min.js` | 2.0.10 | [0BSD](https://github.com/bigskysoftware/htmx/blob/master/LICENSE) |

0BSD grants use without conditions — no notice needs to be preserved.
It is recorded here anyway, and its SHA-256 is asserted by a test, so a
change to that file cannot land unnoticed. See
`internal/panel/ui/static/VENDOR.md`.

## Go dependencies

Every direct dependency, from `go.mod`:

| Module | Licence |
|---|---|
| `github.com/BurntSushi/toml` | MIT |
| `github.com/jackc/pgx/v5` | MIT |
| `github.com/alexedwards/scs/v2`, `.../scs/pgxstore` | MIT |
| `github.com/pquerna/otp` | Apache-2.0 |
| `golang.org/x/crypto` | BSD-3-Clause |
| `golang.org/x/term` | BSD-3-Clause |
| `golang.org/x/text` | BSD-3-Clause |

Indirect dependencies (`github.com/boombuler/barcode`,
`github.com/jackc/pgpassfile`, `github.com/jackc/pgservicefile`,
`github.com/jackc/puddle/v2`, `golang.org/x/sync`, `golang.org/x/sys`)
are MIT or BSD-3-Clause.

All of these are permissive and compatible with redistributing this
project under MIT. None is copyleft, so no obligation propagates to
anyone who forks this.

## What MIT does and does not give you

It gives anyone the right to use, modify, sell and redistribute this
code, including in closed-source products, provided the copyright notice
travels with it.

It does **not** grant rights to the name "Crucible Analytic" or to
CrucibleLAB's branding. A fork is expected to carry its own name.

It also carries no warranty. This software is under active development;
see `PLAN.md` for what is finished and what is not.
