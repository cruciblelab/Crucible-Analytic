-- The release update queue: "please move this deployment to version X".
--
-- # The same shape as panel_upgrade_requests, for a bigger reason
--
-- The schema queue exists because the panel cannot run DDL. This one
-- exists because the panel must not be able to run *code*, which is the
-- stronger version of the same rule and the one that decides whether an
-- update button can exist at all.
--
-- A panel that could install binaries would be a panel that, once
-- compromised, owns the machine - and the panel is the part of this
-- system that faces the internet. So the button does not install. It
-- writes a row here saying which version is wanted; the upgrader, which
-- runs as its own account and reads a config file the panel cannot open,
-- downloads that version, checks our signature on it, and installs.
--
-- # What this table deliberately does not have
--
-- A URL column. Where packages come from is `[release] base_url` in
-- upgrader.toml, and it must stay there: a URL in this table would be a
-- URL a compromised panel could choose, and the download would then
-- fetch whatever it was pointed at. The row carries a *version*, and
-- the upgrader builds the address from its own configuration.
--
-- The signature is the backstop under that, not the argument for it. A
-- package from anywhere still has to verify against the key in
-- upgrader.toml. Both together are the point: the panel cannot choose
-- the source, and could not use a chosen source if it could.
--
-- # Who may write which column
--
--   panel_user     INSERT and SELECT. It asks, and it reads the answer.
--   schema_admin   SELECT and UPDATE. It answers, and it cannot ask.
--
-- The same split as the schema queue, and for the same reason: a
-- compromised panel can ask for an update - which is a button somebody
-- is allowed to press anyway - and cannot fabricate the result of one.
-- "This deployment is now running v9.9.9" is not a sentence the panel
-- can write.
CREATE TABLE IF NOT EXISTS panel_release_requests (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Who pressed it, by value, in the shape panel_audit_log uses. The
    -- row must still name who asked after that account is renamed or
    -- removed.
    actor_kind  TEXT   NOT NULL,
    actor_id    BIGINT,
    actor_label TEXT   NOT NULL DEFAULT '',

    -- The operation this belongs to, for the log tree.
    operation_id TEXT NOT NULL DEFAULT '',

    -- What was asked for, recorded at request time.
    --
    -- from_version is the build the asking panel was running. It is kept
    -- because the interesting question after a bad update is "what did
    -- this machine come from", and the binary that could answer it has
    -- by then been replaced.
    --
    -- to_version is a version string, never an address. Its shape is
    -- checked in Go before it reaches here - see relupdate.ValidVersion -
    -- because it becomes part of a URL, and a version that can carry a
    -- slash is a version that can carry a path.
    from_version TEXT NOT NULL DEFAULT '',
    to_version   TEXT NOT NULL,

    -- pending -> running -> succeeded | failed.
    --
    -- Text rather than an enum, matching the other two queues: the set
    -- is a fact about the code, and a CHECK listing them would turn
    -- adding a state into a migration.
    state TEXT NOT NULL DEFAULT 'pending',

    claimed_at TIMESTAMPTZ,
    claimed_by TEXT NOT NULL DEFAULT '',

    finished_at TIMESTAMPTZ,

    -- The whole chain on failure. An update has more ways to fail than a
    -- migration - the address did not answer, the signature did not
    -- match, the new binary did not start - and which one it was is the
    -- only thing the reader wants.
    error_chain TEXT NOT NULL DEFAULT '',

    -- What is actually installed now, written by the upgrader when it
    -- finishes. On a failure this is the honest answer to "so what is
    -- running", which after a rollback is the version it came from.
    installed_version TEXT NOT NULL DEFAULT '',

    -- Whether the previous binaries were put back.
    --
    -- Its own column rather than a sentence in error_chain, because it
    -- is the first thing somebody woken up by a down site needs, and
    -- reading it out of prose is not something a page should have to do.
    rolled_back BOOLEAN NOT NULL DEFAULT false
);

-- One request in flight at a time.
--
-- A partial unique index rather than application logic, for the reason
-- the other two queues give: what is being prevented is two processes
-- deciding at the same moment that nothing is running. Here it also
-- prevents two downloads racing to replace the same files, which is a
-- worse outcome than two migrations - one of them would win a rename
-- and the other would install half a release.
CREATE UNIQUE INDEX IF NOT EXISTS idx_release_one_in_flight
    ON panel_release_requests ((state IN ('pending', 'running')))
    WHERE state IN ('pending', 'running');

-- Newest first, which is every query the panel makes.
CREATE INDEX IF NOT EXISTS idx_release_requested_at
    ON panel_release_requests (requested_at DESC);

ALTER TABLE panel_release_requests ENABLE ROW LEVEL SECURITY;
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
ALTER TABLE panel_release_requests FORCE ROW LEVEL SECURITY;


