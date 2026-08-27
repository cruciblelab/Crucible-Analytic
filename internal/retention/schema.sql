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
-- Three SECURITY DEFINER functions, owned by the role that installs,
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

-- ca_check_retention_caller is the guard all three share.
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
REVOKE ALL ON FUNCTION ca_check_retention_caller(text, integer) FROM PUBLIC;

-- And the two services get exactly the three they call. Both roles on
-- all three: the guard decides which table each may touch, so the grant
-- does not have to encode it twice.
--
-- DO blocks because this schema is applied to databases whose roles
-- exist and to development databases where they may not, and a GRANT to
-- a role that does not exist aborts the file.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'collector') THEN
        GRANT EXECUTE ON FUNCTION ca_set_retention(text, integer) TO collector;
        GRANT EXECUTE ON FUNCTION ca_trim_site_rows(text, text, integer) TO collector;
        GRANT EXECUTE ON FUNCTION ca_count_site_rows(text, text, integer) TO collector;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'beacon_writer') THEN
        GRANT EXECUTE ON FUNCTION ca_set_retention(text, integer) TO beacon_writer;
        GRANT EXECUTE ON FUNCTION ca_trim_site_rows(text, text, integer) TO beacon_writer;
        GRANT EXECUTE ON FUNCTION ca_count_site_rows(text, text, integer) TO beacon_writer;
    END IF;
END
$$;
