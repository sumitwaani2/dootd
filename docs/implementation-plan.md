# dootd Implementation Plan

This plan builds dootd in phases. Each phase ends with something that works and can be tested on a real VPS. Design details are in [architecture.md](architecture.md) and the app rules are in [app-contract.md](app-contract.md).

**Status legend:** ⬜ not started · 🟨 in progress · ✅ done

| Phase | Name | Outcome | Status |
|---|---|---|---|
| 0 | Foundations | Repo, CI, release pipeline, test VPS | ✅ |
| 1 | Run an app | Supervise a local binary with users, cgroups and logs | ✅ |
| 2 | Build and deploy | GitHub → Zig build → release → health-checked deploy and rollback | ✅ |
| 3 | Edge | :443, CF-only, Origin CA, AOP, host routing, DNS | ✅ |
| 4 | Dashboard | Login + the full click-to-deploy flow in the browser | ✅ |
| 5 | Backups | Scheduled, pre-deploy and manual backups to S3/R2, plus restore | ✅ |
| 6 | Monitoring | Host and app metrics, request stats, charts, warnings | ✅ |
| 7 | Install, simplify → v1.0 | One command over SSH, everything else in the dashboard, hardening | 🟨 |

Phases 1–3 are mostly used through the CLI or a config file, so the risky parts (cgroups, builds, TLS) get proven before any UI work. Phase 4 connects everything to the browser.

---

## Phase 0: Foundations

- [x] Go module `github.com/sumitwaani2/dootd`, set up the `cmd/` + `internal/` layout (architecture §5).
- [x] `dootd version` subcommand. Version, commit and date are injected at build time with `-ldflags`.
- [x] GitHub Actions:
  - CI on every PR: `go vet`, `staticcheck`, `go test ./...`, and a `CGO_ENABLED=0` build for amd64 and arm64.
  - Release on `v*` tags: build `dootd-linux-amd64` and `dootd-linux-arm64`, write `checksums.txt`, attach `install.sh`.
- [x] `store` package: open SQLite (WAL, single writer) and run embedded numbered migrations.
- [x] `secrets` package: load or create `master.key`, `Seal`/`Open` with AES-256-GCM.
- [x] A cheap Ubuntu 24.04 test VPS (1 GB) and a test Cloudflare zone.

**Done when:** tagging `v0.0.1` produces downloadable binaries that run `dootd version` on the VPS.

## Phase 1: Run an app

