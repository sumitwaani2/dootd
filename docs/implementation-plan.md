# dootd Implementation Plan

This plan builds dootd in phases. Each phase ends with something that works and can be tested on a real VPS. Design details are in [architecture.md](architecture.md) and the app rules are in [app-contract.md](app-contract.md).

**Status legend:** ⬜ not started · 🟨 in progress · ✅ done

| Phase | Name | Outcome | Status |
|---|---|---|---|
| 0 | Foundations | Repo, CI, release pipeline, test VPS | ⬜ |
| 1 | Run an app | Supervise a local binary with users, cgroups and logs | ⬜ |
| 2 | Build and deploy | GitHub → Zig build → release → health-checked deploy and rollback | ⬜ |
| 3 | Edge | :443, CF-only, Origin CA, AOP, host routing, DNS | ⬜ |
| 4 | Dashboard | Login + the full click-to-deploy flow in the browser | ⬜ |
| 5 | Backups | Scheduled, pre-deploy and manual backups to S3/R2, plus restore | ⬜ |
| 6 | Monitoring | Host and app metrics, request stats, charts, warnings | ⬜ |
| 7 | Install and self-update → v1.0 | One-line install, `dootd init`, update from the UI, hardening | ⬜ |

Phases 1–3 are mostly used through the CLI or a config file, so the risky parts (cgroups, builds, TLS) get proven before any UI work. Phase 4 connects everything to the browser.

---

## Phase 0: Foundations

- [ ] Go module `github.com/sumitwaani2/dootd`, set up the `cmd/` + `internal/` layout (architecture §5).
- [ ] `dootd version` subcommand. Version, commit and date are injected at build time with `-ldflags`.
- [ ] GitHub Actions:
  - CI on every PR: `go vet`, `staticcheck`, `go test ./...`, and a `CGO_ENABLED=0` build for amd64 and arm64.
  - Release on `v*` tags: build `dootd-linux-amd64` and `dootd-linux-arm64`, write `checksums.txt`, attach `install.sh`.
- [ ] `store` package: open SQLite (WAL, single writer) and run embedded numbered migrations.
- [ ] `secrets` package: load or create `master.key`, `Seal`/`Open` with AES-256-GCM.
- [ ] A cheap Ubuntu 24.04 test VPS (1 GB) and a test Cloudflare zone.

**Done when:** tagging `v0.0.1` produces downloadable binaries that run `dootd version` on the VPS.

## Phase 1: Run an app

