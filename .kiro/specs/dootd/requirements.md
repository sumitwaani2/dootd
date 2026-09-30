# Requirements Document

## Introduction

dootd is a self-contained, single-binary PaaS for hosting a few (3–5) small server-rendered Zig/C web apps that use SQLite, on one Ubuntu 24.04+ VPS behind Cloudflare. It is an internal tool for **one non-technical user**. The user runs **one command over SSH, once**; it prints a one-time password and an address. Everything after that happens in the web dashboard: the admin email and password, the Cloudflare and GitHub tokens, the dashboard's own domain, the backup bucket, adding apps, env vars, deploys, logs, monitoring, backups and restores.

Design rule: **one way to do each thing.** No CLI for day-to-day work, no config file to edit, no optional features that need their own maintenance. Correctness and ease beat features.

Scope: 1 machine, 1 dashboard user, 1 GitHub account (PAT), 1 Cloudflare account (API token), 1 S3 bucket, 1 domain per app, plus 1 dashboard domain.

Reference docs: #[[file:docs/architecture.md]] and #[[file:docs/app-contract.md]]

## Glossary

- **dootd**: the single binary / systemd service described here.
- **Installer**: `install.sh`, the one command run over SSH.
- **One-time password**: a random password printed by the installer; it signs in once and expires after 24 hours.
- **Setup address**: `https://<server IP>`, served with a self-signed certificate while setup is open.
- **App**: a user Zig/C web application that follows the app contract. Its name is derived from its GitHub repository name.
- **Release**: an immutable build output of one git commit of an app.
- **DATA_DIR**: the per-app persistent directory that holds its SQLite databases.
- **Backup folder**: the folder `<app name>/` at the root of the bucket that holds an app's backups.
- **AOP**: Cloudflare Authenticated Origin Pulls (mTLS from Cloudflare to the origin).
- **Origin CA certificate**: a Cloudflare-issued certificate that Cloudflare trusts in Full (strict) mode.

## Requirements

### Requirement 1: Packaging and resource footprint

**User Story:** As the operator, I want dootd to be one small static binary that uses very little memory, so it runs on a cheap 1 GB VPS alongside my apps.

#### Acceptance Criteria

1. THE dootd SHALL be distributed as a single statically linked binary for `linux/amd64` and `linux/arm64`, with no runtime dependency on a C library, Docker, a container runtime or an external reverse proxy.
2. THE dootd SHALL report its version, git commit and build date through `dootd version` (used by the installer and CI). Its only other commands, `serve` and `setup-host`, are run by systemd and the installer and are not part of the user's workflow.
3. WHILE idle with 5 apps configured, THE dootd SHALL use less than 30 MB of resident memory and less than 1% CPU on average.
4. WHEN a git tag matching `v*` is pushed, THE release pipeline SHALL publish the binaries for both architectures, a `checksums.txt` of SHA-256 sums, and `install.sh` as GitHub Release assets.
5. WHEN a pull request is opened or updated, THE CI pipeline SHALL run formatting, vet, static analysis and tests, and SHALL build both architectures with `CGO_ENABLED=0`.

### Requirement 2: Install, first sign-in and recovery

**User Story:** As a non-technical operator, I want to SSH in once and run one command that prints how to sign in, so that everything else happens in the browser.

#### Acceptance Criteria

