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
