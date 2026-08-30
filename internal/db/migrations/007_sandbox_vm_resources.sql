-- migrate:up
-- Resource sizing for the sandbox's execution backend. Defaults match the
-- Firecracker config validated manually during development (1 vcpu / 256 MiB).
-- Existing rows and any caller not yet aware of these fields get sane values
-- instead of a Firecracker-rejecting 0.
ALTER TABLE sandboxes
    ADD COLUMN vcpu_count INT NOT NULL DEFAULT 1,
    ADD COLUMN mem_size_mib INT NOT NULL DEFAULT 256;

-- migrate:down
ALTER TABLE sandboxes
    DROP COLUMN mem_size_mib,
    DROP COLUMN vcpu_count;
