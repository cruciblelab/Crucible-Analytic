-- The privilege matrix.
--
-- This file is the single source: install.sh applies it and KURULUM.md
-- points at it rather than repeating it. A privilege block that exists in
-- both a document and a script is a block that drifts, and the direction
-- it drifts in is always the same - the script gets fixed and the
-- document keeps telling the next operator to grant something else.

-- collector: writes its own table.
GRANT SELECT, INSERT ON traffic_snapshots TO collector;

-- beacon: writes its own table and nothing else. No SELECT: it never
-- reads back what it wrote.
GRANT INSERT ON beacon_events TO beacon_writer;

-- The read API: writes nothing, and needs BOTH tables.
--
-- Granting only traffic_snapshots leaves /beacon/ and /crossover/
-- answering 500 while everything else works - a fault that is annoying to
-- diagnose precisely because most of the product is fine.
GRANT SELECT ON traffic_snapshots, beacon_events TO analytics_reader;

-- The panel: its own tables only. Never the analytics ones.
GRANT SELECT, INSERT, UPDATE, DELETE ON
  panel_users, panel_sessions, panel_site_members,
  panel_settings, panel_api_tokens, panel_dev_access,
  panel_owner_claims, panel_login_attempts, panel_recovery_codes,
  panel_smtp
  TO panel_user;

-- The audit log is append-only. No UPDATE and no DELETE, deliberately:
-- a compromised panel process must not be able to erase what it did.
GRANT SELECT, INSERT ON panel_audit_log TO panel_user;

-- Sequences, to panel_user only, and by name.
--
-- Every sequence in this database is the panel's: traffic_snapshots and
-- beacon_events have no BIGSERIAL, so collector and beacon_writer need
-- none. Writing "ALL SEQUENCES IN SCHEMA public" would hand them
-- authority over the panel's sequences in exchange for nothing.
--
-- By name for the same reason: "ALL" is a grant to whoever adds a
-- sequence to public tomorrow, which is a decision being made now by
-- somebody who is not here.
--
-- # Only the BIGSERIAL ones, and that distinction was measured
--
-- Three grants used to be here that nobody needed: the sequences behind
-- panel_upgrade_requests.id, panel_logs.id, and (nearly) the fetch log's.
-- All three columns are GENERATED ALWAYS AS IDENTITY, and PostgreSQL
-- treats an identity sequence as part of its column - INSERT on the
-- table is the whole permission. Measured on this database rather than
-- read from the manual: with every privilege on
-- panel_upgrade_requests_id_seq revoked, panel_user inserted and got
-- back id 740.
--
-- A privilege nobody needs is one nobody audits, which is what H5 was
-- about. The seven below are BIGSERIAL and genuinely do need it;
-- TestNoIdentitySequenceIsGranted keeps the two kinds from being
-- confused again.
--
-- # Deleting the GRANT does not remove the privilege
--
-- Measured, and it is the half that would have been missed: this file is
-- re-run on every install, and re-running it without a line simply does
-- not grant that line again. Every database already installed keeps what
-- it was given. So the removal has to be said out loud, once, here -
-- otherwise "we removed that privilege" would be true of the repository
-- and false of every deployment.
--
-- IF EXISTS on the sequence names is not available for REVOKE, so these
-- run against a database where the tables exist - which is every
-- database this file is applied to, since install.sh applies the schemas
-- first.
REVOKE ALL ON SEQUENCE panel_upgrade_requests_id_seq FROM PUBLIC, panel_user;
REVOKE ALL ON SEQUENCE panel_logs_id_seq
  FROM PUBLIC, collector, beacon_writer, analytics_reader, panel_user;
GRANT USAGE, SELECT ON
  panel_users_id_seq, panel_audit_log_id_seq, panel_api_tokens_id_seq,
  panel_dev_access_id_seq, panel_owner_claims_id_seq,
  panel_login_attempts_id_seq, panel_recovery_codes_id_seq
  TO panel_user;

