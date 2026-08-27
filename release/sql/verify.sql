-- What must be true after install.sh, asserted by the database itself.
--
-- The point of this file is the negative half. A grant block that ran
-- without error proves the statements were accepted; it does not prove
-- that the panel *cannot* read analytics, because nothing in a GRANT
-- says what was not granted. That claim is the security property, and
-- this is where it is checked.
--
-- Every row must come back t. A single f is an installation that looks
-- finished and is not, so install.sh refuses rather than reporting.

SELECT 'collector can write its own table' AS iddia,
       has_table_privilege('collector', 'traffic_snapshots', 'INSERT') AS dogru
UNION ALL SELECT 'beacon can write its own table',
       has_table_privilege('beacon_writer', 'beacon_events', 'INSERT')
UNION ALL SELECT 'the API can read traffic',
       has_table_privilege('analytics_reader', 'traffic_snapshots', 'SELECT')
UNION ALL SELECT 'the API can read beacon events',
       has_table_privilege('analytics_reader', 'beacon_events', 'SELECT')
UNION ALL SELECT 'the panel can use its own tables',
       has_table_privilege('panel_user', 'panel_users', 'SELECT')

-- The negatives. These are the reason the file exists.
UNION ALL SELECT 'the panel CANNOT read traffic',
       NOT has_table_privilege('panel_user', 'traffic_snapshots', 'SELECT')
UNION ALL SELECT 'the panel CANNOT read beacon events',
       NOT has_table_privilege('panel_user', 'beacon_events', 'SELECT')
UNION ALL SELECT 'the API CANNOT write traffic',
       NOT has_table_privilege('analytics_reader', 'traffic_snapshots', 'INSERT')
UNION ALL SELECT 'the API CANNOT write beacon events',
       NOT has_table_privilege('analytics_reader', 'beacon_events', 'INSERT')
UNION ALL SELECT 'the beacon CANNOT read what it wrote',
       NOT has_table_privilege('beacon_writer', 'beacon_events', 'SELECT')
UNION ALL SELECT 'the collector CANNOT touch beacon events',
       NOT has_table_privilege('collector', 'beacon_events', 'SELECT')
UNION ALL SELECT 'nobody can erase the audit log',
       NOT has_table_privilege('panel_user', 'panel_audit_log', 'DELETE')
UNION ALL SELECT 'nobody can rewrite the audit log',
       NOT has_table_privilege('panel_user', 'panel_audit_log', 'UPDATE')

-- The mail account. The only recoverable secret in this database, so the
-- only one where a SELECT is worth something to whoever holds it.
--
-- The collector and the beacon are granted SELECT on panel_settings, and
-- that grant is the reason panel_smtp is not a settings key: it would
-- have carried the mail password to two processes that face the public
-- internet and have no use for it. Asserted rather than assumed, because
-- the failure mode is a line added to the settings GRANT months from now
-- by somebody who never read this paragraph.
UNION ALL SELECT 'the panel can use the mail account',
       has_table_privilege('panel_user', 'panel_smtp', 'SELECT')
UNION ALL SELECT 'the collector CANNOT read the mail account',
       NOT has_table_privilege('collector', 'panel_smtp', 'SELECT')
UNION ALL SELECT 'the beacon CANNOT read the mail account',
       NOT has_table_privilege('beacon_writer', 'panel_smtp', 'SELECT')
UNION ALL SELECT 'the API CANNOT read the mail account',
       NOT has_table_privilege('analytics_reader', 'panel_smtp', 'SELECT')

-- And that the roles exist at all.
--
-- Without this, a typo turns every negative above into a pass:
-- has_table_privilege reports no privileges for a role nobody created,
-- so "the panel cannot read analytics" is true of a role that does not
-- exist. The isolation would look verified when nothing was.
UNION ALL SELECT 'all four roles exist',
       (SELECT count(*) FROM pg_roles
        WHERE rolname IN ('collector','beacon_writer','analytics_reader','panel_user')) = 4

-- And that none of them is a superuser.
--
-- A superuser holds every privilege by definition, so every negative
-- assertion above turns false for one - which is exactly what should
-- happen, and without this line the failure is baffling: the grants are
-- correct, the file says the panel can read analytics, and nothing
-- explains why.
--
-- It is also a real check rather than a diagnostic. A role that got
-- SUPERUSER from a previous setup, or from an operator debugging a
-- connection problem, has silently lost every isolation property this
-- design rests on while continuing to work perfectly.
UNION ALL SELECT 'none of the four is a superuser',
       NOT EXISTS (SELECT 1 FROM pg_roles
                   WHERE rolsuper
                     AND rolname IN ('collector','beacon_writer','analytics_reader','panel_user'))

