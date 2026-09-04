-- Yedek alma: the queue and the catalogue.
--
-- # Why the panel cannot take a backup
--
-- Not "should not". Cannot. `panel_user` has no SELECT on
-- traffic_snapshots or beacon_events, which is the isolation this whole
-- product is built on, so it could not produce a dump if it tried.
--
-- The only role that can read everything is schema_admin, whose
-- credential lives in upgrader.toml - a file the panel cannot open. So
-- the shape is the one V4b already established: the panel writes a row
-- saying what is wanted, the upgrader carries it out, and the bytes
-- never pass through the process that faces the internet.
--
-- # Why a backup is the most dangerous file this product makes
--
-- Every protection here is a role boundary, and a dump crosses all of
-- them at once. Worse than that: addresses in traffic_snapshots are
-- pseudonymous only because `ip_hash_key` lives somewhere else, in
-- collector.toml. A file holding both the data and the key would undo
-- the pseudonymisation for anybody who has it.
--
-- Hence two artifacts, never one. This schema is about the data backup.
-- Configuration files are a separate artifact behind the developer
-- password (F1e), and neither one alone lets its holder re-identify
-- anybody.
--
-- The panel tables are not the safe half either: panel_users.totp_secret
-- is plain text, and the recovery codes sit beside it. So a data backup
-- is credential-bearing too, and the file protections below are not
-- decoration.


-- The queue: "please take a backup".
--
-- # Who may write which column
--
--   panel_user     INSERT and SELECT. It asks, and it reads the answer.
--   schema_admin   SELECT and UPDATE. It answers, and it cannot ask.
--
-- The same split as the release queue and for the same reason: a
-- compromised panel may ask for a backup, which is a button somebody is
-- allowed to press anyway, and cannot fabricate the result of one.
-- "A backup was taken at 03:00 and it is 4 GB" is not a sentence the
-- panel can write.
--
-- # What this table deliberately does not have
--
-- A path column. Where backups are written is `[backup] dir` in
-- upgrader.toml, and it stays there: a path in this row would be a path
-- a compromised panel could choose, and the upgrader would then write a
-- dump of every table to it. Root-owned directories, another customer's
-- tree, a web root - all of them reachable through a text field.
--
-- The row says *what to include*. Where it goes is not a decision the
-- asking side gets to make.
CREATE TABLE IF NOT EXISTS panel_backup_requests (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Who pressed it, by value, in the shape panel_audit_log uses. The
    -- row must still name who asked after that account is removed.
    actor_kind  TEXT   NOT NULL,
    actor_id    BIGINT,
    actor_label TEXT   NOT NULL DEFAULT '',

    -- The operation this belongs to, for the log tree.
    operation_id TEXT NOT NULL DEFAULT '',

    -- Which sets to include, from the closed list in internal/backup.
    --
    -- An array of names rather than a column per set: the sets are a
    -- product decision that will change, and a schema migration per
    -- decision is how a feature stops being adjusted. Validated in Go
    -- against a constant, and a name this build does not know is
    -- refused rather than ignored - a request naming "analitik" on a
    -- build that renamed it must not quietly take a backup of nothing.
    sets TEXT[] NOT NULL,

    -- pending -> running -> succeeded | failed
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'succeeded', 'failed')),

    claimed_by TEXT        NOT NULL DEFAULT '',
    claimed_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,

    -- What went wrong, in the words the page shows. Empty on success.
    error_chain TEXT NOT NULL DEFAULT '',

    -- The catalogue row this produced, so the page can link a request to
    -- its file. Null while running and after a failure.
    --
    -- Deliberately not a foreign key. Backups get deleted, and a request
    -- whose file is gone is still a true record of a backup having been
    -- taken - which is the thing somebody is looking for when they ask
    -- "did the pre-upgrade backup run".
    backup_id BIGINT
);

-- One request in flight at a time.
--
-- A partial unique index rather than application logic. Two dumps of the
-- same database at once would double the disk cost of the operation
-- whose whole risk is disk cost, and they would race for the same
-- temporary name.
CREATE UNIQUE INDEX IF NOT EXISTS idx_backup_one_in_flight
    ON panel_backup_requests ((state IN ('pending', 'running')))
    WHERE state IN ('pending', 'running');

CREATE INDEX IF NOT EXISTS idx_backup_requested_at
    ON panel_backup_requests (requested_at DESC);

ALTER TABLE panel_backup_requests ENABLE ROW LEVEL SECURITY;
-- FORCE, because the owner is exempt without it and schema_admin owns
-- this table. Without FORCE the "one role asks, another answers" split
-- is false for the answering role - measured on the other three queues,
-- where a plain INSERT as schema_admin succeeded.
ALTER TABLE panel_backup_requests FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS backup_read ON panel_backup_requests;
CREATE POLICY backup_read ON panel_backup_requests FOR SELECT USING (true);

