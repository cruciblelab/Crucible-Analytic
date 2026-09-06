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

    -- What is being asked for.
    --
    -- # Why one queue and not a second one
    --
    -- Taking a backup and checking one are the same shape: the panel
    -- cannot do either, the upgrader does both, and both are heavy
    -- reads of the same disk. A second queue would be a second copy of
    -- Ask, Claim, Finish and ExpireStale - and this project has already
    -- paid for a queue whose Claim nobody called.
    --
    -- Sharing the queue also shares the one-in-flight index, which is
    -- the right answer rather than a side effect: a verification that
    -- ran while a backup was being written would be two processes
    -- reading the same disk to do the customer's least urgent work.
    --
    -- 'al' is the default so that every row written before this column
    -- existed reads as what it was.
    kind TEXT NOT NULL DEFAULT 'al'
        CHECK (kind IN ('al', 'dogrula', 'geri_yukle')),

    -- Which sets to include, from the closed list in internal/backup.
    --
    -- An array of names rather than a column per set: the sets are a
    -- product decision that will change, and a schema migration per
    -- decision is how a feature stops being adjusted. Validated in Go
    -- against a constant, and a name this build does not know is
    -- refused rather than ignored - a request naming "analitik" on a
    -- build that renamed it must not quietly take a backup of nothing.
    --
    -- Empty for a verification, which is about a file that already
    -- exists and has no sets to choose.
    sets TEXT[] NOT NULL DEFAULT '{}',

    -- Which catalogue row this is about, for the kinds that are about
    -- one.
    --
    -- Deliberately not the same column as backup_id below, which means
    -- the opposite thing: this is the row the request names going in,
    -- that is the row the request produced coming out. One column
    -- carrying both meanings is the kind of saving that costs somebody
    -- a day.
    --
    -- Not a foreign key, for the same reason backup_id is not: a
    -- backup can be deleted, and "somebody checked this backup on
    -- Tuesday" stays true afterwards.
    target_id BIGINT,

    -- pending -> running -> succeeded | failed
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'succeeded', 'failed')),

    claimed_by TEXT        NOT NULL DEFAULT '',
    claimed_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,

    -- What went wrong, in the words the page shows. Empty on success.
    error_chain TEXT NOT NULL DEFAULT '',

    -- What the work produced, in the words the page shows.
    --
    -- Separate from error_chain, which is what went wrong. A restore
    -- succeeds and still has something to say - which tables came back
    -- and with how many rows - and putting that in the error column
    -- would make every successful restore look like a failure to
    -- anything reading the row rather than the page.
    --
    -- Empty for work whose whole outcome is "it happened". Taking a
    -- backup is that: the catalogue row is the result.
    result TEXT NOT NULL DEFAULT '',

    -- The catalogue row this produced, so the page can link a request to
    -- its file. Null while running and after a failure.
    --
    -- Deliberately not a foreign key. Backups get deleted, and a request
    -- whose file is gone is still a true record of a backup having been
    -- taken - which is the thing somebody is looking for when they ask
    -- "did the pre-upgrade backup run".
    backup_id BIGINT,

    -- Each kind needs exactly the fields it needs, and the database is
    -- what says so.
    --
    -- Not a comment and not a check in Go. Go already refuses a
    -- malformed request in two places - the panel before the row exists
    -- and the upgrader after it reads one - and both of those are
    -- programs that can be wrong. This one is the statement the row
    -- itself cannot violate: a verification with no target is a row
    -- nothing could ever carry out, and a take with no sets is a backup
    -- of nothing that would report success.
    CONSTRAINT panel_backup_requests_kind_fields CHECK (
        (kind =  'al' AND cardinality(sets) >  0 AND target_id IS NULL) OR
        (kind <> 'al' AND cardinality(sets) =  0 AND target_id IS NOT NULL)
    )
);