1. WHEN the installer runs on a host that is not Ubuntu 24.04+, lacks systemd, lacks cgroup v2, or has an unsupported CPU architecture, THE installer SHALL stop with a clear message and change nothing.
2. WHEN the installer runs on a supported host, THE installer SHALL download the binary for the host architecture, verify its SHA-256 against `checksums.txt`, and install it to `/usr/local/bin/dootd`. IF the checksum does not match, THEN it SHALL delete the download, change nothing and stop with an error.
3. THE installer SHALL ask no questions. It SHALL create `/etc/dootd` and `/var/lib/dootd`, generate the master key if missing, install `make` if missing, create a 2 GB swapfile if the host has no swap, install and enable the systemd unit, start dootd, and print the setup address and a new one-time password.
4. THE dootd SHALL store only an argon2id hash of the one-time password, and the password SHALL stop working after one successful sign-in or after 24 hours, whichever comes first.
5. WHEN the user signs in with the one-time password, THE dashboard SHALL allow nothing but setting the admin email and a new password (at least 12 characters). Saving them SHALL consume the one-time password and revoke every other session.
6. WHEN the installer runs again on a server that already has dootd, THE installer SHALL replace the binary with the latest release (the update path), keep all data and settings, and print a new one-time password. This is also the recovery path for a forgotten password or an unreachable dashboard domain.

### Requirement 3: Setup address

**User Story:** As the operator, I want to reach the dashboard before any domain or Cloudflare setup exists, without that opening a permanent back door.

#### Acceptance Criteria

1. WHILE setup is open, THE dootd SHALL accept connections on :443 from any address that is not a Cloudflare address and serve them the dashboard with a self-signed certificate. The dashboard is the only thing reachable this way; app domains SHALL never be served on such a connection.
2. Setup SHALL be open WHILE no dashboard domain is ready, OR WHILE an unused, unexpired one-time password exists. A dashboard domain is ready once it has its Origin CA certificate and AOP is enforced for its zone.
3. WHILE setup is open AND a dashboard domain is ready, THE setup address SHALL accept only the one-time password, not the admin password.
4. WHEN setup closes, THE dootd SHALL close non-Cloudflare connections before the TLS handshake again, exactly as in Requirement 12, and answer any request still arriving on an open setup connection with a page pointing to the dashboard domain.
5. ON the setup address, THE dootd SHALL use the TCP peer address as the client IP (never a client-supplied header) for rate limiting and logs.

### Requirement 4: Secrets at rest

**User Story:** As the operator, I want tokens and env vars encrypted on disk, so a leaked database file alone does not expose them.

#### Acceptance Criteria

1. THE dootd SHALL encrypt the GitHub PAT, the Cloudflare token, the S3 credentials and all app env var values with AES-256-GCM, using a 32-byte master key stored at `/etc/dootd/master.key` with mode 0600.
2. WHEN no master key exists, THE dootd SHALL generate one from a cryptographically secure random source and create the file so that it never overwrites an existing key.
3. IF the master key file is readable or writable by group or others, or has the wrong length or format, THEN THE dootd SHALL refuse to start and state the reason.
4. THE dootd SHALL bind each ciphertext to its purpose (associated data) so that a ciphertext copied to another field fails to decrypt.
5. THE dashboard SHALL never display a stored secret. Secrets can only be replaced.

### Requirement 5: State storage

**User Story:** As the maintainer, I want dootd's state in a single SQLite file with versioned migrations, so upgrades are safe.

#### Acceptance Criteria

1. THE dootd SHALL store its state in `/var/lib/dootd/dootd.db` using SQLite in WAL mode, with foreign keys enabled and a busy timeout.
2. THE dootd SHALL serialize all writes through a single writer connection.
3. WHEN dootd opens the database, THE dootd SHALL apply every pending embedded migration in ascending version order, each in its own transaction, and record each applied version.
4. IF a migration fails, THEN THE dootd SHALL roll back that migration, keep earlier migrations, and refuse to start.
5. IF the database schema version is newer than the binary knows, THEN THE dootd SHALL refuse to start with an error naming both versions.

### Requirement 6: Authentication and account

**User Story:** As the only user, I want an email/password sign-in that I manage in the dashboard, so nobody else can control my server.

#### Acceptance Criteria

