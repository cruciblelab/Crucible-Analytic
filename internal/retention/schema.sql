-- Retention, as three privileged functions rather than three grants.
--
-- # What was wrong
--
-- internal/retention has worked in its integration suite since the day
-- it was written and had never once run on an installed deployment.
-- Both facts are true for the same reason: the suite connects as
-- `collector` to a development database that `collector` created, so
-- collector owns the hypertables there. On a machine installed by
-- release/install.sh the tables belong to the superuser that ran it,
-- and TimescaleDB checks ownership rather than privilege:
--
--     ERROR:  must be owner of hypertable "traffic_snapshots"
--
-- measured against a freshly installed database with EXECUTE on
-- add_retention_policy granted to collector explicitly. The grant is
-- not the obstacle; the ownership is. And the per-site trim needed
-- DELETE, which grants.sql has never given either.
--
-- So the end-to-end run found the collector logging
--
--     retention: remove existing policy on traffic_snapshots:
--     ERROR: permission denied for function remove_retention_policy
--
-- on every start, both hypertables growing forever, and the disk
-- filling on a machine that also serves the customer's website - which
-- is the exact outcome the retention package's own comment says it
-- exists to prevent.
--
-- # Why not the obvious fixes
--
-- **Give the roles the functions.** Measured above: ownership, not
-- privilege. It does not work.
--
-- **Give the roles the tables.** It works, and it hands the collector
-- DROP and ALTER on the table it writes to. A process that is reachable
-- from the internet on the traffic path should not be able to drop the
-- table.
--
-- **Give the roles DELETE.** It makes the per-site trim work and lets a
-- compromised collector erase every row of history in one statement.
--
-- **Do it once, from install.sh, as the superuser.** It works on the
-- day of the install and never again. Retention is a number in a config
-- file; when the operator changes it the service restarts and has to
-- apply the new value, and there is nobody else there to do it.
--
-- # What this is
--
-- Four SECURITY DEFINER functions, owned by the role that installs,
-- each one narrower than the privilege it replaces:
--
--   - ca_set_retention cannot schedule anything except a retention
--     policy, on one of two named tables, for a number of days inside
--     the same bounds internal/retention enforces. add_job takes an
--     arbitrary function name; this takes an interval.
--   - ca_trim_site_rows deletes only rows of one named site older than
--     a bounded number of days. DELETE on the table would be every row
--     of every site.
--   - ca_count_site_rows is the dry run for it, and exists separately
--     because beacon_writer holds INSERT and not SELECT: the panel is
--     supposed to be able to say "this will remove 4.2 million rows"
--     before the button is pressed, and the role that would do the
--     removing cannot count them.
--
-- What remains, said plainly: a compromised collector can shorten its
-- own table's retention and destroy history. It cannot lengthen it past
-- the legal ceiling, touch the other service's table, run anything of
-- its own on a schedule, or drop a table. That residue is accepted
-- because the same compromised process already decides what every
-- future row says, and the alternative on offer is a retention feature
-- that has never run.
--
-- # The hazard in the mechanism
--
-- SECURITY DEFINER runs as the definer, so a caller who controls
-- search_path controls which `add_retention_policy` gets called. Every
-- function below pins search_path, and the REVOKE from PUBLIC at the
-- end is load-bearing rather than tidy: PostgreSQL grants EXECUTE on a
-- new function to PUBLIC by default, and a SECURITY DEFINER function
-- left at that default is precisely the back door this file is written
-- to avoid.

-- ca_set_retention installs or replaces one hypertable's policy.
--
-- Removed before added: TimescaleDB refuses a second policy on the same
-- hypertable, so add alone would fail on every change after the first
-- and the failure would read like a permissions problem.
CREATE OR REPLACE FUNCTION ca_set_retention(p_table text, p_days integer)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
    PERFORM ca_check_retention_caller(p_table, p_days);
    -- ::regclass rather than leaving the text to coerce itself. Both
    -- TimescaleDB functions take a regclass, and a bare text *variable*
    -- does not resolve to one the way a bare literal does - which is
    -- the sort of difference that works at a psql prompt and fails
    -- inside the function that was written from it.
    PERFORM public.remove_retention_policy(p_table::regclass, if_exists => true);
    PERFORM public.add_retention_policy(p_table::regclass,
                                        drop_after => make_interval(days => p_days));
END
$$;

