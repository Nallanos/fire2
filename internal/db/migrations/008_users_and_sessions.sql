-- migrate:up
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Opaque, revocable session tokens rather than JWTs — no signing key to
-- manage, and logout/revoke is a plain DELETE instead of waiting out an
-- expiry. token is the primary key: lookups are a straight equality match.
CREATE TABLE sessions (
    token TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- Nullable: existing sandboxes predate auth and have no owner. New sandboxes
-- always get one — enforced at the application layer, not the schema.
ALTER TABLE sandboxes
    ADD COLUMN user_id TEXT NULL REFERENCES users(id);

CREATE INDEX sandboxes_user_id_idx ON sandboxes (user_id);

-- migrate:down
DROP INDEX IF EXISTS sandboxes_user_id_idx;
ALTER TABLE sandboxes DROP COLUMN user_id;
DROP TABLE sessions;
DROP TABLE users;