1. THE dootd SHALL serve the dashboard only on the dashboard domain, and on the setup address while setup is open (Requirement 3).
2. THE dootd SHALL store the password as an argon2id hash.
3. WHEN valid credentials are submitted, THE dootd SHALL create a session with a 32-byte random ID, store only its hash, and set a `__Host-` cookie with `Secure`, `HttpOnly` and `SameSite=Strict`.
4. THE dootd SHALL expire sessions after 7 days of inactivity or 30 days in total.
5. IF more than 5 failed sign-ins come from one client IP within 15 minutes, THEN THE dootd SHALL reject further attempts from that IP until the window passes.
6. THE dootd SHALL require a valid CSRF token and a same-origin `Origin` (or `Referer`) on every state-changing request.
7. THE account page SHALL let the user change the admin email and the password, each after confirming the current password. A password change SHALL sign out every other session.

### Requirement 7: Settings

**User Story:** As the operator, I want to enter my Cloudflare token, dashboard domain, GitHub PAT and bucket once in the dashboard, so every app can use them.

#### Acceptance Criteria

1. WHEN a Cloudflare token is saved, THE dootd SHALL verify it and list any missing permissions from: Zone Read, DNS Edit, SSL and Certificates Edit, Zone Settings Edit.
2. WHEN a dashboard domain is saved, THE dootd SHALL require a Cloudflare token, reject a domain used by an app, then create the proxied DNS record, the Origin CA certificate and the AOP setup for it, and show its progress. WHEN the domain is replaced, THE dootd SHALL remove the old domain's DNS records and revoke its certificate.
3. WHEN a GitHub PAT is saved, THE dootd SHALL validate it against the GitHub API and show which account it belongs to.
4. WHEN S3 settings (endpoint, region, bucket, access key, secret) are saved, THE dootd SHALL perform a test upload, read-back and delete, and save the settings only if that succeeds.
5. THE dootd SHALL detect the server's public IPv4 (and IPv6, if any) itself; the user never enters an IP address.

### Requirement 8: App management

**User Story:** As the operator, I want to add an app by choosing a repo, branch, type and domain, so it's ready to deploy.

#### Acceptance Criteria

