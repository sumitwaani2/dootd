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
    - _Requirements: 3.1, 3.2, 3.3_
  - [x] 2.2 Implement `Box.Seal` / `Box.Open` with AES-256-GCM, a versioned format, a random nonce, and purpose as associated data
    - _Requirements: 3.1, 3.4_
  - [x]* 2.3 Unit tests: round-trip, wrong purpose, tampered ciphertext, bad permissions, bad length, never overwriting an existing key
    - _Requirements: 3.2, 3.3, 3.4_

- [x] 3. Store package
  - [x] 3.1 Implement `store.Open` with writer (1 conn, immediate tx) and reader pools and WAL/busy_timeout/foreign_keys pragmas
    - _Requirements: 4.1, 4.2_
  - [x] 3.2 Implement the embedded migration runner: validation, per-migration transactions, `ErrSchemaTooNew`; add `0001_init.sql` (`settings`)
    - _Requirements: 4.3, 4.4, 4.5_
  - [x] 3.3 Add `GetSetting` / `SetSetting` / `SchemaVersion`
    - _Requirements: 4.1_
  - [x]* 3.4 Unit tests: fresh DB, idempotent re-open, failing migration rolls back, schema too new
    - _Requirements: 4.3, 4.4, 4.5_

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
    - _Requirements: 10.1_
  - [x] 6.2 Set and read limits (`memory.max`, `memory.swap.max`, `cpu.max`, `pids.max`, `cpu.weight`), kill by PID (not `cgroup.kill`), watch `memory.events`
    - _Requirements: 10.1, 10.3, 10.6_
- [x] 7. Per-app users and directory layout (`data/`, `tmp/`, `releases/`, `logs/`, zig cache)
  - _Requirements: 7.2, 10.1_
- [x] 8. Supervisor
  - [x] 8.1 Spawn with `CgroupFD`, `Credential`, `Setpgid`, `Pdeathsig`; contract env; working directory = current release
    - _Requirements: 10.1, 10.2_
  - [x] 8.2 State machine, health check (TCP connect, then HTTP GET), SIGTERM → 10 s → SIGKILL the cgroup
    - _Requirements: 10.3_
  - [x] 8.3 Backoff restarts, crashed after 5 exits in 5 min, start desired apps sequentially at boot
    - _Requirements: 10.4, 10.5, 10.7_
- [x] 9. Logs: timestamped capture, 10 MB × 3 rotation, 1000-line ring buffer with subscriber fan-out
  - _Requirements: 13.1, 13.2_
- [x] 10. Temporary `dev-apps.toml` loader + `dootd serve` running supervised apps
- [x] 11. Sample apps `examples/sample-zig` (Zig 0.16.0) and `examples/sample-c` (zig cc) that follow the app contract
- [x] 12. Phase 1 checkpoint: sample app runs as its own user in its cgroup; OOM is logged and the app restarted; stopping dootd leaves no processes
  - Automated: `scripts/e2e/phase1.sh` + `.github/workflows/e2e.yml` (Ubuntu 24.04 VM, 43 checks)

## Phase 2: Build and deploy

- [x] 13. `github` client: PAT validation, branch HEAD SHA, go-git shallow clone with in-memory auth
  - _Requirements: 6.1, 9.2_
- [x] 14. `dootd.toml` parser and validator with complete error reporting
  - _Requirements: 8.1_
  - [x]* 14.1 Unit and fuzz tests for the parser (done in task 48)
- [x] 15. `toolchain`: index.json, download, SHA-256 verification, safe tar.xz extraction, atomic install, list/delete
  - _Requirements: 8.2, 8.3_
- [x] 16. `builder`: build cgroup, app user, PATH/CC/CXX, timeout, live build log, keep 20 logs
  - _Requirements: 8.4, 8.6_
