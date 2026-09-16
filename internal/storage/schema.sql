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

-- The keyed token written in full mode - see internal/beacon/schema.sql
-- for what it is and what it is not. The two tables must agree exactly:
-- they are the two halves of the crossover join, and the two processes
-- that fill them are now held to the same config rules by
-- internal/invariants.
--
-- # Why the ALTER COLUMN is wrapped in a test rather than run outright
--
-- TimescaleDB refuses ALTER COLUMN on a hypertable that has compression
-- enabled - and refuses it unconditionally, including when the column
-- is already nullable and the statement would change nothing. Measured
-- on 2.17.2: DROP NOT NULL, ALTER COLUMN TYPE, ADD CONSTRAINT CHECK and
-- ENABLE ROW LEVEL SECURITY are all rejected; ADD COLUMN, DROP COLUMN,
-- SET DEFAULT, CREATE INDEX, GRANT and OWNER TO are not.
--
-- Since O1 every deployment whose TimescaleDB can compress does
-- compress this table, from the first minute the collector runs. So an
-- unguarded statement here does not fail on some exotic install: it
-- fails on the *second* schema upgrade of every ordinary one, which is
-- the panel's Health -> Schema upgrade button, which is the only
-- upgrade path a customer has.
--
-- The guard makes it a no-op once applied, which is what it already was
-- in effect. Holding the general rule is a test, not this comment:
-- internal/applier's TestAnUpgradeStillWorksOnACompressedDeployment
-- applies every schema file to a compressed database and requires it to
-- succeed - so the next statement of this shape is caught when it is
-- written rather than when a customer presses the button.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
                WHERE attrelid = 'traffic_snapshots'::regclass
                  AND attname  = 'ip'
                  AND attnotnull) THEN
        ALTER TABLE traffic_snapshots ALTER COLUMN ip DROP NOT NULL;
    END IF;
END
$$;
ALTER TABLE traffic_snapshots ADD COLUMN IF NOT EXISTS ip_hash BYTEA;
CREATE INDEX IF NOT EXISTS traffic_snapshots_ip_hash_idx
    ON traffic_snapshots (site_id, ip_hash, time DESC) WHERE ip_hash IS NOT NULL;

