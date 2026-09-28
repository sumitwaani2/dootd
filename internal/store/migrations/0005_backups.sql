-- Backups of app databases (app = '_dootd' for dootd.db itself).
CREATE TABLE backups (
    id          INTEGER PRIMARY KEY,
    app         TEXT    NOT NULL,
    kind        TEXT    NOT NULL,            -- scheduled | pre-deploy | manual | pre-restore
    status      TEXT    NOT NULL,            -- running | ok | failed
    object_key  TEXT    NOT NULL DEFAULT '', -- key in the bucket ('' = not uploaded)
    local_path  TEXT    NOT NULL DEFAULT '', -- local copy ('' = pruned)
    size        INTEGER NOT NULL DEFAULT 0,
    sha256      TEXT    NOT NULL DEFAULT '', -- of the archive
    files       INTEGER NOT NULL DEFAULT 0,  -- databases in the archive
    release_id  TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '', -- snapshot or upload error
    created_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX backups_app ON backups (app, id);
