-- One row per scope per minute, kept 7 days (docs/architecture.md §13).
-- scope: "_host", "_dootd" or an app name. Rates are per minute; NULL
-- latency means no requests in that minute.
CREATE TABLE metrics_1m (
    scope       TEXT    NOT NULL,
    ts          INTEGER NOT NULL, -- minute start, unix seconds
    cpu         REAL    NOT NULL, -- percent (host: of all cores; apps/dootd: of one core)
    mem         INTEGER NOT NULL, -- bytes (max in the minute)
    mem_limit   INTEGER NOT NULL, -- host: total RAM; apps: memory.max
    swap        INTEGER NOT NULL,
    load1       REAL    NOT NULL,
    disk_used   INTEGER NOT NULL,
    disk_total  INTEGER NOT NULL,
    pids        INTEGER NOT NULL,
    io_read     REAL    NOT NULL, -- bytes/s
    io_write    REAL    NOT NULL,
    req         REAL    NOT NULL, -- requests/min
    req_5xx     REAL    NOT NULL,
    p50         REAL,             -- ms
    p95         REAL,
    oom         INTEGER NOT NULL, -- OOM kills in the minute
    PRIMARY KEY (scope, ts)
) STRICT, WITHOUT ROWID;
CREATE INDEX metrics_1m_ts ON metrics_1m (ts);
