# Implementation Plan

Tasks are grouped by the phases in `docs/implementation-plan.md`. Each phase ends with a manual "done when" check on a real VPS. Tasks marked `*` are optional (extra tests).

## Phase 0: Foundations

- [x] 1. Project skeleton and build info
  - [x] 1.1 Create Go module `github.com/sumitwaani2/dootd`, `cmd/dootd`, `internal/`, `.gitignore`, `Makefile` (build, build-all, lint, test)
    - _Requirements: 1.1_
  - [x] 1.2 Implement `internal/buildinfo` and `dootd version`; unknown subcommands and not-yet-built ones (`serve`, `init`, `reset-password`) print a clear message and exit non-zero
    - _Requirements: 1.2_

- [x] 2. Secrets package
  - [x] 2.1 Implement `secrets.LoadOrCreate` / `Load`: hex key file, `O_EXCL` creation with 0600, fsync, permission and format validation
    - _Requirements: 4.1, 4.2, 4.3_
  - [x] 2.2 Implement `Box.Seal` / `Box.Open` with AES-256-GCM, a versioned format, a random nonce, and purpose as associated data
    - _Requirements: 4.1, 4.4_
  - [x]* 2.3 Unit tests: round-trip, wrong purpose, tampered ciphertext, bad permissions, bad length, never overwriting an existing key
    - _Requirements: 4.2, 4.3, 4.4_

- [x] 3. Store package
  - [x] 3.1 Implement `store.Open` with writer (1 conn, immediate tx) and reader pools and WAL/busy_timeout/foreign_keys pragmas
    - _Requirements: 5.1, 5.2_
  - [x] 3.2 Implement the embedded migration runner: validation, per-migration transactions, `ErrSchemaTooNew`; add `0001_init.sql` (`settings`)
    - _Requirements: 5.3, 5.4, 5.5_
  - [x] 3.3 Add `GetSetting` / `SetSetting` / `SchemaVersion`
    - _Requirements: 5.1_
  - [x]* 3.4 Unit tests: fresh DB, idempotent re-open, failing migration rolls back, schema too new
    - _Requirements: 5.3, 5.4, 5.5_

- [x] 4. CI and release pipeline
  - [x] 4.1 `.github/workflows/ci.yml`: gofmt, vet, staticcheck, `go test -race`, amd64/arm64 static builds
    - _Requirements: 1.5_
  - [x] 4.2 `.github/workflows/release.yml`: build on `v*` tags with ldflags, `checksums.txt`, GitHub Release with binaries and `install.sh`
    - _Requirements: 1.4_
  - [x] 4.3 `install.sh` (Phase 0 subset): OS/arch/systemd/cgroup v2 checks, download, SHA-256 verification, install to `/usr/local/bin`
    - _Requirements: 2.1, 2.2, 2.3_

- [x] 5. Phase 0 checkpoint (manual)
  - Merge to `main`, tag `v0.0.1`, confirm the release assets exist
  - On a 1 GB Ubuntu 24.04 VPS: `curl -fsSL …/install.sh | sudo bash` then `dootd version`
  - Set up a test Cloudflare zone for Phase 3

## Phase 1: Run an app

- [x] 6. cgroup v2 management
  - [x] 6.1 Detect the delegated root, move self into `supervisor/`, create `apps/<app>` and `builds/<app>`, enable controllers
    - _Requirements: 11.1_
  - [x] 6.2 Set and read limits (`memory.max`, `memory.swap.max`, `cpu.max`, `pids.max`, `cpu.weight`), kill by PID (not `cgroup.kill`), watch `memory.events`
    - _Requirements: 11.1, 11.3, 11.6_
- [x] 7. Per-app users and directory layout (`data/`, `tmp/`, `releases/`, `logs/`, zig cache)
  - _Requirements: 8.2, 11.1_
- [x] 8. Supervisor
  - [x] 8.1 Spawn with `CgroupFD`, `Credential`, `Setpgid`, `Pdeathsig`; contract env; working directory = current release
    - _Requirements: 11.1, 11.2_
  - [x] 8.2 State machine, health check (TCP connect, then HTTP GET), SIGTERM → 10 s → SIGKILL the cgroup
    - _Requirements: 11.3_
  - [x] 8.3 Backoff restarts, crashed after 5 exits in 5 min, start desired apps sequentially at boot
    - _Requirements: 11.4, 11.5, 11.7_