1. WHEN an app is added, THE dootd SHALL require a GitHub repo, a branch, a type (`zig` or `c`) and optionally one domain not used by another app or by the dashboard. `dootd.toml` is read from the repository root.
2. THE app name SHALL be derived from the repository name: lowercased, with `_` and `.` replaced by `-`. IF the result is not a valid app name (2–24 characters, `a–z`, `0–9`, `-`, starting with a letter) or is already used, THEN THE dootd SHALL refuse the app and say why.
3. WHEN an app is added, THE dootd SHALL create a dedicated system user, its directories and cgroup, assign a stable port, and, if a domain is set, create or update a proxied DNS record and obtain an Origin CA certificate for it.
4. WHEN an app is added AND a bucket is configured, THE form SHALL list the backup folders in the bucket and preselect the one named like the app, if it exists. IF a folder is chosen, THEN THE dootd SHALL restore that folder's newest backup into the new app's DATA_DIR (verified as in Requirement 16) before the first deploy.
5. WHEN env vars are changed, THE dootd SHALL store them encrypted and indicate that a restart is required. It SHALL reject reserved names (`PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, `DOOTD_*`).
6. WHEN an app is deleted, THE dootd SHALL stop it and remove its user, cgroup, releases and routing, and SHALL ask whether to keep or delete its DATA_DIR and its backups.
7. THE dootd SHALL support at least 5 apps on one host.

### Requirement 9: Build with pinned toolchain

**User Story:** As a Zig/C developer, I want each app built with exactly the Zig version it pins, so builds are reproducible.

#### Acceptance Criteria

1. WHEN a build starts, THE dootd SHALL read `dootd.toml` from the repo root and validate `contract`, `zig_version` and `run`, reporting every problem clearly.
2. WHEN the pinned Zig version is not cached, THE dootd SHALL download it via `ziglang.org/download/index.json`, verify its SHA-256, and install it atomically.
3. IF the pinned version does not exist or its checksum does not match, THEN THE dootd SHALL fail the build without affecting the running release.
4. THE dootd SHALL run the build as the app's user, in a build cgroup with a memory limit (default 1 GB), a lower CPU weight and a timeout (default 15 minutes), with the pinned `zig` first on `PATH` and `CC="zig cc"` and `CXX="zig c++"` set.
5. THE dootd SHALL run at most one build at a time on the host and queue any others.
6. THE dootd SHALL stream the build log live to the dashboard and keep the last 20 build logs per app.

### Requirement 10: Deploy and rollback

**User Story:** As the operator, I want deploys to happen only when I click, and a failed deploy to never leave my app down, so shipping is safe.

#### Acceptance Criteria

1. THE dootd SHALL deploy only when the user clicks Deploy, Redeploy or Rollback. It SHALL never deploy on git push.
2. WHEN a deploy is triggered, THE dootd SHALL clone the branch HEAD and build it while the current release keeps serving.
3. WHEN the build succeeds, THE dootd SHALL show a 503 "deploying" page, stop the old process, take a pre-deploy backup, switch to the new release, start it and run the health check.
4. IF the new release fails its health check within 30 seconds, THEN THE dootd SHALL stop it, restore and start the previous release, and mark the deploy failed. It SHALL leave the database as is and offer to restore the pre-deploy backup.
5. THE dootd SHALL keep the last 3 releases and allow rollback to any of them without rebuilding.
6. THE dootd SHALL keep a deploy history with commit SHA, status, duration and error.

### Requirement 11: Runtime supervision and isolation

**User Story:** As the operator, I want each app isolated with resource limits and restarted automatically when it crashes, so one misbehaving app can't take down the server.

#### Acceptance Criteria

1. THE dootd SHALL run each app as its own unprivileged system user inside its own cgroup v2, with `memory.max` (default 256 MB), `cpu.max` (default 1 core) and `pids.max` (default 256).
2. THE dootd SHALL provide the contract environment (`PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, `DOOTD_*`) plus the user's env vars.
3. WHEN an app is stopped, THE dootd SHALL send SIGTERM, wait 10 seconds, then kill every process left in the app's cgroup.
4. WHEN an app exits unexpectedly, THE dootd SHALL restart it with exponential backoff from 1 s up to 60 s.
5. IF an app exits 5 times within 5 minutes, THEN THE dootd SHALL mark it crashed and stop restarting it until the user restarts it.
6. WHEN an app is OOM-killed, THE dootd SHALL record the event and show it in the dashboard.
7. WHEN dootd starts, THE dootd SHALL start every app whose desired state is running, one at a time.

### Requirement 12: Edge TLS and Cloudflare-only access

**User Story:** As the operator, I want Full (strict) TLS and my server reachable only through my own Cloudflare account, so nobody can bypass Cloudflare.

#### Acceptance Criteria

1. THE dootd SHALL listen publicly on port 443 only.
2. WHEN a TCP connection comes from an address outside Cloudflare's published IP ranges, THE dootd SHALL close it before the TLS handshake, except while setup is open (Requirement 3).
3. THE dootd SHALL refresh Cloudflare's IP ranges every 24 hours and fall back to the last known good list or the built-in list.
4. THE dootd SHALL serve an Origin CA certificate for each configured hostname, choose it by SNI, fail the handshake for unknown SNI, and renew certificates with less than 30 days of validity.
5. THE dootd SHALL upload a client certificate signed by its own private CA to every zone it serves, enable zone-level AOP, and require a valid client certificate from that CA once Cloudflare reports it active. AOP cannot be turned off.
6. IF a zone's SSL mode is not Full (strict), THEN THE dashboard SHALL show a warning with a one-click fix, and dootd SHALL NOT change the mode automatically.

### Requirement 13: Routing and proxy

**User Story:** As the operator, I want requests routed to the right app by domain, with correct client info, so apps behave normally behind dootd.

