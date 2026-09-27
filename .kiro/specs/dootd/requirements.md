# Requirements Document

## Introduction

dootd is a self-contained, single-binary PaaS for hosting a few (up to 5) small server-rendered Zig/C web apps that use SQLite, all on one Ubuntu 24.04+ VPS behind Cloudflare. The user SSHes in once to install it. Everything after that happens in an auth-protected web dashboard: adding apps from GitHub, setting env vars, deploying by click, TLS, host-based routing, cgroup isolation, SQLite backups to S3-compatible storage, logs and monitoring. It uses no Docker, no Kubernetes and no external reverse proxy.

Scope: 1 machine, 1 dashboard user, 1 GitHub account (PAT), 1 Cloudflare account (API token), 1 S3 bucket, 1 domain per app, plus 1 dashboard subdomain.

Reference docs: #[[file:docs/architecture.md]] and #[[file:docs/app-contract.md]]

## Glossary

- **dootd**: the single binary / systemd service described here.
- **App**: a user Zig/C web application that follows the app contract.
- **Release**: an immutable build output of one git commit of an app.
- **DATA_DIR**: the per-app persistent directory that holds its SQLite databases.
- **AOP**: Cloudflare Authenticated Origin Pulls (mTLS from Cloudflare to the origin).
- **Origin CA certificate**: a Cloudflare-issued certificate that Cloudflare trusts in Full (strict) mode.

## Requirements

### Requirement 1: Packaging and resource footprint

**User Story:** As the operator, I want dootd to be one small static binary that uses very little memory, so it runs on a cheap 1 GB VPS alongside my apps.

#### Acceptance Criteria

1. THE dootd SHALL be distributed as a single statically linked binary for `linux/amd64` and `linux/arm64`, with no runtime dependency on a C library, Docker, a container runtime or an external reverse proxy.
2. THE dootd SHALL report its version, git commit and build date through a `dootd version` command.
3. WHILE idle with 5 apps configured, THE dootd SHALL use less than 30 MB of resident memory and less than 1% CPU on average.
4. WHEN a git tag matching `v*` is pushed, THE release pipeline SHALL publish the binaries for both architectures, a `checksums.txt` of SHA-256 sums, and `install.sh` as GitHub Release assets.
5. WHEN a pull request is opened or updated, THE CI pipeline SHALL run formatting, vet, static analysis and tests, and SHALL build both architectures with `CGO_ENABLED=0`.

### Requirement 2: Installation and bootstrap

**User Story:** As a non-technical operator, I want to SSH in once and run one install command plus one setup command, so I never need SSH again for routine operations.

#### Acceptance Criteria

1. WHEN `install.sh` runs on a host that is not Ubuntu 24.04+, lacks systemd, lacks cgroup v2, or has an unsupported CPU architecture, THE installer SHALL stop with a clear message and change nothing.
2. WHEN `install.sh` runs on a supported host, THE installer SHALL download the binary for the host architecture, verify its SHA-256 against `checksums.txt`, and install it to `/usr/local/bin/dootd`.
3. IF the downloaded checksum does not match, THEN THE installer SHALL delete the download and stop with an error.
4. WHEN the full installer runs, THE installer SHALL create `/etc/dootd` and `/var/lib/dootd`, generate the master key, install the systemd unit, install `make`, and offer to create a swapfile (if there is no swap) and to enable ufw allowing only the SSH port and 443.
5. WHEN `dootd init` runs, THE dootd SHALL ask for the admin email and password, the dashboard domain and the Cloudflare API token, auto-detect the public IPs, then create the proxied DNS record, the Origin CA certificate and the AOP setup for the dashboard, and start the service.
6. WHEN `dootd reset-password` runs as root, THE dootd SHALL set a new admin password and revoke all sessions.

### Requirement 3: Secrets at rest

**User Story:** As the operator, I want tokens and env vars encrypted on disk, so a leaked database file alone does not expose them.

#### Acceptance Criteria

