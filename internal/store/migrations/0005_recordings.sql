-- Recordings from earlier schemas had no writer and no chunk format; any
-- rows are kept as incomplete tombstone candidates and their chunks dropped.
CREATE TABLE recordings_new (
    id INTEGER PRIMARY KEY,
    public_id TEXT NOT NULL UNIQUE,
    terminal_session_id INTEGER NOT NULL REFERENCES terminal_sessions (id) ON DELETE CASCADE,
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

INSERT INTO recordings_new (id, public_id, terminal_session_id, status, format_version, codec, started_at,
        ended_at, cols, rows, failure_code, retain_until, updated_at)
    SELECT id, lower(hex(randomblob(16))), terminal_session_id, 'incomplete', 1, 'gzip', started_at,
        COALESCE(ended_at, started_at), cols, rows, 'legacy_format', COALESCE(ended_at, started_at),
        COALESCE(ended_at, started_at)
    FROM recordings;

DROP TABLE recording_chunks;
DROP TABLE recordings;
ALTER TABLE recordings_new RENAME TO recordings;

CREATE INDEX recordings_terminal ON recordings (terminal_session_id, started_at);
CREATE INDEX recordings_started ON recordings (started_at, id);
CREATE INDEX recordings_retention ON recordings (retain_until) WHERE status IN ('complete', 'incomplete');
CREATE INDEX recordings_active ON recordings (id) WHERE status = 'recording';

CREATE TABLE recording_chunks (
    recording_id INTEGER NOT NULL REFERENCES recordings (id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL CHECK (chunk_index >= 0),
    first_seq INTEGER NOT NULL CHECK (first_seq > 0),
    last_seq INTEGER NOT NULL CHECK (last_seq >= first_seq),
    first_offset_ms INTEGER NOT NULL CHECK (first_offset_ms >= 0),
    last_offset_ms INTEGER NOT NULL CHECK (last_offset_ms >= first_offset_ms),
    event_count INTEGER NOT NULL CHECK (event_count = last_seq - first_seq + 1),
    uncompressed_bytes INTEGER NOT NULL CHECK (uncompressed_bytes > 0),
    checksum BLOB NOT NULL CHECK (length(checksum) = 32),
    codec TEXT NOT NULL CHECK (codec IN ('gzip')),
    data BLOB NOT NULL CHECK (length(data) > 0),
    PRIMARY KEY (recording_id, chunk_index)
) STRICT;

CREATE INDEX recording_chunks_offset ON recording_chunks (recording_id, last_offset_ms);
CREATE INDEX recording_chunks_seq ON recording_chunks (recording_id, last_seq);

CREATE TRIGGER recording_chunks_require_active
    BEFORE INSERT ON recording_chunks
    WHEN NOT EXISTS (SELECT 1 FROM recordings WHERE id = NEW.recording_id AND status = 'recording')
BEGIN
    SELECT RAISE(ABORT, 'recording is not active');
END;

CREATE TRIGGER recording_chunks_are_immutable
    BEFORE UPDATE ON recording_chunks
BEGIN
    SELECT RAISE(ABORT, 'recording chunks are immutable');
END;

-- Chunks are removed only together with the tombstone of their recording.
CREATE TRIGGER recording_chunks_delete_with_tombstone
    BEFORE DELETE ON recording_chunks
    WHEN EXISTS (SELECT 1 FROM recordings WHERE id = OLD.recording_id AND status != 'deleted')
BEGIN
    SELECT RAISE(ABORT, 'recording chunks are deleted only with their recording');
END;

CREATE TRIGGER recordings_tombstone_is_final
    BEFORE UPDATE ON recordings
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
