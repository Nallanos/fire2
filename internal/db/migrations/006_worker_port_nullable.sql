-- migrate:up
-- Workers bind an OS-assigned ephemeral port and report it via heartbeat, so a
-- worker row may exist before its port is known. NULL means "port not reported
-- yet" — the scheduler skips such workers instead of guessing a default.
ALTER TABLE worker ALTER COLUMN port DROP NOT NULL;

-- migrate:down
ALTER TABLE worker ALTER COLUMN port SET NOT NULL;
