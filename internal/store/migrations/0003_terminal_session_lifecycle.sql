-- webpty:foreign-keys=off
CREATE TABLE terminal_sessions_new (
    id INTEGER PRIMARY KEY,
    public_id TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('starting', 'running', 'exited', 'failed', 'terminated')),
    command TEXT NOT NULL DEFAULT '',
    args TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(args) AND json_type(args) = 'array'),
    rows INTEGER NOT NULL DEFAULT 24 CHECK (rows > 0),
    cols INTEGER NOT NULL DEFAULT 80 CHECK (cols > 0),
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    ended_at INTEGER,
    last_activity_at INTEGER NOT NULL,
    exit_code INTEGER,
    exit_signal TEXT NOT NULL DEFAULT '',
    failure TEXT NOT NULL DEFAULT ''
) STRICT;

INSERT INTO terminal_sessions_new (id, public_id, title, state, created_at, ended_at, last_activity_at, exit_code)
    SELECT id, public_id, title, state, created_at, ended_at, COALESCE(ended_at, created_at), exit_code
    FROM terminal_sessions;

DROP TABLE terminal_sessions;
ALTER TABLE terminal_sessions_new RENAME TO terminal_sessions;

CREATE INDEX terminal_sessions_state ON terminal_sessions (state);
