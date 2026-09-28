-- Origin CA certificates issued by Cloudflare, one per hostname.
CREATE TABLE edge_certs (
    hostname   TEXT    PRIMARY KEY,
    zone_id    TEXT    NOT NULL,
    cf_cert_id TEXT    NOT NULL,
    not_after  INTEGER NOT NULL,
    issued_at  INTEGER NOT NULL
) STRICT;

-- Per-zone Cloudflare state managed by dootd.
CREATE TABLE edge_zones (
    zone_id         TEXT    PRIMARY KEY,
    name            TEXT    NOT NULL,
    ssl_mode        TEXT    NOT NULL DEFAULT '',
    aop_cert_id     TEXT    NOT NULL DEFAULT '', -- our uploaded client certificate
    aop_cert_serial TEXT    NOT NULL DEFAULT '', -- serial of that certificate
    aop_active      INTEGER NOT NULL DEFAULT 0,  -- 1 = active at Cloudflare and AOP enabled: enforce
    checked_at      INTEGER NOT NULL DEFAULT 0
) STRICT;
