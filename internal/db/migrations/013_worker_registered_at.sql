-- migrate:up
-- registered_at: when the current worker incarnation registered (Register
-- bumps it with the fencing token). Sandboxes last seen before it belong to
-- a previous incarnation, whose VMs died with it.
ALTER TABLE worker ADD COLUMN registered_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- migrate:down
ALTER TABLE worker DROP COLUMN registered_at;