- [x] `cgroup`: find the delegated root, move self into `supervisor/`, create `apps/<app>`, set and read limits, kill by PID (not `cgroup.kill`, see architecture D10).
- [x] `users`: create or delete `dootd-<app>` and set up directory ownership (architecture §6, §10.1).
- [x] `supervisor`: start with `CgroupFD` + `Credential` + `Pdeathsig`, SIGTERM → 10 s → kill, the state machine, backoff and the crashed state.
- [x] Health check: TCP connect, then an HTTP GET to `health_path`.
- [x] `logs`: stdout/stderr → rotating file + ring buffer.
- [x] Contract env vars (`PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, `DOOTD_*`) + user env.
- [x] Temporary dev config (`/etc/dootd/dev-apps.toml`) that defines an app using a prebuilt binary.
- [x] A **sample Zig app** (`examples/sample-zig`, Zig 0.16.0) that follows the contract: SQLite in DATA_DIR, `/healthz`, SIGTERM handling. The same for C (`examples/sample-c`, using the sqlite amalgamation and a Makefile).

**Done when:** the sample app runs as its own user inside its cgroup; `memory.max` causes an OOM kill that is logged and restarted; and stopping dootd leaves no stray processes behind. Automated in `scripts/e2e/phase1.sh`, which runs on every PR on an Ubuntu 24.04 VM.

## Phase 2: Build and deploy

- [x] `github`: validate the PAT, resolve the branch SHA, shallow clone with go-git.
- [x] `dootd.toml` parser and validator with clear error messages (app-contract §2).
- [x] `toolchain`: read `index.json`, download, check SHA-256, extract xz safely (no path traversal), install atomically, list and delete.
- [x] `builder`: run the build as the app user in `builds/<app>` with a memory limit, CPU weight, timeout, PATH/CC/CXX and a persistent zig cache; stream the build log.
- [x] `deployer`: the full pipeline from architecture §11, including the global build queue, `current` symlink swap, rollback when the health check fails, and pruning to 3 releases.
- [x] Rollback to any of the kept releases, with no build.
- [x] `dootd ctl` over a local Unix socket to deploy, roll back and inspect apps (removed in Phase 7).

**Done when:** both sample apps deploy from GitHub with their pinned Zig version. A broken commit leaves the old version serving, and a failing health check rolls back automatically. Automated in `scripts/e2e/phase2.sh` (64 checks, including a private GitHub clone of this repo's PR branch).

## Phase 3: Edge

- [x] `cloudflare` client: verify the token and report its permissions, find zones, upsert DNS A/AAAA records (proxied), Origin CA create/revoke, AOP certificate upload/list/delete, the `tls_client_auth` setting, read the SSL mode, `/ips`.
- [x] Listener with the Cloudflare IP filter, a built-in fallback list and a refresh every 24 h.
- [x] Origin certificate manager: CSR → certificate, stored on disk and in DB, SNI lookup, daily renewal check.
- [x] AOP: private CA + client certificate, upload per zone, `RequireAndVerifyClientCert`, rotation.
- [x] Host router + per-app ReverseProxy with header rewriting, timeouts, and 503/502/404 pages.
- [x] Per-app request counters (feeds Phase 6).

**Done when:** `https://sample.<zone>` works through Cloudflare in Full (strict) mode; `curl --resolve` straight to the VPS IP fails; a request through a different Cloudflare account's zone pointed at the IP fails (AOP); and WebSockets work. Automated against a fake Cloudflare API in `scripts/e2e/phase3.sh` (50 checks); the real-Cloudflare check is part of the Phase 7 VPS run.

## Phase 4: Dashboard

- [x] `auth`: argon2id, sessions, CSRF, rate limiting, security headers (architecture §14).
- [x] Layout + CSS + a small vanilla script (no htmx, see architecture D9).
- [x] Settings: GitHub PAT (with validation), Cloudflare token (verified, zones listed). S3 settings moved to Phase 5, next to the backup code that uses them.
- [x] **Add app**: name, type (zig/c), repo picker, branch, domain, env vars, limits → creates the DNS record, certificate and user.
- [x] App page: status, **Deploy / Redeploy**, Rollback, Start/Stop/Restart, live build log and live app log (SSE), env var editor (requires a restart), limits, delete app (with a confirm step and an option to keep backups).
- [x] Deploy history list with status, SHA, duration and error.
- [x] Zone SSL mode warning with a "Set to Full (strict)" button.
- [x] Apps live in the database. (The admin was created over SSH here; Phase 7 replaced that with the installer's one-time password.)

**Done when:** a new app goes from repo URL to live HTTPS entirely in the browser, with no SSH (after the one-time admin setup). Automated in `scripts/e2e/phase4.sh` (83 checks, driving the dashboard with curl through the edge).

## Phase 5: Backups

- [x] `s3` wrapper: put (multipart), list, get, delete, and a connection test.
- [x] Settings page: S3 endpoint, bucket, keys (sealed), with a test upload and delete (moved from Phase 4).
- [x] Snapshot using SQLite header detection + `VACUUM INTO` + `quick_check`.
- [x] Archive (tar+zstd) + manifest + upload with retries.
- [x] Scheduler: every 3 h, manual "Backup now", and the pre-deploy hook in the deployer.
- [x] Retention: 48 h, always keeping the newest backup.
- [x] Restore flow with a `.pre-restore` safety copy.
- [x] Daily backup of `dootd.db` + recovery kit download + reminder.
- [x] Backup status and failure badges in the UI.

**Done when:** writes to the sample app survive a "delete the data dir, then restore" test; retention deletes old objects on R2; and a backup taken during heavy writes passes `integrity_check`. Automated in `scripts/e2e/phase5.sh` (55 checks, `rclone serve s3` as the bucket).

## Phase 6: Monitoring

- [x] Collector: host `/proc` + `statfs`, per-app cgroup stats, proxy counters, and supervisor stats, every 10 s.
- [x] Ring buffer (1 h) + `metrics_1m` rollups kept for 7 days, with hourly pruning.
- [x] Server-rendered SVG charts: CPU, memory, requests, 5xx and p95 for the host and each app.
- [x] Warnings: disk > 85 %, memory > 90 %, OOM, crashed, backup failed, certificate expiring.

**Done when:** the charts match `top` and `systemd-cgtop` within a reasonable margin, and dootd's own idle RSS stays under 30 MB with 5 apps running. Automated in `scripts/e2e/phase6.sh` (48 checks): CPU within 3 points of `cpu.stat` for a free and a 0.5-core app, memory equal to `memory.current`, dootd at 28 MB and 0.03 % CPU.

## Phase 7: Install, simplify, v1.0

The first pass of this phase added a CLI bootstrap (`dootd init`, `init --restore`, recovery kit), `dootd ctl`, a config file and self-update. That went against the goal (one SSH command; everything else in the dashboard; one way to do each thing), so the second pass removes it again (architecture D33–D37).

- [x] `install.sh`: OS, cgroup and architecture checks; download + checksum; `make`; directories; master key; systemd unit (embedded in the binary, `dootd setup-host`).
- [x] Hardening pass: fuzz the `dootd.toml` parser, the tar extraction and backup archives, `go test -race`, fd limits, restart timing with 5 apps.
- [x] Remove `dootd ctl` and the control socket, `dootd init` (+ `--restore`), `reset-password`, self-update, `--dev-apps`, `/etc/dootd/config.toml`, the recovery kit, dootd.db backups and the monorepo app path.
- [x] The installer asks nothing: it installs or updates, starts dootd and prints the setup address and a one-time password (24 h, single use). Re-running it is the update and the recovery path.
- [x] Setup address `https://<server IP>` with a self-signed certificate, open only until the dashboard domain is ready and no one-time password is pending; dashboard only, never apps.
- [x] Dashboard: set up the account from the one-time password, change the admin email, set the dashboard domain, enter the Cloudflare/GitHub tokens and the bucket; the home page lists missing setup steps.
- [x] App name = repository name; bucket folder per app; Add app restores a chosen folder's newest backup.
- [x] E2E scripts use only the installer and the dashboard (`scripts/e2e/phase1.sh` … `phase7.sh`, 459 checks).
- [ ] dootd never builds (D38): apps are tested, built and released by their own GitHub Actions workflow; dootd deploys a chosen GitHub release (download, checksum, safe unpack, ELF check). Removes the Zig toolchain manager, the builder, git cloning, build cgroups, `make` and the swapfile.
- [ ] Soak test: 5 apps running for 7 days on a 1 GB VPS, with real Cloudflare and R2.

**Done when:** a fresh VPS goes from `curl | sudo bash` to a deployed app in under 10 minutes without touching SSH again, and v1.0.0 is tagged.

---

## After v1 (backlog)

- 2FA (TOTP) for the dashboard.
- Alerts by email or webhook for the warnings from Phase 6.
- Scheduled jobs (cron) per app, run in the app's cgroup.
- Backups for non-SQLite files in `DATA_DIR`, such as uploads.
- Continuous replication (Litestream-style) if an RPO under 3 h is ever needed.
- Minisign verification for Zig downloads and dootd releases.
- Keeping apps running while dootd restarts, by re-attaching to the running processes.

## Working agreements

- Every phase gets merged to `main` through a PR with its checklist ticked.
- Anything that changes the app contract bumps `contract` and updates [app-contract.md](app-contract.md) in the same PR.
- Architecture changes update [architecture.md](architecture.md) and add a row to its decisions log.