DROP POLICY IF EXISTS backup_ask ON panel_backup_requests;
CREATE POLICY backup_ask ON panel_backup_requests
    FOR INSERT TO panel_user
    WITH CHECK (true);

DROP POLICY IF EXISTS backup_answer ON panel_backup_requests;
CREATE POLICY backup_answer ON panel_backup_requests
    FOR UPDATE TO schema_admin
    USING (true)
    WITH CHECK (true);

DROP POLICY IF EXISTS backup_sweep ON panel_backup_requests;
CREATE POLICY backup_sweep ON panel_backup_requests
    FOR DELETE TO panel_user
    USING (true);

GRANT SELECT, INSERT, DELETE ON panel_backup_requests TO panel_user;
GRANT SELECT, UPDATE ON panel_backup_requests TO schema_admin;


-- The catalogue: one row per backup that exists on disk.
--
-- # The path column, and the grant that hides it
--
-- `path` is where the file is. The panel is not granted that column.
--
-- Not by policy and not by convention: by a column-level GRANT, so a
-- SELECT naming it is refused by the database. The panel shows sizes,
-- dates and contents - all of which it needs - and cannot learn where
-- the bytes are.
--
-- The reason is what a path would let a compromised panel do. Nothing
-- here serves a backup over HTTP and nothing is planned to; the file
-- staying on the machine is the protection. A panel that knew the path
-- would be one bug away from being asked to read it, and "there is no
-- route that does that" is a weaker sentence than "the process does not
-- know where the file is".
--
-- *İstemciye güvenme, sadece sunucuya güven.*
CREATE TABLE IF NOT EXISTS panel_backups (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    taken_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- What is in it, the same closed names as the request.
    sets TEXT[] NOT NULL,

    -- The file.
    bytes  BIGINT NOT NULL,
    sha256 TEXT   NOT NULL,

    -- Where it is. Not granted to panel_user; see above.
    path TEXT NOT NULL,

    -- What produced it, so a restore can refuse a file this build cannot
    -- read. Both are needed: the schema version says whether the tables
    -- match, and the binary version is what an operator recognises.
    binary_version TEXT   NOT NULL DEFAULT '',
    schema_version BIGINT NOT NULL DEFAULT 0,

    -- Whether the file is still there.
    --
    -- A backup deleted by an operator with a shell leaves a row that
    -- would otherwise promise a file that is gone. The upgrader marks
    -- rows it can no longer stat, rather than deleting them: "there was
    -- a backup here on Tuesday and it is missing" is a sentence somebody
    -- needs to read.
    state TEXT NOT NULL DEFAULT 'present'
        CHECK (state IN ('present', 'missing'))
);

CREATE INDEX IF NOT EXISTS idx_backups_taken_at
    ON panel_backups (taken_at DESC);

ALTER TABLE panel_backups ENABLE ROW LEVEL SECURITY;
ALTER TABLE panel_backups FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS backups_read ON panel_backups;
CREATE POLICY backups_read ON panel_backups FOR SELECT USING (true);

-- Only the upgrader records a backup. A panel that could write here
-- could tell itself a backup exists, and "there is a recent backup" is
-- exactly the sentence somebody checks before doing something
-- irreversible.
DROP POLICY IF EXISTS backups_record ON panel_backups;
CREATE POLICY backups_record ON panel_backups
    FOR INSERT TO schema_admin
    WITH CHECK (true);

DROP POLICY IF EXISTS backups_mark ON panel_backups;
CREATE POLICY backups_mark ON panel_backups
    FOR UPDATE TO schema_admin
    USING (true)
    WITH CHECK (true);

-- And only the upgrader forgets one, because forgetting the row is the
-- half that follows deleting the file - and only the upgrader can do
-- that.
DROP POLICY IF EXISTS backups_forget ON panel_backups;
CREATE POLICY backups_forget ON panel_backups
    FOR DELETE TO schema_admin
    USING (true);

-- Column-level, and this is the enforcement rather than a comment about
-- it: `path` is absent from the panel's grant, so `SELECT path FROM
-- panel_backups` is refused for panel_user by the database itself.
--
-- Listed rather than `GRANT SELECT ON panel_backups`, which would
-- include every column including ones added later. A column added in a
-- future migration is not granted to the panel until somebody writes it
-- here, which is the right default for a table whose whole point is
-- that one of its columns is dangerous.
GRANT SELECT (id, taken_at, sets, bytes, sha256, binary_version, schema_version, state)
    ON panel_backups TO panel_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON panel_backups TO schema_admin;
