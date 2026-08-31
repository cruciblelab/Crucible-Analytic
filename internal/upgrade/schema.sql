-- The upgrade request queue.
--
-- # Why a queue and not a function call
--
-- The panel cannot run DDL, and that is not an oversight - it is what
-- B6 and H5 established. grants.sql gives panel_user full rights on the
-- panel_* tables, no rights at all on the analytics tables, and the file
-- contains no ALTER, no CREATE and no OWNER anywhere.
--
-- So the button does not migrate. It writes a row here, saying what it
-- wants; a separate component with the authority to do DDL reads the
-- row, applies the schema, and writes the outcome back. DDL is never
-- reachable from the panel process, in any code path, including one
-- somebody adds later without thinking about it.
--
-- # Who may write which column
--
-- This is the load-bearing part of the design and it is enforced by
-- GRANT rather than by convention:
--
--   panel_user     INSERT and SELECT. It asks, and it reads the answer.
--   schema_admin   SELECT and UPDATE. It answers, and it cannot ask.
--
-- The panel therefore *cannot* mark a request succeeded. A compromised
-- panel process can ask for an upgrade - which is a button anybody
-- signed in can press anyway - but it cannot fabricate the result of
-- one, and it cannot make the version row say something that did not
-- happen. Splitting insert from update is what buys that.
CREATE TABLE IF NOT EXISTS panel_upgrade_requests (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Who pressed it, in the shape panel_audit_log records an actor.
    -- Kept by value rather than joined at read time, for the reason the
    -- audit log records: the row must still name who asked after that
    -- account is renamed or removed.
    actor_kind  TEXT   NOT NULL,
    actor_id    BIGINT,
    actor_label TEXT   NOT NULL DEFAULT '',

    -- The operation this belongs to. An upgrade is an operation by
    -- definition - "what happened, which step failed, was it rolled
    -- back" - and this is the id its log lines carry. See
    -- internal/panel/operations.go and internal/logsink.
    operation_id TEXT NOT NULL DEFAULT '',

    -- What was being asked for, recorded at request time.
    --
    -- from_version is what the database said when the button was
    -- pressed; to_version and to_fingerprint are what the asking binary
    -- expects. All three are stored rather than looked up later because
    -- the answer to "what did we think we were doing" must not change
    -- when somebody deploys a different binary an hour afterwards.
    from_version   INTEGER NOT NULL,
    to_version     INTEGER NOT NULL,
    to_fingerprint TEXT    NOT NULL,

    -- pending -> running -> succeeded | failed.
    --
    -- Text rather than an enum, matching panel_logs: the set is a fact
    -- about the code, and a CHECK listing them would turn adding a state
    -- into a migration - which is a poor thing to need in the table that
    -- exists to apply migrations.
    state TEXT NOT NULL DEFAULT 'pending',

    -- Which applier took it, and when.
    --
    -- Claiming is what stops two appliers - a timer that fired twice, a
    -- second host - from running the same migration at once. The claim
    -- is an UPDATE guarded by the state, so the database decides the
    -- winner rather than the processes agreeing among themselves.
    claimed_at TIMESTAMPTZ,
    claimed_by TEXT NOT NULL DEFAULT '',

    finished_at TIMESTAMPTZ,

    -- The whole chain on failure, for the reason panel_operations keeps
    -- it whole: the outer link says what was being attempted and the
    -- inner one says why the database refused.
    error_chain TEXT NOT NULL DEFAULT '',

    -- What the database actually reached. Null until it finishes, and on
    -- a failure it is the honest answer to "how far did it get" - which
    -- may be neither from_version nor to_version.
    applied_version INTEGER
);

-- One request in flight at a time.
--
-- A partial unique index rather than application logic, because the
-- thing being prevented is two processes deciding at the same moment
-- that nothing is running. Two customers clicking twice, a timer
-- overlapping its previous run, an operator retrying while the first
-- attempt is still going: all of them arrive here, and the second one
-- gets a constraint violation instead of a concurrent migration.
CREATE UNIQUE INDEX IF NOT EXISTS idx_upgrade_one_in_flight
    ON panel_upgrade_requests ((state IN ('pending', 'running')))
    WHERE state IN ('pending', 'running');

-- Newest first, which is every query the panel makes.
CREATE INDEX IF NOT EXISTS idx_upgrade_requested_at
    ON panel_upgrade_requests (requested_at DESC);

ALTER TABLE panel_upgrade_requests ENABLE ROW LEVEL SECURITY;

-- Reading is open to whoever holds SELECT: the panel shows the result
-- and the applier reads what to do.
DROP POLICY IF EXISTS upgrade_read ON panel_upgrade_requests;
CREATE POLICY upgrade_read ON panel_upgrade_requests FOR SELECT USING (true);

-- Asking is the panel's, answering is the applier's.
--
-- Two policies rather than one, because they are two different rights
-- held by two different roles, and a single FOR ALL policy would give
-- whichever role it named both. The GRANTs already say this; the
-- policies say it a second time, so removing one grant by accident
-- does not silently widen the other role.
DROP POLICY IF EXISTS upgrade_ask ON panel_upgrade_requests;
CREATE POLICY upgrade_ask ON panel_upgrade_requests
    FOR INSERT TO panel_user
    WITH CHECK (true);

DROP POLICY IF EXISTS upgrade_answer ON panel_upgrade_requests;
CREATE POLICY upgrade_answer ON panel_upgrade_requests
    FOR UPDATE TO schema_admin
    USING (true)
    WITH CHECK (true);

-- The sweep, for the same reason panel_logs has one: without a policy
-- naming the deleting role, a permissive FOR ALL policy would bound
-- DELETE to rows that role inserted.
DROP POLICY IF EXISTS upgrade_sweep ON panel_upgrade_requests;
CREATE POLICY upgrade_sweep ON panel_upgrade_requests
    FOR DELETE TO panel_user
    USING (true);
