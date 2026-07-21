# Crucible Analytics — MVP Collector

A bot-aware web analytics collector: an open-source alternative to
Google Analytics that separately detects and surfaces bot/DDoS traffic.

This is the **first phase only**: `Collector → Cache/Score → TimescaleDB`.
There is no dashboard, no query API, and no path-scanning detection yet —
those are later phases.

## What it does

The collector is a minimal TCP/TLS **passthrough** reverse proxy:

1. It listens on `LISTEN_ADDR` and accepts raw TCP connections.
2. For TLS connections, it peeks the TLS record(s) containing the
   ClientHello, parses it just enough to compute a
   [JA4](https://github.com/FoxIO-LLC/ja4) client fingerprint, and forwards
   every byte read (and everything that follows) to `BACKEND_ADDR`
   **unmodified**. It never terminates TLS, never buffers or rewrites
   application data, and never drops or delays a connection because
   fingerprinting failed — observation is best-effort and side-channel to
   the proxying.
3. Each connection's source IP is recorded into an in-memory `RateStore`
   using a cheap two-counter sliding window (previous window + current
   window request counts), not a full request log.
4. Every `FLUSH_INTERVAL` (default 10s), the collector snapshots every IP
   active since the last flush, computes a simple 0-100 bot-likelihood
   score (request rate + known-bad-JA4 match), and batch-writes the
   summaries to TimescaleDB via `COPY`.
5. IP state idle for longer than `IDLE_TTL` (default 5m) is automatically
   dropped from memory.

Nothing in this phase blocks or rejects traffic based on the score — it's
computed and persisted for a future phase to act on.

**Rate counting is per TCP connection, not per HTTP request.** Because the
proxy is deliberately content-blind (it never parses HTTP framing), it has
no way to see individual requests multiplexed over a keep-alive or HTTP/2
connection — each such connection counts once, however many requests it
carries. This under-counts legitimate browser traffic relative to bots
that churn through a new connection per request, which is directionally
fine for bot detection but worth knowing when interpreting the numbers. A
per-HTTP-request view would require an HTTP-aware (L7) proxy, which is out
of scope for this phase's TCP/TLS-level design.

## Design notes

- **Language: Go.** Goroutines suit the high-concurrency connection model,
  and it ships as a single static binary — deployment is "point it at your
  backend and run it."
- **JA4 parsing: hand-rolled, no dependency.** The ClientHello wire format
  is simple structurally (no crypto needed to parse it), so
  `internal/ja4` implements it directly against the public JA4 spec using
  only the standard library. This keeps the dependency footprint at zero
  for the most security-sensitive piece of the pipeline and gives full
  control over exactly which fields feed the fingerprint. It has **not**
  been cross-validated byte-for-byte against the official reference
  implementation (no network access to run it during development) — treat
  it as a strong, internally-consistent fingerprint for clustering/matching
  rather than a certified-compatible one. `internal/ja4`'s tests cover the
  parser and the fingerprint assembly (sorting, GREASE filtering, hashing)
  independently.
- **Cache: single in-memory store behind a `RateStore` interface.** Only
  `MemoryRateStore` exists today (no Redis), but callers depend on the
  interface so a distributed implementation can be added later without
  touching the proxy or flush code.
- **Locking: one `sync.RWMutex` over a plain map**, not sharded. Both are
  reasonable at small/medium scale per the project brief; a sharded map is
  the known follow-up if lock contention shows up under real load.
- **Database: TimescaleDB via `pgx/v5`**, using `COPY` (`pgx.CopyFrom`) for
  the periodic batch flush rather than row-by-row `INSERT`s. IP addresses
  are handled as `netip.Addr` end-to-end (proxy → RateStore → storage) and
  map straight onto Postgres's `inet` column type through pgx's built-in
  codec — this was verified by reading pgx's `pgtype` source directly
  rather than assumed. pgx is pinned to v5.7.6 (rather than latest) because
  the latest release bumps the required Go version to 1.25; v5.7.6 only
  needs 1.23, which keeps the toolchain requirement lower for whatever the
  target VPS already has installed.
- **The known-bot JA4 list (`scoring.KnownBotJA4`) is illustrative, not real
  threat intel.** It exists to demonstrate the scoring mechanism end to
  end. Replace/extend it with real fingerprint data (e.g. from
  [ja4db.com](https://ja4db.com)) before relying on it for anything.

## Running locally

Requires Go 1.23+.

```bash
# 1. Start a local TimescaleDB (applies internal/storage/schema.sql on
#    first boot via the Postgres image's docker-entrypoint-initdb.d hook).
docker compose up -d
docker compose ps --format '{{.Name}}: {{.Health}}' # wait for "healthy"

# 2. Run the collector against your backend.
export BACKEND_ADDR=127.0.0.1:8080      # your existing site
export DATABASE_URL="postgres://collector:collector@localhost:5432/analytics"
go run ./cmd/collector
```

If you're pointing `docker compose` at an already-existing TimescaleDB
instead, apply the schema once yourself:

```bash
psql "$DATABASE_URL" -f internal/storage/schema.sql
```

### Configuration

All configuration is via environment variables; only the first two are
required.

| Variable            | Default  | Meaning                                                     |
| ------------------- | -------- | ------------------------------------------------------------ |
| `BACKEND_ADDR`       | —        | **Required.** `host:port` of the site to proxy to.           |
| `DATABASE_URL`       | —        | **Required.** Postgres connection string for TimescaleDB.    |
| `LISTEN_ADDR`        | `:8443`  | Address the proxy accepts connections on.                    |
| `FLUSH_INTERVAL`     | `10s`    | How often summaries are batch-written to TimescaleDB.        |
| `WINDOW_SIZE`        | `60s`    | Sliding-window width used for rate estimation.                |
| `IDLE_TTL`           | `5m`     | How long an idle IP's state is kept before eviction.          |
| `CLEANUP_INTERVAL`   | `1m`     | How often the idle-TTL sweep runs.                            |
| `HANDSHAKE_TIMEOUT`  | `5s`     | Max time spent waiting to see a full ClientHello before giving up on fingerprinting (the connection is still proxied). |
| `DIAL_TIMEOUT`       | `10s`    | Max time to connect to `BACKEND_ADDR`.                        |

## Testing

```bash
go test -race ./...
```

Coverage includes a hand-rolled ClientHello parser exercised against
independently-built byte fixtures (including truncation and multi-record
fragmentation), sliding-window math with injected timestamps (no sleeps),
and an end-to-end proxy test that performs a **real** TLS handshake
(self-signed cert generated in-test, stdlib only) through the passthrough
proxy and asserts both byte-for-byte passthrough and a correctly-shaped
extracted JA4.

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
weighted multi-signal correlation, and a Redis-backed `RateStore`
implementation. The `RateStore` interface and scoring package are shaped
to make these additive later, not to pre-build them now.
