-- Built releases that still exist on disk (at most 3 per app).
CREATE TABLE releases (
    app         TEXT    NOT NULL,
    id          TEXT    NOT NULL,           -- e.g. 20260927-141500-a1b2c3d
    git_sha     TEXT    NOT NULL,
    subject     TEXT    NOT NULL DEFAULT '', -- commit message first line
    branch      TEXT    NOT NULL,
    subdir      TEXT    NOT NULL,           -- app root inside the checkout
    zig_version TEXT    NOT NULL,
    run         TEXT    NOT NULL,           -- JSON array (argv)
    health_path TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (app, id)
) STRICT;

-- Deploy / rollback history.
CREATE TABLE deployments (
    id          INTEGER PRIMARY KEY,
    app         TEXT    NOT NULL,
    kind        TEXT    NOT NULL,           -- deploy | rollback
    status      TEXT    NOT NULL,           -- queued | building | deploying | succeeded | failed
    release_id  TEXT    NOT NULL DEFAULT '',
    git_sha     TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    started_at  INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX deployments_app ON deployments (app, id);

-- Whether the operator wants an app running (survives dootd restarts).
CREATE TABLE app_state (
    app           TEXT PRIMARY KEY,
    desired_state TEXT NOT NULL             -- running | stopped
) STRICT;