-- ca_trim_site_rows removes one site's rows older than its own figure.
--
-- The one thing dropping a chunk cannot express: a chunk holds every
-- site's rows for its time range, so a site that asked to keep less
-- than the deployment does needs a row-level delete. It runs only for
-- such a site, which is why it is not the ordinary path.
CREATE OR REPLACE FUNCTION ca_trim_site_rows(p_table text, p_site text, p_days integer)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    removed bigint;
BEGIN
    PERFORM ca_check_retention_caller(p_table, p_days);

    -- Written out per table rather than assembled from p_table, so what
    -- runs is visible in full at the point it is read. p_table has
    -- already been checked against a closed set; this is the second
    -- reason it can never reach a statement as text.
    IF p_table = 'traffic_snapshots' THEN
        DELETE FROM traffic_snapshots
         WHERE site_id = p_site AND time < now() - make_interval(days => p_days);
    ELSE
        DELETE FROM beacon_events
         WHERE site_id = p_site AND time < now() - make_interval(days => p_days);
    END IF;

    GET DIAGNOSTICS removed = ROW_COUNT;
    RETURN removed;
END
$$;

-- ca_count_site_rows is the same question without the deletion.
CREATE OR REPLACE FUNCTION ca_count_site_rows(p_table text, p_site text, p_days integer)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    n bigint;
BEGIN
    PERFORM ca_check_retention_caller(p_table, p_days);

    IF p_table = 'traffic_snapshots' THEN
        SELECT count(*) INTO n FROM traffic_snapshots
         WHERE site_id = p_site AND time < now() - make_interval(days => p_days);
    ELSE
        SELECT count(*) INTO n FROM beacon_events
         WHERE site_id = p_site AND time < now() - make_interval(days => p_days);
    END IF;

    RETURN n;
END
$$;

-- ca_set_compression turns on site-segmented compression and compresses
-- the chunks that are old enough, or reports why it could not.
--
-- # Why this is here and not in internal/storage/schema.sql
--
-- Compression is a Timescale-License feature. On a deployment running
-- the Apache-licensed build it does not exist, and an
-- `ALTER TABLE ... SET (timescaledb.compress ...)` sitting in a schema
-- file would abort that file - on every install and, worse, on every
-- schema upgrade afterwards. A customer whose PostgreSQL happens to
-- carry the other build would find their upgrades failing on a feature
-- they never asked for.
--
-- So nothing about compression is in a schema file. It happens here, at
-- run time, and the caller treats failure as a warning - the same
-- discipline the retention loop already follows, and for the same
-- reason: this runs inside a process on the traffic path.
--
-- # Why segment by site_id
--
-- Measured, 2026-09-09. The hypertable orders rows by time; every query
-- the dashboard makes asks by site. So one site's rows are interleaved
-- with every other site's, and reading 20.748 rows for one site touched
-- 13.632 heap pages - about one page per row, 3,1 GB for a single
-- summary. Compressing with site_id as the segment stores each site's
-- rows together.
--
-- # The segment is written out even though this version would guess it
--
-- Measured: TimescaleDB 2.17 picks site_id by itself here, reading it
-- off the (site_id, time DESC) index, and says so - with a warning that
-- it was not certain. A mutation removing the setting below therefore
-- survives every test, correctly: on this version it changes nothing.
--
-- It stays because it costs one line and buys not depending on a
-- heuristic. The guess is made from an index, so a future migration that
-- reorders or drops that index would silently change how the data is
-- laid out - and nothing would fail, it would only get slow again.
-- Naming the column makes the layout a decision rather than an
-- inference.
--
-- Setting it to the *wrong* column is caught, which is what makes the
-- assertion in the suite a real one.
--
-- # Why it compresses the chunks itself instead of scheduling a policy
--
-- The obvious implementation is add_compression_policy: hand TimescaleDB
-- the age and let its background worker do the work forever. The first
-- version did exactly that, and its tests passed.
--
-- They passed because of who owned this function in the development
-- database. A SECURITY DEFINER function runs as its owner, and there it
-- happened to be the superuser. On a real install it is not:
-- release/sql/grants.sql hands every routine in public over to
-- schema_admin at the end of an install, deliberately, so that no part
-- of this product runs as a superuser.
--
-- And schema_admin cannot call add_compression_policy. Measured on
-- 2.17.2:
--
--	add_retention_policy       schema_admin may  (granted in grants.sql)
--	remove_retention_policy    schema_admin may  (granted in grants.sql)
--	add_compression_policy     denied            {postgres=X/postgres}
--	remove_compression_policy  denied            {postgres=X/postgres}
--	compress_chunk             may               (default: PUBLIC)
--	show_chunks                may               (default: PUBLIC)
--
-- The two retention entry points are granted by hand in grants.sql,
-- which is where a hand-written list of names had already been read once
-- and not updated. So the policy call would have failed on every real
-- deployment, and failed the quiet way: the caller reads any failure of
-- this function as "this database cannot compress", logs one line at
-- Info, and carries on. Nothing red, nothing compressed, a dashboard
-- that stays broken and a fix that everybody believes shipped.
--
-- The grant cannot be moved into a schema file either, which is the
-- repository's usual answer (see the fetch-log note in NOTES.md): these
-- functions belong to the timescaledb extension and are owned by
-- postgres, so schema_admin has nothing to grant. The upgrade button
-- runs as schema_admin and never runs grants.sql.
--
-- So this compresses the chunks itself, with compress_chunk, which needs
-- only ownership of the hypertable - which schema_admin has, and which
-- is the same privilege the other three wrappers already rest on. The
-- work then rides the loop that already calls this function every
-- retention interval, and no install path needs a privilege it does not
-- already have.
--
-- What is given up: TimescaleDB's policy also recompresses a chunk that
-- received late rows after being compressed. Rows written into a
-- compressed chunk stay readable either way - they land in the
-- uncompressed part - so this costs disk on a table nobody backfills,
-- not correctness. if_not_compressed skips what is already done.
--
-- # What it will not do
--
-- Change the settings on a table that already has them. Altering
-- segmentby requires decompressing every chunk first, and a service
-- doing that to a customer's history at startup - unasked, on the
-- traffic path - is not a thing this function is allowed to decide. It
-- reports the mismatch instead and leaves the data alone.
CREATE OR REPLACE FUNCTION ca_set_compression(p_table text, p_after_days integer)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    current_segmentby text;
    chunk             regclass;
