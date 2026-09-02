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
    -- campaign parameters, re-serialized - see
    -- internal/beacon.CampaignPolicy for why the raw query string is
    -- deliberately not stored.
    --
    -- query keeps the exact combination a link carried, so "which
    -- precise campaign URL performed best" stays answerable. The typed
    -- columns below answer the questions it cannot: grouping by one
    -- dimension at a time.
    path          TEXT        NOT NULL DEFAULT '',
    query         TEXT        NOT NULL DEFAULT '',
    title         TEXT        NOT NULL DEFAULT '',

    -- Campaign dimensions, split out of query so each can be grouped and
    -- filtered on its own. '' means the parameter was absent (or the
    -- deployment's CampaignPolicy drops it), the same "empty means not
    -- present" convention every other text column here uses.
    utm_source    TEXT        NOT NULL DEFAULT '',
    utm_medium    TEXT        NOT NULL DEFAULT '',
    utm_campaign  TEXT        NOT NULL DEFAULT '',
    utm_term      TEXT        NOT NULL DEFAULT '',
    utm_content   TEXT        NOT NULL DEFAULT '',
    ref           TEXT        NOT NULL DEFAULT '',
    -- Which ad network's click identifier was present: 'google',
    -- 'facebook', 'microsoft' or ''. This is the groupable half of a
    -- paid click. The identifier itself is unique per click, so grouping
    -- by it would produce one row per visit and answer nothing.
    click_source  TEXT        NOT NULL DEFAULT '',
    -- The raw per-click identifier, stored only when the deployment sets
    -- campaign.store_click_ids = true.
    --
    -- Off by default deliberately. It is unique per click by
    -- construction and resolvable to a person by the ad network that
    -- issued it (never by us), and its only legitimate use - uploading
    -- offline conversions back to that network - is something this
    -- project does not do. Storing an identifier nothing consumes is
    -- the kind of thing a data inventory has to explain and cannot
    -- justify.
    click_id      TEXT        NOT NULL DEFAULT '',
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

-- Self-migrating, the same convention internal/storage/schema.sql
-- follows: CREATE TABLE IF NOT EXISTS does nothing to a table that
-- already exists, so a column added to the definition above reaches an
-- existing deployment only through an explicit ALTER. These are purely
-- additive with defaults, so re-running this file against a table
-- created before campaign columns existed just adds them in place.
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS utm_source   TEXT NOT NULL DEFAULT '';
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS utm_medium   TEXT NOT NULL DEFAULT '';
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS utm_campaign TEXT NOT NULL DEFAULT '';
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS utm_term     TEXT NOT NULL DEFAULT '';
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS utm_content  TEXT NOT NULL DEFAULT '';
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS ref          TEXT NOT NULL DEFAULT '';
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS click_source TEXT NOT NULL DEFAULT '';
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS click_id     TEXT NOT NULL DEFAULT '';

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

-- Campaign traffic is a small fraction of all traffic, so a partial
-- index over the rows that carry any acquisition context at all keeps
-- "which campaigns brought people this month" from scanning every
-- pageview. One index covering the three dimensions actually filtered
-- on, rather than one per column: the panel filters by source, medium
-- and campaign name together far more often than by any alone.
CREATE INDEX IF NOT EXISTS idx_beacon_events_campaign
    ON beacon_events (site_id, utm_source, utm_medium, utm_campaign, time DESC)
    WHERE utm_source <> '' OR utm_medium <> '' OR utm_campaign <> ''
       OR ref <> '' OR click_source <> '';

-- The keyed token written in full mode (privacy.ip_storage = "full").
--
-- This comment described a different design until 2026-09-02: a
-- "hashed" mode in which ip was left NULL, this column carried
-- HMAC(key, masked_ip), and the crossover join moved here. None of that
-- is true, and none of it has been for some time. What is true:
--
--   * There are two modes, masked and full. See internal/privacy.
--   * ip is never NULL in either. It always holds the masked network,
--     because no mode stores a raw address.
--   * In full mode this column additionally holds a token derived from
--     the WHOLE address, which is the point: it tells two visitors
--     inside one /24 apart, which the masked address in the same row
--     cannot.
--   * The crossover join therefore stays on ip. This column adds
--     precision to it; it does not replace it.
--
-- The column stays nullable and ip stays nullable, which is what the two
-- ALTERs below are for - masked mode simply leaves this one NULL, and
-- the partial index below skips those rows.
--
-- NULL rather than a placeholder, deliberately. A query that forgets
-- this column is empty in masked mode returns nothing, which is visibly
-- wrong; a shared placeholder would join every row to every other row
-- and return a plausible number that is completely false.
ALTER TABLE beacon_events ALTER COLUMN ip DROP NOT NULL;
ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS ip_hash BYTEA;
CREATE INDEX IF NOT EXISTS beacon_events_ip_hash_idx
    ON beacon_events (site_id, ip_hash, time DESC) WHERE ip_hash IS NOT NULL;
