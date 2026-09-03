-- The "refresh the IP range datasets now" queue.
--
-- # Why a queue rather than a call
--
-- The panel opens no outbound connections. Not by omission - by design:
-- it is the process a customer's browser reaches, and a button that made
-- it fetch a URL would be an SSRF surface on exactly the process that
-- must not have one. PLAN.md's permanent list forbids "any operation
-- whose parameter is a hostname the deployment will connect to", and a
-- fetch the panel performs is that operation with the hostname hidden in
-- the code rather than in the request.
--
-- So the button writes a row saying what it wants. The component that
-- already fetches - the collector, or the beacon, whichever has
-- asn_lookup enabled - sees the row on its next poll, does the work it
-- was already built to do, and writes back what happened.
--
-- This is L3's shape, one table over: internal/upgrade does the same for
-- schema migrations, and for the same reason (the panel may not run DDL
-- either). What is different here is who answers - a fetching service
-- rather than the upgrader - and that nobody may be listening at all,
-- which the next section is about.
--
-- # Ask and answer are different privileges
--
--   panel_user                    INSERT, SELECT, DELETE
--   collector, beacon_writer      SELECT, UPDATE
--
-- Neither side holds both. A compromised panel can ask for a refresh -
-- which is a button any entitled customer can press anyway - and cannot
-- write "succeeded" on a refresh that never happened.
--
-- DELETE is the panel's and not the fetcher's, and that is not
-- symmetrical by accident: see the expiry note on the in-flight index.
--
-- # asn_lookup is off by default, so "nobody is listening" is normal
--
-- The difference from L3 that matters. An upgrader is installed with the
-- package; a resolver only exists when a deployment has turned
-- asn_lookup on. So a request written on a deployment with it off is not
-- an error state to be recovered from - it is the ordinary consequence
-- of pressing a button for a feature that is not running, and the panel
-- has to be able to say so rather than showing a row that never moves.
CREATE TABLE IF NOT EXISTS ip_range_refresh_requests (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Who pressed it, in the shape panel_audit_log records an actor and
    -- for the same reason: kept by value, so the row still names who
    -- asked after that account is renamed or removed.
    --
    -- No REFERENCES panel_users(id), deliberately. The foreign key would
    -- buy nothing here - the label is what gets read - and it would make
    -- every insert take a lock on panel_users. v0.11.1 is the record of
    -- what that costs: an insert that locks two tables in one order,
    -- against a schema file that locks them in the other, is a deadlock
    -- with a customer's write on one side of it.
    actor_kind  TEXT   NOT NULL,
    actor_id    BIGINT,
    actor_label TEXT   NOT NULL DEFAULT '',

    -- The operation this belongs to, so the log lines it produces can be
    -- found. See internal/panel/operations.go and internal/logsink.
    operation_id TEXT NOT NULL DEFAULT '',

    -- pending -> running -> succeeded | failed.
    --
    -- Text rather than a CHECK, matching panel_upgrade_requests: the set
    -- is a fact about the code, and a CHECK would turn adding a state
    -- into a migration on every deployment.
    state TEXT NOT NULL DEFAULT 'pending',

    -- Which fetcher took it, and when. Claiming is an UPDATE guarded by
    -- the state, so the database picks the winner rather than two
    -- processes agreeing between themselves - which matters here more
    -- than in L3, because a deployment can genuinely run two resolvers
    -- (the collector's and the beacon's) against one database.
    claimed_at TIMESTAMPTZ,
    claimed_by TEXT NOT NULL DEFAULT '',

    finished_at TIMESTAMPTZ,

    -- How it went, in one line, so the row says whether it worked
    -- without the reader correlating timestamps against
    -- ip_range_fetches. The detail is there; this is the summary.
    files_ok     INTEGER NOT NULL DEFAULT 0,
    files_failed INTEGER NOT NULL DEFAULT 0,

    -- The whole chain on failure, for the reason panel_operations keeps
    -- it whole: the innermost cause is usually the one that names the
    -- fix.
    error_chain TEXT NOT NULL DEFAULT ''
);

-- One request in flight at a time.
--
-- The "pressing twice does not start two fetches" half of M3's done
-- criterion, and a partial unique index rather than application logic
-- because what is being prevented is two processes deciding at the same
-- moment that nothing is running.
--
-- # And the expiry that has to come with it
--
-- An index like this turns "nobody answered" into "the button is dead
-- for ever". L3 can live with that because an upgrader is always
-- installed; here the answering component is optional, so a deployment
-- with asn_lookup off would jam on its first press and stay jammed.
--
-- internal/rangerefresh.ExpireStale removes pending rows older than a
-- few minutes, and the panel calls it before every ask. That is why
-- DELETE belongs to the panel: it is the side that is still running when
-- nothing claimed the row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_range_refresh_one_in_flight
    ON ip_range_refresh_requests ((state IN ('pending', 'running')))
    WHERE state IN ('pending', 'running');

