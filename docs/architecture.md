# dootd Architecture

This is the long-term technical reference for maintaining dootd. The rules apps must follow are in [app-contract.md](app-contract.md).

## 1. Goals and non-goals

**Goals**
- A single static binary. Installing it takes one SSH session, and every day-to-day task afterwards happens in the dashboard.
- It runs on a 1 GB VPS. Target: **< 30 MB RSS when idle**, and no background daemons other than dootd itself.
- Zig/C server-rendered apps with SQLite as the only database.
- Cloudflare in front, **Full (strict)** TLS, and the origin reachable **only** through Cloudflare.
- SQLite backups to any S3-compatible storage, with **RPO ≤ 3 h**.

**Non-goals (by design)**
- Multiple machines, clustering or high availability.
- Multiple users, teams or RBAC.
- Containers or strong multi-tenant isolation. All apps are trusted and belong to the same person.
- Zero-downtime deploys. SQLite with a single writer makes "stop then start" the safe choice.
- Automatic deploys on git push. Deploys happen **only when you click**.
- Non-Cloudflare setups, such as ACME/Let's Encrypt, in v1.

## 2. Environment assumptions

| Item | Assumption |
|---|---|
| OS | Ubuntu 24.04 LTS or newer, x86_64 or arm64 |
| Init | systemd |
| cgroups | v2 unified hierarchy, the Ubuntu 24.04 default |
| Size | 1–2 GB RAM, about 3–5 apps (max 5 supported and tested) |
| Accounts | 1 dashboard user, 1 GitHub PAT, 1 Cloudflare API token, 1 S3 bucket |
| Ports | Public: 22 (SSH) and 443 (dootd). Nothing else. |

## 3. Tech stack

**Go**, built with `CGO_ENABLED=0` into one fully static binary for `linux/amd64` and `linux/arm64`.

| Concern | Choice | Why |
|---|---|---|
| HTTP, TLS, reverse proxy | Go stdlib (`net/http`, `crypto/tls`, `httputil.ReverseProxy`) | Mature and built in, so no separate proxy is needed |
| dootd's own DB | SQLite via `modernc.org/sqlite` (pure Go) | No CGO, same database technology as the apps |
| Password hashing | `golang.org/x/crypto/argon2` (argon2id) | Current best practice |
| Secret encryption | AES-256-GCM (stdlib) | Encrypts PAT, tokens and env vars at rest |
| Git clone | `github.com/go-git/go-git/v5` | No dependency on the host `git` binary |
| S3 / R2 | `github.com/minio/minio-go/v7` | Small, works with any S3-compatible storage |
| Compression | `github.com/klauspost/compress/zstd` | Fast, good compression ratio |
| Zig tarballs (`.tar.xz`) | `github.com/ulikunitz/xz` | Pure Go |
| Config parsing | `github.com/BurntSushi/toml` | For `dootd.toml` |
| Dashboard UI | `html/template` + one CSS file + ~100 lines of plain JS (SSE, refresh), server-rendered SVG charts later, all embedded with `go:embed` | No Node or JS build step, very light |

Dependency rule: add new modules only when the stdlib would require more than about 300 lines of risky code instead.

## 4. Process and component overview

```
                    ┌──────────────────────────── dootd (one process, systemd unit) ───────────────────────────┐
 Cloudflare ─:443─► │ Edge: CF-IP filter → TLS (Origin CA cert, AOP mTLS) → Host router                        │
                    │        ├─ dashboard host ─► Web UI (auth, SSE)                                            │
                    │        └─ app host ───────► ReverseProxy ─► 127.0.0.1:$PORT ─────────► app process        │
                    │                                                                          (own uid,       │
                    │ Deployer ─► Builder ─► Toolchain mgr (zig versions)                       own cgroup)     │
                    │ Supervisor (start/stop/restart/backoff, log capture)                                      │
                    │ Cloudflare client (DNS, Origin CA, AOP, IP ranges)   GitHub client (PAT, clone)           │
                    │ Backup scheduler ─► snapshot (VACUUM INTO) ─► zstd ─► S3/R2                               │
                    │ Metrics collector (cgroup + /proc + proxy stats)                                          │
                    │ Store: /var/lib/dootd/dootd.db (SQLite)       Secrets: /etc/dootd/master.key              │
                    └──────────────────────────────────────────────────────────────────────────────────────────┘
```

