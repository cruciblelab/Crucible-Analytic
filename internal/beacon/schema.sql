-- Schema for the beacon ingest's TimescaleDB sink. Apply this once,
-- separately from running the beacon (e.g. `psql "$DATABASE_URL" -f
-- internal/beacon/schema.sql`). The beacon itself never runs DDL, the
-- same rule internal/storage/schema.sql states for the collector.
--
-- This lives in the same database as traffic_snapshots on purpose. The
-- two tables answer different questions about the same traffic -
-- traffic_snapshots says what connected, beacon_events says what a
-- browser that ran JavaScript actually did - and the whole point of
-- running both is being able to join them. `ip` is the join key, which
-- is why it is stored here even though a pageview-only analytics tool
-- would not need it.

CREATE EXTENSION IF NOT EXISTS timescaledb;

-- One row per beacon event: a pageview or a named custom event, sent by
-- the JS snippet from a real browser. Unlike traffic_snapshots (a
-- periodic per-IP summary), this IS a per-event log - a browser that
-- runs JavaScript is a bounded, self-limiting population, so the
-- unbounded-cardinality problem that rules out per-request logging on
-- the collector side does not apply here.
CREATE TABLE IF NOT EXISTS beacon_events (
    time          TIMESTAMPTZ NOT NULL,
    site_id       TEXT        NOT NULL,
    -- visitor_id is a cookieless, daily-rotating hash - see
    -- internal/beacon/visitor.go. It is stable within a day and
    -- unrecoverable afterwards by design, so it is not a durable
    -- identifier for a person.
    visitor_id    TEXT        NOT NULL,
    -- event_type is 'pageview' or 'event'; event_name is set only for
    -- 'event' (the custom-event name the site's own code passed).
    event_type    TEXT        NOT NULL,
    event_name    TEXT        NOT NULL DEFAULT '',
    -- path excludes the query string. query holds only allowlisted
    -- campaign parameters (utm_*, ref, gclid, fbclid), re-serialized -
    -- see internal/beacon.sanitizeQuery for why the raw query string is
    -- deliberately not stored.
    path          TEXT        NOT NULL DEFAULT '',
    query         TEXT        NOT NULL DEFAULT '',
    title         TEXT        NOT NULL DEFAULT '',
    -- Referrer is split so grouping by source ("google.com") is an index
    -- lookup rather than a string function over a full URL. The
    -- referrer's own query string is dropped entirely.
    referrer_host TEXT        NOT NULL DEFAULT '',
    referrer_path TEXT        NOT NULL DEFAULT '',
    -- The join key back to traffic_snapshots.
    ip            INET        NOT NULL,
    -- Classified from the User-Agent header server-side; the browser
    -- never sends these. '' means "not recognized", not "absent" - see
    -- internal/beacon/useragent.go.
    browser       TEXT        NOT NULL DEFAULT '',
    os            TEXT        NOT NULL DEFAULT '',
    device        TEXT        NOT NULL DEFAULT '',
    -- is_bot_ua is TRUE when the User-Agent self-identifies as a bot.
    -- Such events are flagged and kept, never dropped: a client that
    -- both runs JavaScript and admits to being a bot is exactly the
    -- population this project exists to make visible, and a reader that
    -- wants "humans only" can filter on this column.
    is_bot_ua     BOOLEAN     NOT NULL DEFAULT FALSE,
    screen_w      INTEGER     NOT NULL DEFAULT 0,
    screen_h      INTEGER     NOT NULL DEFAULT 0,
    language      TEXT        NOT NULL DEFAULT '',
    -- Best-effort enrichment from internal/asnlookup, exactly as in
    -- traffic_snapshots: '' / 0 means not resolved.
    country       TEXT        NOT NULL DEFAULT '',
    asn           INTEGER     NOT NULL DEFAULT 0,
    asn_org       TEXT        NOT NULL DEFAULT ''
);

SELECT create_hypertable('beacon_events', 'time', if_not_exists => TRUE);

-- The primary read pattern: "everything for site X between A and B".
CREATE INDEX IF NOT EXISTS idx_beacon_events_site_time
    ON beacon_events (site_id, time DESC);

-- Sessions are derived at read time from the gaps between one visitor's
-- events (rather than assigned at ingest, which would make the ingest
-- path stateful). That derivation is a window function partitioned by
-- visitor_id and ordered by time, which is exactly this index.
CREATE INDEX IF NOT EXISTS idx_beacon_events_visitor_time
    ON beacon_events (site_id, visitor_id, time);

-- Supports joining an IP's beacon events against its traffic_snapshots
-- rows - the "did this IP also run JavaScript?" question that neither
-- table can answer alone.
CREATE INDEX IF NOT EXISTS idx_beacon_events_ip_time
    ON beacon_events (ip, time DESC);