1. THE dootd SHALL encrypt the GitHub PAT, the Cloudflare token, the S3 credentials and all app env var values with AES-256-GCM, using a 32-byte master key stored at `/etc/dootd/master.key` with mode 0600.
2. WHEN no master key exists at startup, THE dootd SHALL generate one from a cryptographically secure random source and create the file so that it never overwrites an existing key.
3. IF the master key file is readable or writable by group or others, or has the wrong length or format, THEN THE dootd SHALL refuse to start and state the reason.
4. THE dootd SHALL bind each ciphertext to its purpose (associated data) so that a ciphertext copied to another field fails to decrypt.
5. THE dashboard SHALL never display a stored secret in full. Secrets can only be replaced.

### Requirement 4: State storage

**User Story:** As the maintainer, I want dootd's state in a single SQLite file with versioned migrations, so upgrades are safe and state is easy to back up.

#### Acceptance Criteria

1. THE dootd SHALL store its state in `/var/lib/dootd/dootd.db` using SQLite in WAL mode, with foreign keys enabled and a busy timeout.
2. THE dootd SHALL serialize all writes through a single writer connection.
3. WHEN dootd opens the database, THE dootd SHALL apply every pending embedded migration in ascending version order, each in its own transaction, and record each applied version.
4. IF a migration fails, THEN THE dootd SHALL roll back that migration, keep earlier migrations, and refuse to start.
5. IF the database schema version is newer than the binary knows, THEN THE dootd SHALL refuse to start with an error naming both versions.

### Requirement 5: Authentication

**User Story:** As the only user, I want an email/password login on the dashboard, so nobody else can control my server.

#### Acceptance Criteria

1. THE dootd SHALL serve the dashboard only on the configured dashboard domain.
2. THE dootd SHALL store the password as an argon2id hash.
3. WHEN valid credentials are submitted, THE dootd SHALL create a session with a 32-byte random ID, store only its hash, and set a `__Host-` cookie with `Secure`, `HttpOnly` and `SameSite=Strict`.
4. THE dootd SHALL expire sessions after 7 days of inactivity or 30 days in total.
5. IF more than 5 failed logins come from one client IP within 15 minutes, THEN THE dootd SHALL reject further attempts from that IP until the window passes.
6. THE dootd SHALL require a valid CSRF token and a matching `Origin` on every state-changing request.

### Requirement 6: Integrations settings

**User Story:** As the operator, I want to enter my GitHub PAT, Cloudflare token and S3 details once, at account level, so every app can use them.

#### Acceptance Criteria

1. WHEN a GitHub PAT is saved, THE dootd SHALL validate it against the GitHub API and show which repositories it can read.
2. WHEN a Cloudflare token is saved, THE dootd SHALL verify it and list any missing permissions from: Zone Read, DNS Edit, SSL and Certificates Edit, Zone Settings Edit.
3. WHEN S3 settings are saved, THE dootd SHALL perform a test upload and delete, and report the result.

### Requirement 7: App management

**User Story:** As the operator, I want to add an app by choosing a repo, branch, type, domain and env vars, so it's ready to deploy.

#### Acceptance Criteria

