-- migrate:up
-- NULL expires_at means the session never expires. Only issued by internal
-- tooling (cmd/devtoken) for local dev/make workflows — never via the public
-- signup/login endpoints, which always set a normal TTL.
ALTER TABLE sessions ALTER COLUMN expires_at DROP NOT NULL;

-- migrate:down
DELETE FROM sessions WHERE expires_at IS NULL;
ALTER TABLE sessions ALTER COLUMN expires_at SET NOT NULL;