-- The heartbeat: every service writes its own row, the panel reads them.
--
-- Four writers on one table, which is the only place in this schema
-- where a GRANT cannot express the whole rule - "only your own row" is
-- not something GRANT can say. Row-level security in
-- internal/heartbeat/schema.sql says it, keyed on current_user, so this
-- grant is deliberately broader than the actual permission.
--
-- No DELETE for anybody. A service has no reason to remove its own row,
-- and a row that disappears reads as "this service was never installed"
-- rather than "this service is gone" - which is the wrong sentence at
-- the moment it matters.
GRANT SELECT, INSERT, UPDATE ON service_heartbeat
  TO collector, beacon_writer, analytics_reader, panel_user;

-- The schema version, readable by the panel and writable by nobody.
--
-- SELECT only, and only for the panel, because today the one thing that
-- reads this is the health page. install.sh writes the row as the
-- installing superuser; no service role needs to.
--
-- L3 will add a writer - the upgrade applier - and it will be a GRANT
-- added beside this one rather than a change to it. That is why the
-- table's owner was settled when it was created rather than when it is
-- first written: an ownership decision made later would have moved this
-- file, verify.sql and install.sh a second time.
--
-- No INSERT or UPDATE for panel_user in particular. The panel showing a
-- version it could have written itself is not a report, and the whole
-- point of asking the database is that the answer comes from outside
-- the process being asked about.
GRANT SELECT ON schema_version TO panel_user;

-- The applier writes it. L1 decided this line's neighbour when it
-- created the table, precisely so that grants.sql would not have to be
-- reopened here - see PLAN.md, "L'nin iç bağları", Bağ 1.
GRANT SELECT, INSERT, UPDATE ON schema_version TO schema_admin;

-- ---------------------------------------------------- the upgrade queue
--
-- Asking and answering are different privileges held by different roles,
-- and that split is the whole security argument for the button:
--
--   panel_user    INSERT, SELECT     asks, and reads the answer
--   schema_admin  SELECT, UPDATE     answers, and cannot ask
--
-- Neither holds both. A compromised panel process can request an upgrade
-- - which is a button any signed-in customer can press anyway - and
-- cannot fabricate the result of one, so a row saying "succeeded" is
-- always something the applier wrote.
GRANT SELECT, INSERT, DELETE ON panel_upgrade_requests TO panel_user;
GRANT SELECT, UPDATE ON panel_upgrade_requests TO schema_admin;

-- ------------------------------------------------------ table ownership
--
-- Every table is owned by schema_admin, because ALTER TABLE requires
-- ownership and applying a migration is mostly ALTER TABLE.
--
-- This is the fifth role's whole reason for existing. The alternative is
-- a running service holding a superuser DSN on disk for its entire life,
-- which is worse in every direction: a superuser can create roles, read
-- every other database on the cluster, and install extensions, and none
-- of that is revocable without taking the account away entirely.
--
-- schema_admin can do none of those. It is bounded to this schema, and
-- it is a LOGIN role that can be locked.
--
-- Measured before it was adopted: the four SECURITY DEFINER retention
-- wrappers are owned by the superuser and run as their owner, so moving
-- table ownership does not disturb them. internal/retention's suite was
-- run against the moved ownership and passes.
--
-- An owner bypasses row-level security, so schema_admin sees every row
-- in every table. That is inherent to being able to migrate them, and it
-- is why nothing but the applier is given this DSN.
DO $$
DECLARE t text;
BEGIN
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public'
  LOOP
    EXECUTE format('ALTER TABLE public.%I OWNER TO schema_admin', t);
  END LOOP;
END $$;