-- traffic_rollup is the pre-aggregated answer to the questions a long
-- range asks, at a bucket width fine enough to be summed into any
-- customer's day.
--
-- # What broke, measured
--
-- 2026-09-09, 12 million rows over 90 days, three sites, the
-- analytics-api binary's own response time: the summary endpoint took
-- 26,96 s at 90 days and 7,97 s at 30. The panel's client gives up at
-- 5 s, so two of its four range buttons did not work - and the panel
-- explained it as "the data source cannot be reached", which is a reason
-- that was not true for numbers that were sitting right there.
--
-- The cause is not size. It is that the hypertable orders rows by time
-- while every query asks by site, so one site's rows are interleaved
-- with every other site's: reading 20.748 rows for one site touched
-- 13.632 heap pages. O1 laid each site's rows together under compression
-- and halved the time. Halving 27 seconds is not enough.
--
-- # Why a table and not a continuous aggregate
--
-- The obvious tool is TimescaleDB's continuous aggregate, and it is not
-- available. Measured on 2.17.2, a cluster started with
-- timescaledb.license=apache:
--
--	CREATE MATERIALIZED VIEW ... WITH (timescaledb.continuous)
--	ERROR: functionality not supported under the current "apache" license
--
-- The same wall O1 hit with compression, and the same reasoning: a
-- statement that aborts under the Apache build cannot sit in a schema
-- file, because it would fail every install and every schema upgrade on
-- a deployment whose PostgreSQL happens to carry that build. Compression
-- could be moved out to a run-time wrapper and skipped where absent. A
-- rollup cannot be skipped the same way: the read path would then have
-- two shapes, one for deployments that have it and one for those that do
-- not, and the two would be free to disagree.
--
-- An ordinary table filled by ordinary SQL behaves identically on both
-- builds. That is the whole reason it is one.
--
-- # Why the bucket is 15 minutes
--
-- A rollup bucket has to divide the boundary of whatever the query asks
-- for, and since O2a the query asks in the customer's own zone: a day is
-- their day, and a local midnight lands wherever their offset puts it.
--
-- An hourly bucket serves a zone whose offset is a whole hour and fails
-- India (+05:30), Nepal (+05:45) and Lord Howe (+10:30). A quarter-hour
-- bucket serves every zone if and only if no zone's offset is off a
-- 15-minute grid, and that is a claim about the tz database rather than
-- about arithmetic - so it was measured, not assumed. Every zone
-- PostgreSQL knows, sampled daily across the retention window:
--
--	499 zones, 379.739 samples, 0 offsets not a multiple of 900 s
--
-- internal/api holds it as a test against the real database, with the
-- zone list read from pg_timezone_names rather than written down. A
-- future tz release that reintroduces an odd offset turns that test red,
-- which is the only warning anybody would get.
--
-- # Why these four numbers and not the other three
--
-- The summary reports six figures. Four of them are aggregable - a
-- bucket's answer plus the next bucket's answer is the pair's answer -
-- and three of those four are here plus the count they are weighted by:
--
--	snapshots     count(*)                    summed
--	sum_rate      sum(request_rate)           summed, then / snapshots
--	max_rate      max(request_rate)           max of maxes
--	max_window    max over flushes of the     max of maxes
--	              window counters
--
-- The average is stored as a sum and a count rather than as an average,
-- because an average of averages weights a quiet bucket the same as a
-- busy one.
--
-- unique_ips and bot_ips are not here and cannot be. count(distinct ip)
-- does not aggregate: two days' visitors are not the sum of each day's,
-- because the same person may have come on both. And bot_ips has a
-- second, independent reason - it counts IPs whose peak score reached a
-- threshold that arrives *in the request*, so there is no one number to
-- precompute. A rollup column cannot depend on a query parameter.
--
-- What that costs is measured and stated rather than left to be
-- discovered: on the 12-million-row set the summary's own halves are
--
--	unique and bot counts   25,36 s   (stays, until O3)
--	rate aggregates          1,46 s   (goes)
--	peak window             10,26 s   (goes)
--
-- so this table removes about twelve seconds of thirty-seven and the
-- endpoint is still over the panel's limit. O3 is what answers the
-- other twenty-five, and it has a product decision in it that is not
-- the developer's to take.
--
-- The peak-window figure above is the one to distrust: the data it was
-- measured on gives every row its own timestamp, and a real collector
-- writes one row per active IP per flush - about fourteen of them
-- sharing a timestamp. So its GROUP BY builds fourteen times as many
-- groups here as it would in production. The direction is right and the
-- number is quoted as what it is.
CREATE TABLE IF NOT EXISTS traffic_rollup (
    -- The bucket's start, in UTC, on a 15-minute grid.
    bucket      TIMESTAMPTZ NOT NULL,
    site_id     TEXT        NOT NULL,
    snapshots   BIGINT      NOT NULL,
    sum_rate    DOUBLE PRECISION NOT NULL,
    max_rate    DOUBLE PRECISION NOT NULL,
    max_window  BIGINT      NOT NULL,
    -- site first: every query asks for one site over a range of
    -- buckets, which is exactly the order that makes this a range scan
    -- rather than the scattered read the raw table forces.
    PRIMARY KEY (site_id, bucket)
);

