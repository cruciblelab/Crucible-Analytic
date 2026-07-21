# Crucible Analytics — MVP Collector

A bot-aware web analytics collector: an open-source alternative to
Google Analytics that separately detects and surfaces bot/DDoS traffic.

This is the **first phase only**: `Collector → Cache/Score → TimescaleDB`.
There is no dashboard, no query API, and no path-scanning detection yet —
those are later phases.

## What it does

The collector runs in one of two modes, selected by `mode` in the config
file. Both fingerprint every connection via JA4, feed the same in-memory
`RateStore` (a cheap two-counter sliding window, not a full request log),
compute the same 0-100 bot-likelihood score (request rate + known-bad-JA4
match), and batch-write summaries to TimescaleDB via `COPY` every
`flush_interval_seconds` (default 10s). IP state idle for longer than
`ttl_seconds` (default 5m) is automatically dropped from memory. Nothing in
this phase blocks or rejects traffic based on the score - it's computed and
persisted for a future phase to act on.

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

# 3. Run it (looks for ./config.toml by default; override with -config).
go run ./cmd/collector
# or: go run ./cmd/collector -config /path/to/config.toml
```

If you're pointing `docker compose` at an already-existing TimescaleDB
instead, apply the schema once yourself:

```bash
psql "$TIMESCALE_DSN" -f internal/storage/schema.sql
```

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

## Testing

```bash
go test -race ./...
```

Coverage includes a hand-rolled ClientHello parser exercised against
independently-built byte fixtures (including truncation and multi-record
fragmentation) *and* against 5 real ClientHellos from FoxIO's official test
pcaps, cross-checked against both FoxIO's own reference implementation and
Wireshark's independent JA4 dissector
(`internal/ja4/foxio_reference_test.go`), sliding-window math with injected
timestamps (no sleeps), and end-to-end proxy tests for *both* modes that
perform a **real** TLS handshake (self-signed cert generated in-test,
stdlib only): passthrough's asserts byte-for-byte passthrough and a
correctly-shaped extracted JA4; full mode's asserts real backend responses,
a correctly-shaped JA4, that HTTP/2 actually gets negotiated, and - the
core point of full mode - that N requests over one connection produce N
separate `RecordRequest` calls, not one (`internal/fullproxy/server_test.go`).

`internal/storage`'s DB writer itself isn't covered by an automated
integration test — this repo was built in a sandbox without a usable Docker
daemon, so there was no live TimescaleDB to test against. The row-building
and flush-scheduling logic around it (`BuildRows`, `Flusher`) is unit
tested against a fake writer, and the pgx/`netip.Addr` ↔ `inet` encoding
path was verified by reading the pgx source rather than by running it.
**Run the collector against a real TimescaleDB (e.g. via the
`docker-compose.yml` here) before depending on it.**

## Explicitly out of scope for this phase

Dashboard, query API, path-scanning detection, header-consistency checks,
weighted multi-signal correlation, a Redis-backed `RateStore`
implementation, and an HTTPS (as opposed to plaintext) backend for full
mode. The `RateStore` interface and scoring package are shaped to make
these additive later, not to pre-build them now.
