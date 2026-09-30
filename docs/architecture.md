# dootd Architecture

This is the long-term technical reference for maintaining dootd. The rules apps must follow are in [app-contract.md](app-contract.md).

## 1. Goals and non-goals

**Goals**
- A single static binary. Installing it is **one command in one SSH session**; it prints a one-time password, and everything afterwards (admin account, tokens, the dashboard's own domain, apps, backups) happens in the dashboard.
- **One way to do each thing.** There is no admin CLI, no config file to edit and no optional feature that needs its own maintenance. dootd is an internal tool for one non-technical user: ease and correctness beat features.
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
- Self-update, backups of dootd's own settings, and rebuilding a whole server from a backup. Updating means re-running the installer; a new server gets its settings typed in again and its apps' data restored from the bucket (§12).

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
 (setup only) ─────► │        └ non-CF connection while setup is open → self-signed TLS → Web UI only            │
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
cmd/dootd/            main: `serve` (run by systemd), `setup-host` (run by install.sh), `version`
contrib/systemd/      dootd.service, embedded into the binary (`dootd setup-host` installs it)
internal/
  store/              SQLite schema, migrations, queries
  secrets/            master key, AES-GCM seal/open
  auth/               argon2id, sessions, CSRF, login rate-limit, one-time password
  web/                dashboard handlers, templates, static (embedded)
  apps/               app registry: stored apps + env vars, create/update/delete, restore on add
  hostinfo/           host summary from /proc and statfs
  edge/               listener, CF IP filter, setup access, certificates, AOP CA, TLS config, host router, proxy, request stats, Cloudflare sync, dashboard domain
  cloudflare/         minimal REST client (zones, DNS, origin CA, AOP, settings, IPs)
  github/             PAT validation, repo listing, clone via go-git
  toolchain/          zig download/verify/cache
  builder/            build workspace, run build in cgroup, capture build log
  manifest/           dootd.toml parsing + validation
  deployer/           deploy queue + pipeline, releases, rollback, history
  supervisor/         process lifecycle, restart policy, log capture
  cgroup/             cgroup v2 create/limit/read-stats, delegation setup
  users/              per-app system users
  backup/             snapshot, archive, upload, retention, restore, bucket folders
  s3/                 thin wrapper over minio-go
  metrics/            sampling, ring buffers, rollups
  logs/               rotating log files + live tail fan-out
  layout/             paths under /var/lib/dootd
  testenv/            the DOOTD_TEST_* overrides used by the end-to-end tests (§18)
```

## 6. Filesystem layout

```
/usr/local/bin/dootd                      binary
/etc/dootd/master.key                     32 random bytes, 0600 root; encrypts secrets in DB
/etc/systemd/system/dootd.service
/var/lib/dootd/
  dootd.db                                dootd state (every setting lives here; there is no config file)
  setup/{cert.pem,key.pem}                self-signed certificate for the setup address (§8.1)
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
  backups/staging/                        temporary snapshots and downloads (emptied at startup)
  backups/local/<app>/                    newest archives (<time>-<kind>-<id>.tar.zst)
  deleted/<app>-<time>/data/              data kept when an app is deleted
```

The last **3 releases** are kept for instant rollback. Older ones are deleted after a successful deploy.

## 7. Data model (dootd.db)

| Table | Key columns |
|---|---|
| `settings` | key, value (plain or encrypted blob). Holds `dashboard_domain`, the one-time password hash and expiry (`setup_password`), GitHub PAT*, CF token*, S3 endpoint/region/bucket/keys*, Cloudflare IP ranges |
| `users` | id (always 1), email, password_hash (argon2id) |
| `sessions` | id_hash (SHA-256 of the token), csrf, created_at, last_seen, ip, user_agent, setup (1 = signed in with the one-time password) |
| `apps` | name, type, repo, branch, domain (unique), port (unique), memory_max, cpu_max, pids_max, build_memory, build_timeout, created_at |
| `app_env` | app, name, value* |
| `deployments` | id, app, kind (deploy/rollback), status (queued/building/deploying/succeeded/failed), release_id, git_sha, error, created/started/finished_at |
| `releases` | app, id, git_sha, subject, branch, zig_version, run (JSON argv), health_path, created_at (only kept releases) |
| `app_state` | app, desired_state (running/stopped) |
| `backups` | id, app, kind, status (running/ok/failed), object_key, local_path, size, sha256, files, release_id, error, created_at, finished_at |
| `edge_certs` | hostname, zone_id, cf_cert_id, not_after, issued_at |
| `edge_zones` | zone_id, name, ssl_mode, aop_cert_id, aop_cert_serial, aop_active, checked_at |
| `metrics_1m` | scope (`_host`, `_dootd` or app), ts (minute), cpu, mem, mem_limit, swap, load1, disk_used, disk_total, pids, io_read, io_write, req, req_5xx (per minute), p50, p95 (ms, NULL = no requests), oom |
| `schema_migrations` | version |

`*` = encrypted with AES-256-GCM using `master.key`. `dootd.db` uses WAL mode, and dootd uses a single writer connection. (The unused `apps.path` and `releases.subdir` columns from the removed monorepo option stay empty.)

## 8. Install, first sign-in, updates

The whole SSH part is one command:

```bash
curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
```

`install.sh` asks no questions. It:
1. Checks Ubuntu ≥ 24.04, systemd, cgroup v2 and the CPU architecture, and changes nothing if one is missing.
2. Downloads `dootd-linux-<arch>` and `checksums.txt` from the latest GitHub release, verifies the SHA-256 and checks that the binary runs (`dootd version`). On a mismatch nothing is installed.
3. Installs `make` if it is missing (the only system package), and creates a 2 GB swapfile if the host has no swap (Zig builds can use a lot of memory).
4. Stops dootd if it is running, moves the new binary into place and runs `dootd setup-host`, which:
   - creates `/etc/dootd` (0700) and `/var/lib/dootd` (0711) and generates `master.key` (never overwritten);
   - writes the systemd unit embedded in the binary (`contrib/systemd/dootd.service`) and enables it;
   - opens `dootd.db` (running migrations) and stores a **new one-time password** (argon2id hash, valid 24 h);
   - starts dootd and prints the setup address `https://<public IPv4>` and the one-time password. If a dashboard domain is already set up, it prints that too.

Running the same command again is how dootd is **updated** (it installs the latest release and keeps every setting and app) and how access is **recovered** (forgotten password, broken dashboard domain): every run prints a fresh one-time password. `DOOTD_VERSION=vX.Y.Z` installs a specific release instead of the latest (e.g. a release candidate).

### 8.1 Setup address and one-time password

Before a dashboard domain exists there is no way to reach the dashboard through Cloudflare, so dootd has a **setup address**: `https://<server IP>`, served with a self-signed certificate (`/var/lib/dootd/setup/`, created on first start). The browser warns about the certificate once.

- **Setup is open** while no dashboard domain is ready, **or** while an unused, unexpired one-time password exists. A dashboard domain is *ready* once dootd holds its Origin CA certificate and AOP is enforced for its zone (§9.3).
- While setup is open, a connection from outside Cloudflare's ranges is let through the IP filter, marked as a *setup connection*, and gets the self-signed certificate without a client-certificate requirement. Every request on it goes to the dashboard, whatever its `Host`; **apps are never served on a setup connection**. Connections from Cloudflare addresses are handled exactly as before (§9).
- On a setup connection the client IP is the TCP peer; `CF-Connecting-IP` is ignored (it could be forged). CSRF checks compare `Origin` with `https://<Host>` of the request.
- While a dashboard domain is ready, the setup address accepts **only the one-time password**, never the admin password, so the admin password is never exposed outside Cloudflare.
- When setup closes, new setup connections are refused before TLS again, and a request still arriving on an open one gets a page linking to the dashboard domain.

**Signing in with the one-time password** (email left empty) creates a session marked `setup`. That session can only open *Set up your account*: the admin email and a new password (≥ 12 characters). Saving it replaces the admin credentials, consumes the one-time password, revokes every session and signs the user in normally. The first-run order is then: Settings → Cloudflare token → dashboard domain → (optional) GitHub token and bucket → add apps. The home page lists whatever is still missing.

### systemd unit (essentials)

```ini
[Unit]
StartLimitIntervalSec=0   # never give up; the delay grows to 60 s (RestartSteps)
[Service]
ExecStart=/usr/local/bin/dootd serve
Restart=always
RestartSec=2s
RestartSteps=5
RestartMaxDelaySec=60s
Delegate=yes              # dootd owns its cgroup subtree
KillMode=mixed            # SIGTERM to dootd only (it stops apps gracefully); SIGKILL leftovers after the timeout
TimeoutStopSec=45s
LimitNOFILE=65536
```

dootd runs as **root**. It needs this to bind :443, create users, manage cgroups and chown app directories. App processes never run as root.

## 9. Edge: TLS, Cloudflare-only access, and routing

### 9.1 Listener and IP filter
- dootd listens only on `:443`. There is no `:80`. Enable Cloudflare's "Always Use HTTPS" instead.
- A custom `net.Listener` checks the TCP peer address against Cloudflare's IPv4 and IPv6 ranges. **Connections from any other address are closed before the TLS handshake** (the only exception is the setup address while setup is open, §8.1).
- The ranges are fetched from `GET /client/v4/ips` at startup and every 24 h, with a list compiled into the binary as a fallback. The last good list is stored in `settings` and used on the next start. An empty or implausible list (e.g. a /4) is refused, so a bad response can neither lock Cloudflare out nor open the port to everyone.
- Rejected connections are counted (Settings → Cloudflare) and logged at most once a minute.

### 9.2 Origin certificates (Full strict)
- For every hostname (the dashboard plus each app domain), dootd generates an ECDSA P-256 key and a CSR locally. It then calls the Cloudflare **Origin CA** API (`POST /certificates`, `request_type=origin-ecc`, validity 5475 days).
- The API token authenticates this call. Legacy Origin CA service keys are deprecated and removed after 30 Sep 2026 ([Cloudflare docs](https://developers.cloudflare.com/fundamentals/api/get-started/ca-keys)).
- `tls.Config.GetConfigForClient` chooses the certificate by SNI. **Unknown SNI means the handshake fails.** TLS 1.2+ with h2 and http/1.1.
- Keys and certificates live in `certs/<host>/` (0600); the Cloudflare certificate ID is in `edge_certs`. The sync (at startup, every 6 h, after a change, and on *Sync now* in Settings) re-issues any certificate with less than 30 days left and then revokes the replaced one.
- The zone's SSL mode is **checked, not forced**. If it isn't `strict`, the dashboard shows a warning and a "Set to Full (strict)" button, because the setting affects every hostname in the zone.

### 9.3 Authenticated Origin Pulls (mTLS)
The IP filter alone only proves the traffic comes from *someone's* Cloudflare account. **Zone-level AOP with our own CA** proves it comes from *yours*:
- On first run, dootd creates a private CA (RSA 3072, 10 years) and a client certificate signed by it (RSA 2048, 5 years), stored in `aop/`. RSA is used because it is what Cloudflare documents for AOP uploads.
- For each zone it serves, dootd uploads the client certificate and key through the zone-level AOP API (`origin_tls_client_auth`) and turns the zone setting `tls_client_auth` **on** ([Cloudflare docs](https://developers.cloudflare.com/ssl/origin-configuration/authenticated-origin-pull/set-up/zone-level/)).
- The TLS config uses `ClientAuth: RequireAndVerifyClientCert` with dootd's CA as `ClientCAs`, **per zone and only once Cloudflare reports our certificate `active` and AOP enabled**. Enforcing earlier would break the site, because Cloudflare would not yet present the certificate.
- The enforcement state is saved in `edge_zones`, so after a restart it applies immediately, even if the Cloudflare API is unreachable. If a sync fails only because the API is down, enforcement stays on.
- If someone disables AOP or deletes our certificate in the Cloudflare dashboard, the next sync re-uploads and re-enables it (otherwise the origin would reject all traffic).
- dootd renews the client certificate 60 days before it expires: it uploads the new one, waits until it is active, then deletes the old one.
- AOP cannot be turned off, and the IP filter always stays on.

### 9.4 Router and proxy
- The `Host` header (lowercased, port removed) is looked up in an in-memory map `domain → app`. This map is rebuilt whenever apps change.
  - Dashboard domain → web UI handler.
  - App domain → that app's `httputil.ReverseProxy` → `http://127.0.0.1:<port>`.
  - Anything else → `404`.
  - `Host` must equal the TLS SNI, otherwise `421 Misdirected Request`. Without this, a client could use the TLS settings of a zone without AOP to reach an app in a zone with AOP.
- The proxy sets `X-Forwarded-For` and `X-Real-IP` from `CF-Connecting-IP` (replacing anything the client sent), `X-Forwarded-Proto: https` and `X-Forwarded-Host`, keeps the original `Host`, and flushes responses immediately (streaming/SSE).
- Timeouts: read header 10 s, idle 120 s, no overall write timeout (so long-polling and WebSockets work). Upstream dial timeout is 5 s.
- Deploying → 503 "Deploying" (`Retry-After: 3`); starting → 503 "Starting"; stopped/crashed/never deployed → 503 "App not running"; proxy error → 502. Pages carry `X-Dootd-Page` and `Cache-Control: no-store`.
- Each request updates in-memory counters per app (count, status class, latency histogram), which are used for metrics.

### 9.5 Cloudflare API token permissions
Scope: the zones you use, or all zones.
- Zone → Zone → Read
- Zone → DNS → Edit
- Zone → SSL and Certificates → Edit (Origin CA + AOP certs)
- Zone → Zone Settings → Edit (enable AOP, read and set the SSL mode)

When an app's domain is set, dootd finds the zone (longest suffix match), then creates or updates a **proxied** `A` record, plus an `AAAA` record if the server has IPv6. Next it issues the certificate and makes sure AOP is set up for the zone. It refuses to touch a conflicting `CNAME` or duplicate records and reports them instead.

The token is entered in Settings → Cloudflare, verified (the readable zones are listed) and stored sealed (`settings:cloudflare_token`).

### 9.6 Dashboard domain and public IPs
- The dashboard domain is set in Settings (it needs the Cloudflare token first) and stored in `settings:dashboard_domain`. Saving it re-routes immediately and starts a sync in the background: DNS record, Origin CA certificate, AOP for its zone. The settings page shows the progress. A domain used by an app is refused, and an app cannot use the dashboard domain. Replacing it removes the old domain's DNS records and revokes its certificate; sessions are per domain, so the user signs in again on the new one.
- The public IPv4 and IPv6 are always detected (`https://www.cloudflare.com/cdn-cgi/trace`, over IPv4 and IPv6); no IPv6 means no AAAA records. There is nothing to configure.

### 9.7 Testing without Cloudflare
`scripts/e2e/e2etool cfmock` is a fake Cloudflare API (its own Origin CA root; it exposes the uploaded AOP client certificate the way Cloudflare's edge would present it). The E2E scripts add `198.18.0.10` to `lo` and list `198.18.0.0/15` in the fake `/ips`, so requests from it count as Cloudflare, while requests to `127.0.0.1` use the setup address. `DOOTD_TEST_CLOUDFLARE_API` and `DOOTD_TEST_PUBLIC_IPV4/IPV6` point dootd at the fake (§18).

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
- dootd restarts, including updates, **restart all apps** (a few seconds of downtime). That's an accepted trade-off.

### 10.4 Logs
- stdout and stderr go through pipes into `logs/app.log`. Each line gets a timestamp and stream tag, and files rotate at 10 MB with 3 kept.
- There is also an in-memory ring buffer (last 1000 lines) per app. The dashboard streams live logs over **SSE** from this buffer.
- Build logs are stored per release, and the last 20 per app are kept.

## 11. Build and deploy pipeline

Only **one build runs at a time** across the whole server, so small VPSes aren't overloaded. A second deploy request waits in a queue, and the UI shows it as queued.

```
Deploy clicked
 1-2. Shallow clone (depth 1, single branch) into builds/<app>/<deployment-id>/,
      record the HEAD SHA, remove .git; release id = <UTC time>-<sha7>   [app keeps serving]
 3. Parse + validate dootd.toml in the repo root
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
- Release metadata (SHA, commit subject, zig version, `run`, `health_path`) is stored in the `releases` table, so rollbacks and restarts don't re-read `dootd.toml`.
- Whether each app should run after a restart is stored in `app_state` (stopping an app keeps it stopped across dootd restarts).
- **Redeploy** runs the same pipeline from the latest commit. **Rollback** runs only steps 8–13 using an earlier release, with no build.
- Deploys only ever happen when you click. There are no webhooks.

### 11.1 Zig toolchain manager
- The tarball URL and SHA-256 come from `https://ziglang.org/download/index.json`, using the entry for the exact version and `<arch>-linux`. The index is cached for 6 h.
- dootd downloads, checks the SHA-256, extracts to a temporary directory and then renames it into `toolchains/zig/<version>/`. A partly finished download can never be used.
- Toolchains stay until you delete them. The dashboard lists them, shows which apps use each one, and lets you delete unused ones.
- Future: minisign signature verification and community mirrors.

### 11.2 GitHub
- A fine-grained PAT with **Contents: Read-only** on the selected repos is enough. dootd validates it when it's saved and uses it to list repos in the "Add app" form.
- The PAT is only used as HTTP basic auth (`x-access-token`) for go-git, in memory, and **only for `github.com` URLs**. It is never written to disk in plain text or into `.git/config` (`.git` is deleted after the clone). It is stored sealed in `settings` under purpose `settings:github_token`.
- `file:///` repositories are accepted for development and the E2E tests.

## 12. Backups

Only app databases are backed up. The policy is fixed:

| Setting | Value |
|---|---|
| Schedule | every **3 h**, aligned to UTC (00:00, 03:00, …) |
| Also | before every deploy/rollback, and **Back up now** on the app page |
| Retention | **48 h**; the newest backup of each app is always kept |
| Local copies | the newest **2** archives per app stay on the server; while a bucket is configured, archives not uploaded yet are also kept until the retention ends |
| Target | any S3-compatible storage (R2, B2, MinIO, AWS) configured in Settings: endpoint, region (`auto` for R2), bucket, access key, secret |
| Bucket layout | one folder per app at the bucket root: `<app>/<UTC time>-<kind>-<id>.tar.zst` |

**S3 settings** are stored sealed (`settings:s3`) and only saved after a test upload, read-back and delete of `.dootd-connection-test` succeeds. The secret is never shown again; saving the form with an empty secret keeps the old one. Requests use path-style addressing and SigV4 (minio-go). Use a bucket (or an R2 API token scoped to one) for dootd only.

**What is backed up**: every file under `DATA_DIR` that starts with the SQLite header (`SQLite format 3\0`), whatever its name, including sub-folders; `-wal`, `-shm`, `-journal` files and `.pre-restore-*` folders are skipped. If there are none, nothing is recorded (a pre-deploy backup logs "nothing to back up yet").

**Snapshot**: dootd (root) opens each database with `busy_timeout=10000` and runs `VACUUM INTO '<staging>/…'`: a consistent, compacted copy taken while the app keeps writing. Each copy is checked with `PRAGMA quick_check`. If dootd's connection makes SQLite create `-wal`/`-shm` files, they are chowned back to the database owner so the app can still open the database.

**Archive**: `tar` (first entry `manifest.json`: version, app, kind, time, release, dootd version, and path/size/SHA-256 of every database; then `data/<path>`) compressed with zstd, written to `backups/local/<app>/<UTC time>-<kind>-<id>.tar.zst`. The archive's SHA-256 is stored in the `backups` row.

**Upload**: to `<app>/<same name>`. Scheduled and manual backups upload before returning; pre-deploy backups upload in the background so deploy downtime only includes the snapshot. 3 attempts with backoff; a failed upload is retried after every scheduled run (and when S3 settings are saved) until it succeeds or the retention ends.

**Retention**: after each upload dootd lists the app's folder, reads the time from each key and deletes objects older than 48 h except the newest. It is done by dootd itself, not bucket lifecycle rules, so it behaves the same everywhere. Rows without any copy left, and failed rows older than the retention, are removed.

**Restore** (Restore button next to a backup on the app page):
1. Block deployments of the app for the duration (refused if one is queued or running).
2. Take the local copy if its SHA-256 still matches, otherwise download from the bucket and check the SHA-256 against the row.
3. Unpack into staging, check every file against the manifest (size, SHA-256, safe paths) and run `quick_check`. **Any failure stops here; the app is not touched.**
4. Stop the app, move the current databases and their `-wal`/`-shm`/`-journal` files to `DATA_DIR/.pre-restore-<UTC time>/`, copy the restored files in (write + fsync + rename), give them to the app user.
5. Start the app again if it was running.

The last 2 `.pre-restore-*` folders are kept.

**Visibility**: the app page lists backups (time, kind, status, size, bucket/server, release, Restore button); the home page shows the last backup time per app, a *backup failed* badge when the latest backup failed or could not be uploaded (a backup whose upload is still being retried does not hide an earlier problem), and a warning when no bucket is set. Deleting an app keeps its backups unless "Also delete its backups" is ticked.

### 12.1 Restoring into a new app (new server, re-created app)

The app name is derived from the repository name (§14.1), so the backup folder of an app is predictable. When a bucket is configured, **Add app** lists the folders in the bucket, preselects the one with the app's name (if it exists) and offers "start empty". If a folder is chosen, dootd downloads that folder's newest archive after creating the app, verifies it (SHA-256 of every file against the manifest, `quick_check`) and unpacks it into the new, still empty `DATA_DIR`, owned by the app user, before the first deploy. If that fails, the app is still created and the error is shown; nothing else is touched.

Choosing a folder with a different name covers a renamed repository. From then on the app's own backups go to its own folder.

Moving to a new server is therefore: run the installer, enter the settings again (Cloudflare token, dashboard domain, GitHub token, bucket), and add each app, picking its folder. DNS records and certificates follow automatically. Shut the old server down first, so both don't write to the same folders.

## 13. Monitoring

| Scope | Source | Metrics |
|---|---|---|
| Server (`_host`) | `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `statfs(data root)` | CPU % of all cores, memory used (total − available) and total, swap used, load, disk used/size |
| dootd (`_dootd`) | `/proc/self/status`, `/proc/self/stat`, `/proc/self/task` | resident memory, CPU % of one core, threads |
| Each app | cgroup `cpu.stat`, `memory.current`, `memory.max` (from the spec), `pids.current`, `io.stat`, `memory.events` | CPU % of one core, memory and limit, processes, disk read/write bytes/s, OOM kills |
| Each app | edge counters (§9.4) | requests/min, 5xx/min, p50/p95 response time |

- `internal/metrics.Collector` samples every **10 s**. CPU and I/O are deltas of cumulative counters divided by the elapsed time; a counter that goes backwards (app re-created, route changed) counts as a reset.
- Latency percentiles come from the per-app histogram (buckets 5, 10, 25, 50, 100, 250, 500 ms, 1, 2.5, 5, 10 s, and above); the value is the upper bound of the bucket containing the percentile, so it is an estimate with bucket resolution. No requests → no value (a gap in the chart).
- The last hour of samples per scope is kept in memory (360 points each). At every minute boundary the samples of the previous minute are rolled up into `metrics_1m` (CPU, load and I/O time-weighted averages; memory, swap and processes maxima; requests, 5xx and OOM kills summed; percentiles from the summed histogram). If dootd restarts inside a minute, the partial minute is written at shutdown and merged with the rest (no requests are lost). Rows older than **7 days** are pruned at startup and hourly.
- **Charts**: `GET /charts?scope=&chart=&range=1h|24h|7d` returns an SVG (server-rendered in `internal/web/chart.go`, no JavaScript chart library). 1h uses the in-memory samples (plus rollups from before a restart); 24h and 7d use rollups, downsampled to at most 360 points. Points more than 2.5 steps apart are not joined; single points are drawn as dots. Pages add a changing `t=` parameter to chart URLs so the 10 s refresh fetches new images.
- **Pages**: *Monitoring* (server charts: CPU, memory, load, disk; a table of every app's current numbers; dootd's own CPU and memory against its 30 MB target) and a *Usage* section on each app page (CPU with its limit, memory with its limit, requests and 5xx, p50/p95).

### 13.1 Warnings (dashboard only in v1)
| Condition | Threshold (fixed) |
|---|---|
| Disk used on the data root | > 85 % |
| Server memory used | > 90 % |
| An app was OOM-killed in the last hour | always, with its limit |
| An app uses > 90 % of its memory limit | always |
| A certificate expires soon and was not renewed | < 14 days |
| App crashed, backup failed or only local, configuration changed, setup still missing a step | always (§8.1, §10, §12, §14) |

Email/webhook alerts are on the post-v1 backlog.

## 14. Dashboard and security

- Served on the dashboard domain through the same Cloudflare-only, AOP-protected edge, and on the setup address while setup is open (§8.1). The dashboard is the only interface.
- **UI**: server-rendered `html/template` pages and one CSS file, plus ~100 lines of plain JavaScript (`internal/web/static/app.js`) for live logs (Server-Sent Events), refreshing status sections every 5 s and confirmation prompts. Everything embedded with `go:embed`; every action is a normal form POST followed by a redirect, so the dashboard also works without JavaScript (except the live parts).
- **Admin account**: exactly one user (`users` row id 1). It is created from the one-time password (§8.1). The Account page changes the email and the password (each needs the current password). A forgotten password is recovered by re-running the installer.
- **Passwords**: argon2id (64 MiB, t=3, p=1), at least 12 characters. At most 2 hashes run at the same time so a burst of logins can't exhaust a 1 GB VPS. Unknown emails are checked against a dummy hash so timing doesn't reveal the email.
- **Rate limiting** (in memory): 5 failed sign-ins per client IP (`CF-Connecting-IP`) per 15 minutes → 429. Above 50 failures in 15 minutes overall, sign-ins are additionally limited to one attempt per 2 s, which slows a distributed attack without locking the owner out.
- **Sessions**: 32-byte random token in the cookie `__Host-dootd` (`Secure; HttpOnly; SameSite=Strict; Path=/`); only its SHA-256 is stored (`sessions`). Expiry: 7 days idle, 30 days total. The account page lists sessions and can sign out all others; a password change does that automatically.
- **CSRF**: every POST needs `Origin` (or, failing that, `Referer`) equal to `https://<Host of the request>` (the router guarantees that Host is the dashboard domain on Cloudflare connections), and signed-in POSTs also need the session's CSRF token in the `csrf` form field.
- **Headers**: `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, HSTS, `Cache-Control: no-store` on pages. Request bodies are capped at 256 KB.
- **Messages** after an action use a short-lived `__Host-dootd-flash` cookie (60 s).
- **Secrets**: tokens and env var values are never shown again after saving; env vars are listed by name only.
- **Pages**: Apps (host summary, warnings and missing setup steps, app table) · App (status and actions, releases with rollback, deployments, env vars, settings, backups, delete) · Add app · Deployment (live build log) · Logs (live app log) · Monitoring · Settings (Cloudflare token, dashboard domain, domains, zones with "Set Full (strict)", GitHub, backups bucket, Zig toolchains) · Account (email, password, sessions) · Set up your account (one-time password only).

### 14.1 Apps in the database
- The app **name is derived from the repository name**: lowercased, `_` and `.` become `-`. It must then be 2–24 characters of `a–z`, `0–9` and `-`, starting with a letter; otherwise the app is refused with the reason. One repository is one app, and `dootd.toml` is at its root.
- `apps` holds the configuration, `app_env` the sealed env vars (`app_env:<app>:<NAME>` as associated data). `internal/apps.Service` validates input (every problem reported at once), stores it and wires it into the deployer, supervisor and edge router at runtime; at startup it loads every stored app.
- Ports are assigned once, from 20001 upwards, and never change.
- Editing settings or env vars updates the stored config immediately; the running process keeps its old settings until a restart or deploy, and the dashboard says "restart needed" until then. Domain changes re-route immediately and clean up the old hostname.
- **Delete** (type the name to confirm): unregister (refused while a deployment is running) → stop → remove the cgroups → delete rows, releases, logs, caches and build folders → delete the DNS records that point at this server and revoke the Origin CA certificate → `userdel`. With "keep data" (default) `DATA_DIR` is moved to `/var/lib/dootd/deleted/<app>-<unix time>/data`, owned by root.

## 15. Updates

dootd never updates itself. Re-running the installer (§8) downloads and verifies the latest release, stops dootd (apps stop as on any restart), replaces the binary, and starts it again; migrations run at start (§7, Req 5). A release that cannot start shows its error in `journalctl -u dootd`, and `DOOTD_VERSION=<previous tag>` installs the previous release again. That works unless the new release added a migration, because an older binary refuses a newer schema (Req 5.5); releases are tested end to end on CI before they are tagged.

## 16. Resource budget (targets)

| Item | Target | Measured (Phase 6 E2E, Ubuntu 24.04 runner, 5 apps, idle) |
|---|---|---|
| dootd idle RSS | < 30 MB | 28 MB (11 MB anonymous, 18 MB mapped binary pages) |
| dootd idle CPU | < 1 % (10 s sampling, no busy loops) | 0.03 % |
| Proxy overhead | < 1 ms p50 added latency | not measured yet (soak test on a VPS) |
| Restart with 5 apps | a few seconds of downtime | measured in the E2E (`systemctl restart` → all apps healthy) |
| Binary size | < 30 MB | 21 MB (linux/amd64, stripped) |

To stay inside the memory budget dootd sets a soft heap limit of 12 MB (`debug.SetMemoryLimit`), `GOGC=50`, and returns freed memory to the kernel every 2 minutes. Setting `GOMEMLIMIT` or `GOGC` in the unit overrides this. The margin is small: most of the resident memory is the binary's own code pages, which grow with every dependency, so the Phase 6 E2E fails if RSS reaches 30 MB.

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
| D14 | Optional app path (monorepo subdirectory) | Lets one repo hold several apps; also lets E2E deploy `examples/*` straight from this repo (superseded by D36) |
| D15 | Local admin API on a Unix socket + `dootd ctl` | Deploys before the dashboard exists; permanent SSH fallback; no network exposure (superseded by D33) |
| D16 | Enforce AOP per zone, only after Cloudflare reports it active; persist the state | Never break a site during setup; never fail open after a restart |
| D17 | Host must equal SNI (421 otherwise) | Stops cross-zone requests from bypassing a zone's AOP |
| D18 | Edge sync is best effort and per host | One misconfigured domain does not block certificates or DNS for the others |
| D19 | The admin is created over SSH (`dootd ctl admin set-password`), never through a web setup page | A public first-run page could be claimed by whoever reaches it first (superseded by D34) |
| D20 | Config changes apply on restart/deploy, never silently to a running app | Predictable; the dashboard shows "restart needed" |
| D21 | Deleting an app keeps its data by default (moved aside) | Deletion is one click; losing a database should not be |
| D22 | Keep the newest archives on the server as well as in the bucket | Restores work without the bucket (and without a bucket at all), and an outage loses nothing |
| D23 | Verify a backup completely before stopping the app | A broken or tampered backup can never cost uptime or data |
| D24 | Backup interval and retention are settings (`[backups]`) | Lower RPO when needed; the tests use 30 s (superseded by D33: fixed policy) |
| D25 | Charts are SVG images rendered by dootd | No JS library, works with the strict CSP, cheap to refresh |
| D26 | Latency percentiles from a fixed histogram | Constant memory per app; bucket-level precision is enough to spot slow apps |
| D27 | Soft heap limit + periodic FreeOSMemory | Keeps idle RSS under 30 MB without hand-tuning allocations |
| D28 | The systemd unit is embedded in the binary and installed by `dootd setup-host` | One copy of the unit; `install.sh` stays small and the binary can repair it |
| D29 | Self-update keeps `dootd.prev`, and the previous binary is the start guard | A broken release cannot guard itself; the last binary that started can (superseded by D35) |
| D30 | Copy dootd.db before any migration, not only during updates | A rollback always has a schema the old binary can open; cheap for a small database (superseded by D35) |
| D31 | `dootd init --restore` rebuilds from the bucket and redeploys from git instead of copying releases | Backups stay small (databases only), and builds are reproducible from the pinned Zig (superseded by D37) |
| D32 | `init` and `init --restore` can run without a terminal (flags + environment variables) | The same code path is tested end to end on CI and can be scripted (superseded by D33) |
| D33 | The dashboard is the only interface: no admin CLI, no control socket, no config file, fixed backup policy and warning thresholds | One non-technical user; every extra way to do something is more to maintain and to get right |
| D34 | First sign-in with a one-time password printed by the installer, on a self-signed setup address that closes once the dashboard domain is ready | Setup before any domain exists without a public first-come claim page; the admin password is never exposed outside Cloudflare |
| D35 | Updates by re-running the installer; no self-update | The same single command for install, update and recovery; no update guard or rollback machinery |
| D36 | App name = repository name; `dootd.toml` at the repo root; no monorepo path | Predictable names and backup folders; one repo is one app |
| D37 | Bucket folder per app (`<app>/`), restore chosen at Add app; dootd's own settings are not backed up | Moving servers only needs the bucket and a few settings typed again; no master-key export (recovery kit) |

## 18. Test-only environment variables

The end-to-end scripts run dootd against fakes. These variables are read only by `internal/testenv`, are set only in a systemd drop-in by the scripts, and are not a user feature:

| Variable | Effect |
|---|---|
| `DOOTD_TEST_CLOUDFLARE_API` | Cloudflare API base URL (the fake from `e2etool cfmock`) |
| `DOOTD_TEST_PUBLIC_IPV4`, `DOOTD_TEST_PUBLIC_IPV6` | Skip IP detection (`off` = no IPv6) |
| `DOOTD_TEST_BACKUP_INTERVAL`, `DOOTD_TEST_BACKUP_RETENTION` | Short backup schedule (e.g. `30s`, `10m`) |
| `DOOTD_TEST_WARN_PERCENT` | Disk and memory warning threshold |

`install.sh` also reads `DOOTD_BASE_URL` (download from a local server) for the installer test.
