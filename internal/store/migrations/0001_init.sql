-- Key/value settings. Secret values are stored sealed by internal/secrets.
CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;
