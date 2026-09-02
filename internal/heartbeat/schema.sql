-- The one channel from a service to the panel.
--
-- # Why this table exists at all
--
-- The panel's role cannot read the analytics tables, which is half this
-- system's security foundation. It is also why, until this table, the
-- panel could not answer the simplest question an operator has: is the
-- collector still writing?
--
-- The collector has no HTTP server - deliberately, it is the process
-- that touches attacker bytes - so there was no channel at all. The
-- beacon and the read API have /healthz, and it answers a different
-- question: "this process is up right now". An operator needs "the last
-- write succeeded, at 14:02", and a liveness endpoint cannot say that.
--
-- So each service writes one row here, on a timer, carrying what only it
-- knows: its build, when it started, its counters, and its last failure.
--
-- # What it must never become
--
-- Not analytics. Nothing in this table is derived from a visitor: no
-- addresses, no site ids, no paths, no user agents. It is a service
-- describing itself. The moment a column here answers a question about
-- traffic, the panel has a second route to the data the isolation was
-- built to deny it - and one that no GRANT would reveal, because the
-- panel is supposed to read this table.

CREATE TABLE IF NOT EXISTS service_heartbeat (
    -- The database role the service connects as, not a friendly name.
    --
    -- Keyed by the writer's identity on purpose: the row-level policy
    -- below compares this against current_user, so the key *is* the
    -- authorisation. A display name would have to be mapped somewhere,
    -- and the mapping would be the thing that drifts.
    service TEXT PRIMARY KEY,

    -- The build this service is running. The reason B7 exists: with
    -- twelve installations, "which one is still on the old build" is a
    -- question somebody asks weekly.
    version TEXT NOT NULL DEFAULT '',

    -- When the process started, and when it last said anything.
    --
    -- Both, because they answer different questions. A started_at that
    -- keeps moving is a service in a crash loop, which looks perfectly
    -- healthy in any check that only asks "is it up".
    started_at TIMESTAMPTZ NOT NULL,
    beat_at    TIMESTAMPTZ NOT NULL,

    -- Whatever the service counts, as {name: number}.
    --
    -- JSONB rather than columns because the four services count
    -- different things and a shared column list would be four sets of
    -- nulls. The keys are a closed set in Go and the panel has a label
    -- for each; a key with no label fails a test rather than appearing
    -- on screen as a raw identifier.
    counters JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- The last thing that went wrong, and when.
    --
    -- Kept as one line rather than a log: this table is a status, and
    -- the log tree is where history lives. What it buys is the sentence
    -- an operator needs first - "the collector is up, and every write
    -- for the last hour has failed" is invisible to anything that only
    -- checks liveness.
    last_error    TEXT NOT NULL DEFAULT '',
    last_error_at TIMESTAMPTZ
);

-- A service may read every row and write only its own.
--
-- Row-level security, which nothing else in this schema uses, and the
-- reason is that nothing else needed it: every other table is granted to
-- exactly one writer, so the GRANT is the whole rule. Here four roles
-- write to one table, and a GRANT cannot express "only your own row".
--
-- Without it, a compromised beacon could write "collector: healthy,
-- beat_at: now" over the collector's row and hide an outage from the one
-- page built to show it. That is a small hole and this project's habit
-- is not to argue that a hole is small.
--
-- The installing superuser owns the table and bypasses these policies,
-- which is correct: it is the role that applies the schema, and it is
-- not a role any service connects as.
ALTER TABLE service_heartbeat ENABLE ROW LEVEL SECURITY;

-- Reading is open to anything granted SELECT. The panel needs every row;
-- a service seeing another service's row costs nothing.
DROP POLICY IF EXISTS heartbeat_read ON service_heartbeat;
CREATE POLICY heartbeat_read ON service_heartbeat
    FOR SELECT USING (true);

-- Writing is restricted to the row whose key is the writer.
--
-- The WITH CHECK is written out even though PostgreSQL would supply it.
-- The first version of this comment claimed USING alone would let a
-- service rename its own row into another's; measuring it showed that is
-- false - with WITH CHECK deleted, the rename was still refused, because
-- a policy with no WITH CHECK uses its USING expression for the
-- post-image as well. The wrong claim is recorded here rather than
-- quietly corrected: a comment warning about a hole that does not exist
-- is how a reader learns to disbelieve the comments about holes that do.
--
-- The real reason to write it is decoupling. USING says which rows are
-- visible to a write and WITH CHECK says what they may become, and those
-- are two questions that happen to have the same answer today. An edit
-- that widens USING - to let a service read a neighbour's row, say -
-- would silently widen what it may write, because the implicit fallback
-- follows along. Two expressions cannot drift into each other by
-- accident.
DROP POLICY IF EXISTS heartbeat_write ON service_heartbeat;
CREATE POLICY heartbeat_write ON service_heartbeat
    FOR ALL
    USING (service = current_user)
    WITH CHECK (service = current_user);

-- The resource profile the collector is actually running (A2).
--
-- # Why this is a column and not a counter
--
-- counters is JSONB of name to number, and a profile is a name. Encoding
-- it as a number would mean a mapping, and a mapping is the second
-- source of truth this whole feature is built to avoid: A2's rule is
-- that the profile is derived from the settings that cost memory and
-- stored nowhere.
--
-- # Why the panel reads it here rather than from a config file
--
-- The panel's role cannot read collector.toml, and must not learn to:
-- that file carries the collector's database password, and five separate
-- roles exist precisely so no service holds another's credentials.
--
-- It is also the more truthful answer. A file says what somebody wrote;
-- this says what the running process actually loaded. When the two
-- differ - the file was edited and the service was never restarted - the
-- panel shows the old value, and the old value is what is running.
--
-- Empty for every service that is not the collector, and for a collector
-- built before this column existed. Both mean "not reported", which the
-- panel renders as nothing rather than as a profile named "".
ALTER TABLE service_heartbeat ADD COLUMN IF NOT EXISTS profile TEXT NOT NULL DEFAULT '';
