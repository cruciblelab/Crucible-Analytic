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
  panel_owner_claims, panel_login_attempts, panel_recovery_codes
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

-- Live settings. Optional, and strongly recommended: without it the
-- collector and the beacon read only their own files, and nothing changed
-- in the panel ever reaches them.
GRANT SELECT ON panel_settings TO collector, beacon_writer;
