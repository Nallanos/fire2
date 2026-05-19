-- migrate:up
ALTER TABLE worker
    ADD COLUMN cpu_budget INT NOT NULL DEFAULT 0,
    ADD COLUMN mem_budget INT NOT NULL DEFAULT 0,
    ADD COLUMN cpu_usage INT NOT NULL DEFAULT 0,
    ADD COLUMN mem_usage INT NOT NULL DEFAULT 0,
    ADD COLUMN last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- migrate:down
ALTER TABLE worker
    DROP COLUMN last_heartbeat,
    DROP COLUMN mem_usage,
    DROP COLUMN cpu_usage,
    DROP COLUMN mem_budget,
    DROP COLUMN cpu_budget;