-- The functions too, and this was found by measurement rather than
-- foresight: moving only the tables left the applier unable to re-apply
-- internal/retention/schema.sql, which does CREATE OR REPLACE FUNCTION
-- and therefore needs to own what it is replacing. The failure appeared
-- half way through a migration - after six files had been applied - so
-- it is exactly the shape this queue exists to record honestly.
--
-- These four are SECURITY DEFINER and run as their owner, which this
-- changes from the superuser to schema_admin.
--
-- The first version of this comment claimed that was harmless, because
-- schema_admin now owns the hypertables. It was wrong, and the retention
-- suite said so within the minute: owning the hypertable is not the only
-- thing the wrappers need. They call TimescaleDB's own
-- add_retention_policy and remove_retention_policy, and a definer that
-- is not a superuser needs EXECUTE on those - which is the block below,
-- and which is the same error internal/retention/schema.sql already
-- documents happening to the collector.
--
-- Left here as written because it is the useful kind of mistake: a
-- comment asserting a measurement that had not been taken, corrected by
-- taking it.
-- ALTER ROUTINE rather than ALTER FUNCTION, and extension members
-- excluded. Both were found by running it: TimescaleDB installs
-- procedures as well as functions into public, and ALTER FUNCTION
-- refuses a procedure - so the first version of this block stopped part
-- way through, having moved some objects and not others.
--
-- The extension's own objects must not move in any case. They belong to
-- the extension, ALTER EXTENSION is what manages them, and changing
-- their owner underneath it is a way to make a future TimescaleDB
-- upgrade fail for reasons nobody will connect to this file.
-- EXECUTE on the two TimescaleDB entry points the wrappers call.
--
-- Guarded, because a deployment without TimescaleDB has neither function
-- and must not fail its install over a policy it cannot use anyway -
-- the same guard internal/retention/schema.sql uses for its own grants.
-- The signature is looked up rather than written down. TimescaleDB has
-- added parameters to add_retention_policy across releases - the
-- installed one here takes seven - and a hardcoded signature would make
-- this file fail on some versions and silently grant nothing on others.
DO $$
DECLARE f record;
BEGIN
  FOR f IN
    SELECT p.oid::regprocedure AS sig
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = 'public'
      AND p.proname IN ('add_retention_policy', 'remove_retention_policy')
  LOOP
    EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO schema_admin', f.sig);
  END LOOP;
END $$;

DO $$
DECLARE f record;
BEGIN
  FOR f IN
    SELECT p.oid::regprocedure AS sig
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = 'public'
      AND NOT EXISTS (
        SELECT 1 FROM pg_depend d
        WHERE d.objid = p.oid
          AND d.classid = 'pg_proc'::regclass
          AND d.deptype = 'e')
  LOOP
    EXECUTE format('ALTER ROUTINE %s OWNER TO schema_admin', f.sig);
  END LOOP;
END $$;

-- The log table every service writes and only the panel reads.
--
-- INSERT for all four, SELECT for the panel. The row-level policy in
-- internal/panel/schema.sql is what keeps a service to its own rows;
-- this grant is deliberately broader than the actual permission, exactly
-- as service_heartbeat's is.
GRANT INSERT ON panel_logs TO collector, beacon_writer, analytics_reader, panel_user;
GRANT SELECT ON panel_logs TO panel_user;

-- DELETE for the panel alone, and it needs its own policy.
--
-- The write policy is FOR ALL keyed on current_user, so without this the
-- panel could delete only rows it wrote itself - and the retention sweep
-- exists precisely to remove the other three services' rows. Postgres
-- ORs permissive policies, so this widens DELETE for panel_user without
-- touching what a service may do.
--
-- Deleting log rows is allowed where deleting audit rows is not, and the
-- difference is the point of having two tables: a log line is
-- diagnostic and expires by design; an audit entry is the record, and
-- nothing in this schema may remove one.
GRANT DELETE ON panel_logs TO panel_user;

-- The operation log, which only the panel writes and reads. The services
-- do not know operations exist; they emit log lines carrying an id the
-- panel minted.
GRANT SELECT, INSERT, UPDATE, DELETE ON panel_operations TO panel_user;