BEGIN
    PERFORM ca_check_retention_caller(p_table, p_after_days);

    -- segmentby is text in this view, not an array. Measured rather
    -- than assumed: the first version of this function wrapped it in
    -- array_to_string and failed with "function array_to_string(text,
    -- unknown) does not exist" - which the caller then reported as
    -- "this database cannot compress", because that is what every
    -- failure of this call was being read as.
    SELECT cs.segmentby INTO current_segmentby
      FROM timescaledb_information.hypertable_compression_settings cs
     WHERE cs.hypertable = p_table::regclass;

    IF current_segmentby IS NULL THEN
        -- Written out per table rather than assembled from p_table, for
        -- the reason ca_trim_site_rows gives: what runs is visible in
        -- full at the point it is read.
        IF p_table = 'traffic_snapshots' THEN
            ALTER TABLE traffic_snapshots SET (
                timescaledb.compress,
                timescaledb.compress_segmentby = 'site_id',
                timescaledb.compress_orderby   = 'time DESC');
        ELSE
            ALTER TABLE beacon_events SET (
                timescaledb.compress,
                timescaledb.compress_segmentby = 'site_id',
                timescaledb.compress_orderby   = 'time DESC');
        END IF;
        current_segmentby := 'site_id';
    ELSIF current_segmentby <> 'site_id' THEN
        RETURN 'segmentby is ' || current_segmentby ||
               ', not site_id; left alone because changing it decompresses every chunk';
    END IF;

    -- One pass over the chunks that are old enough. Bounded by
    -- p_after_days rather than by a count: a deployment turning this on
    -- for the first time has a backlog, and the alternative to
    -- compressing it is leaving the dashboard slow for as many retention
    -- intervals as there are chunks.
    --
    -- if_not_compressed so a chunk already done is skipped rather than
    -- raising - this runs on every pass of the retention loop, so all
    -- but the first pass finds most of them done.
    FOR chunk IN
        SELECT c FROM public.show_chunks(p_table::regclass,
                                         older_than => make_interval(days => p_after_days)) c
    LOOP
        PERFORM public.compress_chunk(chunk, if_not_compressed => true);
    END LOOP;

    RETURN 'ok';
END
$$;