-- traffic_rollup_state is how far the rollup has been materialized, per
-- site.
--
-- # Why it is stored rather than computed
--
-- The read path needs to know where the rollup stops and the raw table
-- takes over. It could guess - "everything older than an hour is surely
-- rolled up" - and that guess is wrong exactly when it matters: after a
-- restart, after a long outage, on the first run against an existing
-- table. A guess that is wrong makes the dashboard show numbers that
-- are missing rows, silently, and nothing anywhere would say so.
--
-- Stored, the split is a fact rather than an estimate. If the refresh is
-- behind, the raw table simply covers more of the range and the answer
-- is slower; if it is current, the rollup covers more and the answer is
-- fast. Either way the answer is complete. That is the difference
-- between a dashboard that is sometimes stale and one that is sometimes
-- slow, and only one of those is honest.
--
-- Per site rather than one row for the deployment, because the refresh
-- walks sites one at a time - as the retention sweep already does - and
-- a single watermark would be dragged back to whichever site is
-- furthest behind.
CREATE TABLE IF NOT EXISTS traffic_rollup_state (
    site_id             TEXT        PRIMARY KEY,
    -- Every bucket strictly before this instant is materialized in
    -- traffic_rollup. Buckets at or after it are not, and the raw table
    -- is the only place that knows about them.
    materialized_before TIMESTAMPTZ NOT NULL,
    refreshed_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The privileges for the two tables above, here rather than only in
-- release/sql/grants.sql.
--
-- # Why a schema file grants at all
--
-- There are two ways a deployment's database gets its shape, and only
-- one of them runs grants.sql:
--
--   fresh install   release/install.sh: every schema file, then
--                   release/sql/grants.sql
--   upgrade         cmd/upgrader: the embedded schema files, and
--                   nothing else
--
-- So a table created by a schema file whose GRANT lives only in
-- grants.sql exists on an upgraded deployment with no role able to
-- touch it. Measured on a database built at v0.23.0 and upgraded to
-- this tree: traffic_rollup and traffic_rollup_state came out with
-- collector holding nothing and analytics_reader holding nothing,
-- which is the collector's refresh cycle and the summary endpoint,
-- both broken, on every existing installation.
--
-- grants.sql keeps its copy: it is the privilege matrix somebody reads
-- to answer "what can this role do", and it is what a fresh install
-- applies. The two are held equal by internal/upgradepath, which builds
-- both kinds of database and diffs their privileges - a second copy
-- nobody compares is the shape this project has found broken more than
-- once.
--
-- DO blocks and the has_table_privilege question for the reasons
-- written out in internal/retention/schema.sql: the roles may not exist
-- in a development database, and a GRANT that changes nothing still
-- rewrites the ACL tuple, which two appliers running at once collide
-- on.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'collector') THEN
        IF NOT has_table_privilege('collector', 'traffic_rollup', 'INSERT') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON traffic_rollup TO collector;
        END IF;
        IF NOT has_table_privilege('collector', 'traffic_rollup_state', 'INSERT') THEN
            GRANT SELECT, INSERT, UPDATE ON traffic_rollup_state TO collector;
        END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'analytics_reader') THEN
        IF NOT has_table_privilege('analytics_reader', 'traffic_rollup', 'SELECT') THEN
            GRANT SELECT ON traffic_rollup TO analytics_reader;
        END IF;
        IF NOT has_table_privilege('analytics_reader', 'traffic_rollup_state', 'SELECT') THEN
            GRANT SELECT ON traffic_rollup_state TO analytics_reader;
        END IF;
    END IF;
END
$$;