-- Live settings. Optional, and strongly recommended: without it the
-- collector and the beacon read only their own files, and nothing changed
-- in the panel ever reaches them.
--
-- Note what is NOT in this line. panel_smtp holds the outgoing mail
-- account and is granted to panel_user alone - the collector and the
-- beacon have no reason to read a mail password, and the reason the
-- account lives in its own table rather than in panel_settings is
-- precisely that this GRANT would otherwise have handed it to them.
-- verify.sql asserts the absence, because a privilege nobody granted and
-- a privilege nobody checked look identical from here.
GRANT SELECT ON panel_settings TO collector, beacon_writer;

-- The address ranges the collector and the beacon look ASN and country
-- up in.
--
-- Missing entirely until an end-to-end run of the installed package went
-- looking. Both services build an asnlookup.Resolver against these two
-- tables and both refresh them - TRUNCATE and COPY, in one transaction -
-- and neither held a single privilege on either. The failure is quiet by
-- design: cmd/collector logs "failed to set up ASN/country lookup,
-- continuing without it" and carries on, because losing geography must
-- not take down the traffic path. So every installed deployment ran with
-- the ASN and country columns permanently empty, and the only symptom
-- was a breakdown page that looked like a quiet week.
--
-- The third hole of exactly this shape found in one afternoon, and all
-- three had the same cause: the development database was created by the
-- role the tests connect as, so it owned every table and no missing
-- grant could ever show up. A test fixture that is more privileged than
-- production does not test production.
--
-- TRUNCATE and not just DELETE: the refresh replaces the whole table,
-- and a row-by-row delete of a routing table is a different operation
-- with different costs. It adds no authority - a role that may INSERT
-- every row may already replace the contents.
--
-- Both services, because both refresh. They hold the same data and the
-- TRUNCATE takes an exclusive lock, so two refreshes serialise rather
-- than interleave.
GRANT SELECT, INSERT, TRUNCATE ON ip_asn_ranges, ip_country_ranges
  TO collector, beacon_writer;

-- The fetch log (M2): written by whoever fetches, read by the panel.
--
-- DELETE to the writers and not to the panel, and that asymmetry is the
-- phase's one real deviation from its plan - which said the sweep would
-- hang off internal/panel/housekeeping.go, and also said "the collector
-- writes, the panel reads". Both cannot hold: a sweep needs DELETE.
--
-- The writers keep it. They are the ones with a ticker already running,
-- the table only grows while they are running, and the alternative was
-- either widening the panel's rights on a table it does not own or
-- adding a SECURITY DEFINER function to do one DELETE. internal/asnlookup
-- says the rest, and a test there fails if the sweep loses its caller.
--
-- No UPDATE for anybody. A fetch row is finished the moment it is
-- written - the attempt is over - so the authority to change one after
-- the fact would only ever be the authority to make a failure look like
-- a success.
GRANT SELECT, INSERT, DELETE ON ip_range_fetches TO collector, beacon_writer;
GRANT SELECT ON ip_range_fetches TO panel_user;

-- The refresh queue (M3): the panel asks, whoever fetches answers.
--
-- The same split panel_upgrade_requests makes, one table over. Neither
-- side holds both rights, so a compromised panel can ask for a refresh -
-- a button any entitled customer can press anyway - and cannot write
-- "succeeded" on a refresh that never happened.
--
-- DELETE is the panel's and not the fetchers', which is not symmetry
-- being broken for convenience. asn_lookup is off by default, so on most
-- deployments nothing is polling this table at all; the panel is the
-- side still running when a request goes unclaimed, and the in-flight
-- index would otherwise turn one press into a permanently dead button.
-- internal/rangerefresh.ExpireStale is what uses it.
GRANT SELECT, INSERT, DELETE ON ip_range_refresh_requests TO panel_user;
GRANT SELECT, UPDATE ON ip_range_refresh_requests TO collector, beacon_writer;
