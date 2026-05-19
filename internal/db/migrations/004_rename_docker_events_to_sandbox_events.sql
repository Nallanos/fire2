-- migrate:up
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'docker_events')
       AND NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'sandbox_events') THEN
        ALTER TABLE docker_events RENAME TO sandbox_events;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'docker_events_sandbox_id_idx') THEN
        ALTER INDEX docker_events_sandbox_id_idx RENAME TO sandbox_events_sandbox_id_idx;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'docker_events_container_id_idx') THEN
        ALTER INDEX docker_events_container_id_idx RENAME TO sandbox_events_container_id_idx;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'docker_events_occurred_at_idx') THEN
        ALTER INDEX docker_events_occurred_at_idx RENAME TO sandbox_events_occurred_at_idx;
    END IF;
END $$;

-- migrate:down
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables WHERE table_name = 'sandbox_events'
    ) THEN
        ALTER TABLE sandbox_events RENAME TO docker_events;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'sandbox_events_sandbox_id_idx') THEN
        ALTER INDEX sandbox_events_sandbox_id_idx RENAME TO docker_events_sandbox_id_idx;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'sandbox_events_container_id_idx') THEN
        ALTER INDEX sandbox_events_container_id_idx RENAME TO docker_events_container_id_idx;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'sandbox_events_occurred_at_idx') THEN
        ALTER INDEX sandbox_events_occurred_at_idx RENAME TO docker_events_occurred_at_idx;
    END IF;
END $$;
