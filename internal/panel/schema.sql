-- Schema for the management panel. Apply this once, separately, like
-- every other schema in this project - the panel never runs DDL.
--
-- These tables are deliberately isolated from the analytics ones. The
-- panel's database role should be able to write here and NOWHERE else:
-- it must not be able to touch traffic_snapshots or beacon_events at
-- all, because it reads analytics through the read-only HTTP API rather
-- than the database. See the README's role setup.
--
-- No TimescaleDB features are used here. These are ordinary relational
-- tables with ordinary row counts (users, sessions, memberships), not
-- time series, and making panel_audit_log a hypertable would buy
-- compression on a table that will hold thousands of rows, not billions.

-- One account. Email is stored already lowercased by the application,
-- so the UNIQUE constraint is the whole case-insensitivity mechanism -
-- no citext extension needed, and no chance of two accounts differing
-- only by capitalisation.
CREATE TABLE IF NOT EXISTS panel_users (
    id            BIGSERIAL   PRIMARY KEY,
    email         TEXT        NOT NULL UNIQUE,
    display_name  TEXT        NOT NULL DEFAULT '',
    -- The full argon2id encoded string ($argon2id$v=19$m=...,t=...,p=...$salt$hash),
    -- which carries its own parameters, so raising the cost later
    -- re-verifies old hashes correctly without a migration.
    password_hash TEXT        NOT NULL,
    -- Base32 TOTP secret, empty when two-factor is off.
    --
    -- Stored unencrypted, deliberately. Encrypting it would need a key,
    -- and that key would have to live in the config file next to the
    -- database password - so anyone who could read the secret could
    -- already read the key. It buys the appearance of protection and
    -- not the substance. What actually protects it is the panel role
    -- having no read access to anything else and the database not being
    -- exposed; see the README.
    totp_secret   TEXT        NOT NULL DEFAULT '',
    -- The last TOTP time step accepted for this account, so a code
    -- cannot be presented twice. Codes are valid across three 30-second
    -- steps to tolerate clock drift, which means one observed over a
    -- shoulder or captured by a phishing proxy stays usable for up to
    -- ninety seconds - ample for an attacker who already has the
    -- password and is waiting for exactly that. See VerifyTOTP.
    totp_last_step BIGINT     NOT NULL DEFAULT 0,
    -- The operator (whoever hosts this), as distinct from a customer who
    -- owns one site. Superadmins see every site.
    is_superadmin BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Developer mode is a per-user preference, not a per-site setting:
    -- it changes what *you* see, and a shop owner and their developer
    -- looking at the same site should be able to disagree about it. Only
    -- owners and admins may turn it on - see internal/panel/roles.go.
    developer_mode BOOLEAN    NOT NULL DEFAULT FALSE,
    disabled      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

-- Self-migrating, the same convention internal/storage/schema.sql
-- follows: CREATE TABLE IF NOT EXISTS does nothing to a table that
-- already exists, so a column added to the definition above reaches an
-- existing deployment only through an explicit ALTER.
ALTER TABLE panel_users ADD COLUMN IF NOT EXISTS totp_last_step BIGINT NOT NULL DEFAULT 0;

-- Session storage for alexedwards/scs. The column names and types are
-- fixed by that package; only the table name is ours.
CREATE TABLE IF NOT EXISTS panel_sessions (
    token  TEXT        PRIMARY KEY,
    data   BYTEA       NOT NULL,
    expiry TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_panel_sessions_expiry ON panel_sessions (expiry);

-- Which users may see which sites, and with what authority.
--
-- site_id is a plain TEXT rather than a foreign key: sites are created
-- by a collector writing rows, not by the panel, so there is no table
-- to reference. A membership for a site that has no data yet is a
-- perfectly ordinary state - it is how you set access up before
-- pointing the collector at anything.
CREATE TABLE IF NOT EXISTS panel_site_members (
    site_id    TEXT        NOT NULL,
    user_id    BIGINT      NOT NULL REFERENCES panel_users(id) ON DELETE CASCADE,
    -- 'owner'  - full authority over this site, including removing others
    -- 'admin'  - may manage members and settings, may not delete the site
    -- 'viewer' - read-only, and never sees the technical views
    role       TEXT        NOT NULL CHECK (role IN ('owner', 'admin', 'viewer')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Who granted this. NULL when the row was created by the first-run
    -- setup, which has no actor yet.
    created_by BIGINT      REFERENCES panel_users(id) ON DELETE SET NULL,
    PRIMARY KEY (site_id, user_id)
);

-- "Which sites can this user see" is the panel's most frequent query,
-- and the primary key above leads with site_id, so it cannot serve it.
CREATE INDEX IF NOT EXISTS idx_panel_site_members_user ON panel_site_members (user_id);

-- Append-only record of everything that changed, and of the privileged
-- reads worth being able to reconstruct.
--
-- Append-only is enforced by the GRANT, not by a trigger: the panel role
-- gets INSERT and SELECT here and no UPDATE or DELETE, so the
-- application cannot rewrite its own history even if it is compromised.
CREATE TABLE IF NOT EXISTS panel_audit_log (
    id          BIGSERIAL   PRIMARY KEY,
    time        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 'user'      - an ordinary account, actor_id names it
    -- 'developer' - a one-time developer login (see panel_dev_logins);
    --               actor_id is NULL because there is no account, which
    --               is exactly why it needs its own kind rather than
    --               being flattened into a user row
    -- 'system'    - the panel itself (startup, cleanup, first-run setup)
    -- 'anonymous' - somebody who is not signed in: a failed sign-in, a
    --               refusal by the rate limiter. actor_id is NULL and
    --               actor_label is the address they typed rather than
    --               one anybody has proved
    actor_kind  TEXT        NOT NULL
        CHECK (actor_kind IN ('user', 'developer', 'system', 'anonymous')),
    actor_id    BIGINT      REFERENCES panel_users(id) ON DELETE SET NULL,
    -- A human label captured at the time of the action. Kept alongside
    -- actor_id rather than joined at read time, so the log still says
    -- who did something after that account is renamed or deleted.
    actor_label TEXT        NOT NULL,
    action      TEXT        NOT NULL,
    -- The site the action concerned, empty for account-level actions.
    site_id     TEXT        NOT NULL DEFAULT '',
    -- What was acted on: a user's email, a token's name, a setting key.
    target      TEXT        NOT NULL DEFAULT '',
    -- Anything else worth keeping, as JSON. Never credentials.
    detail      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ip          INET,
    user_agent  TEXT        NOT NULL DEFAULT ''
);

-- The fourth kind, for databases created before it existed.
--
-- Dropped and re-added rather than altered: PostgreSQL has no ADD
-- CONSTRAINT IF NOT EXISTS, and this pair is idempotent - on a fresh
-- database it replaces the constraint the CREATE above just made with an
-- identical one, and on an existing database it widens the old three.
--
-- Widening only. No row can be invalidated by it, so it needs no
-- validation pass and cannot fail on a table of any size.
--
-- # What this was fixing
--
-- Every failed sign-in and every rate-limit refusal wrote an entry with
-- no actor_kind at all, the three-value constraint refused the row, and
-- the caller discarded the error. The audit log had 1075 successful
-- sign-ins on the development database and not one failure. See
-- panel.PrincipalAnonymous.
ALTER TABLE panel_audit_log DROP CONSTRAINT IF EXISTS panel_audit_log_actor_kind_check;
ALTER TABLE panel_audit_log ADD CONSTRAINT panel_audit_log_actor_kind_check
    CHECK (actor_kind IN ('user', 'developer', 'system', 'anonymous'));

CREATE INDEX IF NOT EXISTS idx_panel_audit_time ON panel_audit_log (time DESC);
CREATE INDEX IF NOT EXISTS idx_panel_audit_site_time ON panel_audit_log (site_id, time DESC);
CREATE INDEX IF NOT EXISTS idx_panel_audit_actor_time ON panel_audit_log (actor_id, time DESC);

-- API tokens minted from the panel's developer options, alongside the
-- ones in the API's own config file. Only the hash is stored, the same
-- rule the config-file tokens follow: a leaked database hands over no
-- working credential.
CREATE TABLE IF NOT EXISTS panel_api_tokens (
    id          BIGSERIAL   PRIMARY KEY,
    name        TEXT        NOT NULL,
    sha256      TEXT        NOT NULL UNIQUE,
    -- Site IDs this token may read, or a single '*' for all of them -
    -- exactly the grant shape api.Token already uses.
    sites       TEXT[]      NOT NULL,
    created_by  BIGINT      REFERENCES panel_users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    -- Revoked rather than deleted, so the audit log's reference to it
    -- still resolves to a name.
    revoked_at  TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);

-- The read API looks tokens up by presented hash, so that is the lookup
-- the index has to serve; the UNIQUE constraint above already provides
-- it, and this partial index keeps the common "list the live ones" page
-- from scanning revoked history.
CREATE INDEX IF NOT EXISTS idx_panel_api_tokens_live
    ON panel_api_tokens (created_at DESC) WHERE revoked_at IS NULL;

-- Developer access, requested by `crucible dev-access request` on the
-- server itself and - once anyone owns this deployment - approved from
-- the panel by that owner.
--
-- The rule this table exists to enforce: shell access to the machine is
-- enough to get in *before* anyone has an account, and is not enough
-- afterwards. Installing the system is our job; reading a customer's
-- traffic once it is theirs is not, and "we could read the database
-- anyway" is a reason to make the access visible and consented to, not
-- a reason to skip asking.
--
-- Superseded panel_dev_logins, which had no approval step. Dropped
-- rather than migrated: it only ever held single-use tokens with a
-- fifteen-minute life, so there is nothing in it worth carrying over.
DROP TABLE IF EXISTS panel_dev_logins;

CREATE TABLE IF NOT EXISTS panel_dev_access (
    id       BIGSERIAL PRIMARY KEY,
    sha256   TEXT      NOT NULL UNIQUE,
    -- Why access is being asked for. Shown to the owner making the
    -- decision, and kept afterwards so the audit trail says what the
    -- visit was for.
    reason   TEXT      NOT NULL DEFAULT '',
    requested_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- How long the owner has to decide, and therefore how long an
    -- unapproved request stays usable at all.
    request_expires_at TIMESTAMPTZ NOT NULL,
    -- How long the session lasts once the link is redeemed, so approving
    -- grants a visit rather than a standing key.
    session_ttl_seconds INTEGER NOT NULL,

    approved_at    TIMESTAMPTZ,
    approved_by    BIGINT REFERENCES panel_users(id) ON DELETE SET NULL,
    approved_label TEXT NOT NULL DEFAULT '',
    -- TRUE when the request was granted because no account existed yet.
    -- Kept as its own column rather than inferred from approved_by being
    -- NULL, because redemption treats the two differently: a
    -- human-approved grant stays valid, a bootstrap one dies the moment
    -- somebody owns this deployment.
    auto_approved  BOOLEAN NOT NULL DEFAULT FALSE,

    denied_at TIMESTAMPTZ,
    denied_by BIGINT REFERENCES panel_users(id) ON DELETE SET NULL,

    -- Set atomically on redemption; a second attempt finds it non-NULL.
    used_at            TIMESTAMPTZ,
    used_from          INET,
    session_expires_at TIMESTAMPTZ
);

-- The panel polls for outstanding requests to show the owner a banner,
-- so "pending, not yet expired" has to be cheap.
CREATE INDEX IF NOT EXISTS idx_panel_dev_access_pending
    ON panel_dev_access (requested_at DESC)
    WHERE used_at IS NULL AND denied_at IS NULL;

-- Owner claims: the one-time link that turns a finished technical
-- installation into an account somebody owns.
--
-- A separate table rather than a disabled row in panel_users, because a
-- user with no usable password and a flag saying so is two states that
-- have to be kept in agreement, and the failure when they disagree is an
-- account nobody can sign in to or an account anybody can. An invitation
-- that has not been accepted is not a user; it is an invitation.
--
-- The token is stored as its SHA-256 for the same reason the developer
-- links are: whoever reads this table must not thereby be able to use
-- what is in it.
CREATE TABLE IF NOT EXISTS panel_owner_claims (
    id     BIGSERIAL PRIMARY KEY,
    sha256 TEXT      NOT NULL UNIQUE,
    -- The address the account will be created with. Held here rather
    -- than typed again on claiming, so the person handing over decides
    -- who this is for and the person claiming cannot change it.
    email        TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Who minted it. NULL for one minted at a shell, which is the
    -- ordinary case: at handover nobody owns the deployment yet.
    created_by    BIGINT REFERENCES panel_users(id) ON DELETE SET NULL,
    created_label TEXT NOT NULL DEFAULT '',

    expires_at TIMESTAMPTZ NOT NULL,

    -- Set atomically on claiming, in the same transaction that creates
    -- the account. A second attempt finds it non-NULL and is refused,
    -- so two tabs opened at once produce one owner rather than two.
    used_at   TIMESTAMPTZ,
    used_from INET,
    -- The account that was created, kept so the audit trail can answer
    -- "which invitation produced this owner" years later.
    used_by BIGINT REFERENCES panel_users(id) ON DELETE SET NULL
);

-- Unclaimed invitations, newest first: what the handover page lists.
CREATE INDEX IF NOT EXISTS idx_panel_owner_claims_open
    ON panel_owner_claims (created_at DESC)
    WHERE used_at IS NULL;

-- Login attempts, for throttling and for seeing an attack in progress.
--
-- A table rather than an in-memory counter, for two reasons: an
-- in-memory one resets on restart, which turns "restart the panel" into
-- a lockout bypass, and a persisted one is evidence afterwards.
CREATE TABLE IF NOT EXISTS panel_login_attempts (
    id      BIGSERIAL   PRIMARY KEY,
    at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Lowercased, and recorded even when no such account exists - the
    -- pattern of guesses is the interesting part.
    email   TEXT        NOT NULL,
    ip      INET,
    success BOOLEAN     NOT NULL
);

-- Throttling counts recent failures per email and per address, so both
-- need to be cheap to scan by time.
CREATE INDEX IF NOT EXISTS idx_panel_login_attempts_email ON panel_login_attempts (email, at DESC);
CREATE INDEX IF NOT EXISTS idx_panel_login_attempts_ip ON panel_login_attempts (ip, at DESC);

-- Operational settings: the values a deployment can change while it is
-- running, as opposed to the handful of bootstrap values that must exist
-- in a config file before the database is reachable.
--
-- The split is what decides whether a support call needs SSH. Anything
-- here is fixable from the panel; anything in the config file is not.
--
-- `key` is deliberately TEXT with no foreign key, and is nonetheless not
-- a free string: the application validates every key against a closed
-- registry (internal/panel/settings.go) before writing, and rejects
-- anything it does not know. Enforcing that in the database would mean a
-- CHECK constraint listing every key, which would turn adding a setting
-- into a migration for no additional safety - the panel role is the only
-- writer here.
--
-- `value` is JSONB rather than a text column plus a type column, so a
-- list-valued setting does not need its own encoding convention.
CREATE TABLE IF NOT EXISTS panel_settings (
    scope      TEXT        NOT NULL CHECK (scope IN ('global', 'site')),
    -- Empty for a global setting. The CHECK below keeps the two from
    -- drifting apart: a row claiming to be global while naming a site
    -- would read back as "unset" forever, which is the worst kind of
    -- storage bug because nothing errors.
    site_id    TEXT        NOT NULL DEFAULT '',
    key        TEXT        NOT NULL,
    value      JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by BIGINT      REFERENCES panel_users(id) ON DELETE SET NULL,
    PRIMARY KEY (scope, site_id, key),
    CHECK ((scope = 'global') = (site_id = ''))
);

-- "What is this key set to for this site, falling back to the
-- deployment-wide row" is the read every service performs on its refresh
-- interval, so it has to be an index lookup rather than a scan.
CREATE INDEX IF NOT EXISTS idx_panel_settings_key ON panel_settings (key, site_id);

-- Recovery codes: how somebody gets back into their own panel without
-- anybody else being awake.
--
-- The alternative this replaces was "ring whoever installed it". That
-- works for one customer and becomes a support queue at thirty, with the
-- person in the queue locked out of their own analytics at eleven at
-- night. It is also what the panel already promised - the account page
-- said recovery codes did not exist "yet".
--
-- Stored as a SHA-256 hex digest, the same form as every other
-- high-entropy credential here (invitations, developer links, API
-- tokens). Not argon2id: these are 60 bits this process drew from
-- crypto/rand, not a phrase a person chose, so there is no dictionary to
-- resist and the slow hash would buy nothing. What guards them is the
-- entropy plus the login throttle, which this flow shares with sign-in.
CREATE TABLE IF NOT EXISTS panel_recovery_codes (
    id      BIGSERIAL PRIMARY KEY,
    user_id BIGINT    NOT NULL REFERENCES panel_users(id) ON DELETE CASCADE,
    sha256  TEXT      NOT NULL UNIQUE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Who issued this set. NULL when the account minted its own at
    -- creation; set when an operator regenerated them for somebody who
    -- lost theirs, which is the second path and the reason this column
    -- exists rather than being inferred.
    created_by BIGINT REFERENCES panel_users(id) ON DELETE SET NULL,

    -- Consumed atomically, like an invitation: a code that another
    -- request has already taken does not match, so two tabs produce one
    -- reset rather than two.
    used_at   TIMESTAMPTZ,
    used_from INET
);

-- Redemption looks a code up by its digest alone - the address is
-- checked afterwards, so that a wrong address and a wrong code cost the
-- same work and answer the same way.
CREATE INDEX IF NOT EXISTS idx_panel_recovery_user
    ON panel_recovery_codes (user_id) WHERE used_at IS NULL;

-- The outgoing mail account. One per deployment.
--
-- A single row rather than a settings key, for three reasons that are all
-- about the password.
--
-- It cannot be hashed. Everything else credential-shaped in this database
-- is a digest - passwords are argon2id, invitations and API tokens and
-- recovery codes are SHA-256 - because nothing needs the original. An
-- SMTP password has to be handed to somebody else's server on every send,
-- so it has to be readable, and that makes it the only recoverable secret
-- here. It gets its own table so that fact is visible in the schema
-- instead of buried in one JSONB value among forty.
--
-- It is not stored in the clear either. password_sealed is AES-256-GCM
-- under a key from the panel's config file (see internal/sealed), so a
-- database copied without that file - a nightly dump, a staging restore,
-- a decommissioned replica - carries no working mail password. That is
-- the exposure this defends against and it is worth being exact: an
-- attacker holding the panel process holds the key too, and this stops
-- none of that.
--
-- And panel_settings is readable by collector and beacon_writer, which
-- is right for a refresh interval and wrong for a credential. Keeping the
-- account here means the GRANT can say panel_user and nothing else.
CREATE TABLE IF NOT EXISTS panel_smtp (
    -- One row, enforced rather than assumed. Without the CHECK a second
    -- row is a legal insert, and the panel would start sending through
    -- whichever one came back first.
    id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    host TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    -- 'starttls' - connect in the clear and upgrade. Port 587.
    -- 'implicit' - TLS from the first byte. Port 465.
    -- No third value: an unencrypted option in this column would be a
    -- setting whose only effect is to stop the password being sent, and
    -- the code refuses to send it anyway.
    encryption TEXT NOT NULL CHECK (encryption IN ('starttls', 'implicit')),

    username TEXT NOT NULL DEFAULT '',
    -- Sealed, never plaintext, and never a hash. Empty means an account
    -- with no credentials - a local relay - which the code allows only
    -- when there is no password to expose.
    --
    -- The column name says sealed rather than encrypted so a reader
    -- writing a query cannot mistake it for something they can compare
    -- against, GROUP BY, or index usefully.
    password_sealed TEXT NOT NULL DEFAULT '',

    -- The envelope sender and the From: header. Often but not always the
    -- same as username: a provider may authenticate one mailbox and allow
    -- sending as another.
    from_address TEXT NOT NULL,
    from_name    TEXT NOT NULL DEFAULT '',

    -- Whether the panel should try to send at all. Separate from "is it
    -- configured" so an operator whose provider is having a bad week can
    -- turn it off without losing the settings and retyping the password.
    enabled BOOLEAN NOT NULL DEFAULT FALSE,

    -- The last verification, from the wizard's test button.
    --
    -- Stored because the answer to "why did nobody get the invitation"
    -- is usually three weeks old by the time it is asked, and a panel
    -- that can say "this account last verified in March and has been
    -- failing authentication since" answers it without anybody
    -- reproducing anything.
    --
    -- verified_diagnosis holds a mail.Diagnosis value, empty when the
    -- last attempt succeeded. Deliberately not a foreign key to a
    -- lookup table: the set of diagnoses is a fact about the code, and
    -- a CHECK listing them would turn adding one into a migration.
    verified_at         TIMESTAMPTZ,
    verified_ok         BOOLEAN NOT NULL DEFAULT FALSE,
    verified_diagnosis  TEXT    NOT NULL DEFAULT '',
    -- What the server itself said, kept apart from the diagnosis - the
    -- panel shows both, and neither field ever attributes to the server
    -- something the server never said.
    verified_server_said TEXT NOT NULL DEFAULT '',

    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by BIGINT REFERENCES panel_users(id) ON DELETE SET NULL
);


-- ====================================================== B2: operasyonlar
--
-- panel_logs bu dosyada değil, internal/logsink/schema.sql'de: onu dört
-- servis de yazıyor, bu tabloyu yalnız panel. Aynı ayrım
-- internal/heartbeat'te de var, ve sebebi aynı - bir tablonun yazarları
-- kimse, şeması onların yanında durmalı.
--
-- İkisi yine de tek fazda yapıldı, çünkü panel_logs.operation_id bu
-- tablonun kimliğini taşıyor: korelasyon sütunu olmadan tasarlanmış bir
-- log tablosu, operasyon günlüğü geldiği gün yeniden yazılırdı.

-- What happened while somebody was changing something.
--
-- # Why this is not the audit log
--
-- panel_audit_log answers "who did what", and it is the record that has
-- to survive: short, legally meaningful, kept for a long time. This
-- answers "what happened while they did it", which is a different
-- question asked by a different person at a different time - usually the
-- developer, usually within the hour, usually because a customer said
-- "bir şeyi ayarlarken hata olmuş".
--
-- Two tables rather than more columns on one, because their retentions
-- differ by more than an order of magnitude and because an audit row
-- must never be crowded out by diagnostic detail.
--
-- # The field that matters most
--
-- rolled_back. "Something went wrong while setting it" is only
-- answerable if a half-applied change is *recorded as half-applied*. A
-- log that says "failed" without saying whether the change survived
-- leaves the reader to guess, and the guess is what turns a small
-- failure into an afternoon.
CREATE TABLE IF NOT EXISTS panel_operations (
    -- Text, not a sequence: the id is minted before the operation starts
    -- so it can be attached to log lines the operation is about to
    -- produce. A database-assigned id would not exist yet.
    id TEXT PRIMARY KEY,

    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,

    -- What was attempted, in the audit log's vocabulary, so the two
    -- records can be read side by side.
    action TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    site_id TEXT NOT NULL DEFAULT '',

    -- Who, in the same shape panel_audit_log records them.
    actor_kind  TEXT NOT NULL,
    actor_id    BIGINT REFERENCES panel_users(id) ON DELETE SET NULL,
    actor_label TEXT NOT NULL DEFAULT '',

    -- The audit entry this operation belongs to, when there is one.
    audit_id BIGINT REFERENCES panel_audit_log(id) ON DELETE SET NULL,

    -- Before and after, as JSON, so a value of any shape fits and the
    -- panel can render "14 → 21" without knowing the setting's type.
    before_value JSONB,
    after_value  JSONB,

    -- Each step and how it went, appended as the operation runs.
    --
    -- An array rather than a row per step: the steps of one operation
    -- are read together, always, and never queried across operations.
    steps JSONB NOT NULL DEFAULT '[]'::jsonb,

    -- How it ended. Empty while it is still running, which is a state
    -- the panel has to be able to draw - an operation that never
    -- finished is exactly the interesting case.
    outcome TEXT NOT NULL DEFAULT '',

    -- The whole error chain, not its last link. wrapStoreError and its
    -- callers build a chain on purpose and the innermost cause is
    -- usually the one that names the fix.
    error_chain TEXT NOT NULL DEFAULT '',

    -- Whether a failed change was undone.
    --
    -- Three states, and NULL is one of them: true undone, false left
    -- standing, NULL not applicable because nothing had been applied
    -- yet. Collapsing NULL into false would claim a change was left
    -- standing when none was ever made.
    rolled_back BOOLEAN
);

CREATE INDEX IF NOT EXISTS idx_panel_operations_time ON panel_operations (started_at DESC);
CREATE INDEX IF NOT EXISTS idx_panel_operations_site ON panel_operations (site_id, started_at DESC);
