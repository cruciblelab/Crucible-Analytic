# Third-party code and data

Crucible Analytic is licensed under Apache-2.0 (see `LICENSE` and
`NOTICE`). This file records
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
project under Apache-2.0. None is copyleft, so no obligation propagates
to anyone who forks this.

## What Apache-2.0 does and does not give you

It gives anyone the right to use, modify, sell and redistribute this
code, including in closed-source products, and including running it as a
paid service. There is no clause here that stops a competitor doing
that, and that is a decision rather than an oversight: the people this
is built for are the agencies and developers who install it for a
customer, and a licence their client's lawyer has to think about is
friction aimed at the wrong person.

What it asks in return is **attribution that survives redistribution**.
This is where Apache-2.0 is stricter than MIT, and why the project is
under it: section 4 requires a redistributor to keep the licence, state
what they changed, and carry the `NOTICE` file's attribution text. MIT
asks only that a copyright line travel with the source. Running the
software as a service is not redistribution and requires none of this.

It does **not** grant rights to the name "Crucible Analytic" or to
CrucibleLAB's branding - section 6 is explicit that trademark rights are
not granted. A fork is expected to carry its own name.

It carries **no warranty and no liability**, in sections 7 and 8, in
considerably more detail than MIT's single sentence. This software is
under active development and sits in the traffic path of a live site;
see `PLAN.md` for what is finished and what is not.

## What is in a release, and what is never in one

The licence covers code. What this repository and its releases contain
is source code and nothing else:

| Not distributed | Why |
|---|---|
| Collected analytics, database dumps | A deployment's own data, and it describes real visitors |
| Logs | Same, and access logs contain IP addresses |
| Build output, binaries | Reproducible from source; a binary nobody can check is not a contribution |
| Third-party datasets | The rule at the top of this file: ship the mechanism, not the data |
| Config files | They carry the database password, the IP hash key and the developer password's hash |

`.gitignore` enforces this rather than trusting it, with one exception
it names: anything under `testdata/` is source however it is spelled.
