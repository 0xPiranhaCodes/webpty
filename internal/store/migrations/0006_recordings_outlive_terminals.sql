-- webpty:foreign-keys=off
-- Recordings, and the tombstones of deleted ones, survive deletion of their
-- terminal's metadata: the terminal's public ID is kept on the recording and
-- the reference is cleared instead of cascading.
DROP TRIGGER recording_chunks_require_active;
DROP TRIGGER recording_chunks_delete_with_tombstone;

CREATE TABLE recordings_new (
    id INTEGER PRIMARY KEY,
    public_id TEXT NOT NULL UNIQUE,
    terminal_session_id INTEGER REFERENCES terminal_sessions (id) ON DELETE SET NULL,
    terminal_public_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('recording', 'complete', 'incomplete', 'deleted')),
    format_version INTEGER NOT NULL CHECK (format_version > 0),
    codec TEXT NOT NULL CHECK (codec IN ('gzip')),
    started_at INTEGER NOT NULL,
    ended_at INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    cols INTEGER NOT NULL CHECK (cols > 0),
    rows INTEGER NOT NULL CHECK (rows > 0),
    event_count INTEGER NOT NULL DEFAULT 0 CHECK (event_count >= 0),
    chunk_count INTEGER NOT NULL DEFAULT 0 CHECK (chunk_count >= 0),
    compressed_bytes INTEGER NOT NULL DEFAULT 0 CHECK (compressed_bytes >= 0),
    uncompressed_bytes INTEGER NOT NULL DEFAULT 0 CHECK (uncompressed_bytes >= 0),
    failure_code TEXT NOT NULL DEFAULT '',
    retain_until INTEGER,
    deleted_at INTEGER,
    updated_at INTEGER NOT NULL,
    CHECK ((status = 'recording') = (ended_at IS NULL)),
    CHECK ((status = 'recording') = (retain_until IS NULL)),
    CHECK ((status = 'deleted') = (deleted_at IS NOT NULL)),
    CHECK (status != 'incomplete' OR failure_code != ''),
    CHECK (status NOT IN ('recording', 'complete') OR failure_code = '')
) STRICT;

INSERT INTO recordings_new (id, public_id, terminal_session_id, terminal_public_id, status, format_version, codec,
        started_at, ended_at, duration_ms, cols, rows, event_count, chunk_count, compressed_bytes,
        uncompressed_bytes, failure_code, retain_until, deleted_at, updated_at)
    SELECT r.id, r.public_id, r.terminal_session_id, t.public_id, r.status, r.format_version, r.codec,
        r.started_at, r.ended_at, r.duration_ms, r.cols, r.rows, r.event_count, r.chunk_count, r.compressed_bytes,
        r.uncompressed_bytes, r.failure_code, r.retain_until, r.deleted_at, r.updated_at
    FROM recordings r JOIN terminal_sessions t ON t.id = r.terminal_session_id;

DROP TABLE recordings;
ALTER TABLE recordings_new RENAME TO recordings;

CREATE INDEX recordings_terminal ON recordings (terminal_public_id, started_at, id);
CREATE INDEX recordings_terminal_session ON recordings (terminal_session_id);
CREATE INDEX recordings_started ON recordings (started_at, id);
CREATE INDEX recordings_retention ON recordings (retain_until) WHERE status IN ('complete', 'incomplete');
CREATE INDEX recordings_active ON recordings (id) WHERE status = 'recording';

CREATE TRIGGER recording_chunks_require_active
    BEFORE INSERT ON recording_chunks
    WHEN NOT EXISTS (SELECT 1 FROM recordings WHERE id = NEW.recording_id AND status = 'recording')
BEGIN
    SELECT RAISE(ABORT, 'recording is not active');
END;

-- Chunks are removed only together with the tombstone of their recording.
CREATE TRIGGER recording_chunks_delete_with_tombstone
    BEFORE DELETE ON recording_chunks
    WHEN EXISTS (SELECT 1 FROM recordings WHERE id = OLD.recording_id AND status != 'deleted')
BEGIN
    SELECT RAISE(ABORT, 'recording chunks are deleted only with their recording');
END;

-- Every column but the terminal reference, which is cleared when the
-- terminal's metadata is deleted.
CREATE TRIGGER recordings_tombstone_is_final
    BEFORE UPDATE OF public_id, terminal_public_id, status, format_version, codec, started_at, ended_at,
        duration_ms, cols, rows, event_count, chunk_count, compressed_bytes, uncompressed_bytes, failure_code,
        retain_until, deleted_at, updated_at ON recordings
    WHEN OLD.status = 'deleted'
BEGIN
    SELECT RAISE(ABORT, 'recording tombstone is final');
END;

CREATE TRIGGER recordings_end_is_final
    BEFORE UPDATE OF status ON recordings
    WHEN OLD.status != 'recording' AND NEW.status = 'recording'
BEGIN
    SELECT RAISE(ABORT, 'ended recordings cannot resume');
END;