- [x] 9. Logs: timestamped capture, 10 MB × 3 rotation, 1000-line ring buffer with subscriber fan-out
  - _Requirements: 14.1, 14.2_
- [x] 10. Temporary `dev-apps.toml` loader + `dootd serve` running supervised apps (loader removed in task 51)
- [x] 11. Sample apps `examples/sample-zig` (Zig 0.16.0) and `examples/sample-c` (zig cc) that follow the app contract
- [x] 12. Phase 1 checkpoint: sample app runs as its own user in its cgroup; OOM is logged and the app restarted; stopping dootd leaves no processes
  - Automated: `scripts/e2e/phase1.sh` + `.github/workflows/e2e.yml` (Ubuntu 24.04 VM, 43 checks)

## Phase 2: Build and deploy

- [x] 13. `github` client: PAT validation, branch HEAD SHA, go-git shallow clone with in-memory auth
  - _Requirements: 7.1, 10.2_
- [x] 14. `dootd.toml` parser and validator with complete error reporting
  - _Requirements: 9.1_
  - [x]* 14.1 Unit and fuzz tests for the parser (done in task 48)
- [x] 15. `toolchain`: index.json, download, SHA-256 verification, safe tar.xz extraction, atomic install, list/delete
  - _Requirements: 9.2, 9.3_
- [x] 16. `builder`: build cgroup, app user, PATH/CC/CXX, timeout, live build log, keep 20 logs
  - _Requirements: 9.4, 9.6_
- [x] 17. `deployer`: global queue, pipeline (architecture §11), `current` symlink swap, health-check rollback, keep 3 releases, deploy history
  - _Requirements: 9.5, 10.2, 10.3, 10.4, 10.5, 10.6_
  - [ ]* 17.1 State machine tests with fake builder and supervisor
- [x] 17b. `dootd ctl` + control socket (deploy, rollback, status, releases, deployments, logs, start/stop/restart, github-token) (removed in task 51)
  - _Requirements: 10.1, 10.5, 10.6_
- [x] 18. Phase 2 checkpoint: both samples deploy with their pinned Zig version; a broken commit leaves the old version serving; a failed health check rolls back
  - Automated: `scripts/e2e/phase2.sh` (Ubuntu 24.04 VM, 64 checks)

## Phase 3: Edge

- [x] 19. `cloudflare` client: token verification and permission report, zones, DNS upsert, Origin CA, AOP certs and setting, SSL mode, `/ips`
  - _Requirements: 7.2, 8.2, 12.4, 12.5, 12.6_
- [x] 20. Listener with CF IP filter, built-in fallback list, 24 h refresh
  - _Requirements: 12.1, 12.2, 12.3_
- [x] 21. Origin certificate manager (CSR, SNI lookup, reject unknown SNI, renewal)
  - _Requirements: 12.4_
- [x] 22. AOP: private CA, client certificate upload per zone, `RequireAndVerifyClientCert`, rotation
  - _Requirements: 12.5_
- [x] 23. Host router + ReverseProxy (headers, timeouts, WebSockets, 404/502/503 pages, request counters)
  - _Requirements: 13.1, 13.2, 13.3_
- [x] 23b. `dootd ctl cloudflare-token`, `edge`, `edge sync`, `edge set-strict <zone>`; `[edge]` config section (removed in task 51; all in the dashboard)
  - _Requirements: 7.2, 12.6_
- [x] 24. Phase 3 checkpoint: Full (strict) works; direct-IP requests fail; requests via another Cloudflare account fail; WebSockets work
  - Automated with a fake Cloudflare API: `scripts/e2e/phase3.sh` (50 checks). Re-check against real Cloudflare on the Phase 7 VPS run.

## Phase 4: Dashboard

- [x] 25. `auth`: argon2id, sessions, `__Host-` cookie, CSRF + Origin check, rate limiting, security headers
  - _Requirements: 6.1–6.6_
- [x] 26. Web skeleton: layout, CSS, small vanilla script (no htmx), SSE helper
- [x] 27. Settings pages: GitHub PAT, Cloudflare token (validated); secrets are never shown. S3 settings → task 33b
  - _Requirements: 4.5, 7.1, 7.2_
- [x] 28. Add app flow (user, cgroup, port, DNS, certificate), env var editor with reserved-name checks, delete app
  - _Requirements: 8.1–8.5_