1. WHEN an app is added, THE dootd SHALL require a unique name, a type (`zig` or `c`), a GitHub repo, a branch, and exactly one domain not used by another app or by the dashboard.
2. WHEN an app is added, THE dootd SHALL create a dedicated system user, its directories and cgroup, assign a stable port, create or update a proxied DNS record, and obtain an Origin CA certificate for the domain.
3. WHEN env vars are changed, THE dootd SHALL store them encrypted and indicate that a restart is required. It SHALL reject reserved names (`PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, `DOOTD_*`).
4. WHEN an app is deleted, THE dootd SHALL stop it and remove its user, cgroup, releases and routing, and SHALL ask whether to keep or delete its DATA_DIR and remote backups.
5. THE dootd SHALL support at least 5 apps on one host.

### Requirement 8: Build with pinned toolchain

**User Story:** As a Zig/C developer, I want each app built with exactly the Zig version it pins, so builds are reproducible.

#### Acceptance Criteria

1. WHEN a build starts, THE dootd SHALL read `dootd.toml` from the repo root and validate `contract`, `zig_version` and `run`, reporting every problem clearly.
2. WHEN the pinned Zig version is not cached, THE dootd SHALL download it via `ziglang.org/download/index.json`, verify its SHA-256, and install it atomically.
3. IF the pinned version does not exist or its checksum does not match, THEN THE dootd SHALL fail the build without affecting the running release.
4. THE dootd SHALL run the build as the app's user, in a build cgroup with a memory limit (default 1 GB), a lower CPU weight and a timeout (default 15 minutes), with the pinned `zig` first on `PATH` and `CC="zig cc"` and `CXX="zig c++"` set.
5. THE dootd SHALL run at most one build at a time on the host and queue any others.
6. THE dootd SHALL stream the build log live to the dashboard and keep the last 20 build logs per app.

### Requirement 9: Deploy and rollback

**User Story:** As the operator, I want deploys to happen only when I click, and a failed deploy to never leave my app down, so shipping is safe.

#### Acceptance Criteria

1. THE dootd SHALL deploy only when the user clicks Deploy, Redeploy or Rollback. It SHALL never deploy on git push.
2. WHEN a deploy is triggered, THE dootd SHALL clone the branch HEAD and build it while the current release keeps serving.
3. WHEN the build succeeds, THE dootd SHALL show a 503 "deploying" page, stop the old process, take a pre-deploy backup, switch to the new release, start it and run the health check.
4. IF the new release fails its health check within 30 seconds, THEN THE dootd SHALL stop it, restore and start the previous release, and mark the deploy failed. It SHALL leave the database as is and offer to restore the pre-deploy backup.
5. THE dootd SHALL keep the last 3 releases and allow rollback to any of them without rebuilding.
6. THE dootd SHALL keep a deploy history with commit SHA, status, duration and error.

### Requirement 10: Runtime supervision and isolation

**User Story:** As the operator, I want each app isolated with resource limits and restarted automatically when it crashes, so one misbehaving app can't take down the server.

#### Acceptance Criteria

1. THE dootd SHALL run each app as its own unprivileged system user inside its own cgroup v2, with `memory.max` (default 256 MB), `cpu.max` (default 1 core) and `pids.max` (default 256).
2. THE dootd SHALL provide the contract environment (`PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, `DOOTD_*`) plus the user's env vars.
3. WHEN an app is stopped, THE dootd SHALL send SIGTERM, wait 10 seconds, then kill every process left in the app's cgroup.
4. WHEN an app exits unexpectedly, THE dootd SHALL restart it with exponential backoff from 1 s up to 60 s.
5. IF an app exits 5 times within 5 minutes, THEN THE dootd SHALL mark it crashed and stop restarting it until the user restarts it.
6. WHEN an app is OOM-killed, THE dootd SHALL record the event and show it in the dashboard.
7. WHEN dootd starts, THE dootd SHALL start every app whose desired state is running, one at a time.

### Requirement 11: Edge TLS and Cloudflare-only access

**User Story:** As the operator, I want Full (strict) TLS and my server reachable only through my own Cloudflare account, so nobody can bypass Cloudflare.

#### Acceptance Criteria

1. THE dootd SHALL listen publicly on port 443 only.
2. WHEN a TCP connection comes from an address outside Cloudflare's published IP ranges, THE dootd SHALL close it before the TLS handshake.
3. THE dootd SHALL refresh Cloudflare's IP ranges every 24 hours and fall back to the last known good list or the built-in list.
4. THE dootd SHALL serve an Origin CA certificate for each configured hostname, choose it by SNI, fail the handshake for unknown SNI, and renew certificates with less than 30 days of validity.
5. WHERE AOP is enabled for a zone (the default), THE dootd SHALL upload a client certificate signed by its own private CA, enable zone-level AOP, and require a valid client certificate from that CA.
6. IF a zone's SSL mode is not Full (strict), THEN THE dashboard SHALL show a warning with a one-click fix, and dootd SHALL NOT change the mode automatically.

