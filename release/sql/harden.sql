-- Hardening: the privileges that are wrong by *default* rather than by
-- omission.
--
-- grants.sql says what each role may do. This file says what nothing may
-- do - and every line in it closes something PostgreSQL or TimescaleDB
-- switched on without being asked, which is why none of it shows up in a
-- privilege listing that reads correctly.
--
-- Each statement is idempotent, so install.sh applies it on every run.
-- verify.sql asserts the result: a REVOKE that ran and a REVOKE that
-- took effect are different facts.

-- 1. Telemetry.
--
-- TimescaleDB ships with telemetry_level = 'basic' and a job that reports
-- to telemetry.timescale.com every twenty-four hours: version, extension
-- list, operating system, hypertable and chunk counts, row counts.
--
-- No visitor data, and that is not the point. This product's premise is
-- that a customer's traffic never leaves their own machine, and the
-- database underneath it opening a daily outbound connection to a third
-- party is a contradiction of that premise whatever is in the payload.
-- Nobody chose it, and a deployment that keeps it should keep it
-- deliberately.
--
-- Set on the database rather than in postgresql.conf: this file owns one
-- database and must not change how the rest of the cluster behaves.
ALTER DATABASE :"dbname" SET timescaledb.telemetry_level = 'off';

-- 2. Who may connect to this database at all.
--
-- PostgreSQL grants CONNECT and TEMP on every new database to PUBLIC.
-- So does this one, which means any role that exists anywhere on the
-- cluster - a role for some other application, a leftover from a
-- migration - can open a connection here.
--
-- On its own that reads harmless: they hold no privileges on any table.
-- It stops reading harmless next to point 3 and next to TimescaleDB's
-- catalog, which is world-readable by design: a connected stranger can
-- enumerate the hypertables, the chunk names and the time ranges they
-- cover. That is a map of the deployment handed to somebody who was
-- never granted a single row.
--
-- The four service roles are granted CONNECT explicitly in grants.sql,
-- so revoking the blanket grant costs them nothing.
REVOKE CONNECT, TEMPORARY ON DATABASE :"dbname" FROM PUBLIC;

-- 3. Background jobs.
--
-- This is the one worth reading twice.
--
-- TimescaleDB grants EXECUTE on its job-management functions to PUBLIC.
-- Measured on a real installation: panel_user - a role with no rights
-- outside the panel_* tables, no CREATE anywhere, and no superuser
-- anything - called add_job() and got job id 1000 back. The job was
-- stored with owner = panel_user and a one-hour schedule, and it
-- survives the session, the connection pool, and a restart of the
-- application that created it.
--
-- It is not privilege escalation: a job runs as the role that owns it,
-- so it can do nothing that role could not do at a psql prompt. What it
-- is, is *persistence*. A process compromised for one minute can leave
-- behind something that keeps running for months, in a place nobody
-- looks, that no amount of restarting the service removes. That is the
-- shape of a back door regardless of what privileges it carries.
--
-- It is also a denial of service on a resource nothing else limits:
-- background worker slots are a fixed cluster-wide pool, and jobs are
-- scheduled from it.
--
-- Nothing in this product schedules a job. Compression and retention
-- policies, if a deployment ever wants them, are applied by the
-- superuser that installs - which is where that decision belongs.
--
-- Revoked by a loop rather than by signature: add_job and alter_job have
-- several overloads, they have gained arguments between TimescaleDB
-- releases, and a hand-written signature list would quietly stop
-- matching after an upgrade - leaving the grant open with a REVOKE above
-- it that no longer names anything.
--
-- ON ROUTINE rather than ON FUNCTION, and that is not tidiness. The
-- first version of this loop said FUNCTION and aborted on run_job, which
-- is a PROCEDURE - "public.run_job(integer) is not a function". A DO
-- block stops at the first error, so every name after it in the list was
-- silently left granted while the script reported having run. ROUTINE
-- covers both kinds, which is why the check in verify.sql exists as
-- well: a REVOKE that ran and a REVOKE that took effect are different
-- facts, and this file proved its own point on its first execution.
DO $$
DECLARE
    fn record;
BEGIN
    FOR fn IN
        SELECT n.nspname AS schema_name,
               p.proname AS func_name,
               pg_get_function_identity_arguments(p.oid) AS args
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE p.proname IN (
                  'add_job', 'alter_job', 'delete_job', 'run_job',
                  'add_retention_policy', 'remove_retention_policy',
                  'add_compression_policy', 'remove_compression_policy',
                  'add_continuous_aggregate_policy',
                  'remove_continuous_aggregate_policy',
                  'add_reorder_policy', 'remove_reorder_policy'
              )
          AND n.nspname NOT IN ('pg_catalog', 'information_schema')
    LOOP
        EXECUTE format('REVOKE EXECUTE ON ROUTINE %I.%I(%s) FROM PUBLIC',
                       fn.schema_name, fn.func_name, fn.args);
    END LOOP;
END
$$;
