-- migrate:up
-- last_seen_at: last time the hosting worker listed this sandbox in a heartbeat
-- inventory (or the time it was assigned to the worker). Drives liveness expiry.
ALTER TABLE sandboxes ADD COLUMN last_seen_at TIMESTAMPTZ;

-- inventory_token: fencing token of the worker incarnation whose first complete
-- inventory has already been reconciled. Zero means none yet.
ALTER TABLE worker
    ADD COLUMN inventory_token BIGINT NOT NULL DEFAULT 0
    CHECK (inventory_token >= 0);

-- migrate:down
ALTER TABLE worker DROP COLUMN inventory_token;
ALTER TABLE sandboxes DROP COLUMN last_seen_at;
