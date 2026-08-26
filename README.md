# Crucible Analytics — MVP Collector

A bot-aware web analytics collector: an open-source alternative to
Google Analytics that separately detects and surfaces bot/DDoS traffic.

The pipeline is `Collector → Cache/Score → TimescaleDB → read-only JSON
API → panel`. The panel (`cmd/panel`) serves a site dashboard of its own
now; path-scanning detection is still a later phase. The API stays a
public contract either way — it is built to be consumed by a panel you
already have, and the one in this repository consumes it exactly as an
outside tool would.

**Installing this on a server: [`KURULUM.md`](KURULUM.md).** A fixed,
step-by-step guide, in Turkish, covering everything from building the
binaries through the database roles, the secrets, the service files and
the first sign-in — plus the recommendations and the mistakes that cost
time. The "Running locally" section further down is for development on
one machine; it is not an installation procedure.

## What this does — and doesn't do

**This project does:**
- Passively observe traffic and separate bot/human activity using JA4 TLS
  fingerprinting plus behavioral signals (request rate), without blocking or
  delaying anything based on that assessment. (The one, unrelated exception:
  an optional, operator-configured country/ASN denylist - see the next
  section - which blocks by explicit policy, not by the bot-likelihood score
  this bullet is about.)
- Surface bot/scraper traffic transparently, alongside a 0-100 score and the
  reason behind it (known-bad JA4 match, request rate, known-bad ASN, or any
  combination).
- In full mode, record analytics **per individual HTTP request** rather than
  per TCP connection - an accurate request count even across HTTP/1.1
  keep-alive or HTTP/2 multiplexed connections (see "Full mode" below). The
  collector itself never breaks requests down by page/path; that comes from
  the beacon below instead.
- Collect **page-level analytics from a client-side JavaScript beacon**
  (`cmd/beacon`) - pages, referrers, campaigns, browsers, devices and custom
  events, with cookieless visitor identification. This is a second, separate
  data source that complements the collector rather than duplicating it, and
  the two join on IP - see "Client-side beacon" below.
- Persist the score and underlying data to TimescaleDB, a real
  Postgres-compatible database your own systems can query directly.
- Serve **both** sources back over a read-only, token-authenticated JSON
  API (`cmd/analytics-api`), so an external management panel can pull each
  site's statistics over HTTP without touching the database - 28
  endpoints, including the cross-source ones that answer "which of the
  addresses that hit us actually rendered a page". See "Read-only
  analytics API" below. That API is the contract, not an internal
  detail: the panel in this repository is one of its callers and has no
  privileged path around it, which is what keeps it usable by a panel
  you wrote yourself.
- Serve a **management panel** (`cmd/panel`) — sign-in, two-factor,
  members, a setup wizard, and a per-site dashboard — as a separate
  process with a database role that cannot read a single analytics row.
  See "The management panel" below.

**This project does not:**
- **Stop network-level (volumetric) DDoS attacks.** Traffic in the
  hundreds-of-thousands-to-millions-of-packets-per-second range needs
  globally distributed infrastructure (Anycast, multiple scrubbing data
  centers) to absorb - that isn't something a single process on a single
  server can do, by design or otherwise.
- **Act as a general-purpose anti-DDoS service or WAF on its own.** It
  detects and scores traffic without blocking or dropping it on the basis
  of that score - the one exception is an optional, narrow,
  operator-configured country/ASN denylist (off by default; see "Optional:
  IP → country / ASN lookup"), not a general blocking/mitigation engine.
  Any blocking decision beyond that is left to your own system - a WAF, a
  firewall, the backend application itself.
- **Replace a CDN or a dedicated anti-DDoS service.** See "Recommended
  deployment order" below for how this fits alongside one.

## Recommended deployment order

If you already run a CDN or anti-DDoS service (Cloudflare or similar), put
it in front of this collector, not behind it:

```
Client → CDN / anti-DDoS service (filters bulk/volumetric traffic)
       → This collector (passthrough or full mode - fine-grained, behavioral analysis)
       → Backend (your actual site)
```

This order matters for two concrete reasons:

1. **Putting the collector in front of the CDN defeats both layers at once.**
   Raw, unfiltered traffic - including anything volumetric - would hit the
   collector directly instead of being absorbed upstream, and the CDN would
   never see the traffic it's meant to filter (it only sees what the
   collector forwards to it). It can't protect what it never receives.
2. **Behind a CDN, the JA4 fingerprint may belong to the CDN, not the
   original client.** If the CDN terminates TLS and opens its own connection
   to your origin, every request reaching the collector can carry the *CDN's*
   TLS fingerprint rather than the original visitor's. This is a known
   limitation of running behind any TLS-terminating intermediary, not
   something this collector works around - weigh JA4-based signals
   accordingly in that setup, and lean more on request-rate behavior.

If you don't have a CDN in front (this collector is directly
internet-facing), the `[limits]` section in `config.example.toml` gives the
collector itself some bounded resilience against being overwhelmed - an
`overload_policy` of `fail_open` (default), `fail_closed`, or `throttle` once
configured concurrency/rate ceilings are exceeded. This is **not** a
substitute for CDN or scrubbing-center-level protection: it only keeps the
collector process itself from becoming a resource-exhaustion target, and
does nothing to absorb volumetric traffic before it reaches your network.

## What it does

The collector runs in one of two modes, selected by `mode` in the config
file. Both fingerprint every connection via JA4, feed the same in-memory
`RateStore` (a cheap two-counter sliding window, not a full request log),
compute the same 0-100 bot-likelihood score (request rate + known-bad-JA4
match), and batch-write summaries to TimescaleDB via `COPY` every
`flush_interval_seconds` (default 10s) - optionally enriched with each
IP's country/ASN at the same time, if `asn_lookup.enabled = true` (see
"Optional: IP → country / ASN lookup" below). IP state idle for longer
than `ttl_seconds` (default 5m) is automatically dropped from memory.
Nothing in this phase blocks or rejects traffic based on the *score* -
it's computed and persisted for a future phase to act on. The one
exception is an optional country/ASN denylist (`asn_lookup.blocked_countries`/
`blocked_asns`), which is unrelated to scoring: it's a deliberate,
operator-configured block, not a behavioral judgment the collector makes
on its own.

### Passthrough mode (default)

A minimal, content-blind TCP/TLS reverse proxy (`internal/proxy`):