All components are goroutines inside one process and talk to each other through Go interfaces and channels. There is no IPC and no extra services.

## 5. Code layout

```
cmd/dootd/            main: subcommands (serve, ctl, init, reset-password, version)
internal/
  config/             static config file + defaults
  store/              SQLite schema, migrations, queries
  secrets/            master key, AES-GCM seal/open
  auth/               argon2id, sessions, CSRF, login rate-limit
  web/                dashboard handlers, templates, static (embedded)
  apps/               app registry: stored apps + env vars, create/update/delete
  hostinfo/           host summary from /proc and statfs
  edge/               listener, CF IP filter, certificates, AOP CA, TLS config, host router, proxy, request stats, Cloudflare sync
  cloudflare/         minimal REST client (zones, DNS, origin CA, AOP, settings, IPs)
  github/             PAT validation, repo/branch listing, clone via go-git
  toolchain/          zig download/verify/cache
  builder/            build workspace, run build in cgroup, capture build log
  manifest/           dootd.toml parsing + validation
  deployer/           deploy queue + pipeline, releases, rollback, history
  control/            local admin API on a Unix socket (used by `dootd ctl`)
  supervisor/         process lifecycle, restart policy, log capture
  cgroup/             cgroup v2 create/limit/read-stats, delegation setup
  users/              per-app system users
  backup/             snapshot, archive, upload, retention, restore
  s3/                 thin wrapper over minio-go
  metrics/            sampling, ring buffers, rollups
  logs/               rotating log files + live tail fan-out
  selfupdate/         fetch release, verify, swap binary, restart
```

## 6. Filesystem layout

```
/usr/local/bin/dootd                      binary
/etc/dootd/config.toml                    static config (dashboard domain, data root, listen addr)
/etc/dootd/master.key                     32 random bytes, 0600 root; encrypts secrets in DB
/etc/systemd/system/dootd.service
/var/lib/dootd/
  dootd.db                                dootd state
  certs/<hostname>/{cert.pem,key.pem}     Origin CA certs (0600)
  aop/{ca.pem,ca.key,client.pem,client.key}  dootd's private AOP CA + client cert
  toolchains/zig/<version>/               extracted Zig releases (shared, read-only)
  cache/zig/<app>/                        per-app zig global cache (owned by app user)
  builds/<app>/<release-id>/              temporary build workspace
  apps/<app>/
    releases/<release-id>/                repo checkout + build outputs (root-owned, read-only to app)
    current -> releases/<release-id>
    data/                                 DATA_DIR (0700, app user)
    tmp/                                  TMPDIR  (0700, app user)
    logs/app.log[.1..3]                   runtime logs (rotated)
    logs/builds/<release-id>.log          build logs
  backups/staging/                        temp snapshot files before upload
```

The last **3 releases** are kept for instant rollback. Older ones are deleted after a successful deploy.

## 7. Data model (dootd.db)

| Table | Key columns |
|---|---|
| `settings` | key, value (plain or encrypted blob). Holds dashboard domain, GitHub PAT*, CF token*, S3 endpoint/bucket/keys*, backup schedule |
| `users` | id (always 1), email, password_hash (argon2id) |
| `sessions` | id_hash (SHA-256 of the token), csrf, created_at, last_seen, ip, user_agent |
| `apps` | name, type, repo, branch, path, domain (unique), port (unique), memory_max, cpu_max, pids_max, build_memory, build_timeout, created_at |
| `app_env` | app, name, value* |
| `deployments` | id, app, kind (deploy/rollback), status (queued/building/deploying/succeeded/failed), release_id, git_sha, error, created/started/finished_at |
| `releases` | app, id, git_sha, subject, branch, subdir, zig_version, run (JSON argv), health_path, created_at (only kept releases) |
| `app_state` | app, desired_state (running/stopped) |
| `backups` | id, app_id, object_key, kind (scheduled/pre-deploy/manual), size, created_at, status |
| `certs` | hostname, cf_cert_id, not_after |
| `metrics_1m` | ts, scope (host/app id), cpu, mem, pids, rx/tx, req_count, req_5xx, p50/p95 ms |
| `schema_migrations` | version |

