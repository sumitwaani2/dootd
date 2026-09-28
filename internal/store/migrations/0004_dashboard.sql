-- The single dashboard user.
CREATE TABLE users (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    email         TEXT    NOT NULL,
    password_hash TEXT    NOT NULL,
    updated_at    INTEGER NOT NULL
) STRICT;

-- Dashboard sessions. Only the SHA-256 of the session token is stored.
CREATE TABLE sessions (
    id_hash    BLOB    PRIMARY KEY,
    csrf       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    ip         TEXT    NOT NULL DEFAULT '',
    user_agent TEXT    NOT NULL DEFAULT ''
) STRICT;

-- Apps managed from the dashboard.
CREATE TABLE apps (
    name          TEXT    PRIMARY KEY,
    type          TEXT    NOT NULL,
    repo          TEXT    NOT NULL,
    branch        TEXT    NOT NULL,
    path          TEXT    NOT NULL DEFAULT '',
    domain        TEXT    NOT NULL DEFAULT '',
    port          INTEGER NOT NULL UNIQUE,
    memory_max    INTEGER NOT NULL,
    cpu_max       REAL    NOT NULL,
    pids_max      INTEGER NOT NULL,
    build_memory  INTEGER NOT NULL,
    build_timeout INTEGER NOT NULL, -- seconds
    created_at    INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX apps_domain ON apps (domain) WHERE domain != '';

-- App environment variables; values sealed by internal/secrets
-- (purpose "app_env:<app>:<NAME>").
CREATE TABLE app_env (
    app   TEXT NOT NULL REFERENCES apps (name) ON DELETE CASCADE,
    name  TEXT NOT NULL,
    value BLOB NOT NULL,
    PRIMARY KEY (app, name)
) STRICT;
