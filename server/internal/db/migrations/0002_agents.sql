-- Windows agents: enrollment, device authentication and presence.
ALTER TABLE devices ADD COLUMN kind TEXT NOT NULL DEFAULT 'agent' CHECK (kind IN ('agent', 'quick'));
ALTER TABLE devices ADD COLUMN token_hash BYTEA UNIQUE;
ALTER TABLE devices ADD COLUMN machine_id TEXT;
ALTER TABLE devices ADD COLUMN public_ip TEXT;
ALTER TABLE devices ADD COLUMN enrolled_at TIMESTAMPTZ;
CREATE UNIQUE INDEX devices_machine_idx ON devices(machine_id) WHERE kind = 'agent' AND machine_id IS NOT NULL;

-- Enrollment tokens created in the portal. Only the SHA-256 of a token is stored.
CREATE TABLE enrollment_tokens (
    id          BIGSERIAL PRIMARY KEY,
    token_hash  BYTEA NOT NULL UNIQUE,
    label       TEXT NOT NULL DEFAULT '',
    customer_id BIGINT REFERENCES customers(id) ON DELETE CASCADE,
    created_by  BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    max_uses    INT,
    uses        INT NOT NULL DEFAULT 0,
    revoked_at  TIMESTAMPTZ
);

ALTER TABLE remote_sessions ADD COLUMN end_reason TEXT;
