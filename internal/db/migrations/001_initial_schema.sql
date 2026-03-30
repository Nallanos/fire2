CREATE TABLE sandboxes (
    id TEXT PRIMARY KEY,
    runtime TEXT NOT NULL,
    status TEXT NOT NULL,
    ttl BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    port INT NOT NULL,
    preview_url TEXT NOT NULL,
    image TEXT NOT NULL
);

CREATE TABLE worker (
    id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    address TEXT NOT NULL,
    Capacity INT NOT NULL,
    port INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);