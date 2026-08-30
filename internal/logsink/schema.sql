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

ALTER TABLE panel_logs ENABLE ROW LEVEL SECURITY;

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