- [x] 29. App page: deploy/redeploy/rollback, start/stop/restart, live build and app logs, history, limits
  - _Requirements: 10.1, 10.5, 10.6, 14.2_
- [x] 30. Zone SSL mode warning + one-click fix
  - _Requirements: 12.6_
- [x] 31. Apps stored in the database (`apps`, `app_env`); `--dev-apps` kept for tests only; `dootd ctl admin set-password` and `dootd reset-password` (removed in task 51; the installer's one-time password replaces them)
  - _Requirements: 2.6_
- [x] 32. Phase 4 checkpoint: repo URL → live HTTPS app entirely in the browser
  - Automated: `scripts/e2e/phase4.sh` (83 checks through the edge with a fake Cloudflare API)

## Phase 5: Backups

- [x] 33. `s3` wrapper (multipart put, list, get, delete, connection test)
  - _Requirements: 7.3, 15.3_
- [x] 33b. Settings page: S3 endpoint, bucket and keys (sealed), test upload + delete
  - _Requirements: 4.1, 7.3_
- [x] 34. Snapshot: header detection, `VACUUM INTO`, `quick_check`
  - _Requirements: 15.2_
- [x] 35. Archive + manifest + upload with retries; scheduler (3 h), manual, pre-deploy hook
  - _Requirements: 15.1, 15.3_
- [x] 36. Retention (48 h, always keep the newest)
  - _Requirements: 15.4_
  - [ ]* 36.1 Unit tests for retention selection
- [x] 37. Restore flow with verification and `.pre-restore` folders (keep 2)
  - _Requirements: 16.1, 16.2, 16.3_
- [x] 38. Daily `dootd.db` backup, recovery kit download + reminder, failure badges
  - _Requirements: 15.5, 15.6_
- [x] 38b. Configurable `[backups] interval/retention`; local copies (2, plus un-uploaded ones during an outage); retry of failed uploads; `dootd ctl backup | backups | restore` (config and ctl removed in task 51); delete-app option to remove backups
  - _Requirements: 8.4, 15.1, 15.3_
- [x] 39. Phase 5 checkpoint: delete-and-restore test; retention verified on R2; a backup taken under heavy writes passes `integrity_check`
  - Automated: `scripts/e2e/phase5.sh` (55 checks, `rclone serve s3` as the bucket). Re-check against real R2 on the Phase 7 VPS run.

## Phase 6: Monitoring

- [x] 40. Collector (host `/proc` + `statfs`, cgroup stats, edge counters, supervisor stats) every 10 s
  - _Requirements: 17.1_
- [x] 41. 1 h ring buffer + `metrics_1m` 7-day rollups and pruning
  - _Requirements: 17.2_
- [x] 42. Server-rendered SVG charts for host and apps
  - _Requirements: 17.3_
- [x] 43. Warnings (disk, memory, OOM, crashed, backup failed, certificate expiring)
  - _Requirements: 17.4_
- [x] 44. Phase 6 checkpoint: numbers match `top`/`systemd-cgtop`; dootd RSS < 30 MB with 5 apps
  - _Requirements: 1.3_
  - Automated: `scripts/e2e/phase6.sh` (48 checks). Measured 28 MB RSS / 0.03 % CPU idle with 5 apps.
- [x] 44b. Memory tuning (soft heap limit); `[monitoring]` thresholds, `dootd ctl top` and `GET /v1/metrics` (removed in task 51)
  - _Requirements: 1.3, 17.4_

## Phase 7: Install, simplify, v1.0

Tasks 46–47 built a CLI bootstrap (`dootd init`, `init --restore`, recovery kit) and self-update. They went beyond the goal (one SSH command, everything else in the dashboard, one way to do each thing) and were removed again by tasks 51–58 (decisions D33–D37).

- [x] 45. Full `install.sh` (directories, master key, systemd unit with `Delegate=yes`, `make`, swapfile); the unit is embedded in the binary and installed by `dootd setup-host` (D28)
  - _Requirements: 2.3_
- [x] 46. ~~`dootd init`, `dootd init --restore <recovery kit>`~~ (removed in task 51)
- [x] 47. ~~Self-update with `dootd.prev` start guard~~ (removed in task 51)
- [x] 48. Hardening: fuzzing (toml, tar/xz, backup archives), `-race`, fd limits, restart timing
  - Fuzzing found and fixed a tar.xz extraction escape through chained symlinks
- [x] 51. Remove the CLI surface and extras: `dootd ctl` + control socket, `init`, `reset-password`, `update`/`update-guard` + `internal/selfupdate`, `--dev-apps`, `/etc/dootd/config.toml`, recovery kit, dootd.db self-backup, app path (monorepo subfolder)
  - _Requirements: 1.2, 15.6, 18.1_
- [x] 52. One-time password: `setup-host` creates it and the installer prints it; sign-in with it only allows setting the admin email and password; 24 h expiry; consumed on use
  - _Requirements: 2.3–2.6_
- [x] 53. Setup address: non-Cloudflare connections allowed only while setup is open, self-signed certificate, dashboard only, TCP peer as client IP; closes when the dashboard domain is ready and no one-time password is pending
  - _Requirements: 3.1–3.5, 12.2_
- [x] 54. Dashboard settings for everything that was CLI or config: dashboard domain (with progress and old-domain cleanup), admin email change; public IPs always detected; fixed backup schedule and warning thresholds
  - _Requirements: 6.7, 7.1–7.5, 15.1, 17.4_
- [x] 55. App name derived from the repo name; `dootd.toml` always at the repo root
  - _Requirements: 8.1, 8.2_
- [x] 56. Bucket layout `<app>/`; Add app lists the bucket's folders (preselecting the app's name) and restores the chosen folder's newest backup before the first deploy
  - _Requirements: 8.4, 15.3, 16.1_
