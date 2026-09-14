-- What the services logged, for a panel with no shell behind it.
--
-- # Why a table when there is already a log tree
--
-- The tree (internal/logging) is the operator's record and it is better
-- than this in every way that matters to an operator: complete, cheap,
-- and readable with grep. It is also unreachable to a customer, who has
-- no shell and is not going to get one. This table is what the panel can
-- show them.
--
-- # The trap, and it is the obvious one
--
-- A log table becomes the largest table in the database. That is the
-- disk-full failure A4 describes, arriving by a different road. Three
-- things keep it small and all three are load-bearing:
--
--   1. Only WARN and above is kept by default. The tree keeps the rest.
--   2. The per-site verbose switch raises that to DEBUG and *expires on
--      its own* - see internal/logging.Controls.Apply, which is already
--      what the collector and the beacon do. Verbose logging left on
--      because somebody forgot is how the disk fills.
--   3. Its own retention, far shorter than the analytics tables'.
--
-- # What a row may not be
--
-- Not analytics. A log line is a service describing what it did; the
-- moment one answers a question about a visitor, the panel has a second
-- route to data its role cannot read - and one no GRANT would reveal,
-- because the panel is supposed to read this table.
CREATE TABLE IF NOT EXISTS panel_logs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- When the record was made, by the service that made it.
    at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The database role the service connects as, not a friendly name.
    --
    -- Keyed by the writer's identity for the same reason
    -- service_heartbeat is: the row-level policy below compares this
    -- against current_user, so the key *is* the authorisation. Without
    -- it a compromised beacon could write log lines attributed to the
    -- collector - forging entries in the record an operator reads to
    -- find out what happened, which is the one place a forgery pays.
    service TEXT NOT NULL,

    -- slog's level and the log tree's category, kept as text.
    --
    -- Text rather than an enum: both sets are facts about the code, and
    -- a CHECK listing them would turn adding a category into a
    -- migration.
    level    TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT '',

    -- The message and its attributes.
    --
    -- Both already passed through logging.SanitizeValue before they got
    -- here: invalid UTF-8 removed, control characters dropped, length
    -- capped. A log line contains text somebody else chose, and a single
    -- hostile string must not be able to break the writer or forge a
    -- second record.
    message TEXT  NOT NULL,
    attrs   JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- The site this line is about, empty for process-level lines.
    --
    -- Empty is not "unknown", it is "not about a site" - which is the
    -- distinction B1 needs, because a process-level line must never be
    -- shown to one customer as if it were theirs.
    site_id TEXT NOT NULL DEFAULT '',

    -- The operation this line belongs to, empty when it belongs to none.
    --
    -- This column is why B1 and B2 are one phase. D4's streaming window
    -- shows *this operation's* lines and not the whole system's, and
    -- without a correlation id the only implementable version of that
    -- window is "everything that happened while you waited" - which is
    -- noise, and noise is what teaches people to click through without
    -- reading.
    operation_id TEXT NOT NULL DEFAULT ''
);

-- Newest first, per site, which is every query the panel makes.
CREATE INDEX IF NOT EXISTS idx_panel_logs_site_time ON panel_logs (site_id, at DESC);
-- And by operation, for the streaming window.
CREATE INDEX IF NOT EXISTS idx_panel_logs_operation ON panel_logs (operation_id, at)
    WHERE operation_id <> '';
-- For the retention sweep, which is the only thing that reads by time
-- alone.
CREATE INDEX IF NOT EXISTS idx_panel_logs_time ON panel_logs (at);

-- Row-level security, forced, for the reason
-- internal/heartbeat/schema.sql writes out at length: without FORCE the
-- table's owner bypasses every policy below, the owner is schema_admin
-- after grants.sql runs, and schema_admin is the role cmd/upgrader
-- connects as - with this very sink attached to that pool.
--
-- Measured before the FORCE was added: as schema_admin, a row labelled
-- service = 'collector' was accepted into this table, which makes the
-- write policy's "only your own row" false for the one role holding the
-- deployment's DDL credential. A log line whose service column can be
-- chosen by its writer is not evidence of anything, and this table is
-- what the panel shows an operator asking what happened.
--
-- Forcing it costs nothing: the sink labels every row from SELECT
-- current_user, so each writer's own rows still pass, and the retention
-- sweep is panel_user's (panel_logs_sweep below) rather than the
-- owner's, so it is not bypassing anything either.
ALTER TABLE panel_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE panel_logs FORCE ROW LEVEL SECURITY;

-- Reading is open to anything granted SELECT; the panel needs every row
-- and a service seeing another's costs nothing.
DROP POLICY IF EXISTS panel_logs_read ON panel_logs;
CREATE POLICY panel_logs_read ON panel_logs FOR SELECT USING (true);

-- Writing is restricted to rows labelled with the writer.
--
-- USING and WITH CHECK written out separately, for the reason
-- service_heartbeat's policy records: they answer two questions that
-- happen to have the same answer today, and an edit that widened one
-- would silently widen the other.
DROP POLICY IF EXISTS panel_logs_write ON panel_logs;
CREATE POLICY panel_logs_write ON panel_logs
    FOR ALL
    USING (service = current_user)
    WITH CHECK (service = current_user);

-- The retention sweep, and it needs a policy of its own.
--
-- The write policy above is FOR ALL, so under it the panel could delete
-- only rows the panel itself wrote - while the sweep exists to remove
-- the other three services'. PostgreSQL ORs permissive policies, so this
-- widens DELETE for panel_user and leaves every other role bounded by
-- the policy above.
--
-- Measured rather than assumed: with only the write policy in place a
-- sweep run as panel_user removes its own rows and silently leaves the
-- collector's, which is the shape of a retention job that looks like it
-- works and does not.
DROP POLICY IF EXISTS panel_logs_sweep ON panel_logs;
CREATE POLICY panel_logs_sweep ON panel_logs
    FOR DELETE TO panel_user
    USING (true);


-- The log table's sequence stays closed to every service role.
--
-- The same REVOKE is in release/sql/grants.sql, and that is not where it
-- can do the job. A fresh install runs grants.sql; an upgrade runs the
-- schema files and nothing else, so a removal written only there is true
-- of the repository and false of every deployment that upgraded.
--
-- Measured by upgrading a deployment from each released version in turn
-- and comparing it with a fresh install: every release up to v0.20.0
-- arrived with collector, beacon_writer, analytics_reader and panel_user
-- all still holding USAGE and SELECT on this sequence - privileges the
-- surface audit removed, on deployments that had already been installed
-- when it was removed. They keep what they were once given, unless an
-- upgrade takes it back.
--
-- So it is taken back here, and it has to be taken back on every
-- upgrade, forever. The condition is what keeps it cheap: a REVOKE that
-- changes nothing still rewrites the ACL tuple, which two appliers
-- running at once collide on, and the roles may not exist at all in a
-- development database.
DO $$
DECLARE
    role_name text;
BEGIN
    FOREACH role_name IN ARRAY
        ARRAY['collector', 'beacon_writer', 'analytics_reader', 'panel_user']
    LOOP
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = role_name)
           AND (has_sequence_privilege(role_name, 'panel_logs_id_seq', 'USAGE')
                OR has_sequence_privilege(role_name, 'panel_logs_id_seq', 'SELECT')
                OR has_sequence_privilege(role_name, 'panel_logs_id_seq', 'UPDATE')) THEN
            EXECUTE format('REVOKE ALL ON SEQUENCE panel_logs_id_seq FROM %I', role_name);
        END IF;
    END LOOP;
END
$$;
