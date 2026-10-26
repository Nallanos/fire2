-- migrate:up
-- Zero marks a worker row that has not acquired a fencing token yet.
ALTER TABLE worker
    ADD COLUMN fencing_token BIGINT NOT NULL DEFAULT 0
    CHECK (fencing_token >= 0);

-- migrate:down
ALTER TABLE worker DROP COLUMN fencing_token;