- [x] 57. Installer without questions: stop, replace, `setup-host`, start, print the address and the one-time password; re-running it is the update and recovery path
  - _Requirements: 2.1–2.6, 18_
- [x] 58. E2E scripts drive only the installer and the dashboard (shared helpers); test-only `DOOTD_TEST_*` variables for the fake Cloudflare API, public IPs and a short backup schedule
  - Automated: `scripts/e2e/lib.sh` + phases 1–7 (459 checks): install, one-time password, setup address, dashboard domain moves, bucket folders and restore on Add app, update and recovery by re-running install.sh, a new server from scratch; 28 MB idle RSS with 5 apps, restart of 6 apps in 1.4 s
- [x] 59. Spec and docs: dootd never builds; apps are released by GitHub Actions (contract 2, D38–D40)
  - _Requirements: 9, 10_
- [x] 60. Release workflows for both samples (test, static musl builds for amd64 + arm64, package, smoke test, publish); CI runs them on every PR without publishing
  - _Requirements: 9.1, 9.2_
- [x] 61. Remove builds from dootd: toolchain manager, builder, git clone (go-git, xz), build cgroups, Zig caches; app type, branch and build limits; `make` and the swapfile in the installer
  - _Requirements: 2.3, 9.6_
- [x] 62. Release deploys: releases API, checksum, safe unpacking, ELF check, manifest v2, deploy dropdown (10 newest, latest preselected), rollback to kept releases
  - _Requirements: 9.3–9.5, 10_
- [x] 63. E2E with a fake GitHub API (`e2etool ghmock`) and releases built by the samples' own workflows
  - Automated: phases 1–7, 487 checks; 25 MB idle RSS with 5 apps, 18 MB binary
- [x] 64. First run on a real VPS (Ubuntu 26.04, 1 vCPU, 1.6 GB) with real Cloudflare, R2 and GitHub releases (`v1.0.0-rc1`); fixes for what it found
  - Dashboard domain ready only when its zone is active and in Full/Full (strict) mode; the pages say what is missing (a Flexible zone had locked the user out, D41)
  - Saving the Cloudflare token lists missing permissions (Req 7.1)
  - Health check errors name the real cause (a 404 was reported as "nothing listening")
  - A kept release whose files are missing is downloaded again instead of failing
  - A visit to the dashboard domain through Cloudflare with AOP also makes it ready (the test token could not read the SSL mode)
  - Dashboard clock times name the server's timezone; verified with the VPS in Asia/Kolkata and in UTC (schedule and files stay UTC)
  - Measured: 25 MB idle RSS with 5 apps, 0.6 ms p50 proxy overhead, restart of 5 apps ~2 s, reboot to serving ~20 s
  - _Requirements: 3.2, 7.1, 10.5, 10.6_
- [x] 49. ~~7-day soak test with 5 apps on a 1 GB VPS~~ (skipped: not possible in the test environment; the real Cloudflare and R2 runs are tasks 64 and 65)
- [ ] 50. Tag `v1.0.0`