#### Acceptance Criteria

1. WHEN a request arrives through Cloudflare, THE dootd SHALL route it by its `Host` header to the dashboard, to the app that owns the domain, or to a 404; the `Host` SHALL match the TLS server name (421 otherwise).
2. THE dootd SHALL proxy app traffic to `127.0.0.1:$PORT`, set `X-Forwarded-For` and `X-Real-IP` from `CF-Connecting-IP`, set `X-Forwarded-Proto: https`, and support WebSockets and long-polling.
3. WHILE an app is deploying or stopped, THE dootd SHALL return a 503 page. WHEN an app is unreachable unexpectedly, it SHALL return a 502 page.

### Requirement 14: Logs

**User Story:** As the operator, I want live and historical app logs in the dashboard, so I can debug without SSH.

#### Acceptance Criteria

1. THE dootd SHALL capture each app's stdout and stderr with timestamps into files that rotate at 10 MB, keeping 3.
2. THE dashboard SHALL stream live logs and show recent history (at least the last 1000 lines) per app.

### Requirement 15: Backups

**User Story:** As the operator, I want each app's SQLite databases backed up automatically to R2/S3, so I lose at most 3 hours of data.

#### Acceptance Criteria

1. THE dootd SHALL back up every SQLite database in each app's DATA_DIR every 3 hours, before every deploy or rollback, and when the user clicks "Back up now". The schedule and the 48-hour retention are fixed.
2. THE dootd SHALL detect SQLite files by their header, take a consistent online snapshot with `VACUUM INTO`, and verify it with `quick_check`.
3. THE dootd SHALL store each backup as a zstd-compressed tar with a manifest of SHA-256 sums, keep the newest 2 on the server, and upload it to `<bucket>/<app name>/`, retrying up to 3 times.
4. THE dootd SHALL delete backups older than 48 hours, always keeping each app's newest backup.
5. IF a backup fails or cannot be uploaded, THEN THE dashboard SHALL show a failure badge on the app and on the home page.
6. THE dootd SHALL back up only app databases. dootd's own settings are not backed up; on a new server they are entered again in the dashboard.

### Requirement 16: Restore

**User Story:** As the operator, I want to restore any backup of an app with one click, so recovering from a bad migration or data loss is easy.

#### Acceptance Criteria

1. WHEN a restore is requested, THE dootd SHALL fetch and verify the backup (SHA-256, manifest, `quick_check`), stop the app, move the current database files (including `-wal` and `-shm`) into a `.pre-restore-<timestamp>` folder, put the restored files in place with correct ownership, and start the app if it was running.
2. IF verification fails, THEN THE dootd SHALL abort the restore without stopping the app.
3. THE dootd SHALL keep the last 2 pre-restore folders.

### Requirement 17: Monitoring

**User Story:** As the operator, I want host and per-app resource usage and request stats, so I can see problems at a glance.

#### Acceptance Criteria

1. THE dootd SHALL sample host CPU, memory, swap, load and disk, and per-app CPU, memory, pids, I/O, OOM count, restarts, request rate, status classes and p50/p95 latency, every 10 seconds.
2. THE dootd SHALL keep 1 hour of raw samples in memory and 7 days of 1-minute rollups in the database.
3. THE dashboard SHALL render these as charts without a JavaScript chart library.
4. WHEN disk usage exceeds 85%, memory exceeds 90%, an app crashes or is OOM-killed, a backup fails, or a certificate is about to expire, THE dashboard SHALL show a warning.

### Requirement 18: Updates

**User Story:** As the operator, I want updating dootd to be the same single command I already know.

#### Acceptance Criteria

1. THE dootd SHALL be updated only by running the installer again (Requirement 2.6); dootd SHALL NOT download or replace itself.
2. WHEN a newer binary starts, THE dootd SHALL apply its migrations as in Requirement 5.
