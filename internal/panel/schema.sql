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
    actor_kind  TEXT        NOT NULL CHECK (actor_kind IN ('user', 'developer', 'system')),
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