-- The defaults nobody chose, from harden.sql.
--
-- Every one of these is something PostgreSQL or TimescaleDB switched on
-- without being asked, so none of them appears as a missing GRANT in any
-- privilege listing. They are only visible if something goes looking,
-- which is what this block is.

-- Telemetry off. TimescaleDB ships with a job that reports to
-- telemetry.timescale.com every twenty-four hours. No visitor data in
-- it, and that is beside the point: this product's premise is that a
-- customer's traffic never leaves their machine, and a daily outbound
-- connection to a third party contradicts the premise whatever the
-- payload is.
UNION ALL SELECT 'timescaledb telemetry is off',
       current_setting('timescaledb.telemetry_level', true) = 'off'

-- Nobody may connect to this database except the roles that were named.
--
-- PostgreSQL grants CONNECT to PUBLIC on every new database. On its own
-- that looks harmless - a stranger holds no privileges on any table -
-- and it stops looking harmless next to TimescaleDB's catalog, which is
-- world-readable by design: a connected stranger can enumerate the
-- hypertables, the chunks and the time ranges they cover.
UNION ALL SELECT 'PUBLIC cannot connect to this database',
       NOT has_database_privilege('public', current_database(), 'CONNECT')
UNION ALL SELECT 'the four roles still can',
       has_database_privilege('collector', current_database(), 'CONNECT')
   AND has_database_privilege('beacon_writer', current_database(), 'CONNECT')
   AND has_database_privilege('analytics_reader', current_database(), 'CONNECT')
   AND has_database_privilege('panel_user', current_database(), 'CONNECT')

-- No role may schedule a background job.
--
-- Measured before it was closed: panel_user - no rights outside the
-- panel_* tables, no CREATE anywhere, no superuser anything - called
-- add_job() and got job id 1000, owner panel_user, on a one-hour
-- schedule. A job outlives the session, the pool and a restart of the
-- application that made it. That is persistence, which is the shape of a
-- back door whatever privileges it carries.
--
-- Counted rather than named. The signatures change between TimescaleDB
-- releases, and a check written against today's would pass tomorrow by
-- matching nothing.
UNION ALL SELECT 'no role can schedule a background job',
       NOT EXISTS (
         SELECT 1 FROM pg_proc p
         JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE p.proname IN (
                 'add_job', 'alter_job', 'delete_job', 'run_job',
                 'add_retention_policy', 'remove_retention_policy',
                 'add_compression_policy', 'remove_compression_policy',
                 'add_continuous_aggregate_policy',
                 'remove_continuous_aggregate_policy',
                 'add_reorder_policy', 'remove_reorder_policy')
           AND n.nspname NOT IN ('pg_catalog', 'information_schema')
           AND has_function_privilege('public', p.oid, 'EXECUTE'))

-- And that the job table is empty. The REVOKE above stops a new one; it
-- does nothing about one planted before the hardening was applied, or by
-- somebody who had the privilege for the ten minutes between install and
-- this file existing.
--
-- TimescaleDB's own internal policies are excluded by owner: they belong
-- to the installing superuser and are what keeps its job-error history
-- from growing forever.
UNION ALL SELECT 'no service role owns a background job',
       NOT EXISTS (
         SELECT 1 FROM timescaledb_information.jobs
         WHERE owner::text IN ('collector','beacon_writer','analytics_reader','panel_user'))

-- The address ranges, which nothing granted until an installed run went
-- looking. Both writers refresh them; nobody else has any business in
-- them at all.
UNION ALL SELECT 'the writers can refresh the address ranges',
       has_table_privilege('collector', 'ip_asn_ranges', 'SELECT')
   AND has_table_privilege('collector', 'ip_asn_ranges', 'INSERT')
   AND has_table_privilege('collector', 'ip_asn_ranges', 'TRUNCATE')
   AND has_table_privilege('collector', 'ip_country_ranges', 'SELECT')
   AND has_table_privilege('beacon_writer', 'ip_asn_ranges', 'SELECT')
   AND has_table_privilege('beacon_writer', 'ip_country_ranges', 'TRUNCATE')
UNION ALL SELECT 'the panel cannot see the address ranges',
       NOT has_table_privilege('panel_user', 'ip_asn_ranges', 'SELECT')
   AND NOT has_table_privilege('panel_user', 'ip_country_ranges', 'SELECT')

-- Retention, which the revoke above would otherwise have turned off.
--
-- The collector applies a retention policy at every start and cannot
-- own the hypertable, so it reaches TimescaleDB through the SECURITY
-- DEFINER wrappers in internal/retention/schema.sql. Three facts about
-- them, and the third is the one that would turn the fix into a hole.
UNION ALL SELECT 'the retention wrappers are installed',
       (SELECT count(*) FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public'
          AND p.proname IN ('ca_set_retention', 'ca_trim_site_rows',
                            'ca_count_site_rows', 'ca_check_retention_caller')) = 4