- [x] 17. `deployer`: global queue, pipeline (architecture §11), `current` symlink swap, health-check rollback, keep 3 releases, deploy history
  - _Requirements: 8.5, 9.2, 9.3, 9.4, 9.5, 9.6_
  - [ ]* 17.1 State machine tests with fake builder and supervisor
- [x] 17b. `dootd ctl` + control socket (deploy, rollback, status, releases, deployments, logs, start/stop/restart, github-token)
  - _Requirements: 9.1, 9.5, 9.6_
- [x] 18. Phase 2 checkpoint: both samples deploy with their pinned Zig version; a broken commit leaves the old version serving; a failed health check rolls back
  - Automated: `scripts/e2e/phase2.sh` (Ubuntu 24.04 VM, 64 checks)

## Phase 3: Edge

- [x] 19. `cloudflare` client: token verification and permission report, zones, DNS upsert, Origin CA, AOP certs and setting, SSL mode, `/ips`
  - _Requirements: 6.2, 7.2, 11.4, 11.5, 11.6_
- [x] 20. Listener with CF IP filter, built-in fallback list, 24 h refresh
  - _Requirements: 11.1, 11.2, 11.3_
- [x] 21. Origin certificate manager (CSR, SNI lookup, reject unknown SNI, renewal)
  - _Requirements: 11.4_
- [x] 22. AOP: private CA, client certificate upload per zone, `RequireAndVerifyClientCert`, rotation
  - _Requirements: 11.5_
- [x] 23. Host router + ReverseProxy (headers, timeouts, WebSockets, 404/502/503 pages, request counters)
  - _Requirements: 12.1, 12.2, 12.3_
- [x] 23b. `dootd ctl cloudflare-token`, `edge`, `edge sync`, `edge set-strict <zone>`; `[edge]` config section
  - _Requirements: 6.2, 11.6_
- [x] 24. Phase 3 checkpoint: Full (strict) works; direct-IP requests fail; requests via another Cloudflare account fail; WebSockets work
  - Automated with a fake Cloudflare API: `scripts/e2e/phase3.sh` (50 checks). Re-check against real Cloudflare on the Phase 7 VPS run.

## Phase 4: Dashboard

- [x] 25. `auth`: argon2id, sessions, `__Host-` cookie, CSRF + Origin check, rate limiting, security headers
  - _Requirements: 5.1–5.6_
- [x] 26. Web skeleton: layout, CSS, small vanilla script (no htmx), SSE helper
- [x] 27. Settings pages: GitHub PAT, Cloudflare token (validated); secrets are never shown. S3 settings → task 33b
  - _Requirements: 3.5, 6.1, 6.2_
- [x] 28. Add app flow (user, cgroup, port, DNS, certificate), env var editor with reserved-name checks, delete app
  - _Requirements: 7.1–7.5_
- [x] 29. App page: deploy/redeploy/rollback, start/stop/restart, live build and app logs, history, limits
  - _Requirements: 9.1, 9.5, 9.6, 13.2_
- [x] 30. Zone SSL mode warning + one-click fix
  - _Requirements: 11.6_
- [x] 31. Apps stored in the database (`apps`, `app_env`); `--dev-apps` kept for tests only; `dootd ctl admin set-password` and `dootd reset-password`
  - _Requirements: 2.6_
- [x] 32. Phase 4 checkpoint: repo URL → live HTTPS app entirely in the browser
  - Automated: `scripts/e2e/phase4.sh` (83 checks through the edge with a fake Cloudflare API)

## Phase 5: Backups

- [x] 33. `s3` wrapper (multipart put, list, get, delete, connection test)
  - _Requirements: 6.3, 14.3_
- [x] 33b. Settings page: S3 endpoint, bucket and keys (sealed), test upload + delete
  - _Requirements: 3.1, 6.3_
- [x] 34. Snapshot: header detection, `VACUUM INTO`, `quick_check`
  - _Requirements: 14.2_