1. It listens on `listen_addr` and accepts raw TCP connections.
2. For TLS connections, it peeks the TLS record(s) containing the
   ClientHello, parses it just enough to compute a
   [JA4](https://github.com/FoxIO-LLC/ja4) client fingerprint, and forwards
   every byte read (and everything that follows) to `backend_addr`
   **unmodified**. It never terminates TLS, never buffers or rewrites
   application data, and never drops or delays a connection because
   fingerprinting failed - observation is best-effort and side-channel to
   the proxying.
3. Each connection is recorded as one request in the `RateStore` - see the
   rate-counting caveat below.

**Rate counting is per TCP connection, not per HTTP request - and this is
permanent, not a to-do.** Because the proxy never terminates TLS, the byte
stream after the ClientHello is opaque ciphertext to it; there is no way to
see individual request lines multiplexed over a keep-alive or HTTP/2
connection without decrypting, which this mode deliberately doesn't do.
This under-counts legitimate browser traffic relative to bots that churn
through a new connection per request, which is directionally fine for bot
detection but worth knowing when interpreting the numbers. **Full mode
below exists specifically to fix this**, at the cost of a larger trust
boundary.

### Full mode

A TLS-terminating HTTP reverse proxy (`internal/fullproxy`, `mode = "full"`),
built on `net/http` + `httputil.ReverseProxy`:

1. It terminates client TLS connections using `tls.cert_file`/`key_file`
   (the backend's real certificate/key - full mode needs it, since it's
   now the actual TLS endpoint) and reverse-proxies each request to
   `backend_addr` over **plaintext HTTP** (the standard "TLS terminates at
   the edge" setup; an HTTPS backend isn't supported yet).
2. It still computes JA4 for every connection, from the *same*
   `ja4.ParseClientHello`/`Fingerprint` passthrough mode uses - the raw
   ClientHello bytes are captured by snooping the connection before
   crypto/tls consumes them (`tls.ClientHelloInfo` itself only exposes a
   parsed subset of fields, not the raw bytes JA4 needs), via the
   `GetConfigForClient` hook and `tls.Conn.NetConn()`. This guarantees
   identical JA4 output to passthrough mode for the same ClientHello,
   rather than a second, separately-validated implementation.
3. **Each real HTTP request is recorded individually** - not once per
   connection. `net/http` itself resolves both HTTP/1.1 keep-alive and
   HTTP/2 multiplexing into separate handler invocations, so this falls
   out for free rather than needing custom request-boundary detection.
   This is what full mode is *for*.

Full mode's `http.Transport` to the backend explicitly sets
`MaxIdleConnsPerHost: 100` (the zero-value default is 2, a well-known cause
of high CPU / low throughput in Go reverse proxies fronting a single
backend host under concurrent load - constant redial instead of connection
reuse) along with matching `MaxIdleConns`/`IdleConnTimeout`.

## Optional: IP → country / ASN lookup

Disabled by default (`asn_lookup.enabled = false`). When enabled,
`internal/asnlookup` resolves a request's IP (IPv4 *and* IPv6) to both the
country it's registered to and the ASN that routes it (number + org name,
e.g. `15169` / `GOOGLE`), using two [sapics/ip-location-db](https://github.com/sapics/ip-location-db)
datasets - `user-country` and `origin-asn` - free, [PDDL](https://opendatacommons.org/licenses/pddl/1.0/)-licensed
(no attribution required, though we're glad to credit it), updated daily,
compiled from RIR delegated-stats, BGP routing archives (RouteViews / RIPE
RIS), and RFC 8805/9632 geofeeds. Huge thanks to sapics and the
organizations behind those underlying sources.

Four CSVs total - two datasets x two address families
(`user-country-ipv4.csv`/`-ipv6.csv`, three columns each; `origin-asn-ipv4.csv`/`-ipv6.csv`,
four columns each) - are fetched from GitHub Releases on a schedule
(`asn_lookup.refresh_interval_seconds`, default weekly) - or, if
`asn_lookup.local_csv_path` is set, all four are read from that directory
on local disk instead, with **no network access of any kind** in that mode
(useful for an offline VDS, or if you'd rather manage the download
yourself, e.g. via your own cron job writing into that directory). Either
way, lookups are then answered locally against four independent in-memory
range tables - country x {v4,v6}, ASN x {v4,v6} - also mirrored to
TimescaleDB for durability across restarts, never a live network call, or
in the common case a database call either, on the request path. Country
and ASN are resolved independently per lookup: an IP can be found in one
dataset and not the other, and `Result.Found` is true if *either*
succeeds, since the two datasets are sourced separately with their own
unrelated range boundaries - see the package doc comment in
`internal/asnlookup/asnlookup.go` for why they're kept as four separate
tables rather than merged into one. An in-memory LRU+TTL cache
(`asn_lookup.cache_max_entries`/`cache_ttl_seconds`, default 50,000
entries / 6 hours) sits in front of that for repeat IPs.

`Result.ASN` and `Result.ASNName` are real now, not placeholders - both
datasets are fetched, parsed, and queried independently, the same way
`Result.Country` always has been.

**Every `traffic_snapshots` row is enriched with them, automatically,
whenever `asn_lookup.enabled = true`** - no separate flag for this: it's
the same lookup already running for `asn_lookup.enabled`'s own sake, just
also consulted once per active IP at flush time (`internal/storage.Flusher`
takes an optional `GeoResolver`; `cmd/collector/main.go` passes the same
`*asnlookup.Resolver` in when the feature is on, `nil` when it's off - no
new per-request work, since it's the flush-time snapshot being enriched,
not the request path). Three columns, `country`/`asn`/`asn_org`, each
independently best-effort the same way `Result`'s own fields are: `''`/`0`
means that half wasn't resolved for this IP, not that it was checked and
came back empty. `internal/storage/schema.sql` is self-migrating for
this - it runs `ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS`
for the three new columns, so re-applying it against a table created
before this feature existed just adds them in place (verified directly
against a live database with existing rows - they keep their other
columns' values and simply get `''`/`0`/`''` for the three new ones,
no drop needed).

**`asn_lookup.blocked_countries` / `asn_lookup.blocked_asns` block traffic
by country/ASN**, checked once per connection (passthrough mode) or once
per HTTP request (full mode) - the resolved country or ASN is compared
against a `limiter.GeoBlocklist` before `internal/limiter`'s own
concurrency/rate admission runs, and a match is an **unconditional
reject regardless of `limits.overload_policy`**: blocking by
geography/ASN is a deliberate security decision, not
collector-load-shedding behavior, so it isn't subject to `fail_open`'s
"never block a legitimate site" guarantee the way an ordinary overload
rejection is. A geo-blocked connection never dials the backend and is
never recorded - passthrough closes the TCP connection outright; full
mode returns `403 Forbidden`, distinct from the `503 Service Unavailable`
an ordinary overload rejection returns, so the two reasons a request was
refused stay distinguishable in logs and responses. Both lists empty
(the default) means no blocking - and, importantly, no `Resolve()` call
added to the request path either: `cmd/collector/main.go` only wires a
resolver into the proxy at all when at least one blocklist entry is
configured, so enabling `asn_lookup` for storage enrichment alone (the
paragraph above) still costs nothing extra per request. Country codes
are case-insensitive. This is a flat denylist by design, not a richer
per-rule-policy engine (e.g. "always allow ASN X regardless of load",
"throttle country Y at N req/s") - see `NOTES.md` for that deferred
design and the open questions a future version of it would need to
resolve.

**`asn_lookup.apply_to_scoring` / `asn_lookup.known_bot_asns` add a real
ASN scoring signal**, mirroring `KnownBotJA4`'s existing flat-bonus
shape: when `apply_to_scoring = true`, a resolved ASN matching
`known_bot_asns` adds `scoring.maxASNScore` (20 points, weighted below
JA4's 30 - an ASN match is a weaker, more circumstantial signal than a
specific fingerprint match, since plenty of legitimate traffic also
originates from cloud/hosting ASNs) to the 0-100 score, alongside the
existing rate and JA4 components (all three sum, capped at 100). This
costs no extra lookup: `storage.BuildRows` already resolves each
snapshot's ASN for storage enrichment (the paragraph above), and simply
reuses that same value for scoring. `false` (the default) means
`internal/scoring` never even sees an ASN or a known-bot-ASN set -
`scoring.Score`'s ASN component is then always 0, byte-for-byte the same
behavior as before this existed. `known_bot_asns` is a *separate* list
from `blocked_asns`: a blocked ASN is rejected before it ever reaches
scoring, so reusing that list here wouldn't do anything - block and
"flag as more suspicious but still let through" are different actions,
kept as different config. `traffic_snapshots` gets one more column for
this, `is_known_bot_asn` (mirrors `is_known_bot_ja4`), the same
self-migrating `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` pattern as
`country`/`asn`/`asn_org`.

This completes the original four-phase plan for country/ASN
intelligence: resolve it, record it, block by it, and score by it are
all real now. What's left deliberately deferred is the richer
per-rule-policy blocking engine described in `NOTES.md` - a flat
denylist and a flat scoring bonus, not per-rule tuning.

Enabling this requires applying `internal/asnlookup/schema.sql` once,
manually, first - see "Running locally" below; like the rest of this
collector, it never runs DDL itself. It now creates **two** tables,
`ip_country_ranges` and `ip_asn_ranges` - if you already applied an
earlier version of this file that only had `ip_country_ranges` (`INET`
columns, IPv4+IPv6), just re-apply it: every statement is `CREATE
TABLE`/`CREATE INDEX ... IF NOT EXISTS`, so this only adds `ip_asn_ranges`
alongside the existing table, no drop needed (verified directly against a
live database with pre-existing data in `ip_country_ranges` - it's left
untouched). **If you're instead still on the very first version**
(`start_addr`/`end_addr` as `BIGINT`, IPv4 only, country only), drop
`ip_country_ranges` and recreate both tables - `BIGINT` can't hold an
IPv6 address, which isn't an in-place-compatible change. This feature is
disabled by default and was only added recently, so there's no expected
production data to migrate either way.

## Read-only analytics API

`cmd/analytics-api` serves each site's statistics as JSON over HTTP, so a
management panel can pull them without holding database credentials. It's
a **separate binary from the collector**, deliberately:

- One server may run several collectors (one per site) but needs only one
  API.
- The collector sits in the traffic path; the API shouldn't have to.
- Giving the API a **read-only Postgres role** is only meaningful if it
  isn't sharing a process with either writer - the collector or the
  beacon. Every query it issues is a `SELECT`;
  `analytics-api.example.toml` includes the `CREATE ROLE` / `GRANT
  SELECT` snippet to set that up. Grant it on **both** tables: the
  `/beacon/` endpoints read `beacon_events` and the `/crossover/` ones
  join it to `traffic_snapshots`.

### Multi-site: `site_id`

Every collector stamps its configured `site_id` onto every row it writes,
so one database can hold several sites' data - the case where one server
hosts more than one customer site, each with its own collector process.
`site_id` is **required** and restricted to `[a-zA-Z0-9_-]{1,64}`, since
it's also the path segment a site is exposed under. It's required rather
than defaulted because an unset one would silently commingle two sites'
rows the moment a second collector pointed at the same database - a
data-integrity problem you'd notice long after the fact.

#### Subdomains are whatever you say they are, once

`site.com` and `blog.site.com` are one site if you put the same `site_id`
in both snippets, and two sites if you do not. Nothing in the product
decides this for you, and the wizard's Sites step says so, because it is
the one decision in an installation that **cannot be revised later**.

A visitor id is `HMAC(daily_salt, site_id ‖ ip ‖ user_agent)`. The site
id is inside the hash, so under two ids the same person browsing both is
two visitors, in the stored data, permanently. There is no id in common
to merge on afterwards - the salt has rotated and the hash does not
invert.

What *can* be added up afterwards is anything that is a count of events:
pageviews, sessions, custom events. What cannot is anything that counts
distinct people. So a deployment that splits subdomains and later wants
one number for "visitors to our site" has to accept that the number it
can compute is "visitors to each, summed", which is larger and is not
the same question.

The safe default, if nobody has an opinion: **one `site_id` for the whole
property**, and use the pages breakdown to see the subdomains separately.
That direction is recoverable; the other is not.

### Tokens

Callers authenticate with `Authorization: Bearer <token>`. The config
stores only each token's **SHA-256 hash**, never the token, so a leaked
config hands over no working credential. Generate a pair with:

```bash
analytics-api -hash-token
```

Each token lists which sites it may read. Use `["*"]` for your own
management panel (which needs every site on that server) and an explicit
list for anything else - especially a token handed to a customer, which
must only ever cover their own site. A token asking for a site outside its
grant gets `403`, and `GET /api/v1/sites` returns only what that token can
see, so other customers' site names don't leak.

The tokens are bearer credentials: **terminate TLS in front of this API**.
It binds to `127.0.0.1:8080` by default so that's a deliberate decision
rather than an accident.

### Endpoints

All are `GET` and return JSON. Common query parameters:

| Parameter | Applies to | Default | Meaning |
| --- | --- | --- | --- |
| `from` / `to` | everything except `/healthz` | last 24 hours | RFC 3339 timestamps; the range is capped at 90 days. |
| `bot_score_min` | anything with a bot/human split, and the `/crossover/` endpoints | `50` | 0-100 cutoff at or above which an IP counts as a bot. **Rejected with a 400 on `/beacon/` endpoints**, which have no such column - see "Client-side endpoints" below. |
| `bots` | the `/beacon/` endpoints | `exclude` | `exclude`, `include` or `only`, selecting whether events from self-identified bot user agents are counted. Echoed back in every response. |
| `limit` / `offset` | the list endpoints | `50` / `0` | Page size (max 1000) and offset (max 100,000). Those responses also carry `total`, so a UI can render "showing 51-100 of 1,234". |
| `interval` | either `timeseries` | `1 hour` | One of `1 minute`, `5 minutes`, `15 minutes`, `1 hour`, `6 hours`, `1 day`, `1 week`. |

#### Collector-side (`traffic_snapshots`)

| Endpoint | Returns |
| --- | --- |
| `/healthz` | Liveness only, no data - the one route needing no token. |
| `/api/v1/sites` | Site IDs this token may read. |
| `/api/v1/overview` | Every site the token can read, each with its headline numbers, in **one** request - built for a panel's landing page so it doesn't have to fan out per customer. |
| `/api/v1/sites/{site}/summary` | Unique IPs, bot/human split, peak+avg request rate, busiest-window request count. |
| `/api/v1/sites/{site}/timeseries` | Unique IPs, bot IPs and rates bucketed over time, for charts. |
| `/api/v1/sites/{site}/top-ips` | Highest-scoring IPs with their country/ASN/JA4, known-bot flags and last-seen time. |
| `/api/v1/sites/{site}/ips/{ip}` | One IP's full detail plus its snapshot timeline - the drill-down behind a row in `top-ips`. Returns `found: false` (not 404) when that IP has no activity in the range, since that's an ordinary answer. |
| `/api/v1/sites/{site}/countries` | Distinct IPs and bot IPs per country. |
| `/api/v1/sites/{site}/asns` | Distinct IPs and bot IPs per ASN, with the organization name. |
| `/api/v1/sites/{site}/ja4` | Distinct IPs per **TLS fingerprint**, with the known-bot label resolved from `internal/scoring`'s embedded list - so a table can show "Googlebot" rather than a raw hash. Traffic with no usable fingerprint (plaintext HTTP, or an unparseable ClientHello) is grouped under an empty key flagged `empty`, rather than dropped, so the numbers still add up. |
| `/api/v1/sites/{site}/score-distribution` | Bot-score histogram in fixed 10-point bands. All ten bands are always present, including empty ones, so a chart needn't synthesise gaps. |
| `/api/v1/sites/{site}/snapshots` | Raw rows, newest first - for CSV export, or for checking where an aggregate number came from. |

#### Client-side (`beacon_events`)

These count **people and pageviews**, where the endpoints above count
addresses and connections. The two are not interchangeable and should
not be charted against each other: an IP is not a visitor, and a visitor
who never ran JavaScript is not here at all.

`bots` defaults to `exclude`, so a panel that passes nothing gets human
numbers. That default is a filter, not a fact - `exclude` drops
*self-identified* bots, which is every honest crawler and no dishonest
one. For the dishonest ones, see `/crossover/js-bots`.

| Endpoint | Returns |
| --- | --- |
| `/api/v1/beacon/sites` | Site IDs with beacon data, which is not necessarily the same list as `/api/v1/sites` - a site can have one collection process running and not the other. |
| `/api/v1/sites/{site}/beacon/summary` | Pageviews, custom events, visitors, sessions, bounce rate, pages per session, average session duration. |
| `/api/v1/sites/{site}/beacon/timeseries` | Pageviews, visitors and sessions bucketed over time. A session is counted in the bucket it *started* in, so the column never sums to more than the range's own total. |
| `/api/v1/sites/{site}/beacon/pages` | Pageviews and visitors per path. |
| `/api/v1/sites/{site}/beacon/titles` | Pageviews and visitors per page *title*. Its own dimension rather than a nicety: a shop owner recognizes "Kadın Spor Ayakkabı" and does not recognize `/c/1042?v=3`, and one page can carry several paths that all mean the same thing to them. |
| `/api/v1/sites/{site}/beacon/entry-pages` | Which pages sessions began on - the landing pages acquisition actually reaches, usually a more actionable list than the most-viewed pages. |
| `/api/v1/sites/{site}/beacon/exit-pages` | Which pages sessions ended on. "Ended" means "last page with a recorded event", so a session still in progress at `to` lands here too. |
| `/api/v1/sites/{site}/beacon/referrers` | Traffic per referring host. Same-origin referrers are dropped in the browser and never stored, so the empty group is genuinely "direct or unknown", not internal navigation. |
| `/api/v1/sites/{site}/beacon/campaigns` | Traffic per *exact* campaign combination, with the stored query string decoded into its individual parameters. Answers "which precise campaign link performed best"; for "how much did this source bring in total", use the per-dimension routes below. Only visits that carried at least one parameter are counted. |
| `/api/v1/sites/{site}/beacon/utm-sources` | Traffic per `utm_source` **alone**, so one source spread across five campaigns is one row rather than five. The empty group is traffic that carried no campaign at all, which is most of it. |
| `/api/v1/sites/{site}/beacon/utm-mediums` | Traffic per `utm_medium` alone (`social`, `email`, `cpc`). |
| `/api/v1/sites/{site}/beacon/utm-campaigns` | Traffic per `utm_campaign` alone. |
| `/api/v1/sites/{site}/beacon/utm-terms` | Traffic per `utm_term` alone. Empty for every deployment that sets `campaign.drop_params = ["utm_term"]`. |
| `/api/v1/sites/{site}/beacon/utm-contents` | Traffic per `utm_content` alone - the A/B variant dimension. |
| `/api/v1/sites/{site}/beacon/refs` | Traffic per `ref`, the informal equivalent of `utm_source` used by sites that never adopted UTM. |
| `/api/v1/sites/{site}/beacon/click-sources` | Paid traffic per ad network (`google`/`facebook`/`microsoft`). The click identifier itself is never a grouping key: it is unique per click, so grouping by it would return one row per visit. |
| `/api/v1/sites/{site}/beacon/browsers` | Traffic per browser. |
| `/api/v1/sites/{site}/beacon/operating-systems` | Traffic per OS. |
| `/api/v1/sites/{site}/beacon/devices` | Traffic per form factor (`desktop`/`mobile`/`tablet`). Bots have no form factor and fall into the empty group. |
| `/api/v1/sites/{site}/beacon/languages` | Traffic per browser language. |
| `/api/v1/sites/{site}/beacon/countries` | Traffic per country, taken from the beacon's own column where it has one and otherwise **recovered by joining `traffic_snapshots` on IP**. That fallback is the normal path: the recommended deployment leaves the beacon's own geo lookup off, and without the join it would return one large empty group. |
| `/api/v1/sites/{site}/beacon/events` | Named custom events with their counts and how many distinct people raised them - which separates "300 clicks from one person" from "300 people clicked once". |
| `/api/v1/sites/{site}/beacon/raw` | Raw stored events, newest first, for export. |

#### Cross-source (both tables)

The endpoints no single-source tool can offer, and the reason both
processes write to one database. None takes a `bots` filter: the
question is whether *anything* from an address executed JavaScript, and
a headless browser that ran the snippet really did run it whatever its
User-Agent claims.

| Endpoint | Returns |
| --- | --- |
| `/api/v1/sites/{site}/crossover/summary` | How much of the traffic that reached the site actually rendered a page: addresses seen, how many ran JavaScript, and the same split per bot-score band. The expected shape is a downward slope; a high-score band with high coverage is automation sophisticated enough to render pages. Also reports `beacon_only_ips`, which should be 0 - anything else means the collector isn't really in the path, or the beacon's `trusted_proxies` is wrong. |
| `/api/v1/sites/{site}/crossover/silent-ips` | The addresses the collector saw that never sent a beacon event, most suspicious first. This is the population a conventional analytics tool reports as not existing. |
| `/api/v1/sites/{site}/crossover/js-bots` | Addresses that ran the snippet **and** either self-identified as a bot or were scored above `bot_score_min` by the collector. A headless browser looks like an ordinary visitor in client-side data alone; what gives it away is the other source - a JA4 that doesn't match the browser it claims to be, a request rate no human produces, a datacentre ASN. |

### What this API cannot tell you

Worth stating plainly before a UI is designed around it, because these are
limits of what the collector *records*, not of the API: there is **no
page/path breakdown, no referrer, no user agent, and no session or
pageview concept** in `traffic_snapshots`. The collector observes
connections and requests at the IP/TLS level and never inspects URLs or
headers, so those simply don't exist in that table.

That gap is what the client-side beacon fills, from the other direction,
and the `/beacon/` endpoints above serve it. Two things remain genuinely
unanswerable by either source:

- **Which paths a non-JavaScript client requested.** The beacon reports
  paths from the browser, so a scanner probing `/wp-admin` and `/.env`
  never appears in them - it runs no script. Seeing that needs
  collector-side path collection, which is listed under "Explicitly out
  of scope" and discussed in `NOTES.md`.
- **A cumulative request total.** See "What the numbers mean" below.

What this API *does* give you that a conventional analytics tool doesn't
is the bot dimension on every collector-side view, plus the `/crossover/`
endpoints - "which of the addresses that hit us actually rendered a
page", which needs both sources and is exactly what a beacon-only tool
cannot see.

One caveat that applies to every collector-side visitor-ish number:
**a unique IP is not a unique visitor.** CGNAT - which every Turkish
mobile carrier uses - puts very many real people behind one address, and
dynamic reassignment splits one person across several addresses over
time. Neither is fixable at the IP layer. `visitors` from the `/beacon/`
endpoints is the number to use when you need it to mean people.

### What the numbers mean (and one thing they deliberately don't)

`traffic_snapshots` is a periodic *sample* of the collector's sliding
window, not a request log - so consecutive samples of the same IP overlap
and **must not be summed**. Every figure this API reports is exact by
construction: distinct-IP counts, max/avg over the sampled rates, and
`peak_window_requests`, which reads the collector's own window counters at
the single busiest flush ("your busiest minute saw N requests").

There is deliberately **no cumulative "total requests" figure**. An
earlier version computed one by integrating the sampled rate over time; it
passed a synthetic test with steady, evenly spaced data and was still
wrong on real traffic - a run that sent 38 requests reported 3, because
the rate averages over the 60s window while the integral multiplied by the
much shorter flush interval, and because bursty traffic makes flush
spacing irregular. It was removed rather than shipped with a caveat. See
`NOTES.md` for what an exact total would actually require.

## Client-side beacon

`cmd/beacon` is the project's **second data source**: a small JavaScript
snippet embedded in the measured site, plus an ingest endpoint that writes
what it reports into a `beacon_events` table.

### Why two sources rather than one

The two see genuinely different populations, and each is blind where the
other sees:

| | Collector (`traffic_snapshots`) | Beacon (`beacon_events`) |
|---|---|---|
| Sees | Every connection that reaches the site | Only clients that execute JavaScript |
| Knows | IP, JA4 fingerprint, request rate, bot score, country/ASN | Page, referrer, campaign, title, screen, language, custom events |
| Bots | Sees and fingerprints them | Structurally near-blind to them |
| Visitor identity | IP only, which is not a person | Cookieless per-person ID |

This is exactly why conventional analytics tools systematically
under-report automated traffic: a beacon only fires for something that ran
a script. Running both against one database is what closes the gap.
`beacon_events.ip` is the join key, so questions neither table can answer
alone become one query - most usefully **"which of the IPs that hit us
actually ran JavaScript?"**, and its inverse, a client that both runs
JavaScript *and* self-identifies as a bot.

### Deployment

Serve it from the site's **own origin**. Point whatever terminates TLS for
the site at the beacon process for one path prefix:

```nginx
location /_ca/ {
    proxy_pass http://127.0.0.1:8081;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

then embed (`beacon -snippet https://example.com mysite` prints this):

```html
<script defer src="https://example.com/_ca/ca.js" data-site="mysite"></script>
```

Same-origin keeps it out of the URL patterns content blockers match on,
and means no CORS is involved at all. A separate origin also works - the
snippet derives the endpoint from its own `src`, and CORS defaults to
allowing every origin, which is safe here because the endpoint is
write-only and its success response is an empty `204`.

**`trusted_proxies` is not optional behind a reverse proxy.** Everything
arrives from `127.0.0.1` there, so the real address only exists in
`X-Forwarded-For` - and that is just a request header anyone can set. The
beacon reads it only when the immediate peer is a network you listed. Set
too broadly, any client can choose its own IP, and with it its own
country, ASN and visitor ID.

### Privacy model

- **IP addresses are masked by default.** IPv4 keeps its /24, IPv6 its
  /64, in both `traffic_snapshots` and `beacon_events`. The masking
  happens when the row is built and as the *last* step: the whole address
  derives the visitor id and resolves country and ASN first, so masked
  mode costs neither of those. What it does cost is resolution in the
  crossover join - two visitors in one /24 become one row there - and the
  views that use it say so rather than quietly showing a smaller number.
  An unset config key, an unreadable settings row and an unset struct
  field all mean masked: the value nobody sets is the one that reaches
  production. Set `privacy.ip_storage = "full"` to keep whole addresses,
  which needs the developer password (below).
- **No cookies, and no consent banner needed.**
  `visitor_id = HMAC(daily_salt, site_id ‖ ip ‖ user_agent)`, the
  construction Plausible popularized. The salt is random, held only in
  memory, and replaced every 24 hours, so an old ID cannot be re-derived
  from an IP afterwards even by whoever holds the database. The cost,
  stated plainly: restarting the process mid-day counts one visitor twice.
- **Query strings are not stored raw.** Only `utm_*`, `ref`, `gclid`,
  `fbclid` and `msclkid` survive, re-serialized in sorted order. Real query
  strings routinely carry password-reset tokens, invite codes and email
  addresses, and an analytics table has a far wider audience than the
  application's own database. A referrer's query string is dropped
  entirely. The allowlist is adjustable per deployment - see `[campaign]`
  in `beacon.example.toml`.
- **Ad-network click identifiers are not stored by default.** A
  `gclid`/`fbclid`/`msclkid` is unique per click and resolvable to a person
  by the network that issued it (never by us), and its only legitimate use
  - uploading offline conversions back to that network - is something this
  project does not do. What is stored instead is `click_source`: that a
  Google, Meta or Microsoft ad click happened. That is the part worth
  analysing, and it identifies nobody. Set
  `campaign.store_click_ids = true` to keep the raw value.
- **Bot user agents are flagged, never dropped** (`is_bot_ua`). A client
  that runs JavaScript and admits to being a bot is the most interesting
  row in the table, not noise. Filter on the column when you want humans.

### Operational shape

- Its **own process and own database role.** It is the only component the
  whole internet may POST to, and it writes - so it shares neither the
  collector's traffic path nor the API's read-only role.
- **Events are buffered and written with `COPY`**, never inline, so a
  database hiccup cannot become latency on a visitor's page. A clean
  shutdown drains the buffer; a crash loses what was in it. Enqueue never
  blocks - a full buffer drops and says so in the logs.
- **`[limits]` works exactly as in the collector**, applied to beacon
  requests. `fail_open` means "accept the request, drop the event" here,
  since there is no backend behind this to protect.
- **`asn_lookup` is off by default and should stay off** when a collector
  runs on the same host: it already resolves country/ASN for every IP, and
  the beacon's geography can be recovered by joining on `ip` at no memory
  cost. Turning it on loads a second full copy of the range tables (~135 MB)
  into this process. When it *is* on, the beacon sets
  `SkipRangePersistence` - two processes rebuilding the shared
  `ip_country_ranges` / `ip_asn_ranges` tables on independent schedules
  would repeatedly destroy each other's data.

### The snippet

`internal/beacon/beacon.js`, served verbatim from the binary via
`go:embed` - what ships is what is in the file, with no build step. It is
**2.1 KB over the wire** (gzipped), the same size class as Umami and
Plausible. It is served with its comments intact rather than minified:
for a script site owners are right to be suspicious of, being readable in
a browser's view-source is worth more than the ~1 KB.

It sends an automatic pageview on load, follows SPA navigation by hooking
`history.pushState`/`replaceState` and `popstate`, and exposes
`crucible('event', 'name')` for custom events. Same-origin referrers are
dropped in the browser, before they are ever sent. Opt out on one browser
with `localStorage.setItem('crucible.disabled', '1')`.

## Design notes

- **Language: Go.** Goroutines suit the high-concurrency connection model,
  and it ships as a single static binary — deployment is "point it at your
  backend and run it."
- **JA4 parsing: hand-rolled, no dependency.** The ClientHello wire format
  is simple structurally (no crypto needed to parse it), so
  `internal/ja4` implements it directly against the public JA4 spec using
  only the standard library. This keeps the dependency footprint at zero
  for the most security-sensitive piece of the pipeline and gives full
  control over exactly which fields feed the fingerprint. It **has now
  been cross-validated against two independent implementations**: FoxIO's
  own reference (`python/ja4.py` in
  [FoxIO-LLC/ja4](https://github.com/FoxIO-LLC/ja4), the JA4 spec's
  original authors) and Wireshark/tshark's native JA4 dissector (a
  separate codebase) — both against real ClientHello bytes from FoxIO's
  official test pcaps. See `internal/ja4/testdata/README.md` for exact
  provenance. That process found and fixed two real bugs against FoxIO
  (an empty-hash-segment special case, and the exact ALPN-sanitization
  rule) — both now pinned by dedicated unit tests. It also surfaced a
  **known, unresolved disagreement between FoxIO and Wireshark themselves**
  on those same two edge cases (non-ASCII ALPN handling; the
  empty-JA4_c-input special case); this package follows FoxIO since it
  wrote the spec, and both fixtures are pinned with the discrepancy
  documented inline rather than papered over — see
  `TestFingerprint_WiresharkCrossValidation` in
  `internal/ja4/foxio_reference_test.go`. 5 fixtures pass end-to-end
  against both references (3 of which all three implementations agree on
  exactly). Only the "t" (TLS-over-TCP) transport is implemented;
  QUIC/DTLS fingerprints are out of scope since the collector never
  terminates QUIC.
- **Full mode's JA4 capture and HTTP/2 both came with real, non-obvious
  gotchas** found only by writing a genuine end-to-end test (real TLS
  handshake, real multi-request client) rather than trusting source-reading
  alone:
  - `tls.ClientHelloInfo` doesn't expose raw ClientHello bytes, so full
    mode wraps the accepted connection (`snoopConn`) to capture them
    itself, retrieved inside the `GetConfigForClient` hook via
    `hello.Conn.(*snoopConn)` and threaded to the HTTP handler layer via
    `http.Server.ConnContext` + `tls.Conn.NetConn()`.
  - `http.Server`'s automatic HTTP/2 setup (triggered because
    `httpServer.TLSConfig` is deliberately left nil - see
    `shouldConfigureHTTP2ForServe` in Go's own `net/http` source) only
    configures **routing** for an already-negotiated h2 connection; it does
    **not** retroactively add `"h2"` to a `tls.Config` built independently
    and passed to `tls.NewListener`, which is what full mode does. Without
    `NextProtos: []string{"h2", "http/1.1"}` set explicitly on that config,
    the real ALPN handshake would never offer h2 to any client, silently
    defeating one of full mode's main reasons to exist. A first version of
    this test suite passed without that line; only asserting on
    `resp.ProtoMajor` caught it.
  - Symmetrically, `http.Transport` on the *client* side won't
    auto-negotiate HTTP/2 either once you set a custom `TLSClientConfig`
    (needed for `InsecureSkipVerify` against a self-signed test cert, or
    for any custom CA in real use) - `ForceAttemptHTTP2: true` is required
    to opt back in. This tripped up the test client, not `fullproxy`
    itself, but it's the same "conservative unless you opt in explicitly"
    behavior on both sides of the connection.
- **Cache: single in-memory store behind a `RateStore` interface.** Only
  `MemoryRateStore` exists today (no Redis), but callers depend on the
  interface so a distributed implementation can be added later without
  touching the proxy or flush code.
- **Locking: one `sync.RWMutex` over a plain map**, not sharded. Both are
  reasonable at small/medium scale per the project brief; a sharded map is
  the known follow-up if lock contention shows up under real load.
- **Config: a TOML file, not environment variables.** `internal/collector`
  parses it via [`BurntSushi/toml`](https://github.com/BurntSushi/toml) -
  chosen over `pelletier/go-toml/v2` mainly for its long track record as
  the de facto standard Go TOML library and its lower minimum Go version
  (1.18 vs 1.21), which keeps this project's toolchain requirement as low
  as possible for whatever's already on the target VPS; both are
  zero-transitive-dependency and otherwise a close call. Fields are decoded
  into a struct pre-populated with defaults (`toml.DecodeFile` only
  overwrites what's actually present in the file - verified empirically,
  not assumed), so a minimal config only needs to set what differs from
  the defaults. See `config.example.toml`.
- **Database: TimescaleDB via `pgx/v5`**, using `COPY` (`pgx.CopyFrom`) for
  the periodic batch flush rather than row-by-row `INSERT`s. IP addresses
  are handled as `netip.Addr` end-to-end (proxy → RateStore → storage) and
  map straight onto Postgres's `inet` column type through pgx's built-in
  codec — this was verified by reading pgx's `pgtype` source directly
  rather than assumed. pgx is pinned to v5.7.6 (rather than latest) because
  the latest release bumps the required Go version to 1.25; v5.7.6 only
  needs 1.23, which keeps the toolchain requirement lower for whatever the
  target VPS already has installed.
- **The known-bot JA4 list is fetched, never shipped.** This repository
  contains no copy of it, deliberately: the project is permissively
  licensed and anyone may take it, but that dataset belongs to somebody
  else, and a permissively
  licensed repository carrying third-party data under unstated terms
  hands that uncertainty to everyone who clones it. See `THIRD-PARTY.md`.

  The deployment fetches it instead, onto its own machine, under the
  source's own terms:

  ```bash
  collector -config collector.toml -update-bot-data
  ```

  Put that in cron, run it by hand, or drive it from somewhere else —
  the schedule is not this software's business. It writes a
  self-describing file to `bot_data.path` (source, timestamp, how many
  entries were filtered out), which the collector reads at startup for
  scoring and the read API reads to label fingerprints in its responses.
  Entries classified `browser` are dropped: they are legitimate
  reference data, and keeping them would make the panel call every
  ordinary visitor a known bot.

  **Never running it is a supported state**, and the honest cost of not
  redistributing: the known-bot signal is simply absent, every other
  signal still works, both services say so at startup, and the setup
  wizard reports it. Nothing goes quietly missing.

  The source is [The Bot Aquarium](https://thebotaquarium.com/fingerprint/archive)'s
  public archive (community-submitted, classification-tagged).
  `ja4db.foxio.io` — the JA4 spec authors' own database, and the
  intended primary source — requires an account for bulk access (every
  endpoint returns HTTP 403 without one), so it is not wired up.

## Running locally

Requires Go 1.25.0+ — the version in `go.mod`. An older toolchain does
not fail at some later feature; it refuses the module outright.

This section brings the pipeline up on one machine for development. For
a server — roles, GRANTs, secrets, service files, TLS, first sign-in —
follow [`KURULUM.md`](KURULUM.md) instead, which is written for that and
does not assume the database is a throwaway container.

```bash
# 1. Start a local TimescaleDB (applies internal/storage/schema.sql on
#    first boot via the Postgres image's docker-entrypoint-initdb.d hook).
docker compose up -d
docker compose ps --format '{{.Name}}: {{.Health}}' # wait for "healthy"

# 2. Copy the example config and fill in site_id / backend_addr /
#    timescale_dsn (and tls.cert_file/key_file if using mode = "full").
cp config.example.toml config.toml
$EDITOR config.toml

# 3. ONLY if you just set asn_lookup.enabled = true in step 2: apply its
#    schema too, once (creates both the country and ASN range tables).
#    Unlike internal/storage/schema.sql, docker-compose.yml does NOT apply
#    this one automatically - there's no docker-entrypoint-initdb.d mount
#    for it, since it's optional and disabled by default. Skip this step
#    entirely if asn_lookup.enabled is false (the default) - the collector
#    runs fine without it existing.
psql "postgres://collector:collector@localhost:5432/analytics" -f internal/asnlookup/schema.sql

# 4. Run it (looks for ./config.toml by default; override with -config).
go run ./cmd/collector
# or: go run ./cmd/collector -config /path/to/config.toml

# 5. Optionally, run the read-only API alongside it, so a panel can pull
#    the collected statistics over HTTP. It's a separate process with its
#    own config - see "Read-only analytics API" above.
cp analytics-api.example.toml analytics-api.toml
go run ./cmd/analytics-api -hash-token   # generate a token + its hash
$EDITOR analytics-api.toml               # paste the hash, set the DSN
go run ./cmd/analytics-api

# 6. Optionally, run the client-side beacon too, for page-level analytics
#    the collector cannot see. Another separate process with its own
#    config and its own schema - see "Client-side beacon" above.
psql "postgres://collector:collector@localhost:5432/analytics" -f internal/beacon/schema.sql
cp beacon.example.toml beacon.toml
$EDITOR beacon.toml                      # set sites, DSN, trusted_proxies
go run ./cmd/beacon
go run ./cmd/beacon -snippet https://example.com mysite  # tag to embed
```

If you're pointing `docker compose` at an already-existing TimescaleDB
instead, apply both schemas once yourself (skip the second if you're
leaving `asn_lookup.enabled = false`):

```bash
psql "$TIMESCALE_DSN" -f internal/storage/schema.sql
psql "$TIMESCALE_DSN" -f internal/asnlookup/schema.sql
```

**If you already had this collector running before country/ASN
enrichment or ASN scoring existed** (i.e. `docker compose up -d` already
created `traffic_snapshots` on an earlier version, so step 1's
`docker-entrypoint-initdb.d` hook won't fire again against that existing
volume), re-apply `internal/storage/schema.sql` yourself the same way:
`psql "postgres://collector:collector@localhost:5432/analytics" -f internal/storage/schema.sql`.
It's self-migrating (see "Optional: IP → country / ASN lookup" above), so
this just adds the new columns (`country`/`asn`/`asn_org`/`is_known_bot_asn`)
in place - no drop, no data loss.

Forgetting step 3 when `asn_lookup.enabled = true` isn't fatal - the
collector still starts and runs normally, since a missing optional table
is treated the same as any other refresh failure (logged, not fatal - see
"Optional: IP → country / ASN lookup" above). It just means every lookup
silently returns `Found: false` until you apply the schema and the next
scheduled refresh runs, which is easy to mistake for "neither dataset
covers this IP" rather than "the tables don't exist yet." Check the logs
for `asnlookup: failed to persist country ranges` / `asnlookup: failed to
persist asn ranges` if lookups seem to never find anything.

### Configuration

Configuration is a single TOML file (default path `config.toml`, override
with `-config`) - see `config.example.toml` for a fully-commented template.
Only `network.backend_addr` and `storage.timescale_dsn` are required; every
other field has a default. `config.toml` is gitignored since
`timescale_dsn` typically carries credentials.

| Field                              | Default             | Meaning                                                        |
| ----------------------------------- | -------------------- | --------------------------------------------------------------- |
| `site_id`                           | —                    | **Required.** Which site this collector fronts; stamped on every row so one database can hold several sites. `[a-zA-Z0-9_-]{1,64}` - see "Read-only analytics API" above. |
| `mode`                              | `"passthrough"`      | `"passthrough"` or `"full"` - see "What it does" above.         |
| `network.listen_addr`               | `:8443`              | Address the proxy accepts connections on.                       |
| `network.backend_addr`              | —                    | **Required.** `host:port` of the site to proxy to.               |
| `network.dial_timeout_seconds`      | `10`                 | Max time to connect to `backend_addr`.                           |
| `network.handshake_timeout_seconds` | `5`                  | Passthrough-only: max time waiting for a full ClientHello before giving up on fingerprinting (the connection is still proxied). |
| `tls.cert_file` / `tls.key_file`    | —                    | **Required when `mode = "full"`.** The backend's real TLS certificate/key. |
| `cache.window_size_seconds`         | `60`                 | Sliding-window width used for rate estimation.                   |
| `cache.ttl_seconds`                 | `300`                | How long an idle IP's state is kept before eviction.              |
| `cache.cleanup_interval_seconds`    | `60`                 | How often the idle-TTL sweep runs.                                |
| `storage.timescale_dsn`             | —                    | **Required.** Postgres connection string for TimescaleDB.         |
| `storage.flush_interval_seconds`    | `10`                 | How often summaries are batch-written to TimescaleDB.             |
| `limits.max_concurrent_connections` | `1000`               | Bounds the collector's own total concurrent connections/requests, summed across all IPs - see "Recommended deployment order" above. |
| `limits.max_requests_per_second`    | `500`                | Same, but an aggregate requests/second ceiling instead of a concurrency one. |
| `limits.overload_policy`            | `"fail_open"`        | `"fail_open"`, `"fail_closed"`, or `"throttle"` once a limit above is exceeded - see "Recommended deployment order" above. |
| `limits.throttle_queue_size`        | `200`                | Only used when `overload_policy = "throttle"`: bounds how many excess connections/requests can queue before falling back to `fail_closed`. |
| `asn_lookup.enabled`                | `false`              | Turns on the optional IP → country / ASN lookup, and with it automatic `traffic_snapshots` enrichment - see "Optional: IP → country / ASN lookup" below. |
| `asn_lookup.apply_to_scoring`       | `false`              | Turns on the ASN scoring signal (`asn_lookup.known_bot_asns` below) - see "Optional: IP → country / ASN lookup" below. |
| `asn_lookup.cache_max_entries`      | `50000`              | Only validated when `asn_lookup.enabled = true`. Size of the in-memory LRU result cache. |
| `asn_lookup.cache_ttl_seconds`      | `21600` (6h)         | Same; how long one resolved IP is cached before being re-checked against the range tables. |
| `asn_lookup.refresh_interval_seconds` | `604800` (1 week)  | Same; how often both datasets are re-fetched (downloaded, or re-read from `local_csv_path`) and re-parsed. |
| `asn_lookup.local_csv_path`         | `""`                 | If set, skip downloading entirely and read `user-country-ipv4/6.csv` and `origin-asn-ipv4/6.csv` from this directory instead - no network access at all in that mode. |
| `asn_lookup.blocked_countries`      | `[]`                 | ISO 3166-1 alpha-2 codes (case-insensitive); a match rejects the connection/request outright, regardless of `limits.overload_policy` - see "Optional: IP → country / ASN lookup" below. |
| `asn_lookup.blocked_asns`           | `[]`                 | ASN numbers; same reject behavior as `blocked_countries`, checked independently. |
| `asn_lookup.known_bot_asns`         | `[]`                 | ASN numbers; only consulted when `apply_to_scoring = true`. A match adds a flat bonus to the bot-likelihood score instead of blocking - a separate list from `blocked_asns`, since a blocked ASN never reaches scoring. |
| `privacy.ip_storage`                | `"masked"`           | `"masked"` (IPv4 /24, IPv6 /64) or `"full"`. Empty means masked. Applied when the row is built, after the visitor id and the geography are derived from the whole address - see "Privacy model" above. |

## The management panel

`cmd/panel` is the fourth binary and the one a customer logs into. It has
its own config, its own Postgres role, and that role has **no access at
all** to the analytics tables - the panel reads traffic numbers over HTTP
from the read-only API, exactly as an external panel would. That is what
keeps the component the whole internet can reach from also being the
component with broad database rights.

```bash
cp panel.example.toml panel.toml
$EDITOR panel.toml                                 # panel_dsn is required
psql "$PANEL_DSN" -f internal/panel/schema.sql     # once, by hand
go build -ldflags "-X main.version=$(git describe --tags --always)" ./cmd/panel
./panel -config panel.toml
```

`panel_dsn` names a **different role in the same database** as the
analytics services — not a database of its own. What separates the panel
from the visitor records is the GRANTs, and the setup wizard proves that
separation by asking Postgres, over the panel's own connection, whether
the other roles can reach the analytics tables. From a separate database
those tables do not exist to ask about, the check reports "could not
look" rather than "looked and it is fine", and a check that could not
look blocks handover to the customer on purpose.
`KURULUM.md` §4 has the full role and GRANT list.

Everything the browser loads is compiled into that binary: the
stylesheet, htmx, every template and every Turkish string. **No CDN, no
npm, no build step** - deploying the panel is copying one file and one
config, which is also what lets it run somewhere with no outbound
network at all.

A few properties that are deliberate rather than incidental:

- **It refuses to start rather than starting broken.** A template that
  does not parse, a message key nothing defines, a stylesheet that did
  not get embedded, an unreachable database, an unknown time zone: each
  is reported by name at startup, on stderr *and* in the log tree,
  because the person who just typed the command is watching the
  terminal.
- **Times are rendered in the site's zone**, set by `timezone` in the
  config. An unknown name is an error, never a silent fall back to UTC -
  that would put every timestamp hours from the customer's clock while
  the config file said otherwise.
- **Pages are never cached; assets are cached for a year.** Panel HTML
  carries the customer's numbers and a session's CSRF token, so it goes
  out `no-store`. Assets are served from URLs containing a hash of their
  content, so a year is safe and a changed file is a changed URL.
- **The Content-Security-Policy allows neither `unsafe-inline` nor
  `unsafe-eval`.** There is not one inline `<script>`, `<style>` or
  `on…` attribute anywhere, and tests fail on the source if one appears.
- `hsts` is off by default and deliberately not tied to
  `secure_cookies`: this is the kind of software somebody runs on a
  spare machine first and puts a certificate on afterwards, and a wrong
  HSTS locks them out of a panel with no HTTPS to fall back to.

### First run, and the developer wizard

A freshly installed deployment has no accounts, so there is nowhere to
sign in. Its front page says exactly that and prints the command that
gets you in:

```bash
panel -config panel.toml -dev-link
```

That mints a **one-time** link, prints it to stdout, and stores only its
hash. Opening it starts a developer session and lands on the setup
wizard. Six steps: what this is, the database and schema, the sites, what
the config files say, retention, and the final check.

The rules that shape it:

- **It verifies more than it configures.** Database roles, the schema,
  TLS, the collector's backend - the panel cannot set any of those and
  should not be able to. Those steps read the real state and report it
  rather than showing a field that writes nothing.
- **Each step commits what it changes, immediately.** No draft in the
  session, no "finish" that applies everything at once. Stopping halfway
  leaves a half-configured deployment, which is true and visible.
- **Retention asks for the developer password**, every time, because
  log retention carries legal weight: access logs contain addresses.
  Analytics retention is not on this step at all - it lives in the
  services' config files, so changing how long visit records are kept
  means reaching the server.
- **The final check runs real queries**, on a button, not on page load.
  Required failures block handover; recommended ones do not. Beside it
  is the list of things the panel can never do, each row saying *why*,
  with the steps that have no verifier kept visibly separate from the
  ones that do. These live in `internal/panel/preflight`, which takes a
  database pool and does not import the panel at all — a test asserts
  that, because the checks inspect a *deployment*, and anything that
  wants to run one should not have to build a panel first.
- **Before anybody owns the deployment the link is approved on the
  spot** - there is nobody to ask, and installing the system is the job.
  The moment an account exists that stops, and the owner has to approve.
  The printed output says which of the two just happened.
- **Every redemption is in the append-only audit log**, filed under a
  visibly separate developer identity, with a bootstrap grant given its
  own action so "granted because nobody owned this yet" is never
  flattened into "granted".

The last step is **handover**. With every required check passing, the
installer enters the owner's email address and the panel mints a
one-time invitation link, shown once - only its SHA-256 is stored.
Whoever opens it sets their own password, and the account is created and
made owner of every configured site in one transaction. Losing the link
means minting another:

```bash
panel -config panel.toml -owner-link musteri@example.com
```

Handover is refused while a required check is failing, and the refusal
names them. Handing over a deployment whose schema is unapplied makes
the customer's first experience an error page, and the person who could
have fixed it has just walked away.

### The owner's wizard

The customer's first ten minutes, and the mirror image of the technical
one: it configures and never verifies, because **it must never require a
technical step**. Those were done before they arrived. Four steps - what
the site is called, which time zone the numbers are read in, the snippet
to embed, and how to add colleagues.

Where it needs a technical value it shows what the developer configured
rather than an empty field. If `beacon_url` is unset the snippet step
says where to get the snippet instead of printing one built from a
guessed address, which would look right and measure nothing.

### The technical door

The owner's panel carries an unassuming link to the technical wizard.
The first click does not open it; it warns:

> This section was completed by your developer. If you want to go
> through it again anyway, confirm below.

Neither hidden nor plainly linked, and both alternatives are wrong.
Hiding it is wrong because **the server is theirs** — a capable owner
should not have to ask us to see their own retention policy. Leaving it
open is wrong because the common case is somebody curious clicking
through a menu, and reconfiguring a working installation is a support
call at best.

The confirmation lives in the session, not on the account: the warning
is about this visit, and what it warns about does not get less true with
familiarity. It is also not the authorisation — every request still
checks that the principal owns something (owners and the operator, never
an admin or a viewer), and the settings with legal weight still ask for
the developer password separately.

The wizard's own text is translated. The check results and the
manual-step list are still Turkish only: they live beside the rule that
produces them rather than in the language packs, and moving them is
recorded as open work in `PLAN.md`.

### The site page

Signing in lands on the list of sites this account can reach; a site's
name is the link to its numbers.

Six cards by default. Four a customer recognises from any analytics tool
— visitors, pageviews, sessions, bounce rate — and two that are the
reason for running two collectors at once: **human traffic** and **bot
traffic**, counted from connections rather than from JavaScript, so they
include every visitor whose browser never ran the snippet.

A few things about that page are deliberate:

- **The numbers arrive over HTTP.** The panel's database role cannot
  read the analytics tables, so the page calls the read-only API exactly
  as an external dashboard would. A structural test refuses any direct
  reference to those tables from the panel's HTTP tree.
- **Every number has a breakdown under it.** Six sections beneath the
  cards answer the question a figure raises: which pages, from where,
  which campaign, on what device, from which country, and which custom
  events. Eight rows each, with the full paginated list one click away.
- **Which of those a customer sees is a setting, not a decision this
  project makes for them.** The person who bought a website does not
  know what a TLS fingerprint is and has no reason to; the installer
  asks what they want and turns those on. A block that is off is not
  merely hidden - its call is never made, so the saving reaches the
  database rather than stopping at the template.
- **The period is whole days in the panel's timezone.** Sessions are
  counted inside the range, so one that began before it is truncated —
  "the last seven days" measured from the current instant produces
  numbers that cannot be added to the neighbouring period and do not
  match what a customer means by last week. This is what makes the
  timezone setting more than cosmetic.
- **The group with no value is a named row, never a gap.** A visit with
  no referrer, a browser nothing recognised, an unresolved country: the
  API flags those rather than dropping them, so the groups still add up
  to the site's total. The panel gives each one a word of its own -
  "Direct" is not the same fact as "Unknown" - and styles it as the
  different kind of thing it is.
- **A share is a share of the number above it.** Both the summary and
  the breakdowns take the API's default bot filter, so a row's
  percentage counts the same population as the card. A missing total
  renders as a dash, never as 0%.
- **Campaigns are the one breakdown that does not cover the whole
  site.** Its endpoint excludes untagged traffic in SQL, so those rows
  do not add up to the pageview count - which is correct and looks
  broken, so the section says so.
- **An empty card says which kind of empty.** The snippet was never
  installed, or it is installed and nobody visited in this period, or
  the API could not be reached. They are three different facts and one
  "no data" sentence would present an unfinished setup step as though it
  were a measurement.
- **A failure is never drawn as zero.** A card reading "0 visitors"
  because a call timed out is not a missing number, it is a wrong one,
  and the reader has no way to tell.
- **The visible set is per site, and unset means the default.** Every
  deployment that predates the setting draws the full page; a page that
  emptied itself on upgrade would be the worst reading of "not
  configured". Saying "none" is therefore a value of its own rather than
  an empty list - a collector-only deployment needs to turn the beacon
  sections off, and its customer should not be handed six tables that
  all say the snippet was never installed.
- **Numbers stop at the site boundary.** The panel's API token reads
  every site — it serves all of them — so the only thing between one
  customer and another is the panel's own access check. An account with
  no membership gets a 404 rather than a 403, because a 403 would
  confirm the site exists.

Six breakdowns is not all of them: the API serves close to thirty,
including the fingerprint, ASN, score-distribution and cross-source
views. Those are deliberately not on this page - they belong to the
developer-facing layer, which adds columns to these same sections rather
than pages beside them.

### Settings that used to need SSH

Some of what a deployment needs to change while it is running started
life in a config file, where changing it meant a shell, an editor and a
restart. Those are moving into the panel's settings table, and the two
that moved first were chosen from the repair catalogue's own evidence
rather than from what would be convenient:

- **`beacon.trusted_proxies`** — the networks whose forwarded headers
  are believed. Behind a proxy, an empty or wrong list does not merely
  lose the visitor's address: it makes every number derived from that
  address wrong at the same time — visitor counts, geography, and the
  join back to the collector's data. It is the most common real
  misconfiguration there is.
- **The admission limits**, per service, for both the collector and the
  beacon: the ceiling, the rate, the overload policy and the throttle
  queue. "The collector itself is the bottleneck" is a thing you fix
  during an incident, and an incident is the worst possible moment to be
  asked for a restart.

- **The blocklists and the known-bot ASN signal** — `blocked_countries`,
  `blocked_asns`, `known_bot_asns` and the `apply_to_scoring` flag that
  turns the last one on. Same reasoning, one step further: "we are being
  hit from there, block it" is the sentence a support call is actually
  made of, and until this it meant SSH, an edit and a restart — the
  longest possible path while an attack is in progress.
- **The log level**, in both services, along with the temporary raise to
  debug that switches itself off again. It was live in the beacon and
  discarded in the collector, which built its controls and threw them
  away; the process a support call most often needs verbose was the one
  that could not be turned up without a restart.

The limits are **per service and not shared**, because one number could
not honestly mean both: the collector sees every connection to the site,
the beacon only the visitors whose browser ran the snippet.

A deployment that blocks nothing — the default — does not pay for the
feature: the servers ask whether anything is blocked at all before
resolving a connection's country, and that question is one atomic load.

Not everything can be live, and the panel says which. Buffer sizes,
cache windows and `asn_lookup.enabled` are fixed when the process builds
its channels and tables, so they are marked as needing a restart rather
than accepted and quietly ignored. `storage.flush_interval_seconds`
could be made live and deliberately is not: it is a performance knob, not
something an incident reaches for.

**A value resolves in three layers, each narrower than the last:** the
stored row if there is one, else the config file, else the built-in
default. The file never stops being the fallback — which is what makes
an unreachable settings table a non-event rather than a silent reset of
somebody's tuning.

Reading them needs one grant, deliberately minimal, and a deployment may
simply not give it:

```sql
GRANT SELECT ON panel_settings TO collector;   -- and to the beacon's role
```

Without it nothing breaks: each process runs on its config file exactly
as before, and says so in its log. With it, a change takes effect within
one polling interval and no restart.

#### One setting went the other way

Everything above moved *out* of the config files. Analytics retention
moved *into* them, and the reasoning is worth stating because it is the
opposite of the rest of this section.

How long visit records are kept is the only setting here with legal
rather than operational weight. Every other value in the panel decides
performance, accuracy or disk; this one decides how long a person's
browsing is held by somebody they have never heard of. KVKK's
proportionality rule is its direct subject.

It used to be a panel setting behind the developer password. That is a
strong lock, and it was on the door of a room the customer was still
standing in: the value was visible over HTTP, editable over HTTP, and one
leaked password away from being somebody else's decision. So it is a
config-file value now, in each service's `[retention]` section, and
changing it means reaching the server.

```toml
[retention]
days = 90                             # the default; 1..730
per_site = { "musteri-a" = 30 }       # the one customer who asked for less
interval_hours = 1
```

The ceiling is **730 days**, down from ten years. Ten was chosen as the
point past which "keep it" and "keep it forever" stop differing, which is
a statement about arithmetic rather than about the law this runs under; a
product whose ceiling is a decade invites a deployment nobody can defend.
Two years rather than one because the honest use for old analytics is
"the same month last year", and a ceiling of 365 makes that comparison
impossible on the last day it is needed.

A file asking for more is **refused at startup** rather than clamped or
ignored. That matters most for a deployment upgrading from an older
build, where 3650 was legal: the previous behaviour for an out-of-range
value was to fall back to 90 days silently, so a deployment believing it
kept five years would have kept three months and found out from a
customer.

Per-site retention survived the move, in the file, because "this customer
asked for thirty days" is a real request. The hypertable keeps whatever
the longest site needs; shorter sites are trimmed by row.

#### Moving what a file already says

An existing deployment's tuning has to survive the move. One command
copies it in:

```bash
panel -config panel.toml -migrate-settings collector \
      -migrate-from /etc/crucible/config.toml
```

It **never overwrites a value somebody already set in the panel**, it
reports what it skipped and why, and every value it moves is recorded in
the audit log with the file and the line it came from — so a year later
a migrated value is distinguishable from one somebody chose.

It is a shell command rather than something a service does at startup,
and that is not an accident: the collector's database role may only read
that table. A service that could write it could change the retention
period and the IP storage mode, which sit behind the developer password
precisely because they carry legal weight.

Do not delete the file afterwards. It is still the fallback; it has
simply stopped being where the value is changed. Once the panel has a
value, editing the file does nothing.

### Letting a developer back in

Once a deployment has an owner, a developer link is inert until that
owner approves it. `/erisim` is where they do — reachable from the
navigation, and announced by a banner that follows them onto every page
while something is waiting, because the request will not arrive while
they happen to be looking at the front page.

Each waiting request shows the reason, when it was asked, how long the
owner has to decide, and how long the session lasts if they say yes.
Approving makes the link usable **once**. Denying is final: it can never
be approved afterwards, so a decision somebody already made cannot be
quietly reversed — the developer has to ask again, and the owner sees a
fresh request.

Three things about this page are deliberate and easy to get wrong:

- **A developer cannot approve developer access.** A redeemed link
  carries superadmin authority, because a developer has to reach every
  site to do the work — so "does this principal own something" answers
  *yes* for them. If that were the question this page asked, an approved
  developer could approve the next request, and the next, and the owner
  would be asked exactly once, ever. The page asks whether the reader is
  a signed-in **person** first, and only then about ownership.
- **The panel does not know who asked, and says so above the first
  request.** A request is minted by somebody with a shell on the server;
  the reason is a sentence that person typed and nothing verified. The
  page states that before the reason rather than after, because somebody
  deciding whether to let a stranger into their customers' data should
  know how much was checked before they read the text.
- **An install-time grant is shown as spent, not as approved.** Those
  links carry an approval — during installation there was nobody to ask
  — and they die the instant an account exists. Since this page cannot
  be reached without an account existing, every one of them on it is
  already dead, and drawing it as "approved" would say somebody can
  still walk in.

Asking, approving, denying and *failing to redeem* are all in the audit
log. The last one is recorded only when the token matches a request this
deployment actually issued: the redemption URL is public, and filing an
entry for every string presented would let a stranger write rows into a
table the panel is deliberately not allowed to delete from.

### Signing in

The customer's door: email and password, an optional second factor, and
the account page behind it.

- **Every failure says the same thing.** One sentence, one status, and
  the password is verified even for an address with no account — because
  skipping that answers "does this address have an account here" in
  about eighty milliseconds, from anywhere.
- **The throttle runs before the password, not after.** Two independent
  counters: per account, and per address. Checking after a failure would
  still pay the argon2id cost per guess, which is the cost the attacker
  had budgeted for anyway.
- **A password alone is not a session** when a second factor is set.
  The half-finished state opens the code form and nothing else.
- **`?next=` is validated, not sanitised.** A sign-in form that will
  redirect anywhere is a phishing springboard on the customer's own
  domain. Anything that is not plainly a path inside this panel becomes
  the site list.
- **Changing a password or removing the second factor costs the current
  password**, so a stolen session does not become a stolen account.
- **Two-factor enrolment writes nothing until a code proves it.** The
  secret lives in the session and the QR is served from its own
  same-origin, `no-store` endpoint — never embedded in the page, where
  it would land in view-source, the cache, and every screenshot.

There are no recovery codes yet. Somebody who loses their phone is
recovered by an owner or the operator resetting their second factor;
a sole owner who loses theirs still needs a shell. Changing a password
also does not end sessions on other devices. Both gaps are stated on
the pages themselves and tracked in `PLAN.md`.

### Roles

`owner`, `admin`, `viewer`, with a capability table in
`internal/panel/roles.go` that is the whole authorisation model in one
place. A viewer sees the numbers and no controls, and the chrome says
why — a page full of missing buttons with no explanation is a support
call about a feature working as designed.

**Hiding a link is not authorisation.** The navigation is filtered so
nobody is shown a door that will not open; every handler then asks for
its own capability again. Each permission test has a pair: the allowed
request, and the same one forged by somebody who may not make it.

Two refusals that are deliberately different codes: a site you have no
access to is **404** (a 403 would confirm it exists, and turn the URL
into a customer list), while a page your role does not open on a site
you *can* see is **403**.

Adding a member adds somebody who already has an account. Creating one
means sending an invitation, which means email — see the scope note
below.

The dashboard behind all this has arrived, cards and breakdowns both —
see "The site page" above. What is left in group D is the developer
layer over the same pages (fingerprints, ASNs, scores, the cross-source
views), the settings screens, and the owner-facing warning strip.

### Languages

The panel ships Turkish and English. **Adding a language is one file and
a rebuild** - drop a `.toml` in `internal/panel/ui/messages/` and the
loader finds it. There is no list in Go to update, and the test suite
carries a language this repository does not ship precisely to prove
that.

Read `messages/tr.toml` first: it is the base pack and carries the full
explanation. Briefly, each file has three sections - `[dil]` (code,
endonym, text direction), `[bicim]` (the locale's date and unit data)
and `[metin]` (the sentences).

Two rules are asymmetric on purpose:

- **The base pack owns the key set.** A template naming a key it does
  not define stops the binary from starting.
- **A translation may be incomplete.** Missing keys fall back to the
  base language, are reported once at startup with the exact list, and
  fail a test. That way an untranslated sentence costs a build rather
  than taking down a deployment whose readers do not speak that language
  at all.

Formatting follows the language, not just the words: `1.234.567` and
`%45,7` in Turkish, `1,234,567` and `45.7%` in English; `17 Ağustos
2026` against `August 17, 2026`. Plural forms come from real CLDR rules
(`golang.org/x/text/feature/plural`), so a pack supplies only the forms
its language has - Turkish supplies one, English two, Russian four - and
a language that does not inflect after a numeral never has to know the
mechanism exists.

Which language a reader gets, in order: the deployment's `language`
setting, then the browser's `Accept-Language`, then the base language.
The panel deliberately has no `?lang=` switch - a page that renders
differently for the same address makes every screenshot in a support
ticket ambiguous.

`<html>` carries `lang` and `dir` from the pack, and the stylesheet uses
logical properties (`margin-inline-start`, `text-align: start`)
throughout. That is groundwork for a right-to-left language, not a
claim of support: no RTL pack has been written or tested, and doing one
properly means reviewing the layout with it in front of you.

## Getting back in

An owner who has forgotten their password, or lost the phone their
second factor lives on, does not have to reach anybody. Eight
single-use recovery codes are generated when the account is created and
shown once; one of them, on the sign-in page's "I cannot get in" link,
sets a new password.

**No email is involved and nothing needs configuring.** That is the
point rather than a limitation: a mail server is a configuration burden
with a silent failure mode, since mail leaving a fresh VPS without SPF,
DKIM and DMARC lands in spam or is rejected outright — and a password
reset that vanishes quietly is the worst failure available, because the
person waits while the panel says "sent".

The details that matter:

- **A code proves who you are; it does not skip your second factor.** An
  account that still has one is sent to the second-factor page as usual.
  The form offers to clear it, for somebody who has genuinely lost their
  phone, and that is a choice the person makes rather than something the
  code does by itself.
- **A wrong code and an unknown address answer identically.** The page
  is reachable without signing in, so any difference between the two
  would tell anybody on the internet which addresses have accounts here.
- **It shares the sign-in throttle**, by address. Two forms into one
  account count against one budget.
- **A mistyped new password does not spend a code**; the form-level
  checks happen before the credential is touched.
- **Somebody who lost the codes too** is not stuck: an operator
  regenerates the set from the member list and passes one on. There is
  no second kind of link and no second flow — the same form redeems it.

Codes are stored as SHA-256 digests, like every other high-entropy
credential here. The account page shows how many are left and warns
before they run out.

## The developer password

A short list of settings changes what personal data this deployment
stores, or for how long: `privacy.ip_storage`, `logs.retention_days`,
`logs.important_retention_days`, `campaign.drop_params`,
`campaign.extra_params`, `campaign.store_click_ids`.

`analytics.retention_days` was on that list and is no longer a panel
setting at all — see below.

Changing any of them from the panel needs a second password, separate
from the one the operator logged in with. The panel password answers
"who are you"; this one answers "may you make this particular change",
and its answer has to come from somebody with access to the server
rather than to the panel.

It lives in the config file, only as an argon2id hash:

```bash
go run ./cmd/devpass          # prompts twice, no echo
```

```toml
[developer]
password_hash = "$argon2id$v=19$m=19456,t=2,p=1$..."
```

Putting a plaintext password in a `password` key is refused at startup
rather than ignored, and a mistyped hash is refused too - otherwise it
would behave exactly like a permanently wrong password, with nothing
anywhere to say why.

The customer still sees these settings. All of them, with their current
values and the reason each one is guarded - what is withheld is the
control, not the information. A setting nobody can see is a setting
nobody can ask about, and it would leave a customer unable to account for
their own deployment. What they get is the value, an explanation, and a
lock. What they do not get is a password field: putting one in front of
somebody who cannot have the password invites them to go looking for it,
and every attempt would spend part of a failure budget that belongs to
the operator.

Five properties are worth knowing before relying on it:

- **It is asked every single time.** Verifying produces an
  authorization that names one setting and expires in seconds, so
  nothing can hold one to skip the next prompt. There is no session.
- **With no hash configured, the gate is shut.** Those settings keep
  their defaults and cannot be changed from the panel at all. Since the
  defaults are the privacy-preserving values, failing closed costs
  nothing that should be free. The setup wizard reports this
  (`config.developer_password`) rather than leaving it to be discovered
  by being refused.
- **Every attempt is recorded**, granted or refused, in the append-only
  audit log - which the panel's database role may INSERT into but not
  UPDATE or DELETE.
- **Only the operator may attempt it.** A customer is refused on who
  they are, before any argon2 work and without touching the shared
  failure counter - otherwise five guesses from a customer would lock
  the operator out of a deployment they are responsible for.
- **Repeated failures stop being answered.** One verification is
  deliberately ~19 MiB of argon2 work, so verifications are serialised
  behind a bounded queue and a run of wrong answers closes the gate for
  a while. Without that, the gate would be a denial-of-service amplifier
  aimed at the machine the collector runs on.

## Security

**[`SECURITY.md`](SECURITY.md)** is the security document: how to report
a vulnerability, the design in one page, and the results of an audit
against the OWASP Top 10 (2021), the CWE Top 25 and the ASVS
requirements that apply to a self-hosted admin panel.

It lists three things, and the second is the useful one:

- **what was fixed** — eight findings, each with a test, two of them in
  dependencies and reported by `govulncheck` as *reachable*;
- **what was checked and found correct** — thirteen classes, written
  down because "we found nothing" and "we did not look" are different
  facts and only one of them should reassure anybody;
- **what is open** — three limitations, each also stated on the page it
  affects, because a limitation the software knows about and the
  customer does not is worse than the limitation.

It also records the deployment this software assumes: a database not
reachable from the internet, TLS terminated in front of the panel,
config files readable only by the service user, and a `0700` log
directory.

## Testing

```bash
go test -race ./...

# Vet under every tag, not just the default one. Test files behind a
# build tag do not compile in the untagged build, so a suite can rot
# against an API that changed months ago and nothing says a word. That
# is not hypothetical - it happened here, to internal/api's integration
# test, and this line is what would have caught it.
go vet ./... && go vet -tags "integration loadtest" ./...

# Dependencies, which reading cannot audit. This is not optional
# housekeeping: the two highest-severity findings in the security audit
# were both here, both reachable, and one of them defeated this
# project's own "every query is parameterised" rule one layer below
# where the rule is enforced.
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

This needs no external dependencies - no Docker, no network access,
nothing running. Two further suites exist for real, live-dependency
verification, each gated behind its own build tag specifically so the
default suite above stays that way:

```bash
# Needs a real TimescaleDB: docker compose up -d first (see "Running
# locally"), plus internal/asnlookup/schema.sql applied (creates both
# tables) if you want that package's suite too, and
# internal/beacon/schema.sql for the beacon's.
go test -tags integration ./internal/storage/... ./internal/asnlookup/... ./internal/api/... ./internal/beacon/... -v

# No external dependency - just slower and more timing-sensitive than the
# default suite should be, since it fires dozens of genuinely concurrent
# connections at a real listening proxy.Server, or drives a realistic
# 100k-request cache access pattern.
go test -tags loadtest ./internal/loadtest/... ./internal/asnlookup/... -v

# Needs node, playwright and a chromium build. Drives a real browser
# against the real handler tree.
CA_BROWSER_TEST=1 go test -tags integration ./internal/panel/... -v
```

The browser suite is not decoration. `httptest.ResponseRecorder` cannot
tell you whether Chromium *refused* the stylesheet under the
Content-Security-Policy, whether htmx actually started, whether the
Turkish text decoded, or whether a second request for an asset came back
`304`. Each of those fails silently, and from the server's side the
response was `200` with correct bytes. The browser test found one such
defect the moment it was written: htmx injects an inline `<style>` at
startup, the policy refused it, and nothing anywhere said so - the panel
now turns that injection off via `<meta name="htmx-config">` and carries
the four rules in its own stylesheet.

Coverage includes a hand-rolled ClientHello parser exercised against
independently-built byte fixtures (including truncation and multi-record
fragmentation) *and* against 5 real ClientHellos from FoxIO's official test
pcaps, cross-checked against both FoxIO's own reference implementation and
Wireshark's independent JA4 dissector (`internal/ja4/foxio_reference_test.go`
- both known FoxIO/Wireshark discrepancies are traced to their actual root
cause in Wireshark's own GitLab issue tracker, not left as an unexplained
difference), sliding-window math with injected timestamps (no sleeps), and
end-to-end proxy tests for *both* modes that perform a **real** TLS
handshake (self-signed cert generated in-test, stdlib only): passthrough's
asserts byte-for-byte passthrough and a correctly-shaped extracted JA4;
full mode's asserts real backend responses (including one explicitly
against a plain HTTP/1.1-only backend built from `net.Listen` +
`http.Server` with no TLS and no h2c involved at all -
`TestServer_WorksAgainstPlainHTTP11OnlyBackend`), a correctly-shaped JA4,
that HTTP/2 actually gets negotiated, and - the core point of full mode -
that N requests over one connection produce N separate `RecordRequest`
calls, not one (`internal/fullproxy/server_test.go`).

Both proxy packages also have real-network geo-blocking tests (a fake
`Resolver` stands in for a loaded `*asnlookup.Resolver`, but the
connection/TLS/HTTP handling around it is exactly the same real machinery
the other end-to-end tests use): a blocklist match closes the connection
before ever dialing the backend in passthrough mode
(`TestServer_GeoBlockedConnectionIsRejectedBeforeBackendDial`), returns
`403` without ever calling `RecordRequest` in full mode
(`TestServer_GeoBlockedRequestGets403AndIsNotRecorded`), and a configured
but non-matching blocklist proxies normally in both
(`TestServer_NonMatching*`) - proving GeoBlocklist being non-nil doesn't
itself change behavior, only an actual match does.
`internal/limiter/geoblock_test.go` covers `GeoBlocklist` itself in
isolation: case-insensitive country matching, ASN matching, that an
unresolved lookup (`""`/`0`) never matches a rule, and that a nil
blocklist is always a no-op.

`go test -tags integration ./internal/storage/... ./internal/asnlookup/...`
confirms both packages' pgx `netip.Addr` ↔ `inet` encoding round-trips
correctly against a real TimescaleDB - read back through a raw query, not
just the same path that wrote it, for `ip_country_ranges` and
`ip_asn_ranges` independently (including an ASN organization name
containing a literal comma, the same real-data shape `parseASNCSV` is
tested against) - and that `internal/storage/schema.sql`'s
`create_hypertable` call actually took effect, not just that a plain table
would have accepted the same writes. It also covers `traffic_snapshots`'s
own `country`/`asn`/`asn_org`/`is_known_bot_asn` columns both ways: a row
with real values round-trips correctly, and a row built the way
`BuildRows` produces one with no resolver (`asn_lookup.enabled = false`)
reads back as `''`/`0`/`''`/`false` rather than `NULL` or some other
encoding surprise. It also proves the point of `site_id`: two sites'
rows written for the *same* IP at the *same* timestamp - so that every
other column collides and only `site_id` can separate them - stay cleanly
filterable.

`internal/api` is covered at two levels. Against a fake store,
`server_test.go` pins the security boundary: every `/api/` route rejects a
missing or wrong token, every per-site route rejects a token whose grant
doesn't cover that site (asserted per route, since one route forgetting
the check would leak another customer's data), `/api/v1/sites` doesn't
leak the names of sites a token can't read, non-`GET` methods are refused,
malformed parameters are `400`, and a database error is genericised rather
than echoed to the client. Against a real TimescaleDB,
`integration_test.go` proves the SQL is valid and - most importantly -
that **every** query is site-scoped, seeding two sites with identical IPs
and timestamps and checking that none of `summary`, `timeseries`,
`top-ips`, `countries`, `asns`, `ja4`, `score-distribution`, `snapshots`
or the per-IP detail leaks one into the other. The route-coverage lists
are deliberately exhaustive rather than sampled, so an endpoint added
later that forgets its site filter fails the suite instead of quietly
shipping.

Both were backed up by running the real binaries end to end: a real
collector fronting a real backend, writing to a real TimescaleDB, with the
real API binary serving queries over HTTP using a token generated by its
own `-hash-token` flag (cross-checked against `sha256sum`). That run is
also what caught the cumulative-request-count defect described above -
the unit and integration tests had both passed, because they shared the
same wrong premise about sampling regularity that the code did.

`internal/scoring/scoring_test.go` and `storage.BuildRows`/`Flusher`'s own
tests cover the ASN scoring component the same way JA4's was already
covered: a matching ASN adds exactly `maxASNScore`, a non-matching one
adds nothing, `asn == 0` (unresolved) never matches even against an
attacker-controlled `knownBotASNs` map, a `nil` `knownBotASNs` is a
complete no-op (not a panic), and rate + JA4 + ASN combine and clamp at
`MaxScore` together. Beyond the unit level, a real running collector
binary confirms the actual wiring: same IP, same fixture-resolved ASN,
`known_bot_asns` configured to match it - `bot_score` came back `20`
(exactly `maxASNScore`, since the request rate was too low to contribute)
with `is_known_bot_asn = true` when `apply_to_scoring = true`, and `0`/
`false` for the identical setup with only `apply_to_scoring = false`
changed - ruling out the score having come from anywhere other than the
ASN check actually being consulted.

`go test -tags loadtest ./internal/loadtest/...` fires 30-100 genuinely
concurrent connections per scenario at a real `proxy.Server` and asserts on
aggregate outcomes, going beyond what the tightly-choreographed
single/double-connection scenarios in `internal/proxy` and
`internal/fullproxy`'s own limiter tests exercise: with
`max_concurrent_connections = 10`, `fail_closed` lets almost exactly 10
through and rejects the rest; `fail_open` proxies all of them but records
only around 10; `throttle` with a queue of 10 eventually serves around 15
(5 concurrent + 10 queued) out of 30; a `max_requests_per_second = 20`
ceiling lets roughly 20 of 100 simultaneous attempts through. Assertions
allow some slack for real scheduling variance rather than pinning to one
exact number.

`go test -tags loadtest ./internal/asnlookup/...` covers this package's
own load/scale behavior separately: a realistic 80/20 hot-set cache access
pattern (`TestLoadTest_TTLCache_HitRatioUnderRealisticAccessPattern`), a
stronger proof that `local_csv_path` makes zero network calls across all
four dataset files - not just a nil-client crash-avoidance check
(`TestLoadTest_LocalCSVPath_NeverContactsNetwork`), and a goroutine-leak
check across `Run`'s full start/refresh/cancel lifecycle
(`TestLoadTest_NewResolverAloneStartsNoBackgroundActivity`). A further
`TestScale_RealDatasetMemoryAndParseTime` and two `BenchmarkResolve_*`
benchmarks are gated behind an additional env var, since they need the
real, fully-downloaded datasets rather than small fixtures:

```bash
ASNLOOKUP_SCALE_TEST_DIR=/path/to/dir go test -tags loadtest ./internal/asnlookup/... -run TestScale -bench BenchmarkResolve -benchmem -v
```

Measured against the real, full `user-country`/`origin-asn` CSVs: 556,155
country ranges + 561,993 ASN ranges across all four files, parsed in ~1.5s
total and retaining ~135 MB of heap for all four in-memory tables combined
- the number a running collector with `asn_lookup.enabled = true` actually
holds, not an estimate. A warm `Resolve` (cache hit) takes ~103 ns/op with
zero allocations; a cold one (cache miss, genuinely falling through to
both the country and ASN binary searches) takes ~1.12 µs/op with 2
allocations.

`internal/beacon` is covered at three levels. Unit tests pin the parts
that face hostile input directly: that only allowlisted campaign
parameters survive a query string (a `session_token` in a URL must never
reach an analytics table), that a forwarded header from an *untrusted*
peer is ignored so a client cannot choose its own IP, that the visitor ID
separates two browsers behind one CGNAT address and *joins* two IPv6
addresses in one `/64` (privacy-extension rotation would otherwise invent
a new visitor daily), that field boundaries in the ID hash are
unambiguous, and that a real Cubot phone is not classified as a crawler
while Googlebot and an unknown `Foobot/1.0` both are.

Against a real TimescaleDB, `-tags integration` proves the COPY column
list actually matches `schema.sql` by reading a fully-populated row back
through a raw query, that the shutdown drain writes what it was holding,
that `Enqueue` drops rather than blocking on a full buffer, and - the one
that most needed a real database - that a payload carrying a NUL byte and
invalid UTF-8 does **not** fail the batch it shares with innocent rows.
It also runs the actual `beacon_events` ⋈ `traffic_snapshots` join, and
asserts an IP that sent no beacon event is not reported as having run
JavaScript.

Neither of those covers the snippet itself, so it was verified by
**driving `internal/beacon/beacon.js` in a real headless Chromium against
the real `cmd/beacon` binary and a real database**. A page load, a
`crucible('event', …)` call and a `history.pushState` navigation produced
exactly three rows: the referrer split to `www.google.com` with its search
terms dropped, `utm_*` kept but a `secret_token` parameter discarded, one
stable `visitor_id` across all three, and `is_bot_ua = true` - because
Playwright's Chromium is headless, which is precisely the "runs
JavaScript and is automated" population the beacon exists to make visible.

The read endpoints were verified the same way, end to end rather than
only against fixtures: two browser contexts (one keeping Playwright's
headless user agent, one overriding it with desktop Chrome) produced
4 pageviews, 1 custom event and 2 visitors, and every figure the API
then reported was checked against what the browser had actually done.
`bots=exclude|include|only` returned 2/4/2 pageviews and 1/2/1 visitors;
entry pages were the two pages the sessions started on and exit pages
the two they ended on, with the middle page in neither; campaigns
decoded back into their `utm_*` parameters; and **`/beacon/countries`
returned `TR` for events that carried no country of their own** -
recovered through the join to `traffic_snapshots`, which is the path a
recommended deployment always takes. On the crossover side, three
collector-side addresses of which one had run the snippet gave
`js_coverage: 0.33`, the two scrapers in `silent-ips` ranked by score,
and the headless context in `js-bots`.

`internal/api`'s route lists are deliberately exhaustive rather than
sampled - every one of the 28 routes is asserted to reject a missing
token, to reject a token whose grant doesn't cover the site, to refuse
non-`GET`, and to return JSON. The seven `/beacon/` breakdowns share one
handler wired from a map, so there is also a test that each route
reaches its *own* query: without it, pointing `/browsers` at the devices
breakdown would fail nothing.

## Explicitly out of scope for this phase

Out of scope **for `cmd/analytics-api` specifically** — these exist in
the project, in the panel, which is a separate process that talks to
this API over HTTP like any other caller: a dashboard UI, and human
login/session management (this API authenticates *callers* with tokens;
authenticating *people* belongs in the panel in front of it).

Out of scope for the project as it stands: exact cumulative request
totals (see "What the numbers
mean" above and `NOTES.md`), path-scanning detection by the collector
itself (the beacon reports paths from the client, which is a different
thing - it cannot see a scanner that never runs JavaScript),
header-consistency checks,
weighted multi-signal correlation, a Redis-backed `RateStore`
implementation, an HTTPS (as opposed to plaintext) backend for full mode,
and a richer country/ASN rule engine beyond the flat `blocked_countries`/
`blocked_asns` denylist and flat `known_bot_asns` scoring bonus - e.g. a
per-rule policy (always-allow a specific ASN regardless of load, throttle
a specific country at N req/s) rather than every blocklist match meaning
the same unconditional reject, or a tunable per-ASN scoring weight rather
than one flat bonus for every match. That richer version was deliberately
scoped out rather than built now; see `NOTES.md` for its design and the
open questions it would need to resolve. The `RateStore` interface and
scoring package are shaped to make these additive later, not to pre-build
them now.

## Licence

Apache-2.0. See `LICENSE` for the terms and `NOTICE` for the attribution
that has to travel with a redistribution; `THIRD-PARTY.md` covers what
else is inside a build and under what terms.

**Running it as a service is allowed**, including as a paid one, and
including by a competitor. There is no clause preventing that, and its
absence is a decision: the people this is built for are the agencies and
developers who install it for a customer, and a licence that a client's
lawyer has to think about is friction aimed at the wrong person.

**Attribution is the part that is asked for**, and it is the reason this
is Apache-2.0 rather than MIT. Section 4 requires a redistributor to keep
the licence, state what they changed, and carry `NOTICE`'s attribution
text; MIT asks only that a copyright line travel with the source. Running
the software is not redistribution and requires none of it.

**No warranty and no liability**, in sections 7 and 8 — which matters
more here than in most projects, because the collector sits in the
traffic path of a live site and the beacon writes personal data. What a
deployment keeps, for how long, and under whose legal responsibility is
the deployment's own decision. See "Privacy model" above and
`SECURITY.md`.

**What ships is source code.** Not a deployment's collected analytics,
database, logs, binaries, config files or third-party datasets — none of
those are ours to publish, and several would carry real visitors'
personal data. `.gitignore` enforces this rather than trusting it.