`*` = encrypted with AES-256-GCM using `master.key`. `dootd.db` uses WAL mode, and dootd uses a single writer connection.

## 8. Bootstrap and install

1. `install.sh` (the only step that needs SSH):
   - Checks Ubuntu ≥ 24.04, systemd, cgroup v2, and the CPU architecture.
   - Downloads `dootd-linux-<arch>` from GitHub Releases and verifies its SHA-256 against `checksums.txt`.
   - Runs `apt-get install -y make`. That is the only system package.
   - Creates `/etc/dootd` and `/var/lib/dootd`, generates `master.key`, and installs the systemd unit.
   - Offers to create a 2 GB swapfile if there is no swap, because Zig builds can use a lot of memory.
   - Offers to enable `ufw` allowing only the detected SSH port and 443.
2. `dootd init` (interactive, in the same session) asks for:
   - Admin email and password.
   - Dashboard domain, e.g. `dootd.example.com`.
   - Cloudflare API token. dootd validates it and shows any missing permissions.
   - Public IPv4/IPv6. These are auto-detected and you confirm them.

   It then creates the proxied DNS record and the Origin CA certificate for the dashboard, sets up AOP for that zone, and starts the service.
3. After that, everything else (GitHub PAT, S3 settings, apps) is configured in the dashboard.

Recovery: `dootd reset-password` over SSH is the only emergency path.

### systemd unit (essentials)

```ini
[Service]
ExecStart=/usr/local/bin/dootd serve
Restart=always
Delegate=yes              # dootd owns its cgroup subtree
KillMode=mixed            # SIGTERM to dootd only (it stops apps gracefully); SIGKILL leftovers after the timeout
TimeoutStopSec=45s
LimitNOFILE=65536
```

dootd runs as **root**. It needs this to bind :443, create users, manage cgroups and chown app directories. App processes never run as root.

## 9. Edge: TLS, Cloudflare-only access, and routing

### 9.1 Listener and IP filter
- dootd listens only on `:443`. There is no `:80`. Enable Cloudflare's "Always Use HTTPS" instead.
- A custom `net.Listener` checks the TCP peer address against Cloudflare's IPv4 and IPv6 ranges. **Connections from any other address are closed before the TLS handshake.**
- The ranges are fetched from `GET /client/v4/ips` at startup and every 24 h, with a list compiled into the binary as a fallback. The last good list is stored in `settings` and used on the next start. An empty or implausible list (e.g. a /4) is refused, so a bad response can neither lock Cloudflare out nor open the port to everyone.
- Rejected connections are counted (`dootd ctl edge`) and logged at most once a minute.

