-- Schema for the MVP collector's TimescaleDB sink. Apply this once,
-- separately from running the collector (e.g. `psql "$DATABASE_URL" -f
-- internal/storage/schema.sql`, or let deploy/docker-compose.yml's init
-- mount apply it automatically). The collector itself never runs DDL.

CREATE EXTENSION IF NOT EXISTS timescaledb;

-- One row per (flush interval, active IP): a periodic summary snapshot of
-- the collector's in-memory sliding-window state - NOT a per-request log.
-- Columns mirror the minimum per-IP state the MVP tracks (IP, the two
-- sliding-window counters, JA4) plus the score derived from it, plus
-- best-effort country/ASN enrichment. country/asn/asn_org are populated
-- only when asn_lookup.enabled = true (internal/asnlookup.Resolver wired
-- into the flusher) - '' / 0 otherwise, the same "empty means not
-- resolved" convention internal/asnlookup's own Result type uses. Each
-- column is independently best-effort: an IP found in the country dataset
-- but not the ASN one (or vice versa) still gets whichever half resolved.
CREATE TABLE IF NOT EXISTS traffic_snapshots (
    time              TIMESTAMPTZ      NOT NULL,
    site_id           TEXT             NOT NULL DEFAULT '',
    ip                INET             NOT NULL,
    ja4               TEXT             NOT NULL DEFAULT '',
    prev_window_count INTEGER          NOT NULL,
    curr_window_count INTEGER          NOT NULL,
    request_rate      DOUBLE PRECISION NOT NULL,
    bot_score         SMALLINT         NOT NULL,
    is_known_bot_ja4  BOOLEAN          NOT NULL DEFAULT FALSE,
    country           TEXT             NOT NULL DEFAULT '',
    asn               INTEGER          NOT NULL DEFAULT 0,
    asn_org           TEXT             NOT NULL DEFAULT '',
    is_known_bot_asn  BOOLEAN          NOT NULL DEFAULT FALSE
);

-- ADD COLUMN IF NOT EXISTS, not a version comment plus a manual-migration
-- instruction: unlike internal/asnlookup/schema.sql's BIGINT -> INET
-- change (a real type change on an existing column, not safely
-- automatable), these columns are purely additive with defaults - so this
-- file is self-migrating. Running it again against a table created before
-- country/ASN enrichment (or ASN scoring) existed just adds the columns
-- in place; no drop/recreate needed, here or in the README.
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS country TEXT NOT NULL DEFAULT '';
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS asn INTEGER NOT NULL DEFAULT 0;
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS asn_org TEXT NOT NULL DEFAULT '';
-- is_known_bot_asn mirrors is_known_bot_ja4: true when the resolved ASN
-- matched asn_lookup.known_bot_asns at flush time (only ever true when
-- asn_lookup.apply_to_scoring = true - see internal/scoring and the
-- README's "Optional: IP → country / ASN lookup").
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS is_known_bot_asn BOOLEAN NOT NULL DEFAULT FALSE;
-- site_id identifies which site a row belongs to, so one database can
-- serve several collectors (one VDS hosting more than one customer site,
-- each with its own collector process). Defaults to '' only so this
-- migration can run against an existing table; the collector itself
-- requires a non-empty site_id in its config, so no new row will ever
-- carry the default.
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS site_id TEXT NOT NULL DEFAULT '';

SELECT create_hypertable('traffic_snapshots', 'time', if_not_exists => TRUE);

-- Supports the "recent history for this IP" lookups a later dashboard/API
-- phase will need; without it, every such query forces a full chunk scan.
CREATE INDEX IF NOT EXISTS idx_traffic_snapshots_ip_time
    ON traffic_snapshots (ip, time DESC);

-- Supports the read API's primary access pattern - "everything for site X
-- between times A and B" - which the ip-leading index above can't serve,
-- since site_id isn't its leading column. Kept as a second index rather
-- than replacing the one above: per-IP history lookups are still a real
-- pattern, and adding an index is a self-migrating change while altering
-- an existing one isn't.
CREATE INDEX IF NOT EXISTS idx_traffic_snapshots_site_time
    ON traffic_snapshots (site_id, time DESC);

-- Hashed IP storage - see internal/beacon/schema.sql for the reasoning.
-- The two tables must agree exactly: they are the two halves of the
-- crossover join.
ALTER TABLE traffic_snapshots ALTER COLUMN ip DROP NOT NULL;
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS ip_hash BYTEA;
CREATE INDEX IF NOT EXISTS traffic_snapshots_ip_hash_idx
    ON traffic_snapshots (site_id, ip_hash, time DESC) WHERE ip_hash IS NOT NULL;
