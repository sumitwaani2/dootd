# dootd Architecture

Reference for whoever maintains dootd. The rules for hosted apps are in [app-contract.md](app-contract.md); installing and using it is in the [README](../README.md).

## 1. What and why

A single static Go binary that hosts about 5 small server-rendered Zig/C + SQLite web apps on one Ubuntu 24.04+ VPS behind Cloudflare, for **one non-technical user**.

- **One SSH command, then only the dashboard.** The installer prints a one-time password; the admin account, tokens, dashboard domain, bucket, apps, env vars, deploys, logs, backups and restores are all in the browser. No admin CLI, no config file, no optional features; one way to do each thing (D33).
- **dootd never builds.** Each app's GitHub Actions workflow tests and builds it and publishes a GitHub release; dootd downloads, verifies and runs it (D38).
- **Cloudflare in front, Full (strict), origin reachable only through the user's own Cloudflare account** (IP filter + zone-level Authenticated Origin Pulls).
- **SQLite backups** to any S3-compatible bucket, RPO ≤ 3 h.
- **Small:** < 30 MB RSS idle, no other daemons.

Non-goals: several machines or users, containers or strong multi-tenant isolation (apps are trusted), zero-downtime deploys (stop-then-start is safe with one SQLite writer), deploys on push, non-Cloudflare setups, self-update, backups of dootd's own settings.

Assumptions: Ubuntu ≥ 24.04 (x86_64/arm64), systemd, cgroup v2, 1–2 GB RAM, ≤ 5 apps, public ports 22 and 443 only; 1 dashboard user, 1 GitHub PAT, 1 Cloudflare API token, 1 bucket.

## 2. Stack and code

Go, `CGO_ENABLED=0`, static `linux/amd64` + `linux/arm64`. stdlib for HTTP, TLS, reverse proxy, GitHub REST, tar/gzip; `modernc.org/sqlite` (pure Go) for dootd's own DB; `x/crypto/argon2`; `minio-go` for S3/R2; `klauspost/compress/zstd`; `BurntSushi/toml` for `dootd.toml`. Dashboard: `html/template`, one CSS file, ~60 lines of vanilla JS, all `go:embed`. Rule: add a module only when the stdlib would need ~300+ lines of risky code.

```
cmd/dootd/        serve (systemd), setup-host (installer), version
contrib/systemd/  dootd.service, embedded in the binary
internal/
  store/          SQLite (WAL, single writer), embedded migrations
  secrets/        master.key, AES-256-GCM seal/open with purpose as associated data
  auth/           argon2id, sessions, CSRF, rate limit, one-time password
  web/            dashboard handlers, templates, static, SSE, SVG charts
  apps/           app registry (apps + sealed env), create/update/delete, restore on add
  edge/           listener, IP filter, setup address, certificates, AOP, router, proxy, Cloudflare sync
  cloudflare/     minimal REST client
  github/         PAT check, repos, releases, asset downloads
  artifact/       tarball verification, safe unpack, ELF check
  manifest/       dootd.toml parsing
  deployer/       queue + pipeline, releases, rollback, history
  supervisor/     process lifecycle, restart policy, health check
  cgroup/ users/  cgroup v2 tree and limits; per-app system users
  backup/ s3/     snapshots, archives, upload, retention, restore, bucket folders
  metrics/ logs/  sampling + rollups; rotating logs + live tail
  layout/ hostinfo/ app/ buildinfo/
```

All components are goroutines in one process (the systemd unit), talking through Go interfaces. dootd runs as root (port 443, users, cgroups, chown); apps never do.

## 3. On disk

```
/usr/local/bin/dootd
/etc/dootd/master.key                  32 random bytes (hex), 0600, never overwritten
/etc/systemd/system/dootd.service
/var/lib/dootd/                        0711
  dootd.db                             all state and settings
  setup/{cert,key}.pem                 self-signed cert of the setup address
  certs/<host>/{cert,key}.pem          Origin CA certs (0600)
  aop/{ca,client}.{pem,key}            dootd's AOP CA (RSA 3072, 10 y) + client cert (RSA 2048, 5 y)
  downloads/<app>/<deployment>/        download + unpack workspace (emptied at start)
  apps/<app>/releases/<tag>/           unpacked release, root-owned, read-only for the app
  apps/<app>/current -> releases/<tag>
  apps/<app>/data/  tmp/               DATA_DIR, TMPDIR (0700, app user)
  apps/<app>/logs/app.log[.1-3]        runtime log (10 MB × 3)
  apps/<app>/logs/builds/<id>.log      deploy logs (last 20)
  backups/staging/  backups/local/<app>/   temp snapshots; newest archives
  deleted/<app>-<time>/data/           data kept when an app is deleted
```

