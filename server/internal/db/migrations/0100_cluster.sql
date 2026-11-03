-- Servers in this installation: control servers (web/api) register themselves through
-- the database, relays without a database register over HTTPS with a node credential.
CREATE TABLE cluster_nodes (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    region          TEXT NOT NULL DEFAULT '',
    roles           TEXT[] NOT NULL,
    version         TEXT NOT NULL DEFAULT '',
    address         TEXT NOT NULL DEFAULT '',
    relay_urls      TEXT[] NOT NULL DEFAULT '{}',
    stats           JSONB NOT NULL DEFAULT '{}',
    remote          BOOLEAN NOT NULL DEFAULT false,
    credential_hash BYTEA,
    draining        BOOLEAN NOT NULL DEFAULT false,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cluster_nodes_seen_idx ON cluster_nodes(last_seen DESC);

-- One-time tokens an administrator creates to add a server.
CREATE TABLE cluster_join_tokens (
    id         BIGSERIAL PRIMARY KEY,
    token_hash BYTEA NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    roles      TEXT[] NOT NULL,
    created_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    node_id    TEXT
);