-- Reading is open to whoever holds SELECT: the panel shows the result,
-- the upgrader reads what to do.
DROP POLICY IF EXISTS release_read ON panel_release_requests;
CREATE POLICY release_read ON panel_release_requests FOR SELECT USING (true);

-- Asking is the panel's, answering is the upgrader's. Two policies
-- rather than one FOR ALL, so removing a grant by accident cannot
-- silently widen the other role.
DROP POLICY IF EXISTS release_ask ON panel_release_requests;
CREATE POLICY release_ask ON panel_release_requests
    FOR INSERT TO panel_user
    WITH CHECK (true);

DROP POLICY IF EXISTS release_answer ON panel_release_requests;
CREATE POLICY release_answer ON panel_release_requests
    FOR UPDATE TO schema_admin
    USING (true)
    WITH CHECK (true);

-- The sweep, for the reason panel_logs has one: without a policy naming
-- the deleting role, a permissive FOR ALL policy would bound DELETE to
-- rows that role inserted.
DROP POLICY IF EXISTS release_sweep ON panel_release_requests;
CREATE POLICY release_sweep ON panel_release_requests
    FOR DELETE TO panel_user
    USING (true);


-- What the upgrader found when it last asked "is there a newer version?"
--
-- One row for the whole deployment, id fixed at 1, like schema_version.
-- There is one release source and one answer; a table that could hold
-- two would need a rule about which one the panel believes, and a rule
-- like that is one nobody writes down.
--
-- # Why this is a table and not a setting
--
-- panel_settings holds decisions somebody made. This holds an
-- observation a machine made, and the two have opposite lifecycles: a
-- setting survives because it was chosen, an observation is worthless
-- the moment it is stale. Keeping them apart means "when was this last
-- checked" has somewhere to live, and means a settings export does not
-- carry a claim about the world.
--
-- # What is in it and what is deliberately not
--
-- The version, when the source said it was released, when we last
-- looked, and the last error. Not the base URL and not the public key:
-- those stay in upgrader.toml. A key in a table is a key an attacker who
-- reached the database could replace, and then every signature in this
-- system would verify against theirs.

CREATE TABLE IF NOT EXISTS panel_release_available (
    id              smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- The latest version the source published, empty when the last check
    -- did not produce one.
    version         text        NOT NULL DEFAULT '',
    -- When the source says that version was released. NULL when the
    -- manifest did not say, which is allowed.
    released_at     timestamptz,
    -- Where a person can read what changed. Empty when absent.
    notes_url       text        NOT NULL DEFAULT '',

    -- When the upgrader last completed a check, successful or not. This
    -- is the field that turns the row from a claim into a dated claim,
    -- and a version with no date beside it is the thing a stale row
    -- looks exactly like.
    checked_at      timestamptz NOT NULL DEFAULT now(),
    -- When a check last succeeded. Separate from checked_at so the page
    -- can say "we last looked five minutes ago and it failed, the last
    -- good answer is from Tuesday" rather than having to choose one of
    -- those two facts to tell.
    succeeded_at    timestamptz,
    -- The last failure, empty when the last check worked.
    error           text        NOT NULL DEFAULT ''
);

-- The upgrader writes, everybody reads.
--
-- FORCE, because the owner of a table is exempt from its own row
-- security without it - and schema_admin owns this one. Three tables in
-- this project were found relying on policies that their own owner
-- silently bypassed.
ALTER TABLE panel_release_available ENABLE ROW LEVEL SECURITY;
ALTER TABLE panel_release_available FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS available_read ON panel_release_available;
CREATE POLICY available_read ON panel_release_available FOR SELECT USING (true);

-- Only the upgrader records an answer. The panel must not be able to
-- write here: a panel that could would be a panel that could tell itself
-- a version exists, and the whole point of asking the upgrader is that
-- it is the component holding the key.
DROP POLICY IF EXISTS available_record ON panel_release_available;
CREATE POLICY available_record ON panel_release_available
    FOR INSERT TO schema_admin
    WITH CHECK (true);

DROP POLICY IF EXISTS available_update ON panel_release_available;
CREATE POLICY available_update ON panel_release_available
    FOR UPDATE TO schema_admin
    USING (true)
    WITH CHECK (true);

-- And the upgrader can throw the answer away.
--
-- Nothing in normal operation deletes this row; it is upserted forever.
-- The case it exists for is an operator repointing base_url at a
-- different publisher: the recorded version then came from a source
-- this deployment no longer uses, and without a way to clear it the
-- page would go on offering a version from somebody else's shelf until
-- the next successful check happened to overwrite it.
DROP POLICY IF EXISTS available_forget ON panel_release_available;
CREATE POLICY available_forget ON panel_release_available
    FOR DELETE TO schema_admin
    USING (true);

GRANT SELECT ON panel_release_available TO panel_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON panel_release_available TO schema_admin;
