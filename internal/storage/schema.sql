-- Schema for the MVP collector's TimescaleDB sink. Apply this once,
-- separately from running the collector (e.g. `psql "$DATABASE_URL" -f
-- internal/storage/schema.sql`, or let deploy/docker-compose.yml's init
-- mount apply it automatically). The collector itself never runs DDL.

CREATE EXTENSION IF NOT EXISTS timescaledb;

-- One row per (flush interval, active IP): a periodic summary snapshot of
-- the collector's in-memory sliding-window state - NOT a per-request log.
-- Columns mirror the minimum per-IP state the MVP tracks (IP, the two
-- sliding-window counters, JA4) plus the score derived from it.
CREATE TABLE IF NOT EXISTS traffic_snapshots (
    time              TIMESTAMPTZ      NOT NULL,
    ip                INET             NOT NULL,
    ja4               TEXT             NOT NULL DEFAULT '',
    prev_window_count INTEGER          NOT NULL,
    curr_window_count INTEGER          NOT NULL,
    request_rate      DOUBLE PRECISION NOT NULL,
    bot_score         SMALLINT         NOT NULL,
    is_known_bot_ja4  BOOLEAN          NOT NULL DEFAULT FALSE
);

SELECT create_hypertable('traffic_snapshots', 'time', if_not_exists => TRUE);

-- Supports the "recent history for this IP" lookups a later dashboard/API
-- phase will need; without it, every such query forces a full chunk scan.
CREATE INDEX IF NOT EXISTS idx_traffic_snapshots_ip_time
    ON traffic_snapshots (ip, time DESC);