- [x] 35. Archive + manifest + upload with retries; scheduler (3 h), manual, pre-deploy hook
  - _Requirements: 14.1, 14.3_
- [x] 36. Retention (48 h, always keep the newest)
  - _Requirements: 14.4_
  - [ ]* 36.1 Unit tests for retention selection
- [x] 37. Restore flow with verification and `.pre-restore` folders (keep 2)
  - _Requirements: 15.1, 15.2, 15.3_
- [x] 38. Daily `dootd.db` backup, recovery kit download + reminder, failure badges
  - _Requirements: 14.5, 14.6_
- [x] 38b. Configurable `[backups] interval/retention`; local copies (2, plus un-uploaded ones during an outage); retry of failed uploads; `dootd ctl backup | backups | restore`; delete-app option to remove backups
  - _Requirements: 7.4, 14.1, 14.3_
- [x] 39. Phase 5 checkpoint: delete-and-restore test; retention verified on R2; a backup taken under heavy writes passes `integrity_check`
  - Automated: `scripts/e2e/phase5.sh` (55 checks, `rclone serve s3` as the bucket). Re-check against real R2 on the Phase 7 VPS run.

## Phase 6: Monitoring

- [x] 40. Collector (host `/proc` + `statfs`, cgroup stats, edge counters, supervisor stats) every 10 s
  - _Requirements: 16.1_
- [x] 41. 1 h ring buffer + `metrics_1m` 7-day rollups and pruning
  - _Requirements: 16.2_
- [x] 42. Server-rendered SVG charts for host and apps
  - _Requirements: 16.3_
- [x] 43. Warnings (disk, memory, OOM, crashed, backup failed, certificate expiring)
  - _Requirements: 16.4_
- [x] 44. Phase 6 checkpoint: numbers match `top`/`systemd-cgtop`; dootd RSS < 30 MB with 5 apps
  - _Requirements: 1.3_
  - Automated: `scripts/e2e/phase6.sh` (48 checks). Measured 28 MB RSS / 0.03 % CPU idle with 5 apps.
- [x] 44b. `[monitoring]` warning thresholds, `dootd ctl top`, `GET /v1/metrics`, memory tuning (soft heap limit)
  - _Requirements: 1.3, 16.4_

## Phase 7: Install, self-update, v1.0

- [x] 45. Full `install.sh` (directories, master key, systemd unit with `Delegate=yes`, `make`, swapfile and ufw prompts)
  - _Requirements: 2.4_
  - The unit is embedded in the binary (`contrib/systemd`) and installed by `dootd setup-host` (D28)
- [x] 46. `dootd init` interactive bootstrap (`dootd reset-password` done in task 31); also non-interactive with flags + env vars (D32)
  - _Requirements: 2.5, 2.6_
- [x] 46b. `dootd init --restore <recovery kit>`: rebuild a server from the kit and the bucket (kit now includes the region); `dootd ctl backup _dootd`
  - _Requirements: 14.6_
- [x] 47. Self-update: release check, verification, atomic swap, `.prev` + pre-update DB copy, `ExecStartPre` fallback (Settings → Updates, `dootd ctl update`)
  - _Requirements: 17.1–17.4_
- [x] 48. Hardening: fuzzing (toml, tar/xz, backup archives, recovery kit), `-race`, fd limits, restart timing
  - Unit tests for secrets, store, config, manifest, kit, archives, selfupdate. Fuzzing found and fixed a tar.xz extraction escape through chained symlinks
  - Automated: `scripts/e2e/phase7.sh` (89 checks): install, init (incl. a pty run), self-update, tampered release, rollback of a release that cannot start, destroy + `init --restore`, fd limits, restart of 6 apps in 1.4 s with 5 apps
- [ ] 49. 7-day soak test with 5 apps on a 1 GB VPS
- [ ] 50. Docs pass (README quick start, Cloudflare token walkthrough, troubleshooting); tag `v1.0.0`