-- Newest first, which is every query the panel makes.
CREATE INDEX IF NOT EXISTS idx_range_refresh_requested_at
    ON ip_range_refresh_requests (requested_at DESC);

ALTER TABLE ip_range_refresh_requests ENABLE ROW LEVEL SECURITY;
-- And the owner is bound by them too, which is not the default.
--
-- # The hole this closes, found by a test that was passing
--
-- Every table here is owned by schema_admin, because ALTER TABLE needs
-- ownership and applying a migration is mostly ALTER TABLE. PostgreSQL
-- exempts a table's owner from row-level security unless told
-- otherwise - so the policies above, and the GRANTs beside them, said
-- nothing at all about the one role that owns the table.
--
-- Measured rather than reasoned: connecting as schema_admin and running
-- a plain INSERT succeeded on every one of these three queues.
--
-- The split each of these tables exists to enforce - one role asks, a
-- different role answers - was therefore false for the answering role.
-- The test that was supposed to catch it passed for the wrong reason:
-- it inserted while a request was already in flight, so the unique
-- index refused it, and the assertion only checked that *something*
-- did.
--
-- FORCE makes the policies apply to the owner as well, which is what
-- the file already reads as though it says.
ALTER TABLE ip_range_refresh_requests FORCE ROW LEVEL SECURITY;


-- Reading is open to whoever holds SELECT: the panel shows the result
-- and the fetcher reads what to do.
DROP POLICY IF EXISTS range_refresh_read ON ip_range_refresh_requests;
CREATE POLICY range_refresh_read ON ip_range_refresh_requests FOR SELECT USING (true);

-- Asking is the panel's, answering is the fetchers'.
--
-- Two policies rather than one, because they are two different rights
-- held by different roles, and a single FOR ALL policy would give
-- whichever role it named both. The GRANTs already say this; the
-- policies say it a second time, so removing one grant by accident does
-- not silently widen the other role.
DROP POLICY IF EXISTS range_refresh_ask ON ip_range_refresh_requests;
CREATE POLICY range_refresh_ask ON ip_range_refresh_requests
    FOR INSERT TO panel_user
    WITH CHECK (true);

DROP POLICY IF EXISTS range_refresh_answer ON ip_range_refresh_requests;
CREATE POLICY range_refresh_answer ON ip_range_refresh_requests
    FOR UPDATE TO collector, beacon_writer
    USING (true)
    WITH CHECK (true);

-- The expiry and the sweep, both the panel's. Without a policy naming
-- the deleting role, a permissive FOR ALL policy would bound DELETE to
-- rows that role inserted - which is the same trap panel_logs and
-- panel_upgrade_requests each have a policy for.
DROP POLICY IF EXISTS range_refresh_sweep ON ip_range_refresh_requests;
CREATE POLICY range_refresh_sweep ON ip_range_refresh_requests
    FOR DELETE TO panel_user
    USING (true);

-- The privileges travel with the table.
--
-- M2's finding, and this table is the second to need it: install.sh
-- applies the schema files *and* release/sql/grants.sql, while the
-- upgrade button applies only the schema files. A new table whose grants
-- live only in grants.sql arrives, on the button path, as a table nobody
-- can use - measured, on ip_range_fetches, as
-- "permission denied for table".
--
-- DO blocks because this file is applied both to installed databases
-- whose roles exist and to development ones where they may not, and a
-- GRANT to a role that does not exist aborts the whole file.
--
-- Each grant also asks whether it is needed, for the reason written out
-- at the same place in internal/asnlookup/schema.sql: GRANT rewrites the
-- ACL tuple even when it changes nothing in it, and two appliers doing
-- that at once collide with "tuple concurrently updated". Per privilege
-- rather than per list, because the comma form of has_table_privilege
-- means "any of these" and would call a half-granted role satisfied.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'panel_user')
       AND NOT (has_table_privilege('panel_user', 'ip_range_refresh_requests', 'SELECT')
            AND has_table_privilege('panel_user', 'ip_range_refresh_requests', 'INSERT')
            AND has_table_privilege('panel_user', 'ip_range_refresh_requests', 'DELETE')) THEN
        GRANT SELECT, INSERT, DELETE ON ip_range_refresh_requests TO panel_user;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'collector')
       AND NOT (has_table_privilege('collector', 'ip_range_refresh_requests', 'SELECT')
            AND has_table_privilege('collector', 'ip_range_refresh_requests', 'UPDATE')) THEN
        GRANT SELECT, UPDATE ON ip_range_refresh_requests TO collector;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'beacon_writer')
       AND NOT (has_table_privilege('beacon_writer', 'ip_range_refresh_requests', 'SELECT')
            AND has_table_privilege('beacon_writer', 'ip_range_refresh_requests', 'UPDATE')) THEN
        GRANT SELECT, UPDATE ON ip_range_refresh_requests TO beacon_writer;
    END IF;
END
$$;
