-- Client builder and central agent updates (step 4).

-- Agent and QuickSupport executables the server can hand out, imported from a
-- GitHub release or uploaded in the portal. Files live in <data_dir>/agents/<sha256>.exe.
CREATE TABLE agent_releases (
    id          BIGSERIAL PRIMARY KEY,
    product     TEXT NOT NULL CHECK (product IN ('agent', 'quick')),
    version     TEXT NOT NULL,
    os          TEXT NOT NULL CHECK (os IN ('windows')),
    arch        TEXT NOT NULL CHECK (arch IN ('amd64', 'arm64')),
    channel     TEXT NOT NULL CHECK (channel IN ('production', 'test')),
    size_bytes  BIGINT NOT NULL,
    sha256      TEXT NOT NULL,
    source      TEXT NOT NULL CHECK (source IN ('github', 'upload')),
    notes       TEXT,
    released_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (product, version, os, arch)
);

-- A client build is a branded client for one customer (or for everyone).
CREATE TABLE client_builds (
    id                BIGSERIAL PRIMARY KEY,
    name              TEXT NOT NULL,
    customer_id       BIGINT REFERENCES customers(id) ON DELETE SET NULL,
    mode              TEXT NOT NULL CHECK (mode IN ('install', 'quick')),
    product_name      TEXT NOT NULL,
    company_name      TEXT NOT NULL,
    accent_color      TEXT,
    logo_png          BYTEA,
    servers           JSONB NOT NULL,
    channel           TEXT NOT NULL CHECK (channel IN ('production', 'test')),
    enroll_token_hash BYTEA UNIQUE,
    enroll_token      TEXT, -- sealed with the server secret, so a rebuild keeps the same token
    revoked_at        TIMESTAMPTZ,
    created_by        BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Generated installers. Files live in <data_dir>/builds/<build_id>/<id>.exe.
CREATE TABLE client_build_files (
    id               BIGSERIAL PRIMARY KEY,
    build_id         BIGINT NOT NULL REFERENCES client_builds(id) ON DELETE CASCADE,
    agent_release_id BIGINT REFERENCES agent_releases(id) ON DELETE SET NULL,
    agent_version    TEXT NOT NULL,
    arch             TEXT NOT NULL,
    file_name        TEXT NOT NULL,
    size_bytes       BIGINT NOT NULL,
    sha256           TEXT NOT NULL,
    signed           BOOLEAN NOT NULL DEFAULT false,
    created_by       BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX client_build_files_build_idx ON client_build_files(build_id);

-- Expiring download links to send an installer to a customer.
CREATE TABLE download_links (
    id            BIGSERIAL PRIMARY KEY,
    token_hash    BYTEA NOT NULL UNIQUE,
    file_id       BIGINT NOT NULL REFERENCES client_build_files(id) ON DELETE CASCADE,
    expires_at    TIMESTAMPTZ NOT NULL,
    max_downloads INT,
    downloads     INT NOT NULL DEFAULT 0,
    revoked_at    TIMESTAMPTZ,
    created_by    BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-customer exceptions to the global agent update policy.
CREATE TABLE customer_update_policies (
    customer_id BIGINT PRIMARY KEY REFERENCES customers(id) ON DELETE CASCADE,
    mode        TEXT NOT NULL CHECK (mode IN ('off', 'pin')),
    version     TEXT,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- What agents report while updating, for the rollout overview.
CREATE TABLE agent_update_events (
    id           BIGSERIAL PRIMARY KEY,
    device_id    BIGINT REFERENCES devices(id) ON DELETE CASCADE,
    from_version TEXT,
    to_version   TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('offered', 'downloaded', 'installed', 'failed')),
    error        TEXT,
    at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX agent_update_events_at_idx ON agent_update_events(at DESC);
CREATE INDEX agent_update_events_device_idx ON agent_update_events(device_id, at DESC);
