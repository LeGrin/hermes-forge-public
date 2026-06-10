CREATE TABLE IF NOT EXISTS projects (
    project     TEXT NOT NULL,
    domain      TEXT NOT NULL,
    target_node TEXT NOT NULL DEFAULT 'mac-forge',
    target_executor TEXT NOT NULL DEFAULT 'claude',
    working_dir TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (project)
);