- [ ] `cgroup`: find the delegated root, move self into `supervisor/`, create `apps/<app>`, set and read limits, `cgroup.kill`.
- [ ] `users`: create or delete `dootd-<app>` and set up directory ownership (architecture §6, §10.1).
- [ ] `supervisor`: start with `CgroupFD` + `Credential` + `Pdeathsig`, SIGTERM → 10 s → kill, the state machine, backoff and the crashed state.
- [ ] Health check: TCP connect, then an HTTP GET to `health_path`.
- [ ] `logs`: stdout/stderr → rotating file + ring buffer.
- [ ] Contract env vars (`PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, `DOOTD_*`) + user env.
- [ ] Temporary dev config (`/etc/dootd/dev-apps.toml`) that defines an app using a prebuilt binary.
- [ ] A **sample Zig app** repo (`dootd-sample-zig`) that follows the contract: SQLite in DATA_DIR, `/healthz`, SIGTERM handling. The same for C (`dootd-sample-c`, using the sqlite amalgamation and a Makefile).

**Done when:** the sample app runs as its own user inside its cgroup; `memory.max` causes an OOM kill that is logged and restarted; and stopping dootd leaves no stray processes behind.

## Phase 2: Build and deploy

- [ ] `github`: validate the PAT, resolve the branch SHA, shallow clone with go-git.
- [ ] `dootd.toml` parser and validator with clear error messages (app-contract §2).
- [ ] `toolchain`: read `index.json`, download, check SHA-256, extract xz safely (no path traversal), install atomically, list and delete.
- [ ] `builder`: run the build as the app user in `builds/<app>` with a memory limit, CPU weight, timeout, PATH/CC/CXX and a persistent zig cache; stream the build log.
- [ ] `deployer`: the full pipeline from architecture §11, including the global build queue, `current` symlink swap, rollback when the health check fails, and pruning to 3 releases.
- [ ] Rollback to any of the kept releases, with no build.
- [ ] A temporary CLI to trigger `deploy <app>`, removed or hidden after Phase 4.

**Done when:** both sample apps deploy from GitHub with their pinned Zig version. A broken commit leaves the old version serving, and a failing health check rolls back automatically.

## Phase 3: Edge

- [ ] `cloudflare` client: verify the token and report its permissions, find zones, upsert DNS A/AAAA records (proxied), Origin CA create/revoke, AOP certificate upload/list/delete, the `tls_client_auth` setting, read the SSL mode, `/ips`.
- [ ] Listener with the Cloudflare IP filter, a built-in fallback list and a refresh every 24 h.
- [ ] Origin certificate manager: CSR → certificate, stored on disk and in DB, SNI lookup, daily renewal check.
- [ ] AOP: private CA + client certificate, upload per zone, `RequireAndVerifyClientCert`, rotation.
- [ ] Host router + per-app ReverseProxy with header rewriting, timeouts, and 503/502/404 pages.
- [ ] Per-app request counters (feeds Phase 6).

**Done when:** `https://sample.<zone>` works through Cloudflare in Full (strict) mode; `curl --resolve` straight to the VPS IP fails; a request through a different Cloudflare account's zone pointed at the IP fails (AOP); and WebSockets work.

## Phase 4: Dashboard

- [ ] `auth`: argon2id, sessions, CSRF, rate limiting, security headers (architecture §14).
- [ ] Layout + embedded htmx + CSS (plain and small).
- [ ] Settings: GitHub PAT (with validation), Cloudflare token (with a permission check), S3 settings (with a test upload).
- [ ] **Add app**: name, type (zig/c), repo picker, branch, domain, env vars, limits → creates the DNS record, certificate and user.
- [ ] App page: status, **Deploy / Redeploy**, Rollback, Start/Stop/Restart, live build log and live app log (SSE), env var editor (requires a restart), limits, delete app (with a confirm step and an option to keep backups).
- [ ] Deploy history list with status, SHA, duration and error.
- [ ] Zone SSL mode warning with a "Set to Full (strict)" button.
- [ ] Remove the dev config and dev CLI.

**Done when:** a new app goes from repo URL to live HTTPS entirely in the browser, with no SSH.

## Phase 5: Backups

- [ ] `s3` wrapper: put (multipart), list, get, delete, and a connection test.
- [ ] Snapshot using SQLite header detection + `VACUUM INTO` + `quick_check`.
- [ ] Archive (tar+zstd) + manifest + upload with retries.
- [ ] Scheduler: every 3 h, manual "Backup now", and the pre-deploy hook in the deployer.
- [ ] Retention: 48 h, always keeping the newest backup.
- [ ] Restore flow with a `.pre-restore` safety copy.
- [ ] Daily backup of `dootd.db` + recovery kit download + reminder.
- [ ] Backup status and failure badges in the UI.

**Done when:** writes to the sample app survive a "delete the data dir, then restore" test; retention deletes old objects on R2; and a backup taken during heavy writes passes `integrity_check`.

## Phase 6: Monitoring

- [ ] Collector: host `/proc` + `statfs`, per-app cgroup stats, proxy counters, and supervisor stats, every 10 s.
- [ ] Ring buffer (1 h) + `metrics_1m` rollups kept for 7 days, with hourly pruning.
- [ ] Server-rendered SVG charts: CPU, memory, requests, 5xx and p95 for the host and each app.
- [ ] Warnings: disk > 85 %, memory > 90 %, OOM, crashed, backup failed, certificate expiring.

**Done when:** the charts match `top` and `systemd-cgtop` within a reasonable margin, and dootd's own idle RSS stays under 30 MB with 5 apps running.

## Phase 7: Install, self-update, v1.0

- [ ] `install.sh`: OS, cgroup and architecture checks; download + checksum; `make`; directories; master key; systemd unit; optional swapfile; optional ufw.
- [ ] `dootd init` (interactive): admin, dashboard domain, CF token, IP detection → DNS, certificate and AOP for the dashboard → start the service.
- [ ] `dootd reset-password`.
- [ ] Self-update from the UI, with a `.prev` fallback (architecture §15).
- [ ] Hardening pass: fuzz the `dootd.toml` parser and the tar extraction, check how many file descriptors each process can open, run `go test -race`, and time a restart with 5 apps.
- [ ] Soak test: 5 apps running for 7 days on a 1 GB VPS.
- [ ] Final pass on the docs: README quick start, Cloudflare token setup walkthrough, troubleshooting.

**Done when:** a fresh VPS goes from `curl | sudo bash` to a deployed app in under 10 minutes, and v1.0.0 is tagged.

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
