# Design Document

## Overview

dootd is one Go process, run as root by systemd. It terminates TLS on :443, routes requests by Host header to the dashboard or to app processes on `127.0.0.1`, deploys apps from their GitHub releases (built and tested by each app's own GitHub Actions workflow; dootd never builds), supervises them in per-app cgroups under per-app users, and backs up their SQLite databases to S3-compatible storage.

The user touches exactly two things: the installer (once over SSH, again to update or recover) and the dashboard (everything else). There is no config file and no admin CLI.

**`docs/architecture.md` is the single source of truth for the detailed design.** This document summarises it, maps components to requirements, and pins down the interfaces the tasks depend on. If the two disagree, update `architecture.md` first, then this file.

- Architecture: #[[file:docs/architecture.md]]
- App contract: #[[file:docs/app-contract.md]]

## Architecture

```
Cloudflare ─:443─► edge (CF-IP filter → TLS/SNI + AOP mTLS → Host router)
                     ├─ dashboard host → web (auth, SSE)
                     └─ app host → ReverseProxy → 127.0.0.1:$PORT → app (own uid + cgroup)
anyone ─:443─► (only while setup is open) self-signed TLS → web
deployer → github (releases, asset download) → artifact (verify, unpack, ELF check) → supervisor
backup (VACUUM INTO → tar+zstd → s3 <app>/)   metrics (/proc, cgroup, edge counters)
store (dootd.db, migrations)                  secrets (master.key, AES-256-GCM)
```

Key decisions (full list in architecture.md §17): Go with `CGO_ENABLED=0`; no containers; stop-then-start deploys; deploy only on click; Cloudflare Origin CA plus zone AOP with dootd's own CA plus a Cloudflare IP filter; snapshot backups every 3 h; apps built in GitHub Actions and deployed from GitHub releases (D38–D40); server-rendered HTML with a small vanilla script and SVG charts; **the dashboard is the only interface** (D33); first sign-in through a one-time password on the setup address (D34); updates by re-running the installer (D35).

## Components and Interfaces

| Package | Responsibility | Requirements |
|---|---|---|
| `cmd/dootd` | `serve` (systemd), `setup-host` (installer), `version` | 1.2, 2 |
| `internal/buildinfo` | Version, commit and date injected with `-ldflags` | 1.2 |
| `internal/store` | SQLite open, single writer, embedded migrations, settings | 5 |
| `internal/secrets` | Master key lifecycle, AEAD seal/open | 4 |
| `internal/auth` | argon2id, sessions, CSRF, rate limiting, one-time password | 2.4–2.5, 3.3, 6 |
| `internal/web` | Dashboard handlers and templates (embedded) | 2.5, 6–8, 10, 14–17 |
| `internal/edge` | Listener, IP filter, setup access, TLS, router, proxy, counters, dashboard domain | 3, 7.2, 7.5, 12, 13, 17 |
| `internal/cloudflare` | Zones, DNS, Origin CA, AOP, settings, IPs | 7.1, 8.3, 12 |
| `internal/github` | PAT validation, repo listing, releases, asset downloads | 7.3, 9.3, 10.2 |
| `internal/artifact` | Checksum, safe tarball unpacking, ELF architecture check | 9.3–9.5 |
| `internal/manifest` | `dootd.toml` (contract 2) | 9.5 |
| `internal/deployer` | Deploy state machine, kept releases, rollback, queue | 10 |
| `internal/supervisor` | Process lifecycle, restart policy | 11 |
| `internal/cgroup`, `internal/users` | cgroup v2 tree, per-app users (runtime isolation) | 8.3, 11 |
| `internal/apps` | App registry, name from repo, restore on add | 8 |
| `internal/logs` | Rotating files, ring buffer, live fan-out | 14 |
| `internal/backup`, `internal/s3` | Snapshot, archive, upload, retention, restore, bucket folders | 8.4, 15, 16 |
| `internal/metrics` | Sampling, rollups, warnings | 17 |
| `contrib/systemd` | The systemd unit, embedded for `dootd setup-host` | 2.3 |

### Core interfaces

**buildinfo**
```go
var Version = "dev"; var Commit = "unknown"; var Date = "unknown" // set via -ldflags -X
func String() string // "dootd v0.1.0 (commit abc1234, built 2026-09-27T12:00:00Z, go1.27.1 linux/amd64)"
```

**secrets**
```go
func LoadOrCreate(path string) (*Box, error) // creates 0600 key atomically if missing; validates perms/format
func Load(path string) (*Box, error)         // never creates
func (b *Box) Seal(plaintext []byte, purpose string) ([]byte, error)
func (b *Box) Open(ciphertext []byte, purpose string) ([]byte, error)
```
- Key file: 64 lowercase hex characters plus a newline (32 bytes).
- Ciphertext format: `0x01 ‖ 12-byte random nonce ‖ AES-256-GCM(ciphertext+tag)`. `purpose` is the associated data, e.g. `settings:github_token` or `app_env:<app>:<NAME>`.
- Created with `O_CREATE|O_EXCL`, fsynced, never overwritten. `Load` rejects group/other permission bits and anything but 64 hex characters.

**store**
```go
func Open(ctx context.Context, path string) (*Store, error) // opens, migrates, returns ready store
func (s *Store) Writer() *sql.DB   // MaxOpenConns=1, _txlock=immediate
func (s *Store) Reader() *sql.DB   // pool, query_only
func (s *Store) GetSetting(ctx, key) (value []byte, ok bool, err error)
func (s *Store) SetSetting(ctx, key string, value []byte) error
```
- Migrations are embedded (`internal/store/migrations/NNNN_name.sql`), contiguous from 1, each applied in one transaction; a newer stored schema returns `ErrSchemaTooNew`. Applied migrations are never edited.

**auth: one-time password** (Req 2.4, 2.5, 3.3)
```go
func NewSetupPassword(ctx, db *store.Store) (string, error) // setup-host: random password, stores argon2id hash + expiry (24 h)
func (a *Auth) SetupUntil() time.Time                        // zero when none is pending (cached in memory)
func (a *Auth) Login(ctx, email, password, ip, ua string, adminAllowed bool) (tok string, setup bool, err error)
func (a *Auth) CompleteSetup(ctx, email, password, ip, ua string) (tok string, err error) // SetAdmin + consume + new session
func (a *Auth) ChangeEmail(ctx, cur *Session, password, email, ip string) error
```
A session created with the one-time password carries `setup = 1` and may only reach `/setup` and `/logout`.

**edge: setup access and dashboard domain** (Req 3, 7.2)
```go
func (m *Manager) SetDashboardHost(ctx, host string) error // validate, store setting, re-route, sync in background, clean up the old host
func (m *Manager) DashboardHost() string
func (m *Manager) DashboardReady() bool                    // certificate present and AOP enforced for its zone
func IsDirect(r *http.Request) bool                        // request arrived on a setup (non-Cloudflare) connection
```
`Manager.SetupOpen` is a callback (`!DashboardReady() || time.Now().Before(auth.SetupUntil())`). The listener lets a non-Cloudflare connection through only while it returns true and tags it; TLS for tagged connections uses a self-signed certificate (`/var/lib/dootd/setup/`) with no client-certificate requirement; the router sends tagged requests to the dashboard only.

**github: releases** (Req 9, 10.2)
```go
func (a *API) Releases(ctx, owner, repo string, n int) ([]Release, error)   // newest first, drafts skipped
func (a *API) ReleaseByTag(ctx, owner, repo, tag string) (Release, error)
func (a *API) Download(ctx, owner, repo string, assetID int64, dst string, max int64) error // asset API, follows the signed redirect without the token
```

**artifact** (Req 9.3–9.5)
```go
func Checksum(sums []byte, name string) (string, error)       // the SHA-256 for name in checksums.txt
func Unpack(tarGz, dst string, max int64) error              // safe extraction (fuzzed)
func CheckELF(path, goarch string) error                     // executable, ELF, machine matches
```

**backup: bucket folders** (Req 8.4, 15.3)
```go
func (s *Service) Folders(ctx) ([]string, error)                        // top-level folders in the bucket
func (s *Service) RestoreFolder(ctx, app, folder string) ([]string, error) // newest archive of folder → verified → DATA_DIR
```
Object key: `<app>/<UTC time>-<kind>-<id>.tar.zst`.

## Data Models

Defined in architecture.md §7.

## CI / Release

- `ci.yml` (PRs and pushes to `main`): `gofmt -l` must print nothing, then `go vet`, `staticcheck`, `go test -race ./...`, and a matrix build for `amd64` and `arm64` with `CGO_ENABLED=0 -trimpath`.
- `e2e.yml` (PRs): the phase scripts on GitHub's Ubuntu 24.04 VMs, driving the dashboard with curl.
- `release.yml` (tags `v*`): binaries, `checksums.txt` and `install.sh` as release assets.

## Error Handling

- Startup errors (bad key permissions, a failed migration, a newer schema) are fatal: one clear line, non-zero exit, visible in `journalctl`.
- Runtime operations (build, deploy, backup, Cloudflare/GitHub calls) never crash the process. They record the error on the relevant row and surface it in the UI.
- Every external call gets a context deadline, and idempotent calls get bounded retries.

## Testing Strategy

- Unit tests for pure logic: secrets, migrations, `dootd.toml` validation, release tarball extraction (fuzzed), the releases client (against `httptest`), backup archives (fuzzed), retention selection, app names from repo names, the one-time password.
- End-to-end scripts on real Ubuntu VMs for everything involving systemd, cgroups, users, TLS and the dashboard. Test-only environment variables (`DOOTD_TEST_*`, architecture §18) point dootd at fake Cloudflare and GitHub APIs and shorten the backup schedule; they are not a user feature.
- Manual check on a real VPS with real Cloudflare and R2 before v1.0.