UNION ALL SELECT 'the two writers can ask for a retention interval',
       has_function_privilege('collector', 'public.ca_set_retention(text, integer)', 'EXECUTE')
   AND has_function_privilege('beacon_writer', 'public.ca_set_retention(text, integer)', 'EXECUTE')

-- PostgreSQL grants EXECUTE on a new function to PUBLIC. A SECURITY
-- DEFINER function left at that default runs as its owner for anybody
-- who can connect, which is the precise shape of what section 3 above
-- was written to close - so the wrappers would have reopened it.
UNION ALL SELECT 'PUBLIC cannot call the retention wrappers',
       NOT EXISTS (
         SELECT 1 FROM pg_proc p
         JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE n.nspname = 'public'
           AND p.proname IN ('ca_set_retention', 'ca_trim_site_rows',
                             'ca_count_site_rows', 'ca_check_retention_caller')
           AND has_function_privilege('public', p.oid, 'EXECUTE'))

-- A SECURITY DEFINER function that inherits the caller's search_path
-- lets the caller decide which add_retention_policy runs. Asserted
-- rather than trusted to the file that declares it, because a later
-- CREATE OR REPLACE that forgets the SET is a one-line change with no
-- visible symptom.
UNION ALL SELECT 'every retention wrapper pins its search_path',
       NOT EXISTS (
         SELECT 1 FROM pg_proc p
         JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE n.nspname = 'public'
           AND p.proname IN ('ca_set_retention', 'ca_trim_site_rows',
                             'ca_count_site_rows', 'ca_check_retention_caller')
           AND (NOT p.prosecdef
                OR p.proconfig IS NULL
                OR NOT EXISTS (SELECT 1 FROM unnest(p.proconfig) c
                                WHERE c LIKE 'search\_path=%')))

-- And the privilege the wrappers exist instead of. A writer that held
-- DELETE could empty its own table in one statement; through the
-- wrapper it can only remove one named site's rows past a bounded age.
UNION ALL SELECT 'neither writer holds DELETE on its own table',
       NOT has_table_privilege('collector', 'traffic_snapshots', 'DELETE')
   AND NOT has_table_privilege('beacon_writer', 'beacon_events', 'DELETE')

-- The heartbeat table, and the one rule a GRANT cannot express.
--
-- Four services write to one table, so "only your own row" has to come
-- from row-level security rather than from a privilege. What is checked
-- here is that RLS is switched on at all: a table with the policies
-- defined and ENABLE ROW LEVEL SECURITY missing enforces nothing, reads
-- perfectly in \d, and hands every service every row.
--
-- Whether the policies actually refuse a cross-service write is not a
-- question a privilege query can answer - it needs an attempt - so that
-- is asserted in release/install_test.go against a real connection.
UNION ALL SELECT 'the heartbeat table has row-level security on',
       (SELECT relrowsecurity FROM pg_class WHERE relname = 'service_heartbeat')
UNION ALL SELECT 'the heartbeat has both a read and a write policy',
       (SELECT count(*) FROM pg_policies
        WHERE tablename = 'service_heartbeat') >= 2
UNION ALL SELECT 'the panel can read the heartbeat',
       has_table_privilege('panel_user', 'service_heartbeat', 'SELECT')
UNION ALL SELECT 'nobody can delete a heartbeat row',
       NOT has_table_privilege('collector', 'service_heartbeat', 'DELETE')
   AND NOT has_table_privilege('beacon_writer', 'service_heartbeat', 'DELETE')
   AND NOT has_table_privilege('analytics_reader', 'service_heartbeat', 'DELETE')
   AND NOT has_table_privilege('panel_user', 'service_heartbeat', 'DELETE')

-- And that no service role owns a table.
--
-- This is the one that no GRANT reveals and no privilege listing makes
-- obvious. A table's owner holds every privilege on it implicitly, for
-- ever, regardless of what was granted or revoked - so if the schemas
-- were applied over a connection authenticated as `collector`, that role
-- owns every table and the entire isolation is void while looking
-- perfect. The grants read correctly. \dp reads correctly. And the panel
-- can read analytics.
--
-- Found exactly that way: the first run of install.sh here used a
-- superuser DSN that happened to be the collector role, and this file
-- reported that the collector could touch beacon_events while every
-- grant in grants.sql was applied correctly.
--
-- Ownership belongs to the superuser that installs, never to a role that
-- a service logs in as.
UNION ALL SELECT 'no service role owns a table',
       NOT EXISTS (
         SELECT 1 FROM pg_tables
         WHERE schemaname = 'public'
           AND tableowner IN ('collector','beacon_writer','analytics_reader','panel_user'));