-- visitor_sketch is the one question the rollup above could not hold: how
-- many different people, over a range nobody has counted yet.
--
-- # Why count(distinct ip) needed something else entirely
--
-- Two days' visitors are not the sum of each day's - the same person may
-- have come on both - so a distinct count cannot be added up the way a
-- sum or a maximum can. That is why traffic_rollup carries four figures
-- and not six, and why, after O2 had removed twelve seconds from the
-- summary endpoint, this half was still the slow one.
--
-- Measured 2026-09-16 with the analytics-api binary's own response time,
-- cold (PostgreSQL stopped, caches dropped, restarted), three repeats,
-- against 11,0 million rows for one site over 90 days:
--
--	 30 days   3,70 M rows   2,24 s
--	 90 days  10,96 M rows   5,68 s
--
-- The panel's per-call timeout is 5 s. So the 90-day button does not
-- work, and the panel reports it as "the data source cannot be
-- reached" - a reason that is not true for numbers that are sitting
-- right there.
--
-- # What this table holds, and what it costs
--
-- One HyperLogLog sketch per site per UTC day, at 65.536 registers, and
-- a second one restricted to the addresses that reached the default bot
-- threshold. Sketches merge: the estimate for a range is the estimate of
-- the union of its days' sketches, which is the property a distinct
-- count does not have.
--
-- Measured on the same data, same harness, after the fact: 91 days for
-- one site is 6,0 MB - 0,76 % of the 792 MB the raw table takes for it -
-- and the 90-day summary answers in 0,47 s instead of 5,68 s, with the
-- estimate landing 0,032 % from the exact count. The 30-day range did
-- not move and is still counted exactly (2,24 s before, 2,43 s after,
-- against a spread of 0,2 s between repeats): the budget below is what
-- keeps it that way.
--
-- # Why the precision is 65.536 and not less
--
-- Because the owner's criterion was "as little error as possible, with
-- a sensible ratio", and the curve was measured (twenty genuinely
-- different draws per precision, in the dense regime, which is the one
-- a growing site ends up in):
--
--	p=4096     1,15 % avg   2,08 % worst    279 kB/90d    5,5 ms
--	p=16384    0,57 % avg   1,38 % worst   1,08 MB/90d   19,5 ms
--	p=65536    0,36 %                      4,40 MB/90d   87,8 ms
--	p=262144   (sparse only)               8,40 MB/90d   9.537 ms
--
-- p=262144 was excluded by measurement rather than by taste: its merge
-- alone eats the whole budget. p=65536 halves p=16384's error while
-- both of its cost ratios stay small - 0,55 % of the raw table, 1,8 % of
-- the 5 s the panel will wait.
--
-- # Why daily buckets when the rollup above needed quarter-hours
--
-- Because a sketch is three orders of magnitude bigger than four
-- numbers. A quarter-hour sketch at this precision would be 4,4 MB per
-- site per *day*, and merging 8.640 of them per 90-day query. The
-- rollup's grid has to divide a local midnight because its buckets are
-- all it reads; this one does not, because the ragged ends a local day
-- leaves - at most two partial UTC days - are read from the raw table
-- and turned into sketches of their own before the merge. An address
-- appearing both in an edge and in a whole day is therefore counted
-- once, which is the property that made this design possible at all.
--
-- # Why the second sketch is bot_ips and there is no third for humans
--
-- An address counts as a bot if *any* snapshot of it in range reached
-- the threshold, and a maximum over a range is the maximum of the
-- per-day maxima - so the union of the daily bot sketches is exactly the
-- range's bot set. The human set is the one that does not survive:
-- "never reached the threshold in range" is an *intersection* of the
-- daily human sets, and HyperLogLog cannot intersect. A daily
-- human-sketch would count an address that was quiet on Monday and
-- noisy on Tuesday in both halves, and the page's three numbers would
-- stop adding up.
--
-- So the human figure is a subtraction, and the read path takes the
-- consequence seriously: see internal/api/sketch.go for why bot is
-- clamped to unique before subtracting, and why all three figures are
-- reported as estimated together or not at all.
--
-- # Why the whole thing is conditional
--
-- hyperloglog comes from timescaledb_toolkit, a separate extension that
-- an existing deployment will not have. A schema file that named that
-- type unconditionally would abort on every install and - worse - on
-- every schema upgrade of a database without it, which is the wall O1
-- hit with compression and O2 hit with continuous aggregates.
--
-- The difference from O2 is that a *missing summary* can be skipped
-- here, and skipping it does not give the read path two shapes free to
-- disagree: the exact count is still the definition, this is an
-- approximation of it, and the API says in every response which one a
-- given number is. A deployment without the extension gets exact
-- numbers slowly - precisely what it has today.
--
-- EXECUTE rather than a plain CREATE TABLE inside the IF: PL/pgSQL
-- resolves a type name when it first prepares the statement, and a
-- branch that is never taken is never prepared - but this project has
-- already been bitten by assuming a guard reaches a name resolver
-- (`WHERE false` does not stop PostgreSQL from resolving a missing
-- table). A dollar-quoted string cannot be resolved early by
-- construction.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = 'timescaledb_toolkit') THEN
        RETURN;
    END IF;

    EXECUTE $ddl$
        CREATE TABLE IF NOT EXISTS visitor_sketch (
            -- The UTC day this sketch covers, at midnight.
            day     TIMESTAMPTZ NOT NULL,
            site_id TEXT        NOT NULL,
            -- Every address seen that day.
            ips     hyperloglog NOT NULL,
            -- Those that reached the default bot threshold that day.
            -- Nullable, because a day with no bots has no sketch rather
            -- than an empty one, and rollup() skips a NULL input.
            bot_ips hyperloglog,
            -- site first, for the same reason traffic_rollup does it:
            -- every query is one site over a range of days.
            PRIMARY KEY (site_id, day)
        )
    $ddl$;

    -- How far the sketch has been materialized, per site. The reasoning
    -- is traffic_rollup_state's, word for word: a stored split makes a
    -- refresh that is behind answer *slower* rather than shorter, and a
    -- guessed one makes the dashboard quietly drop rows. Its own table
    -- rather than a column on the one above, because a day with no
    -- traffic writes no sketch row at all, so max(day) says nothing
    -- about how far a refresh got.
    EXECUTE $ddl$
        CREATE TABLE IF NOT EXISTS visitor_sketch_state (
            site_id             TEXT        PRIMARY KEY,
            materialized_before TIMESTAMPTZ NOT NULL,
            -- The bot threshold the rows for this site were built at.
            --
            -- # Why a stored aggregate has to carry it
            --
            -- bot_ips is the set of addresses that reached a particular
            -- cutoff, so the sketch means nothing without the number it
            -- was built from. Without this column, changing the
            -- product's default cutoff would leave the panel showing
            -- bot counts computed at the old one while labelling them
            -- with the new - two definitions of one figure, which is
            -- the failure C9.3 was about.
            --
            -- It is also the whole of the read path's condition. A
            -- request may ask for any cutoff; the sketch may be used
            -- exactly when the one it was built at is the one being
            -- asked for. That single comparison covers both "this
            -- caller wants a different threshold" and "the product's
            -- default has moved since these rows were written", and
            -- neither needs a rule of its own.
            --
            -- A mismatch is self-healing and needs no DELETE: the
            -- refresh resets the watermark to the site's oldest row and
            -- walks forward again, overwriting each day. Rows ahead of
            -- the watermark are stale, and the read path does not read
            -- past a watermark - which is the same property that makes
            -- a refresh that is merely behind safe.
            bot_score_min       SMALLINT    NOT NULL,
            refreshed_at        TIMESTAMPTZ NOT NULL DEFAULT now()
        )
    $ddl$;

    -- The privileges, here as well as in release/sql/grants.sql, for the
    -- reason written out above traffic_rollup's: an upgrade runs the
    -- schema files and nothing else, so a table whose GRANT lives only
    -- in grants.sql exists on an upgraded deployment with no role able
    -- to touch it.
    --
    -- No RLS on either, and the criterion is the one measured in L6
    -- rather than an omission: RLS is what this schema uses where a
    -- GRANT cannot express the rule - a table more than one tenant's
    -- role touches. These two are written by the collector, read by
    -- analytics_reader, and touched by nobody else, which is the same
    -- class traffic_rollup is in.
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'collector') THEN
        IF NOT has_table_privilege('collector', 'visitor_sketch', 'INSERT') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON visitor_sketch TO collector;
        END IF;
        IF NOT has_table_privilege('collector', 'visitor_sketch_state', 'INSERT') THEN
            GRANT SELECT, INSERT, UPDATE ON visitor_sketch_state TO collector;
        END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'analytics_reader') THEN
        IF NOT has_table_privilege('analytics_reader', 'visitor_sketch', 'SELECT') THEN
            GRANT SELECT ON visitor_sketch TO analytics_reader;
        END IF;
        IF NOT has_table_privilege('analytics_reader', 'visitor_sketch_state', 'SELECT') THEN
            GRANT SELECT ON visitor_sketch_state TO analytics_reader;
        END IF;
    END IF;
END
$$;
