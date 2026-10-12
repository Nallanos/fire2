-- migrate:up
-- sandbox_events was shaped after the Docker events API (id, sandbox_id,
-- container_id, worker_id, event_type, action, actor_id, attributes,
-- occurred_at — see migrations 003/004): a log of type+action entries,
-- keyed by a container/actor id, carrying the actor's free-form labels.
--
-- Firecracker's actual event source is InstanceInfo (firecracker-go-sdk
-- client/models/instance_info.go): {app_name, id, state, vmm_version} — a
-- polled snapshot of the VM, not a log of discrete actions, and no free-form
-- label bag. The VM's id is already sandbox_id (see runtime.NewClient), so:
--   - container_id, actor_id: dropped — no separate container/actor
--     identity exists for a Firecracker VM.
--   - attributes: dropped — it held the Docker actor's labels
--     (event.Actor.Attributes); Firecracker has no equivalent free-form
--     metadata, just the two fixed fields above.
--   - action: dropped — Firecracker has no action verb, only a state.
--   - event_type: renamed to state, matching InstanceInfo.State
--     ("Not started" | "Running" | "Paused").
ALTER TABLE sandbox_events
    DROP COLUMN container_id,
    DROP COLUMN actor_id,
    DROP COLUMN action,
    DROP COLUMN attributes;

ALTER TABLE sandbox_events
    RENAME COLUMN event_type TO state;

-- migrate:down
ALTER TABLE sandbox_events
    RENAME COLUMN state TO event_type;

ALTER TABLE sandbox_events
    ADD COLUMN container_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN actor_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN action TEXT NOT NULL DEFAULT '',
    ADD COLUMN attributes JSONB NOT NULL DEFAULT '{}';

CREATE INDEX sandbox_events_container_id_idx ON sandbox_events (container_id);
