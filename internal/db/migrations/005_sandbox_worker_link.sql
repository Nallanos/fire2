-- migrate:up
ALTER TABLE sandboxes
    ADD COLUMN worker_id TEXT NULL REFERENCES worker(id);

CREATE INDEX sandboxes_worker_id_idx ON sandboxes (worker_id);

-- migrate:down
DROP INDEX IF EXISTS sandboxes_worker_id_idx;
ALTER TABLE sandboxes DROP COLUMN worker_id;
