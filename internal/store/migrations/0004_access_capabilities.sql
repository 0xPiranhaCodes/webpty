-- webpty:foreign-keys=off
CREATE TABLE access_grants_new (
    id INTEGER PRIMARY KEY,
    public_id TEXT NOT NULL UNIQUE,
    terminal_session_id INTEGER NOT NULL REFERENCES terminal_sessions (id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    role TEXT NOT NULL CHECK (role IN ('editor', 'viewer')),
    label TEXT NOT NULL DEFAULT '',
    single_use INTEGER NOT NULL DEFAULT 0 CHECK (single_use IN (0, 1)),
    max_redemptions INTEGER NOT NULL CHECK (max_redemptions > 0 AND (single_use = 0 OR max_redemptions = 1)),
    redemption_count INTEGER NOT NULL DEFAULT 0 CHECK (redemption_count BETWEEN 0 AND max_redemptions),
    redeemed_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK (expires_at > created_at),
    revoked_at INTEGER,
    revoke_reason TEXT NOT NULL DEFAULT '' CHECK (revoke_reason IN ('', 'revoked', 'replaced')),
    CHECK ((revoked_at IS NULL) = (revoke_reason = ''))
) STRICT;

-- Grants from earlier schemas were never redeemable; they keep their
-- expiry and revocation state and may be redeemed at most once.
INSERT INTO access_grants_new (id, public_id, terminal_session_id, token_hash, role, max_redemptions,
        created_at, updated_at, expires_at, revoked_at, revoke_reason)
    SELECT id, lower(hex(randomblob(16))), terminal_session_id, token_hash, role, 1,
        created_at, COALESCE(revoked_at, created_at), expires_at, revoked_at,
        CASE WHEN revoked_at IS NULL THEN '' ELSE 'revoked' END
    FROM access_grants;

DROP TABLE access_grants;
ALTER TABLE access_grants_new RENAME TO access_grants;

CREATE UNIQUE INDEX access_grants_one_active_editor
    ON access_grants (terminal_session_id)
    WHERE role = 'editor' AND revoked_at IS NULL;
CREATE INDEX access_grants_terminal ON access_grants (terminal_session_id);

CREATE TRIGGER access_grants_revocation_is_final
    BEFORE UPDATE OF revoked_at, revoke_reason ON access_grants
    WHEN OLD.revoked_at IS NOT NULL
        AND (NEW.revoked_at IS NOT OLD.revoked_at OR NEW.revoke_reason IS NOT OLD.revoke_reason)
BEGIN
    SELECT RAISE(ABORT, 'access grant revocation is final');
END;

CREATE TABLE access_sessions (
    id INTEGER PRIMARY KEY,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    grant_id INTEGER NOT NULL REFERENCES access_grants (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK (expires_at > created_at),
    revoked_at INTEGER
) STRICT;

CREATE INDEX access_sessions_grant ON access_sessions (grant_id);

-- A session may only be created for a live grant and never outlives it.
CREATE TRIGGER access_sessions_require_live_grant
    BEFORE INSERT ON access_sessions
    WHEN NOT EXISTS (
        SELECT 1 FROM access_grants
        WHERE id = NEW.grant_id AND revoked_at IS NULL
            AND expires_at > NEW.created_at AND expires_at >= NEW.expires_at
    )
BEGIN
    SELECT RAISE(ABORT, 'access session requires a live grant');
END;

CREATE TRIGGER access_sessions_revocation_is_final
    BEFORE UPDATE OF revoked_at ON access_sessions
    WHEN OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS NOT OLD.revoked_at
BEGIN
    SELECT RAISE(ABORT, 'access session revocation is final');
END;
