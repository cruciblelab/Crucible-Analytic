-- Schema for the optional ASN/country lookup module. Only needed if you
-- set asn_lookup.enabled = true in config.toml - apply this once,
-- manually, the same way and for the same reason as
-- internal/storage/schema.sql: the collector itself never runs DDL.
--
-- Plain relational table, NOT a TimescaleDB hypertable: IP ranges have no
-- time dimension to partition on (unlike traffic_snapshots).
--
-- start_addr/end_addr are INET, not BIGINT: this table holds both IPv4
-- and IPv6 ranges (BIGINT can't hold a 128-bit IPv6 address), matching
-- how the rest of this project already stores addresses (see
-- internal/storage/schema.sql's traffic_snapshots.ip) and how pgx already
-- encodes/decodes netip.Addr <-> inet without any extra code.
--
-- BREAKING CHANGE from the original (IPv4-only, BIGINT) version of this
-- table: if you applied that version already, drop and recreate it -
-- this feature is disabled by default and was only added recently, so
-- there's no expected production data to migrate.
CREATE TABLE IF NOT EXISTS ip_country_ranges (
    start_addr INET NOT NULL,
    end_addr   INET NOT NULL,
    country    TEXT NOT NULL,
    PRIMARY KEY (start_addr, end_addr)
);

-- Supports the range lookup ("largest start_addr <= this IP, then verify
-- end_addr covers it") that any external reader querying this table
-- directly - e.g. a future dashboard - would use. The collector's own
-- Resolver does not query this table per lookup; it keeps an in-memory
-- copy rebuilt on every refresh (see asnlookup.go) specifically so a
-- per-request hot path never depends on a database round trip.
CREATE INDEX IF NOT EXISTS idx_ip_country_ranges_start ON ip_country_ranges (start_addr);

-- ASN ranges: a separate table, not columns bolted onto
-- ip_country_ranges, because the country and ASN datasets are
-- independently sourced with their own, unrelated range boundaries (see
-- the package doc comment in asnlookup.go) - merging them into one row
-- per range would need real overlap-resolution logic this project has
-- deliberately stayed away from.
CREATE TABLE IF NOT EXISTS ip_asn_ranges (
    start_addr INET    NOT NULL,
    end_addr   INET    NOT NULL,
    asn        INTEGER NOT NULL,
    asn_org    TEXT    NOT NULL,
    PRIMARY KEY (start_addr, end_addr)
);

CREATE INDEX IF NOT EXISTS idx_ip_asn_ranges_start ON ip_asn_ranges (start_addr);

-- The fetch log: one row per dataset file this deployment tried to
-- fetch.
--
-- # The question it answers
--
-- "Is my geography data current, and if not, why not." Before this
-- table the answer lived in one warning line on the server's own journal
-- - which is the one place a customer with no shell cannot look. A
-- refresh that has been failing for a month looks exactly like a quiet
-- month: the range tables still hold last month's data and every page
-- draws normally.
--
-- # Why one row per file rather than per refresh
--
-- Because that is what actually happens. A refresh fetches the IPv4 and
-- IPv6 files of one dataset separately, and they fail separately -
-- asnlookup.go keeps the previous table for whichever family failed and
-- swaps in the one that worked. A single row per refresh would have to
-- collapse "IPv6 is current, IPv4 is a month old" into one outcome, and
-- there is no honest value for it.
--
-- Fallbacks fall out of the same choice for free: when the chosen source
-- fails and the next one works, the failed attempt and the successful
-- one are both here, in order, saying which was which.
--
-- # Who writes and who reads
--
-- The collector and the beacon write - they are the two services that
-- build a Resolver - and the panel only reads. See release/sql/grants.sql.
-- The split is the same one panel_upgrade_requests makes and for the
-- same reason: a record whose reader can also write it is a record that
-- can be made to say the fetch succeeded.
--
-- # No CHECK on the text columns
--
-- kind, family and outcome look like enums and are deliberately not
-- constrained, matching panel_operations, which this table is otherwise
-- shaped after. A CHECK would turn adding a source kind into a schema
-- migration on every deployment before a single row could be written -
-- and the failure would land on the writer, at runtime, on the machine
-- that upgraded its binary first. The closed set lives in Go, where
-- internal/ipsources already keeps it.
CREATE TABLE IF NOT EXISTS ip_range_fetches (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- Both ends, not a duration: "it started at 03:00 and took 40s" and
    -- "it took 40s" are different facts, and the first is the one that
    -- lines up with everything else in the journal.
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL,

    -- Which dataset, by the id internal/ipsources gives it, so a row
    -- read a year later still names something the library can look up.
    source_id TEXT NOT NULL,
    kind      TEXT NOT NULL,
    family    TEXT NOT NULL,

    -- 'download' or 'mirror'. Without it a byte count of zero from a
    -- local directory reads as a failed download rather than as
    -- asn_lookup.local_csv_path doing exactly what it was set to do.
    origin TEXT NOT NULL,

    outcome TEXT NOT NULL,

    -- What was actually obtained. Both are recorded even on a failure:
    -- a parse that got 9,000 rows into a truncated file is a different
    -- problem from one that got zero bytes, and only the numbers say
    -- which.
    rows_parsed BIGINT NOT NULL DEFAULT 0,
    bytes_read  BIGINT NOT NULL DEFAULT 0,

    -- The whole chain, not its last link, for the reason
    -- panel_operations.error_chain gives: the innermost cause is usually
    -- the one that names the fix.
    error_chain TEXT NOT NULL DEFAULT ''
);

-- The only query the panel makes: the most recent attempts, newest
-- first. Same shape as idx_panel_operations_time and for the same
-- reason.
CREATE INDEX IF NOT EXISTS idx_ip_range_fetches_started ON ip_range_fetches (started_at DESC);

-- The fetch log's privileges live here rather than only in
-- release/sql/grants.sql, and the reason is a gap this table was the
-- first to fall into.
--
-- # The gap, measured
--
-- There are two ways a database reaches a new schema. install.sh applies
-- the schema files *and* grants.sql; the upgrade button (L3) applies the
-- schema files and nothing else - internal/schemafiles.InOrder is
-- exactly the list of schema.sql files and privileges are not in it.
--
-- Every table in this project predates that machinery, so nobody had
-- added one since. This is the first, and the result is what it sounds
-- like: a customer presses the button, the table appears, and every
-- insert into it fails.
--
--	psql -U collector -c "INSERT INTO ip_range_fetches ..."
--	ERROR:  permission denied for table ip_range_fetches
--
-- The failure is quiet in this project's usual way - recordFetch logs a
-- warning and the refresh carries on, so geography keeps working and the
-- fetch log stays permanently empty, which looks exactly like a
-- deployment that has never refreshed.
--
-- So the grants travel with the table. grants.sql still lists them,
-- because it is the one place that answers "what may this role do".
--
-- DO blocks for the same reason internal/retention/schema.sql uses them:
-- this file is applied both to installed databases whose roles exist and
-- to development ones where they may not, and a GRANT to a role that
-- does not exist aborts the whole file.
--
-- # Why each grant asks first whether it is needed
--
-- This used to say "a grant issued twice is idempotent", which is true
-- of the result and false of the write. GRANT rewrites the target's ACL
-- tuple whether or not it changes anything in it, and two sessions
-- rewriting one catalogue tuple do not queue - the loser gets
--
--     ERROR:  tuple concurrently updated (SQLSTATE XX000)
--
-- Measured with three sessions issuing this same already-held grant 300
-- times each: 93 of 900 failed. That reaches an operator as an upgrade
-- that failed with no visible connection to the button they pressed,
-- and it is why applying the schema twice at once was not safe.
--
-- has_table_privilege once per privilege rather than once per list: the
-- comma form answers "any of these", so a role holding SELECT alone
-- would look satisfied and never be given INSERT.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'collector')
       AND NOT (has_table_privilege('collector', 'ip_range_fetches', 'SELECT')
            AND has_table_privilege('collector', 'ip_range_fetches', 'INSERT')
            AND has_table_privilege('collector', 'ip_range_fetches', 'DELETE')) THEN
        GRANT SELECT, INSERT, DELETE ON ip_range_fetches TO collector;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'beacon_writer')
       AND NOT (has_table_privilege('beacon_writer', 'ip_range_fetches', 'SELECT')
            AND has_table_privilege('beacon_writer', 'ip_range_fetches', 'INSERT')
            AND has_table_privilege('beacon_writer', 'ip_range_fetches', 'DELETE')) THEN
        GRANT SELECT, INSERT, DELETE ON ip_range_fetches TO beacon_writer;
    END IF;
    -- Read only, and no UPDATE for anybody: a fetch row is finished the
    -- moment it is written, so the authority to change one afterwards
    -- would only ever be the authority to make a failure look like a
    -- success.
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'panel_user')
       AND NOT has_table_privilege('panel_user', 'ip_range_fetches', 'SELECT') THEN
        GRANT SELECT ON ip_range_fetches TO panel_user;
    END IF;
END
$$;
