CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'technician')),
    language      TEXT NOT NULL DEFAULT 'en' CHECK (language IN ('en', 'nl')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

CREATE TABLE sessions (
    token_hash  BYTEA PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    ip          TEXT,
    user_agent  TEXT
);
CREATE INDEX sessions_user_idx ON sessions(user_id);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
    id         BIGSERIAL PRIMARY KEY,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id    BIGINT REFERENCES users(id) ON DELETE SET NULL,
    user_email TEXT,
    action     TEXT NOT NULL,
    target     TEXT,
    details    JSONB,
    ip         TEXT
);
CREATE INDEX audit_log_at_idx ON audit_log(at DESC);

CREATE TABLE customers (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE devices (
    id            BIGSERIAL PRIMARY KEY,
    customer_id   BIGINT REFERENCES customers(id) ON DELETE SET NULL,
    device_code   TEXT NOT NULL UNIQUE,
    hostname      TEXT NOT NULL,
    os_version    TEXT,
    agent_version TEXT,
    last_user     TEXT,
    last_seen_at  TIMESTAMPTZ,
    online        BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE remote_sessions (
    id           BIGSERIAL PRIMARY KEY,
    device_id    BIGINT REFERENCES devices(id) ON DELETE SET NULL,
    user_id      BIGINT REFERENCES users(id) ON DELETE SET NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('unattended', 'attended', 'quick')),
    transport    TEXT,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at     TIMESTAMPTZ,
    recording    TEXT
);

CREATE TABLE backup_runs (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('full', 'incremental')),
    parent_name TEXT,
    name        TEXT NOT NULL,
    target      TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('running', 'ok', 'failed')),
    size_bytes  BIGINT,
    manifest    JSONB,
    error       TEXT,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