-- ca_check_retention_caller is the guard all four share.
--
-- One function rather than three copies, because three copies of a
-- check are three chances for one of them to drift - and the one that
-- drifts is the one nobody reads.
--
-- session_user rather than current_user: inside a SECURITY DEFINER
-- body current_user is the definer, so a check on it would compare the
-- owner against itself and pass for everybody.
CREATE OR REPLACE FUNCTION ca_check_retention_caller(p_table text, p_days integer)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    owner_role text;
BEGIN
    -- The table, from a closed set. Anything else is refused before the
    -- name can reach a statement.
    owner_role := CASE p_table
        WHEN 'traffic_snapshots' THEN 'collector'
        WHEN 'beacon_events'     THEN 'beacon_writer'
        ELSE NULL
    END;
    IF owner_role IS NULL THEN
        RAISE EXCEPTION 'retention: unknown table %', p_table
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    -- The bounds internal/retention enforces, enforced again here.
    -- Not a duplicate: the Go check protects a config file from a typo,
    -- and this one protects the database from whatever is calling it.
    -- A ceiling that only one of them holds is a ceiling.
    IF p_days < 1 OR p_days > 730 THEN
        RAISE EXCEPTION 'retention: % days is outside 1..730', p_days
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    -- Each service reaches its own table and no other. A superuser is
    -- allowed through so the installer and an operator at a psql prompt
    -- can use the same functions the services do, rather than a second
    -- path that is tested by nobody.
    IF session_user <> owner_role
       AND NOT COALESCE((SELECT rolsuper FROM pg_catalog.pg_roles
                          WHERE rolname = session_user), false) THEN
        RAISE EXCEPTION 'retention: % may not manage retention on %', session_user, p_table
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

-- PUBLIC gets nothing, and this is the line that makes the file safe.
--
-- PostgreSQL grants EXECUTE on every new function to PUBLIC. Left
-- alone, the three functions above would be callable by any role on the
-- cluster - and they run as their owner. The guard inside them would
-- still refuse a stranger, but a SECURITY DEFINER function whose only
-- protection is its own body is one editing mistake away from being the
-- hole it was written to close.
REVOKE ALL ON FUNCTION ca_set_retention(text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION ca_trim_site_rows(text, text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION ca_count_site_rows(text, text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION ca_set_compression(text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION ca_check_retention_caller(text, integer) FROM PUBLIC;

-- And the two services get exactly the three they call. Both roles on
-- all three: the guard decides which table each may touch, so the grant
-- does not have to encode it twice.
--
-- DO blocks because this schema is applied to databases whose roles
-- exist and to development databases where they may not, and a GRANT to
-- a role that does not exist aborts the file.
--
-- Each grant also asks whether it is needed, for the reason written out
-- at the same place in internal/asnlookup/schema.sql: GRANT rewrites the
-- target's ACL tuple even when it changes nothing in it, and two
-- appliers doing that at once collide with "tuple concurrently updated".
-- These are functions rather than tables, so the question is
-- has_function_privilege and the identity is the signature - two
-- functions here differ only in their arguments.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'collector') THEN
        IF NOT has_function_privilege('collector', 'ca_set_retention(text, integer)', 'EXECUTE') THEN
            GRANT EXECUTE ON FUNCTION ca_set_retention(text, integer) TO collector;
        END IF;
        IF NOT has_function_privilege('collector', 'ca_trim_site_rows(text, text, integer)', 'EXECUTE') THEN
            GRANT EXECUTE ON FUNCTION ca_trim_site_rows(text, text, integer) TO collector;
        END IF;
        IF NOT has_function_privilege('collector', 'ca_count_site_rows(text, text, integer)', 'EXECUTE') THEN
            GRANT EXECUTE ON FUNCTION ca_count_site_rows(text, text, integer) TO collector;
        END IF;
        IF NOT has_function_privilege('collector', 'ca_set_compression(text, integer)', 'EXECUTE') THEN
            GRANT EXECUTE ON FUNCTION ca_set_compression(text, integer) TO collector;
        END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'beacon_writer') THEN
        IF NOT has_function_privilege('beacon_writer', 'ca_set_retention(text, integer)', 'EXECUTE') THEN
            GRANT EXECUTE ON FUNCTION ca_set_retention(text, integer) TO beacon_writer;
        END IF;
        IF NOT has_function_privilege('beacon_writer', 'ca_trim_site_rows(text, text, integer)', 'EXECUTE') THEN
            GRANT EXECUTE ON FUNCTION ca_trim_site_rows(text, text, integer) TO beacon_writer;
        END IF;
        IF NOT has_function_privilege('beacon_writer', 'ca_count_site_rows(text, text, integer)', 'EXECUTE') THEN
            GRANT EXECUTE ON FUNCTION ca_count_site_rows(text, text, integer) TO beacon_writer;
        END IF;
        IF NOT has_function_privilege('beacon_writer', 'ca_set_compression(text, integer)', 'EXECUTE') THEN
            GRANT EXECUTE ON FUNCTION ca_set_compression(text, integer) TO beacon_writer;
        END IF;
    END IF;
END
$$;