| Table | Content |
|---|---|
| `settings` | `dashboard_domain`, `dashboard_reached`, `setup_password` (argon2id hash, expiry, used), GitHub token*, Cloudflare token*, `s3_config`*, Cloudflare IP ranges |
| `users` | id 1 only: email, argon2id hash |
| `sessions` | SHA-256 of the token, csrf, created/last seen, ip, user agent, `setup` flag |
| `apps`, `app_env` | name, repo, domain, port, limits; env values* |
| `app_state` | desired state (running/stopped) |
| `deployments`, `releases` | history (kind, status, release, error, times); kept releases (tag, sha256, run argv, health path) |
| `backups` | kind, status, object key, local path, size, sha256, release, error |
| `edge_certs`, `edge_zones` | per host: zone, Cloudflare cert id, expiry; per zone: SSL mode, AOP cert id/serial, AOP enforced |
| `metrics_1m` | 7 days of 1-minute rollups per scope (`_host`, `_dootd`, app) |

`*` sealed with AES-256-GCM (`master.key`), purpose-bound (`settings:cloudflare_token`, `app_env:<app>:<NAME>` …), never shown in the UI. Migrations are numbered, one transaction each; a failing one rolls back and dootd refuses to start; a schema newer than the binary is refused naming both versions. dootd refuses to start if `master.key` is group/other-accessible or malformed. Columns of removed features stay unused (`apps.type/branch/path/build_*`, `releases.git_sha/branch/subdir/zig_version`).

## 4. Install, setup address, updates

`install.sh` (the only SSH step) asks nothing and installs no packages: checks Ubuntu ≥ 24.04, systemd, cgroup v2, CPU; downloads `dootd-linux-<arch>` + `checksums.txt` from the latest release (or `DOOTD_VERSION=vX.Y.Z`), verifies SHA-256 and that the binary runs (changes nothing otherwise); stops dootd, installs the binary, runs `dootd setup-host`: creates the directories and `master.key`, writes and enables the embedded systemd unit (D28), runs migrations, stores a **new one-time password** (argon2id, 24 h), starts dootd and prints the setup address `https://<public IPv4>`, the password and the dashboard domain if any. Re-running it is the **update** (keeps everything; apps restart) and the **recovery** path (forgotten password, broken domain). dootd never updates itself (D35).

Unit essentials: `ExecStart=dootd serve`, `Restart=always` with growing delay (2 s → 60 s, never gives up), `Delegate=yes`, `KillMode=mixed`, `TimeoutStopSec=45s`, `LimitNOFILE=65536`.

**Setup address** (D34): before a dashboard domain works, the dashboard is served on `https://<server IP>` with the self-signed cert.
- Setup is **open** while the dashboard domain is not *ready*, or while an unexpired one-time password exists that has not been used to set up the account. The domain is *ready* when its Origin CA cert is installed, AOP for its zone is enforced or rolling out (§5.3), and either a request for it arrived through Cloudflare with dootd's AOP client cert (remembered per domain) or the zone is active and its SSL mode is known to be Full/Full (strict) (D41). The settings and home pages say what is missing.
- While open, non-Cloudflare connections pass the IP filter as *setup connections*: self-signed cert whatever the SNI, no client cert, **only the dashboard** (never apps), client IP = TCP peer (`CF-Connecting-IP` ignored), CSRF origin = `https://<Host>`.
- While the dashboard domain is ready, the setup address accepts only the one-time password, never the admin password.
- When setup closes, non-Cloudflare connections are refused before TLS again; a request on a still-open setup connection gets a page linking to the dashboard domain.
- Signing in with the one-time password (email empty) gives a `setup` session that can only set the admin email and a password (≥ 12 chars); saving consumes the password and revokes all sessions. First-run order: Cloudflare token → dashboard domain → GitHub token, bucket → apps; the home page lists what is missing.

## 5. Edge