### Requirement 12: Routing and proxy

**User Story:** As the operator, I want requests routed to the right app by domain, with correct client info, so apps behave normally behind dootd.

#### Acceptance Criteria

1. WHEN a request arrives, THE dootd SHALL route it by its `Host` header to the dashboard, to the app that owns the domain, or to a 404.
2. THE dootd SHALL proxy app traffic to `127.0.0.1:$PORT`, set `X-Forwarded-For` and `X-Real-IP` from `CF-Connecting-IP`, set `X-Forwarded-Proto: https`, and support WebSockets and long-polling.
3. WHILE an app is deploying or stopped, THE dootd SHALL return a 503 page. WHEN an app is unreachable unexpectedly, it SHALL return a 502 page.

### Requirement 13: Logs

**User Story:** As the operator, I want live and historical app logs in the dashboard, so I can debug without SSH.

#### Acceptance Criteria

1. THE dootd SHALL capture each app's stdout and stderr with timestamps into files that rotate at 10 MB, keeping 3.
2. THE dashboard SHALL stream live logs and show recent history (at least the last 1000 lines) per app.

### Requirement 14: Backups

**User Story:** As the operator, I want my SQLite databases backed up automatically to R2/S3, so I lose at most 3 hours of data.

#### Acceptance Criteria

1. THE dootd SHALL back up every SQLite database in each app's DATA_DIR every 3 hours, before every deploy or rollback, and on demand.
2. THE dootd SHALL detect SQLite files by their header, take a consistent online snapshot with `VACUUM INTO`, and verify it with `quick_check` before upload.
3. THE dootd SHALL upload each backup as a zstd-compressed tar with a manifest of SHA-256 sums, retrying up to 3 times.
4. THE dootd SHALL delete remote backups older than 48 hours, always keeping each app's newest backup.
5. IF a backup fails, THEN THE dashboard SHALL show a failure badge on the app and on the home page.
6. THE dootd SHALL back up its own `dootd.db` daily and offer a downloadable recovery kit containing the master key.

### Requirement 15: Restore

**User Story:** As the operator, I want to restore any backup with one click, so recovering from a bad migration or data loss is easy.

#### Acceptance Criteria

1. WHEN a restore is requested, THE dootd SHALL download and verify the backup, stop the app, move the current database files (including `-wal` and `-shm`) into a `.pre-restore-<timestamp>` folder, put the restored files in place with correct ownership, and start the app.
2. IF verification fails, THEN THE dootd SHALL abort the restore without stopping the app.
3. THE dootd SHALL keep the last 2 pre-restore folders.

### Requirement 16: Monitoring

**User Story:** As the operator, I want host and per-app resource usage and request stats, so I can see problems at a glance.

#### Acceptance Criteria

1. THE dootd SHALL sample host CPU, memory, swap, load and disk, and per-app CPU, memory, pids, I/O, OOM count, restarts, request rate, status classes and p50/p95 latency, every 10 seconds.
2. THE dootd SHALL keep 1 hour of raw samples in memory and 7 days of 1-minute rollups in the database.
3. THE dashboard SHALL render these as charts without a JavaScript chart library.
4. WHEN disk usage exceeds 85%, memory exceeds 90%, an app crashes or is OOM-killed, a backup fails, or a certificate is about to expire, THE dashboard SHALL show a warning.

### Requirement 17: Self-update

**User Story:** As the operator, I want to update dootd from the dashboard, so I never need SSH to upgrade.

#### Acceptance Criteria

1. WHEN the user checks for updates, THE dootd SHALL query the latest GitHub release and show whether it is newer.
2. WHEN the user applies an update, THE dootd SHALL download the binary, verify its SHA-256 against `checksums.txt`, replace itself atomically keeping `dootd.prev`, and restart through systemd.
3. WHEN an updated binary starts with pending database migrations, THE dootd SHALL save a copy of `dootd.db` before applying them.
4. IF the new binary fails to start 3 times, THEN THE system SHALL restore `dootd.prev` together with the pre-update database copy.