-- For databases created before the columns existed.
--
-- Written as separate idempotent statements rather than folded into the
-- CREATE TABLE above, because CREATE TABLE IF NOT EXISTS does nothing at
-- all on a database that already has the table - including nothing about
-- its new columns. Every column added here has to be added twice, and
-- internal/panel's startup column check is what fails loudly when one of
-- the two is forgotten.
ALTER TABLE panel_backup_requests
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'al';
ALTER TABLE panel_backup_requests
    ADD COLUMN IF NOT EXISTS target_id BIGINT;
ALTER TABLE panel_backup_requests
    ADD COLUMN IF NOT EXISTS result TEXT NOT NULL DEFAULT '';
ALTER TABLE panel_backup_requests
    ALTER COLUMN sets SET DEFAULT '{}';

ALTER TABLE panel_backup_requests
    DROP CONSTRAINT IF EXISTS panel_backup_requests_kind_check;
ALTER TABLE panel_backup_requests
    ADD CONSTRAINT panel_backup_requests_kind_check
    CHECK (kind IN ('al', 'dogrula', 'geri_yukle'));

ALTER TABLE panel_backup_requests
    DROP CONSTRAINT IF EXISTS panel_backup_requests_kind_fields;
ALTER TABLE panel_backup_requests
    ADD CONSTRAINT panel_backup_requests_kind_fields CHECK (
        (kind =  'al' AND cardinality(sets) >  0 AND target_id IS NULL) OR
        (kind <> 'al' AND cardinality(sets) =  0 AND target_id IS NOT NULL)
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
        CHECK (state IN ('present', 'missing')),

    -- Which filesystem the file is on, as the kernel's device number.
    --
    -- # Why a number and not the path
    --
    -- The panel draws the disk. It groups the directories it was
    -- configured with by device, because two directories on one disk are
    -- one bar - and to put the backups into that bar it has to know
    -- which disk they are on.
    --
    -- It cannot find out for itself. The directory is named in
    -- upgrader.toml, which the panel does not read, and `path` is not
    -- granted to its role, which is the point of that grant. So the
    -- component that can see the disk records what it saw, and the panel
    -- reads a number that means nothing on its own.
    --
    -- An opaque device number is the whole of what is shared. It names
    -- no directory and cannot be turned back into one.
    --
    -- Zero means unknown: a row written before this column existed, or
    -- one whose filesystem could not be read. The panel shows those
    -- bytes in the total and leaves them out of the bar, because a bar
    -- is a claim about a specific disk.
    device BIGINT NOT NULL DEFAULT 0,

    -- When somebody last checked this file, and what the check found.
    --
    -- # Why the verdict lives on the backup and not only on the request
    --
    -- The request row answers "what happened when I pressed the
    -- button", and it is swept. The question people actually have is
    -- the other one: *which of these backups do I know is good.* A
    -- verdict recorded on the request would answer that only for as
    -- long as nobody tidied up.
    --
    -- Null means nobody has ever checked it. That is not the same as
    -- "it is fine" and the page says so, because an unchecked backup is
    -- exactly the object this whole phase exists about: a file with a
    -- plausible name that nobody has ever opened.
    verified_at TIMESTAMPTZ,
    -- Empty means the check found nothing wrong. Non-empty is what it
    -- found, in the words the page shows - several lines, because
    -- somebody checking a file wants everything wrong with it rather
    -- than the first thing.
    verify_problems TEXT NOT NULL DEFAULT ''
);

-- For databases created before the columns existed.
ALTER TABLE panel_backups ADD COLUMN IF NOT EXISTS device BIGINT NOT NULL DEFAULT 0;
ALTER TABLE panel_backups ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;
ALTER TABLE panel_backups
    ADD COLUMN IF NOT EXISTS verify_problems TEXT NOT NULL DEFAULT '';

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
GRANT SELECT (id, taken_at, sets, bytes, sha256, binary_version, schema_version, state,
              device, verified_at, verify_problems)
    ON panel_backups TO panel_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON panel_backups TO schema_admin;