### 5.1 Listener and IP filter
Only `:443` (no `:80`; the user enables Cloudflare's *Always Use HTTPS*). A custom listener closes connections from outside Cloudflare's ranges **before TLS** (except setup connections). Ranges come from `/client/v4/ips` at start and every 24 h, the last good list is saved, a compiled-in list is the fallback, and implausible lists are refused. Rejections are counted and logged at most once a minute. Errors `net/http` reports before a request (failed handshakes) are logged: the first at once, then a count per minute (D43). Timeouts: read header 10 s, idle 120 s, no write timeout.

### 5.2 Origin certificates (Full strict)
Per hostname (dashboard + each app domain): local ECDSA P-256 key + CSR → Cloudflare Origin CA (`origin-ecc`, 5475 days) with the API token. Chosen by SNI; **unknown SNI fails the handshake**; TLS 1.2+, h2 and http/1.1. Sync (at start, every 6 h, after changes, *Sync now*) re-issues certs with < 30 days left and revokes replaced ones. The zone SSL mode is checked, never changed automatically; the dashboard warns and offers *Set Full (strict)*.

### 5.3 Authenticated Origin Pulls
The IP filter only proves "some Cloudflare account"; zone-level AOP with dootd's own CA proves "yours" (D6). Per zone: upload the client cert + key (`origin_tls_client_auth`), turn zone AOP on, wait until Cloudflare reports the cert `active` (≤ 3 min per sync), then **wait 10 more minutes** for Cloudflare's edge to roll it out (D43; enforcing earlier gave 2–5 % 520s for ~5 minutes) before requiring `RequireAndVerifyClientCert` for hosts in that zone. The same wait follows any re-upload or re-enable; a timer syncs when it ends; during it no client cert is requested (verifying "if given" would reject Cloudflare's shared certificate). The enforced state is saved in `edge_zones` and applies at once after a restart, and stays on if the API is merely failing (D16). If someone deletes the cert or disables AOP in Cloudflare, the next sync restores it. The client cert is renewed 60 days before expiry (upload, wait active, delete the old one). AOP cannot be turned off.

### 5.4 Router and proxy
`Host` (lowercased, port removed) → dashboard, an app's `httputil.ReverseProxy` to `127.0.0.1:<port>`, or 404; `Host` must equal the SNI or 421 (D17). The proxy replaces `X-Forwarded-For`/`X-Real-IP` with `CF-Connecting-IP`, sets `X-Forwarded-Proto: https` and `X-Forwarded-Host`, keeps `Host`, flushes immediately (`FlushInterval: -1`; SSE, long-poll, WebSockets work), dials with a 5 s timeout. Deploying → 503 (`Retry-After: 3`), starting → 503, stopped/crashed/never deployed → 503, proxy error → 502; dootd pages carry `X-Dootd-Page` and `no-store`. Per-app counters (requests, status classes, latency histogram) feed monitoring.

### 5.5 Cloudflare token, DNS, domains
Token permissions: Zone Read, DNS Edit, SSL and Certificates Edit, Zone Settings Edit; saving it verifies it, lists readable zones and names missing permissions. For each domain: zone by longest suffix (a domain in no zone is refused when saved), proxied `A` (+ `AAAA` if the server has IPv6) created or updated, conflicting CNAMEs or duplicates reported, never touched. Edge sync is best effort per host (D18). Public IPv4/IPv6 are detected via `https://www.cloudflare.com/cdn-cgi/trace`. The dashboard domain (needs the token, can't be an app's domain) is routed at once and set up in the background; replacing it deletes the old DNS records and revokes its cert; sessions are per domain. Deleting an app or changing its domain cleans up the same way.

## 6. Apps and processes

- **Names** come from the repository name: lowercased, `_`/`.` → `-`, 2–24 chars `a-z0-9-`, starting with a letter (D36). One repo = one app. Ports are assigned once from 20001 up.
- **Users:** `dootd-<app>` (system, no home, `nologin`); `data/` and `tmp/` 0700 owned by it; releases root-owned; other apps' and dootd's files unreadable.
- **cgroups** (D2): dootd owns `system.slice/dootd.service/` (`Delegate=yes`), moves itself into `supervisor/`, apps run in `apps/<app>/` with `memory.max` (default 256M), `memory.swap.max=0`, `memory.zswap.max=0`, `cpu.max` (1 core), `pids.max` (256); no `memory.high` (D11); `RLIMIT_NOFILE` 4096 via prlimit. Processes start inside their cgroup (`CLONE_INTO_CGROUP`) with the app's uid/gid, `Setpgid`, `Pdeathsig=SIGKILL`, cwd = current release, a clean env (`PATH`, `LANG`, contract vars, user env). Killing = SIGKILL every PID in the cgroup until empty, **never `cgroup.kill`** (D10: after it was written once, later `CLONE_INTO_CGROUP` children died instantly). `memory.events` OOM kills are recorded.
- **Supervisor:** `stopped → starting → running → stopping`, plus `crashed`, `deploying`. Running = TCP connect then `GET health_path` 2xx/3xx within 30 s. Stop = SIGTERM, 10 s, kill the cgroup. Restarts with backoff 1 s → 60 s; 5 exits in 5 min → `crashed` until the user restarts. At start, apps with desired state `running` start one at a time. A dootd restart restarts all apps (seconds of downtime, accepted).
- **Logs:** stdout/stderr lines get a timestamp and stream tag → `app.log` (10 MB × 3) + a 1000-line ring buffer streamed to the dashboard over SSE; lines > 16 KB are split.
- **Config changes** (env, limits) are stored at once and apply on restart/deploy; the page says "restart needed" (D20). Env names `PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, `DOOTD_*` are reserved.
- **Delete** (type the name): refused while deploying; stop, remove cgroup, rows, releases, logs, DNS records and cert, user; `DATA_DIR` is kept in `deleted/` unless unticked (D21); backups are kept unless "also delete its backups" is ticked.

## 7. Releases and deploys

A release (D39, D40) has `app-linux-amd64.tar.gz`, `app-linux-arm64.tar.gz` (`dootd.toml` at the root, the binary, runtime files) and `checksums.txt`. Tags become directory names: `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`. The app page lists the 10 newest non-draft releases (`/repos/<o>/<r>/releases?per_page=10`, cached 1 min, latest preselected); dootd deploys **only on click** (D4). One deployment runs at a time host-wide; others queue.

```
1. Tag already kept in releases/<tag>/ → use it (skip 2–5)
2. GitHub API: release by tag → asset for this CPU + checksums.txt      [app keeps serving]
3. Download both (stored token; redirect to the signed asset URL without the token; size limit), check SHA-256
4. Unpack into downloads/ (no absolute paths, "..", devices, escaping symlinks; bounded size),
   validate dootd.toml, check `run` is an executable ELF for this CPU
5. Move to apps/<app>/releases/<tag>/ (root-owned)
── downtime (measured 60–220 ms plus the app's start) ──
6. Router: deploying (503)   7. Stop old   8. Pre-deploy backup (snapshot; upload in background)
9. Atomic swap of `current`   10. Wipe tmp/, start, health check
11a. Healthy → serve, succeeded, prune to 3 kept releases
11b. Unhealthy → stop, point `current` back, start the old release if it was running, delete the new one, failed
     (the DB is not restored; the UI offers the pre-deploy backup)
```

A failure in 2–5 never touches the running app, and errors name the cause (missing asset, checksum, no `checksums.txt`, wrong CPU, unsafe path, bad `dootd.toml`). **Rollback** switches to a kept release without downloading; deploying an older tag that isn't kept downloads it again; a kept release whose files are missing is downloaded again. After a crash or restart, unfinished deployments are marked failed and `downloads/` is emptied; a switch that started (6–11) always completes. Release metadata lives in `releases`, so restarts don't re-read `dootd.toml`. GitHub: a fine-grained PAT with *Contents: Read-only* (public repos work without), sent only to `api.github.com`.

## 8. Backups and restore

| Policy (fixed) | |
|---|---|
| Schedule | every 3 h aligned to UTC (00:00, 03:00 …), before every deploy/rollback, *Back up now* |
| Retention | 48 h, the newest backup of each app always kept (done by dootd, not bucket rules) |
| Local copies | newest 2 per app (D22); not-yet-uploaded ones kept until the retention ends |
| Bucket | `<app>/<UTC time>-<kind>-<id>.tar.zst` (D37); S3 settings sealed, saved only after a test upload/read/delete; empty secret keeps the old one; path-style SigV4 |

- **What:** every file under `DATA_DIR` starting with `SQLite format 3\0` (sub-folders included), skipping `-wal`/`-shm`/`-journal` and `.pre-restore-*`.
- **Snapshot:** `VACUUM INTO` with `busy_timeout=10000` while the app runs, then `PRAGMA quick_check`; sidecar files dootd creates are chowned back to the app user.
- **Archive:** tar (first `manifest.json`: app, kind, time, release, dootd version, path/size/SHA-256 per DB) + zstd; its SHA-256 goes into `backups`.
- **Upload:** 3 attempts with backoff; scheduled/manual uploads finish before returning, pre-deploy ones in the background; failed uploads are retried after every scheduled run and when S3 settings are saved.
- **Restore** (D23): refuse while a deployment is queued/running; take the local copy if its SHA-256 matches, else download and check; unpack to staging and verify every file (size, SHA-256, safe paths, `quick_check`) — **any failure stops here, the app untouched**; stop the app, move the current DBs and sidecars to `DATA_DIR/.pre-restore-<time>/` (unique per restore, last 2 kept), copy the restored files in (fsync, rename, chown), start again if it was running.
- **Add app** lists the bucket's folders, preselects the app's own name, and restores the newest archive of the chosen folder into the empty `DATA_DIR` before the first deploy (a failure leaves the app created with an error shown). Moving servers = install, re-enter the settings, add each app with its folder; shut the old server down first.
- **Badges:** app and home pages show *backup failed* when the latest backup failed or isn't uploaded (a retry in progress doesn't hide an earlier problem) and warn when no bucket is set. dootd's own settings are not backed up.

## 9. Monitoring

Every 10 s: host (`/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `statfs`), dootd itself (`/proc/self`), each app (cgroup `cpu.stat`, `memory.current`, `pids.current`, `io.stat`, `memory.events`) and proxy counters (requests, 5xx, p50/p95 from a fixed histogram 5 ms … 10 s, D26). CPU and I/O are counter deltas (a counter going backwards is a reset). 1 h of samples in memory (360 per scope); each minute is rolled up into `metrics_1m` (time-weighted averages, maxima, sums; a partial minute is written at shutdown and merged), pruned after 7 days. Charts are SVGs rendered by dootd (`/charts?scope=&chart=&range=1h|24h|7d`, ≤ 360 points, gaps not joined; D25). Warnings (dashboard only): disk > 85 %, memory > 90 %, OOM kill in the last hour, app > 90 % of its memory limit, crashed, backup failed or local only, cert expiring < 14 days, restart needed, setup step missing.

## 10. Dashboard and security

- Server-rendered pages; every action is a form POST + redirect (works without JS); `app.js` only does live logs (SSE), 5 s refresh of status sections and confirmations. Pages: Apps (home), App, Add app, Deployment, Logs, Monitoring, Settings, Account, Set up your account.
- **Passwords:** argon2id (64 MiB, t=3, p=1), ≥ 12 chars, at most 2 hashes at a time; unknown emails hashed against a dummy. Email and password changes need the current password; a password change signs out other sessions; a forgotten password → re-run the installer.
- **Sessions:** 32-byte token in `__Host-dootd` (`Secure; HttpOnly; SameSite=Strict; Path=/`), only its SHA-256 stored; 7 days idle, 30 days total; *sign out all others*.
- **Rate limit** (in memory, reset by a restart): 5 failures per client IP per 15 min → 429, even for the right password; above 50 failures overall, one attempt per 2 s. Failed current-password checks count too.
- **CSRF:** every POST needs `Origin` (or `Referer`) = `https://<Host>` and, when signed in, the session's token. Bodies ≤ 256 KB.
- **Headers:** strict CSP (`default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`), `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: same-origin`, HSTS, `no-store`. Flash messages use a 60 s cookie.
- Times are stored and scheduled in UTC and shown in the server's timezone with its name (D42).

## 11. Budget (measured on the real VPS, 1 vCPU, 5 apps)

| | Target | Measured |
|---|---|---|
| Idle RSS | < 30 MB | 26.6 MB (soft heap limit 12 MB, `GOGC=50`, memory returned every 2 min; D27) |
| Idle CPU | < 1 % | 0.18 % |
| Proxy overhead | < 1 ms p50 | 0.6 ms |
| Binary | < 30 MB | 18 MB |
| Reboot to serving | seconds | ~10 s after boot; update by the installer: 3 s |

Most of the RSS is the binary's own code pages, so every new dependency costs memory.

## 12. Changing dootd

- `make lint` (gofmt, vet, staticcheck) and `make test` (`-race`) run in CI on every PR with amd64/arm64 builds. Unit tests cover the risky pure parts: secrets, store and migrations, the one-time password, `dootd.toml`, tarball unpacking, backup archives, app names, the health check, the setup address, the GitHub client and page rendering.
- There is **no automated end-to-end suite** (D44). Anything touching processes, cgroups, deploys, backups or the edge must be tried on a real Ubuntu VPS with a real Cloudflare zone: tag a pre-release (`vX.Y.Z-rc1`, published but not "latest"), install it with `DOOTD_VERSION=vX.Y.Z-rc1` in front of `bash` in the install command, and go through the affected flows in the dashboard. Useful checks: install/update, setup address closes, deploy + rollback + a broken release, restart/stop leaves no processes, backup + restore, direct IP access refused, no 520/521 through Cloudflare.
- Release: push a `vX.Y.Z` tag; the release workflow lints, tests, builds both binaries, writes `checksums.txt` and publishes them with `install.sh`. Tags with `-` are pre-releases.
- A schema change is a new numbered migration. An older binary then refuses the database, so going back to an earlier release after such an update needs a copy of `dootd.db` from before it; dootd doesn't make one.

## 13. Decisions

| # | Decision | Reason |
|---|---|---|
| D1 | Go for dootd | stdlib TLS/proxy/HTTP, pure-Go SQLite and S3 |
| D2 | No containers: users + cgroups | trusted single-owner apps; light |
| D3 | Stop-then-start deploys | safe with one SQLite writer; sub-second downtime |
| D4 | Deploy only on click | fewer moving parts |
| D5 | Cloudflare Origin CA, no ACME | all domains on Cloudflare; 15-year certs; no port 80 |
| D6 | Zone AOP with own CA + IP filter | the IP filter alone lets any Cloudflare customer in |
| D7 | Snapshot backups every 3 h | RPO 3 h is enough; far simpler than replication |
| D9 | Server-rendered HTML, tiny JS, no frontend build | strict CSP, nothing to vendor |
| D10 | Kill cgroups by PID, not `cgroup.kill` | `cgroup.kill` made later `CLONE_INTO_CGROUP` children die on Ubuntu kernels |
| D11 | No `memory.high`; no swap/zswap for apps | clean OOM + restart instead of a stalled app |
| D16 | Enforce AOP per zone only once active; persist it | never break a site during setup; never fail open after a restart |
| D17 | `Host` must equal SNI (421) | stops cross-zone requests bypassing a zone's AOP |
| D18 | Edge sync best effort per host | one broken domain doesn't block the others |
| D20 | Config changes apply on restart/deploy | predictable; "restart needed" shown |
| D21 | Deleting an app keeps its data by default | deletion is one click; data loss shouldn't be |
| D22 | Newest archives also kept on the server | restores work without the bucket; outages lose nothing |
| D23 | Verify a backup fully before stopping the app | a bad backup never costs uptime or data |
| D25 | SVG charts rendered by dootd | no JS library; works with the CSP |
| D26 | Latency percentiles from a fixed histogram | constant memory; bucket precision is enough |
| D27 | Soft heap limit + periodic FreeOSMemory | keeps idle RSS < 30 MB |
| D28 | systemd unit embedded, installed by `setup-host` | one copy; the binary can repair it |
| D33 | The dashboard is the only interface | one non-technical user; each extra way is more to maintain |
| D34 | First sign-in with an installer-printed one-time password on a self-signed setup address that closes | no first-come public claim page; admin password never used outside Cloudflare |
| D35 | Update = re-run the installer | one command for install, update, recovery |
| D36 | App name = repository name; `dootd.toml` at the root | predictable names and backup folders |
| D37 | Bucket folder per app; restore chosen at Add app; no settings backup | moving servers needs only the bucket and a few settings |
| D38 | dootd never builds; apps ship as GitHub releases | builds don't fit next to apps on 1 GB; removes toolchains and git from the server |
| D39 | Releases, not Actions artifacts | permanent, versioned; rollback = older tag |
| D40 | Fixed asset names + `checksums.txt`, static musl | no naming rules; no libc dependency; catches bad downloads |
| D41 | Dashboard domain ready after a visit through Cloudflare with AOP, or an active zone in Full/Full (strict) | a Flexible zone once locked the user out; a visit is the strongest proof |
| D42 | UTC everywhere, shown in server time with the zone name | the server's timezone never changes behaviour |
| D43 | Require AOP 10 min after Cloudflare reports it active (and after re-uploads); log handshake errors; check the zone when a domain is saved | Cloudflare's edge rolls certs out over minutes (2–5 % 520s otherwise); the cause had been invisible; typos failed silently |
| D44 | No automated end-to-end suite; unit tests + a real-VPS check for risky changes | single-user tool that rarely changes; the real VPS found what the fakes couldn't |
