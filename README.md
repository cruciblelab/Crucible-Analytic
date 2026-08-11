# Crucible Analytics — MVP Collector

A bot-aware web analytics collector: an open-source alternative to
Google Analytics that separately detects and surfaces bot/DDoS traffic.

This is the **first phase only**: `Collector → Cache/Score → TimescaleDB`.
There is no dashboard, no query API, and no path-scanning detection yet —
those are later phases.

## What this does — and doesn't do

**This project does:**
- Passively observe traffic and separate bot/human activity using JA4 TLS
  fingerprinting plus behavioral signals (request rate), without blocking or
  delaying anything based on that assessment.
- Surface bot/scraper traffic transparently, alongside a 0-100 score and the
  reason behind it (known-bad JA4 match, rate, or both).
- In full mode, record analytics **per individual HTTP request** rather than
  per TCP connection - an accurate request count even across HTTP/1.1
  keep-alive or HTTP/2 multiplexed connections (see "Full mode" below). It
  does not yet break requests down by page/path - that's path-scanning
  detection, listed under "Explicitly out of scope for this phase" below.
- Persist the score and underlying data to TimescaleDB, a real
  Postgres-compatible database your own systems can query directly. A
  purpose-built query API is a later phase, not something already built here
  (see "Explicitly out of scope for this phase").

**This project does not:**
- **Stop network-level (volumetric) DDoS attacks.** Traffic in the
  hundreds-of-thousands-to-millions-of-packets-per-second range needs
  globally distributed infrastructure (Anycast, multiple scrubbing data
  centers) to absorb - that isn't something a single process on a single
  server can do, by design or otherwise.
- **Act as an anti-DDoS service on its own.** It detects and scores traffic;
  it does not block or drop it. Any blocking decision is left to your own
  system - a WAF, a firewall, the backend application itself.
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
Nothing in this phase blocks or rejects traffic based on the score - it's
computed and persisted for a future phase to act on.

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

What's still deliberately out of scope this phase is *acting* on either
signal - as opposed to just recording it, which is what the paragraph
above does: there's no country/ASN rule wired into `internal/limiter`,
and `internal/scoring` doesn't consult either signal yet (see
`asn_lookup.apply_to_scoring` just below). Resolving IP → country/ASN and
deciding something based on it are kept as separate steps on purpose -
this phase covers resolving it and recording it, not deciding with it.

`asn_lookup.apply_to_scoring` is accepted and validated but still not
consulted by `internal/scoring` - country and ASN are both real signals
now, and recorded in every snapshot, but wiring either into a scoring
*decision* (or into a rate-limit rule) is later work, not this round's.
It's part of the config shape now so turning it on later is a behavior
change, not a schema one.

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
- **Config: a TOML file, not environment variables.** `internal/config`
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
- **The known-bot JA4 list (`scoring.KnownBotJA4`) is real data, not a
  placeholder** — 51 unique JA4 fingerprints loaded at build time (via
  `go:embed`) from `internal/scoring/known_bots.json`, sourced from [The
  Bot Aquarium](https://thebotaquarium.com/fingerprint/archive)'s public
  fingerprint archive (community-submitted, classification-tagged; entries
  classified `browser` are excluded since they're legitimate reference
  data, not a bot signal). `ja4db.foxio.io` — the JA4 spec authors' own
  database, and the intended primary source — turned out to require an
  account for any bulk/API access (every endpoint returned HTTP 403
  "Authentication credentials were not provided" without one), so it isn't
  included here. **This is a one-time snapshot (retrieved 2026-07-21), not
  a live feed** — there's no automatic update mechanism in this MVP, and
  both sources' data ages; periodic manual refresh (and adding ja4db once
  access is available) is expected follow-up work, not something to build
  into this phase. See `internal/scoring/known_bots.json`'s own `note`
  field for the exact sourcing/exclusion details baked into the data
  itself.

## Running locally

Requires Go 1.23+.

```bash
# 1. Start a local TimescaleDB (applies internal/storage/schema.sql on
#    first boot via the Postgres image's docker-entrypoint-initdb.d hook).
docker compose up -d
docker compose ps --format '{{.Name}}: {{.Health}}' # wait for "healthy"

# 2. Copy the example config and fill in backend_addr / timescale_dsn
#    (and tls.cert_file/key_file if you're using mode = "full").
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
```

