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