### 9.2 Origin certificates (Full strict)
- For every hostname (the dashboard plus each app domain), dootd generates an ECDSA P-256 key and a CSR locally. It then calls the Cloudflare **Origin CA** API (`POST /certificates`, `request_type=origin-ecc`, validity 5475 days).
- The API token authenticates this call. Legacy Origin CA service keys are deprecated and removed after 30 Sep 2026 ([Cloudflare docs](https://developers.cloudflare.com/fundamentals/api/get-started/ca-keys)).
- `tls.Config.GetConfigForClient` chooses the certificate by SNI. **Unknown SNI means the handshake fails.** TLS 1.2+ with h2 and http/1.1.
- Keys and certificates live in `certs/<host>/` (0600); the Cloudflare certificate ID is in `edge_certs`. The sync (at startup, every 6 h, and on `dootd ctl edge sync`) re-issues any certificate with less than 30 days left and then revokes the replaced one.
- The zone's SSL mode is **checked, not forced**. If it isn't `strict`, the dashboard shows a warning and a "Set to Full (strict)" button, because the setting affects every hostname in the zone.

### 9.3 Authenticated Origin Pulls (mTLS)
The IP filter alone only proves the traffic comes from *someone's* Cloudflare account. **Zone-level AOP with our own CA** proves it comes from *yours*:
- On first run, dootd creates a private CA (RSA 3072, 10 years) and a client certificate signed by it (RSA 2048, 5 years), stored in `aop/`. RSA is used because it is what Cloudflare documents for AOP uploads.
- For each zone it serves, dootd uploads the client certificate and key through the zone-level AOP API (`origin_tls_client_auth`) and turns the zone setting `tls_client_auth` **on** ([Cloudflare docs](https://developers.cloudflare.com/ssl/origin-configuration/authenticated-origin-pull/set-up/zone-level/)).
- The TLS config uses `ClientAuth: RequireAndVerifyClientCert` with dootd's CA as `ClientCAs`, **per zone and only once Cloudflare reports our certificate `active` and AOP enabled**. Enforcing earlier would break the site, because Cloudflare would not yet present the certificate.
- The enforcement state is saved in `edge_zones`, so after a restart it applies immediately, even if the Cloudflare API is unreachable. If a sync fails only because the API is down, enforcement stays on.
- If someone disables AOP or deletes our certificate in the Cloudflare dashboard, the next sync re-uploads and re-enables it (otherwise the origin would reject all traffic).
- dootd renews the client certificate 60 days before it expires: it uploads the new one, waits until it is active, then deletes the old one.
- It can be turned off with `edge.authenticated_origin_pulls = false` (per zone in the dashboard later). The IP filter always stays on.

### 9.4 Router and proxy
- The `Host` header (lowercased, port removed) is looked up in an in-memory map `domain → app`. This map is rebuilt whenever apps change.
  - Dashboard domain → web UI handler.
  - App domain → that app's `httputil.ReverseProxy` → `http://127.0.0.1:<port>`.
  - Anything else → `404`.
  - `Host` must equal the TLS SNI, otherwise `421 Misdirected Request`. Without this, a client could use the TLS settings of a zone without AOP to reach an app in a zone with AOP.
- The proxy sets `X-Forwarded-For` and `X-Real-IP` from `CF-Connecting-IP` (replacing anything the client sent), `X-Forwarded-Proto: https` and `X-Forwarded-Host`, keeps the original `Host`, and flushes responses immediately (streaming/SSE).
- Timeouts: read header 10 s, idle 120 s, no overall write timeout (so long-polling and WebSockets work). Upstream dial timeout is 5 s.
- Deploying → 503 "Deploying" (`Retry-After: 3`); starting → 503 "Starting"; stopped/crashed/never deployed → 503 "App not running"; proxy error → 502. Pages carry `X-Dootd-Page` and `Cache-Control: no-store`.
- The dashboard domain shows a placeholder page until Phase 4.
- Each request updates in-memory counters per app (count, status class, latency histogram), which are used for metrics.

### 9.5 Cloudflare API token permissions
Scope: the zones you use, or all zones.
- Zone → Zone → Read
- Zone → DNS → Edit
- Zone → SSL and Certificates → Edit (Origin CA + AOP certs)
- Zone → Zone Settings → Edit (enable AOP, read and set the SSL mode)

When an app's domain is set, dootd finds the zone (longest suffix match), then creates or updates a **proxied** `A` record, plus an `AAAA` record if the server has IPv6. Next it issues the certificate and makes sure AOP is set up for the zone. It refuses to touch a conflicting `CNAME` or duplicate records and reports them instead.

The token is stored sealed (`settings:cloudflare_token`) via `dootd ctl cloudflare-token`, which verifies it and lists the readable zones.

### 9.6 Configuration (`/etc/dootd/config.toml`)
```toml
[edge]
listen           = ":443"               # "off" disables the edge
dashboard_domain = "dootd.example.com"
public_ipv4      = ""                   # default: detected via cloudflare.com/cdn-cgi/trace
public_ipv6      = ""                   # default: detected; "off" = no AAAA records
authenticated_origin_pulls = true
```

### 9.7 Testing without Cloudflare
`scripts/e2e/e2etool cfmock` is a fake Cloudflare API (its own Origin CA root; it exposes the uploaded AOP client certificate the way Cloudflare's edge would present it). The Phase 3 E2E adds `198.18.0.10` to `lo` and lists `198.18.0.0/15` in the fake `/ips`, so requests from it count as Cloudflare while requests from `127.0.0.1` are rejected.

## 10. Apps: users, cgroups, supervision

### 10.1 Users
- Each app gets its own system user `dootd-<app>` (`useradd --system --no-create-home --shell /usr/sbin/nologin`).
- Its `data/`, `tmp/` and `cache/zig/<app>` directories are owned by that user with mode 0700. Releases are owned by root and world-readable. Other apps' directories cannot be read.

### 10.2 cgroup v2 tree
Because of `Delegate=yes`, dootd owns `/sys/fs/cgroup/system.slice/dootd.service/`. cgroup v2 doesn't allow processes in inner nodes, so dootd first moves itself into a leaf:

```
dootd.service/
  supervisor/            dootd itself
  apps/<app>/            runtime: memory.max, memory.swap.max=0, memory.zswap.max=0, cpu.max, pids.max
  builds/<app>/          build: memory.max=build_mem_max, cpu.weight=50 (builds yield to apps)
```

- Processes start directly inside their cgroup using `SysProcAttr{CgroupFD, UseCgroupFD: true}` (clone3 `CLONE_INTO_CGROUP`), with `Credential{Uid,Gid}`, `Setpgid`, and `Pdeathsig: SIGKILL`.
- On stop, if the process tree hasn't exited after the grace period, dootd SIGKILLs every PID listed in the cgroup, repeating until no new ones appear, so no orphans are left behind. When the main process exits for any reason, the rest of its cgroup is killed the same way.
- dootd does **not** use `cgroup.kill`. On Ubuntu 24.04 (kernel 6.17) we found that after `cgroup.kill` has been written once, every new child placed into that cgroup with `CLONE_INTO_CGROUP` is killed immediately, which breaks restarts (verified in the Phase 1 E2E run).
- No `memory.high`: with swap disabled for apps it cannot reclaim anything and only stalls the app. A clean OOM kill at `memory.max` followed by a restart is more predictable.
- Other rlimits: `RLIMIT_NOFILE` is set per app with `prlimit`. The app gets a clean environment (only `PATH`, `LANG`, the contract variables and user env vars), not dootd's.
- `memory.events` is watched, and OOM kills show up in the app's event log.

### 10.3 Supervisor
- A state machine per app: `stopped → starting → running → stopping → stopped`, plus `crashed` and `deploying`.
- `starting` becomes `running` only when the health check passes: TCP connect, then `GET health_path` returns 2xx/3xx, retried for up to 30 s.
- Restart policy: exponential backoff from 1 s up to 60 s. After 5 exits within 5 min the app becomes `crashed`, and you restart it manually.
- On dootd start, every app whose `desired_state=running` is started, one at a time.
- dootd restarts, including self-updates, **restart all apps** (a few seconds of downtime). That's an accepted trade-off.

### 10.4 Logs
- stdout and stderr go through pipes into `logs/app.log`. Each line gets a timestamp and stream tag, and files rotate at 10 MB with 3 kept.
- There is also an in-memory ring buffer (last 1000 lines) per app. The dashboard streams live logs over **SSE** from this buffer.
- Build logs are stored per release, and the last 20 per app are kept.

## 11. Build and deploy pipeline

Only **one build runs at a time** across the whole server, so small VPSes aren't overloaded. A second deploy request waits in a queue, and the UI shows it as queued.

```
Deploy clicked (Phase 2: `dootd ctl deploy <app>`)
 1-2. Shallow clone (depth 1, single branch) into builds/<app>/<deployment-id>/,
      record the HEAD SHA, remove .git; release id = <UTC time>-<sha7>   [app keeps serving]
 3. Parse + validate dootd.toml in the app root (repo root or the app's path)
 4. Ensure toolchain: toolchains/zig/<version>/ (download if missing)
 5. Run `build` as app user, in builds/<app> cgroup, env = user env + PATH/CC/CXX,
    cwd = workspace, timeout = build_timeout; stream log
 6. Verify `run` binary exists and is executable
 7. Move workspace → apps/<app>/releases/<release-id>/ (chown root, read-only)
 ── downtime starts ──
 8. Router marks app `deploying` (503 page)
 9. Stop old process (SIGTERM, 10 s, then SIGKILL the cgroup)
10. Pre-deploy backup of DATA_DIR SQLite files (snapshot local; upload async)
11. Swap `current` symlink atomically (rename)
12. Wipe tmp/, start new process, health check
 ── downtime ends ──
13a. Healthy → route traffic, mark deployment `succeeded`, prune old releases
13b. Unhealthy → stop new, point `current` back, start old release, mark `failed`
     (DB is NOT restored automatically; UI offers "Restore pre-deploy backup")
```

- If a build fails at steps 2–7, the running app is never affected (verified in E2E: same PID keeps serving).
- A release that fails its health check is deleted; the previous release is restored and restarted only if it was running before.
- Build logs are stored per deployment (`logs/builds/<deployment-id>.log`, last 20 kept) and streamed live by tailing the file.
- On startup, deployments left `queued/building/deploying` by a crash or restart are marked failed, and `builds/<app>/` is emptied. A shutdown cancels the running build; a switch that already started (steps 8–13) always completes with a detached context so `current` and the running process never disagree.
- Release metadata (SHA, commit subject, subdir, zig version, `run`, `health_path`) is stored in the `releases` table, so rollbacks and restarts don't re-read `dootd.toml`.
- Whether each app should run after a restart is stored in `app_state` (stopping an app keeps it stopped across dootd restarts).
- **Redeploy** runs the same pipeline from the latest commit. **Rollback** runs only steps 8–13 using an earlier release, with no build.
- Deploys only ever happen when you click. There are no webhooks.

### 11.1 Zig toolchain manager
- The tarball URL and SHA-256 come from `https://ziglang.org/download/index.json`, using the entry for the exact version and `<arch>-linux`. The index is cached for 6 h.
- dootd downloads, checks the SHA-256, extracts to a temporary directory and then renames it into `toolchains/zig/<version>/`. A partly finished download can never be used.
- Toolchains stay until you delete them. The dashboard lists them, shows which apps use each one, and lets you delete unused ones.
- Future: minisign signature verification and community mirrors.

### 11.2 GitHub
- A fine-grained PAT with **Contents: Read-only** on the selected repos is enough. dootd validates it when it's saved and uses it to list repos and branches in the "Add app" form.
- The PAT is only used as HTTP basic auth (`x-access-token`) for go-git, in memory, and **only for `github.com` URLs**. It is never written to disk in plain text or into `.git/config` (`.git` is deleted after the clone). It is stored sealed in `settings` under purpose `settings:github_token`.
- `file:///` repositories are accepted for development and the E2E tests.

### 11.3 Control socket (`dootd ctl`)
- `dootd serve` listens on `/run/dootd/dootd.sock` (0600, root only) with a small HTTP/JSON API: app status, deploy, rollback, start/stop/restart, releases, deployments, build log (follow), app logs (follow), GitHub token.
- `dootd ctl` is the CLI for it. Until the dashboard exists (Phase 4) it is how apps are deployed; afterwards it stays as the SSH fallback. The dashboard will call the same deployer and supervisor methods directly.

## 12. Backups

| Setting | Default |
|---|---|
| Schedule | every **3 h** (00:00, 03:00, … server time) |
| Also | before every deploy/rollback, and manual "Backup now" |
| Retention | **48 h** for all kinds. The newest backup of each app is always kept, even if it's older. |
| Target | any S3-compatible storage (R2, B2, MinIO, AWS) configured with endpoint, region, bucket, key and secret |

**Snapshot**: for each file in `DATA_DIR` that starts with the SQLite header (`SQLite format 3\0`), dootd opens it with `modernc.org/sqlite` (`busy_timeout=10000`) and runs `VACUUM INTO 'backups/staging/<file>'`. This produces a consistent, compacted copy while the app keeps running, and it works in WAL mode. Each copy is then checked with `PRAGMA quick_check`.

**Archive**: `tar` + `zstd` → `dootd/<host-id>/<app>/<UTC-timestamp>-<kind>.tar.zst`, along with a small JSON manifest (files, sizes, sha256, release id, dootd version). The upload is multipart, retried 3 times with backoff, and staging files are deleted afterwards.

**Retention**: after each run, dootd lists the app prefix and deletes objects older than 48 h, except the newest one. This is done by dootd itself, not by bucket lifecycle rules, so it behaves the same on every provider.

**Restore** (dashboard, per backup):
1. Download and verify the checksums from the manifest.
2. Stop the app.
3. Move the current DB files, including `-wal` and `-shm`, to `data/.pre-restore-<ts>/`.
4. Put the restored files in place and chown them to the app user.
5. Start the app and run the health check.

The last 2 `.pre-restore` folders are kept.

**dootd's own state**: `dootd.db` is backed up daily to `dootd/<host-id>/_dootd/`. Secrets inside it are encrypted, so **the master key is required to use it**. The dashboard's Settings page lets you download a *recovery kit* (the master key plus config), and it reminds you until you have downloaded it once.

**Failure visibility**: a failed backup shows a red badge on the app and on the dashboard home page.

## 13. Monitoring

| Source | Metrics |
|---|---|
| Host `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `statfs` | CPU %, memory used/available, swap, load, disk used/free on the data root |
| cgroup `cpu.stat`, `memory.current`, `memory.events`, `pids.current`, `io.stat` | Per-app CPU %, memory, OOM count, pids, disk I/O |
| Edge counters | Per-app requests/min, 2xx/3xx/4xx/5xx, p50/p95 latency |
| Supervisor | Uptime, restart count, last exit code |

- Sampled every **10 s** into an in-memory ring buffer covering the last hour, which feeds the "live" charts.
- Rolled up to **1-minute** rows in `metrics_1m` and kept for **7 days**. The rollups are pruned hourly.
- Charts are rendered on the server as SVG and refreshed by the dashboard script. No JS chart library is used.
- The dashboard shows warnings (in the UI only for v1): disk > 85 %, memory > 90 %, app crashed, backup failed, certificate expiring.

## 14. Dashboard and security

- Served only on `edge.dashboard_domain`, through the same Cloudflare-only, AOP-protected edge. Without a dashboard domain (or with the edge off) there is no dashboard; `dootd ctl` still works over SSH.
- **UI**: server-rendered `html/template` pages and one CSS file, plus ~100 lines of plain JavaScript (`internal/web/static/app.js`) for live logs (Server-Sent Events), refreshing status sections every 5 s and confirmation prompts. Everything embedded with `go:embed`; every action is a normal form POST followed by a redirect, so the dashboard also works without JavaScript (except the live parts).
- **Admin account**: exactly one user (`users` row id 1). It is created or reset over SSH with `sudo dootd ctl admin set-password --email you@example.com` (alias: `sudo dootd reset-password`); this revokes all sessions (Req 2.6). `dootd init` (Phase 7) will call the same thing.
- **Passwords**: argon2id (64 MiB, t=3, p=1), at least 12 characters. At most 2 hashes run at the same time so a burst of logins can't exhaust a 1 GB VPS. Unknown emails are checked against a dummy hash so timing doesn't reveal the email.
- **Rate limiting** (in memory): 5 failed sign-ins per client IP (`CF-Connecting-IP`) per 15 minutes → 429. Above 50 failures in 15 minutes overall, sign-ins are additionally limited to one attempt per 2 s, which slows a distributed attack without locking the owner out.
- **Sessions**: 32-byte random token in the cookie `__Host-dootd` (`Secure; HttpOnly; SameSite=Strict; Path=/`); only its SHA-256 is stored (`sessions`). Expiry: 7 days idle, 30 days total. The account page lists sessions and can sign out all others; a password change does that automatically.
- **CSRF**: every POST needs `Origin` (or, failing that, `Referer`) equal to `https://<dashboard domain>`, and signed-in POSTs also need the session's CSRF token in the `csrf` form field.
- **Headers**: `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, HSTS, `Cache-Control: no-store` on pages. Request bodies are capped at 256 KB.
- **Messages** after an action use a short-lived `__Host-dootd-flash` cookie (60 s).
- **Secrets**: tokens and env var values are never shown again after saving; env vars are listed by name only.
- **Pages**: Apps (host summary, warnings, app table) · App (status and actions, releases with rollback, deployments, env vars, settings, delete) · Add app · Deployment (live build log) · Logs (live app log) · Settings (GitHub, Cloudflare, domains, zones with "Set Full (strict)", Zig toolchains) · Account (password, sessions).

### 14.1 Apps in the database
- `apps` holds the configuration, `app_env` the sealed env vars (`app_env:<app>:<NAME>` as associated data). `internal/apps.Service` validates input (every problem reported at once), stores it and wires it into the deployer, supervisor and edge router at runtime; at startup it loads every stored app.
- Ports are assigned once, from 20001 upwards, and never change.
- Editing settings or env vars updates the stored config immediately; the running process keeps its old settings until a restart or deploy, and the dashboard says "restart needed" until then. Domain changes re-route immediately and clean up the old hostname.
- **Delete** (type the name to confirm): unregister (refused while a deployment is running) → stop → remove the cgroups → delete rows, releases, logs, caches and build folders → delete the DNS records that point at this server and revoke the Origin CA certificate → `userdel`. With "keep data" (default) `DATA_DIR` is moved to `/var/lib/dootd/deleted/<app>-<unix time>/data`, owned by root.
- `--dev-apps` still exists for the end-to-end tests of phases 1–3 (prebuilt apps); it is not needed on a real server.

## 15. Self-update

1. Settings → "Check for update" calls the GitHub Releases API for `sumitwaani2/dootd`.
2. dootd downloads the binary for its architecture and `checksums.txt`, then verifies the SHA-256.
3. It writes the new binary to `/usr/local/bin/dootd.new`, `fsync`s it and renames it over the old one. The previous binary is kept as `dootd.prev`.
4. It exits with a special code, and systemd restarts it. Before running any pending DB migrations, the new version saves `dootd.db.pre-update` (a `VACUUM INTO` copy).
5. If the new version fails to start 3 times, the `ExecStartPre` guard restores `dootd.prev` and `dootd.db.pre-update`. This matters because an older binary refuses to open a schema newer than it knows.

## 16. Resource budget (targets)

| Item | Target |
|---|---|
| dootd idle RSS | < 30 MB |
| dootd idle CPU | < 1 % (10 s sampling, no busy loops) |
| Proxy overhead | < 1 ms p50 added latency |
| Binary size | < 30 MB |

## 17. Decisions log

| # | Decision | Reason |
|---|---|---|
| D1 | Go, not Zig/C, for dootd itself | stdlib TLS, proxy and HTTP, pure-Go SQLite and S3 → fastest path to something reliable |
| D2 | No Docker, only cgroups + users | Trusted single-user apps; keeps it light |
| D3 | Stop-then-start deploys | Safe with a single SQLite writer; 1–3 s downtime accepted |
| D4 | Deploy only on click | User preference; fewer moving parts |
| D5 | Cloudflare Origin CA, no ACME | All domains are on Cloudflare; 15-year certs; no port 80 |
| D6 | Zone AOP with own CA + IP filter | IP filter alone allows any Cloudflare customer through |
| D7 | Snapshot backups every 3 h (not continuous replication) | RPO 3 h accepted; much simpler than a Litestream-style design |
| D8 | Pinned Zig per app, also used as C compiler | Reproducible builds, a single toolchain |
| D9 | Server-rendered HTML + a small vanilla script (no htmx) + server-rendered SVG | No frontend build pipeline and no vendored library; a strict CSP (`script-src 'self'`) is easy |
| D10 | Kill cgroups by PID, not `cgroup.kill` | `cgroup.kill` makes later `CLONE_INTO_CGROUP` children die instantly on current Ubuntu kernels |
| D11 | No `memory.high`, swap and zswap off for apps | Predictable OOM + restart instead of an app stalled near its limit |
| D12 | E2E scripts run on GitHub-hosted Ubuntu 24.04 VMs | Real systemd + full cgroup v2; catches kernel behaviour unit tests cannot |
| D13 | Clone as root, chown to the app user for the build, then make the tree root-owned (never following symlinks, skipping files owned by others) | The build can't escape into other users' files, and the app can't modify its own release |
| D14 | Optional app path (monorepo subdirectory) | Lets one repo hold several apps; also lets E2E deploy `examples/*` straight from this repo |
| D15 | Local admin API on a Unix socket + `dootd ctl` | Deploys before the dashboard exists; permanent SSH fallback; no network exposure |
| D16 | Enforce AOP per zone, only after Cloudflare reports it active; persist the state | Never break a site during setup; never fail open after a restart |
| D17 | Host must equal SNI (421 otherwise) | Stops cross-zone requests from bypassing a zone's AOP |
| D18 | Edge sync is best effort and per host | One misconfigured domain does not block certificates or DNS for the others |
| D19 | The admin is created over SSH (`dootd ctl admin set-password`), never through a web setup page | A public first-run page could be claimed by whoever reaches it first |
| D20 | Config changes apply on restart/deploy, never silently to a running app | Predictable; the dashboard shows "restart needed" |
| D21 | Deleting an app keeps its data by default (moved aside) | Deletion is one click; losing a database should not be |