If you're pointing `docker compose` at an already-existing TimescaleDB
instead, apply both schemas once yourself (skip the second if you're
leaving `asn_lookup.enabled = false`):

```bash
psql "$TIMESCALE_DSN" -f internal/storage/schema.sql
psql "$TIMESCALE_DSN" -f internal/asnlookup/schema.sql
```

**If you already had this collector running before country/ASN
enrichment existed** (i.e. `docker compose up -d` already created
`traffic_snapshots` on an earlier version, so step 1's
`docker-entrypoint-initdb.d` hook won't fire again against that existing
volume), re-apply `internal/storage/schema.sql` yourself the same way:
`psql "postgres://collector:collector@localhost:5432/analytics" -f internal/storage/schema.sql`.
It's self-migrating (see "Optional: IP → country / ASN lookup" above), so
this just adds the three new columns in place - no drop, no data loss.

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
| `asn_lookup.apply_to_scoring`       | `false`              | Accepted and validated, but not yet consulted by scoring - see "Optional: IP → country / ASN lookup" below. |
| `asn_lookup.cache_max_entries`      | `50000`              | Only validated when `asn_lookup.enabled = true`. Size of the in-memory LRU result cache. |
| `asn_lookup.cache_ttl_seconds`      | `21600` (6h)         | Same; how long one resolved IP is cached before being re-checked against the range tables. |
| `asn_lookup.refresh_interval_seconds` | `604800` (1 week)  | Same; how often both datasets are re-fetched (downloaded, or re-read from `local_csv_path`) and re-parsed. |
| `asn_lookup.local_csv_path`         | `""`                 | If set, skip downloading entirely and read `user-country-ipv4/6.csv` and `origin-asn-ipv4/6.csv` from this directory instead - no network access at all in that mode. |

## Testing

```bash
go test -race ./...
```

This needs no external dependencies - no Docker, no network access,
nothing running. Two further suites exist for real, live-dependency
verification, each gated behind its own build tag specifically so the
default suite above stays that way:

```bash
# Needs a real TimescaleDB: docker compose up -d first (see "Running
# locally"), plus internal/asnlookup/schema.sql applied (creates both
# tables) if you want that package's suite too.
go test -tags integration ./internal/storage/... ./internal/asnlookup/... -v

# No external dependency - just slower and more timing-sensitive than the
# default suite should be, since it fires dozens of genuinely concurrent
# connections at a real listening proxy.Server, or drives a realistic
# 100k-request cache access pattern.
go test -tags loadtest ./internal/loadtest/... ./internal/asnlookup/... -v
```

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

`go test -tags integration ./internal/storage/... ./internal/asnlookup/...`
confirms both packages' pgx `netip.Addr` ↔ `inet` encoding round-trips
correctly against a real TimescaleDB - read back through a raw query, not
just the same path that wrote it, for `ip_country_ranges` and
`ip_asn_ranges` independently (including an ASN organization name
containing a literal comma, the same real-data shape `parseASNCSV` is
tested against) - and that `internal/storage/schema.sql`'s
`create_hypertable` call actually took effect, not just that a plain table
would have accepted the same writes. It also covers `traffic_snapshots`'s
own `country`/`asn`/`asn_org` columns both ways: a row with real values
round-trips correctly, and a row built the way `BuildRows` produces one
with no resolver (`asn_lookup.enabled = false`) reads back as `''`/`0`/`''`
rather than `NULL` or some other encoding surprise.

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

## Explicitly out of scope for this phase

Dashboard, query API, path-scanning detection, header-consistency checks,
weighted multi-signal correlation, a Redis-backed `RateStore`
implementation, an HTTPS (as opposed to plaintext) backend for full mode,
and *acting* on the country/ASN data `internal/asnlookup` now resolves -
no country/ASN-based rate-limit rule wired into `internal/limiter`, and no
ASN-based scoring signal wired into `internal/scoring` yet (see "Optional:
IP → country / ASN lookup" above; resolving the data and acting on it are
kept as separate phases on purpose). The `RateStore` interface and scoring
package are shaped to make these additive later, not to pre-build them
now.
