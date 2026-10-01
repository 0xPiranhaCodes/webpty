CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE admin_credentials (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE admin_sessions (
    id INTEGER PRIMARY KEY,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    kind TEXT NOT NULL CHECK (kind IN ('bootstrap', 'admin')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK (expires_at > created_at)
) STRICT;

CREATE INDEX admin_sessions_expires_at ON admin_sessions (expires_at);

CREATE TABLE terminal_sessions (
    id INTEGER PRIMARY KEY,
    public_id TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('starting', 'running', 'exited')),
    created_at INTEGER NOT NULL,
    ended_at INTEGER,
    exit_code INTEGER
) STRICT;

CREATE TABLE access_grants (
    id INTEGER PRIMARY KEY,
    terminal_session_id INTEGER NOT NULL REFERENCES terminal_sessions (id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    role TEXT NOT NULL CHECK (role IN ('editor', 'viewer')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK (expires_at > created_at),
    revoked_at INTEGER
) STRICT;

CREATE UNIQUE INDEX access_grants_one_active_editor
    ON access_grants (terminal_session_id)
    WHERE role = 'editor' AND revoked_at IS NULL;

CREATE TABLE recordings (
    id INTEGER PRIMARY KEY,
    terminal_session_id INTEGER NOT NULL REFERENCES terminal_sessions (id) ON DELETE CASCADE,
    started_at INTEGER NOT NULL,
    ended_at INTEGER,
    cols INTEGER NOT NULL DEFAULT 80 CHECK (cols > 0),
    rows INTEGER NOT NULL DEFAULT 24 CHECK (rows > 0)
) STRICT;

CREATE TABLE recording_chunks (
    recording_id INTEGER NOT NULL REFERENCES recordings (id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    offset_ms INTEGER NOT NULL DEFAULT 0 CHECK (offset_ms >= 0),
    kind TEXT NOT NULL DEFAULT 'output' CHECK (kind IN ('output', 'resize', 'lifecycle', 'presence')),
    data BLOB NOT NULL,
    PRIMARY KEY (recording_id, sequence)
) STRICT;

CREATE TABLE audit_events (
    id INTEGER PRIMARY KEY,
    occurred_at INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    remote_addr TEXT NOT NULL DEFAULT '',
    details TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX audit_events_occurred_at ON audit_events (occurred_at);
