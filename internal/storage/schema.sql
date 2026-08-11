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
    ip                INET             NOT NULL,
    ja4               TEXT             NOT NULL DEFAULT '',
    prev_window_count INTEGER          NOT NULL,
    curr_window_count INTEGER          NOT NULL,
    request_rate      DOUBLE PRECISION NOT NULL,
    bot_score         SMALLINT         NOT NULL,
    is_known_bot_ja4  BOOLEAN          NOT NULL DEFAULT FALSE,
    country           TEXT             NOT NULL DEFAULT '',
    asn               INTEGER          NOT NULL DEFAULT 0,
    asn_org           TEXT             NOT NULL DEFAULT ''
);

-- ADD COLUMN IF NOT EXISTS, not a version comment plus a manual-migration
-- instruction: unlike internal/asnlookup/schema.sql's BIGINT -> INET
-- change (a real type change on an existing column, not safely
-- automatable), these three columns are purely additive with defaults -
-- so this file is self-migrating. Running it again against a table
-- created before country/ASN enrichment existed just adds the columns in
-- place; no drop/recreate needed, here or in the README.
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS country TEXT NOT NULL DEFAULT '';
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS asn INTEGER NOT NULL DEFAULT 0;
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS asn_org TEXT NOT NULL DEFAULT '';

SELECT create_hypertable('traffic_snapshots', 'time', if_not_exists => TRUE);

-- Supports the "recent history for this IP" lookups a later dashboard/API
-- phase will need; without it, every such query forces a full chunk scan.
CREATE INDEX IF NOT EXISTS idx_traffic_snapshots_ip_time
    ON traffic_snapshots (ip, time DESC);
