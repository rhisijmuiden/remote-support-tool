-- Session audit log and recordings.
-- Names are copied into the session row so the log stays readable after a device,
-- customer or user is deleted.
ALTER TABLE remote_sessions
    DROP COLUMN recording,
    ADD COLUMN customer_id      BIGINT REFERENCES customers(id) ON DELETE SET NULL,
    ADD COLUMN device_name      TEXT,
    ADD COLUMN customer_name    TEXT,
    ADD COLUMN user_email       TEXT,
    ADD COLUMN user_name        TEXT,
    ADD COLUMN viewer_ip        TEXT,
    ADD COLUMN end_reason       TEXT,
    ADD COLUMN recording_status TEXT NOT NULL DEFAULT 'none'
        CHECK (recording_status IN ('none', 'recording', 'complete', 'incomplete', 'deleted')),
    ADD COLUMN recording_chunks INT NOT NULL DEFAULT 0,
    ADD COLUMN recording_bytes  BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN recording_mime   TEXT,
    ADD COLUMN recording_deleted_at TIMESTAMPTZ;
CREATE INDEX remote_sessions_started_idx ON remote_sessions(started_at DESC);
CREATE INDEX remote_sessions_device_idx ON remote_sessions(device_id);
CREATE INDEX remote_sessions_user_idx ON remote_sessions(user_id);
CREATE INDEX remote_sessions_customer_idx ON remote_sessions(customer_id);

CREATE TABLE remote_session_events (
    id         BIGSERIAL PRIMARY KEY,
    session_id BIGINT NOT NULL REFERENCES remote_sessions(id) ON DELETE CASCADE,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind       TEXT NOT NULL,
    details    JSONB
);
CREATE INDEX remote_session_events_session_idx ON remote_session_events(session_id, at);

-- Share links let a customer watch or download one recording without an account.
-- Only a hash of the token is stored.
CREATE TABLE recording_shares (
    id             BIGSERIAL PRIMARY KEY,
    session_id     BIGINT NOT NULL REFERENCES remote_sessions(id) ON DELETE CASCADE,
    token_hash     BYTEA NOT NULL UNIQUE,
    created_by     BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_by_email TEXT,
    note           TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    views          INT NOT NULL DEFAULT 0,
    last_viewed_at TIMESTAMPTZ
);
CREATE INDEX recording_shares_session_idx ON recording_shares(session_id);
