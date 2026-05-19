-- migrate:up
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'sandbox_events')
       AND NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'docker_events') THEN
        CREATE TABLE sandbox_events (
            id TEXT PRIMARY KEY,
            sandbox_id TEXT NOT NULL,
            container_id TEXT NOT NULL,
            worker_id TEXT NOT NULL,
            event_type TEXT NOT NULL,
            action TEXT NOT NULL,
            actor_id TEXT NOT NULL,
            attributes JSONB NOT NULL DEFAULT '{}',
            occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
        );

        CREATE INDEX sandbox_events_sandbox_id_idx ON sandbox_events (sandbox_id);
        CREATE INDEX sandbox_events_container_id_idx ON sandbox_events (container_id);
        CREATE INDEX sandbox_events_occurred_at_idx ON sandbox_events (occurred_at);
    END IF;
END $$;

-- migrate:down
DROP TABLE sandbox_events;
