-- migrate:up
-- inactive_since: when the worker reaper last marked the worker inactive (the
-- database's clock); NULL while it is active. The sandbox reaper only fails the
-- sandboxes of a worker that stayed inactive for the whole worker-lost timeout:
-- a worker the orchestrator merely lost sight of comes back well before.
ALTER TABLE worker ADD COLUMN inactive_since TIMESTAMPTZ;

-- Workers already inactive start their lost-timeout now.
UPDATE worker SET inactive_since = now() WHERE status = 'inactive';

-- migrate:down
ALTER TABLE worker DROP COLUMN inactive_since;
