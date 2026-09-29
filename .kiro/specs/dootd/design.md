# Design Document

## Overview

dootd is one Go process, run as root by systemd. It terminates TLS on :443, routes requests by Host header to the dashboard or to app processes on `127.0.0.1`, builds apps from GitHub with a pinned Zig toolchain, supervises them in per-app cgroups under per-app users, and backs up their SQLite databases to S3-compatible storage.

**`docs/architecture.md` is the single source of truth for the detailed design.** This document summarises it, maps components to requirements, and pins down the interfaces that the implementation tasks depend on. If the two disagree, update `architecture.md` first, then this file.

- Architecture: #[[file:docs/architecture.md]]
- App contract: #[[file:docs/app-contract.md]]

## Architecture

```
Cloudflare ─:443─► edge (CF-IP filter → TLS/SNI + AOP mTLS → Host router)
                     ├─ dashboard host → web (auth, SSE)
                     └─ app host → ReverseProxy → 127.0.0.1:$PORT → app (own uid + cgroup)
deployer → github (clone) → toolchain (zig) → builder (build cgroup) → supervisor
backup (VACUUM INTO → tar+zstd → s3)     metrics (/proc, cgroup, edge counters)
store (dootd.db, migrations)             secrets (master.key, AES-256-GCM)
```

Key decisions (full list in architecture.md §17): Go with `CGO_ENABLED=0`; no containers; stop-then-start deploys; deploy only on click; Cloudflare Origin CA plus zone AOP with dootd's own CA plus a Cloudflare IP filter; snapshot backups every 3 h; pinned Zig per app, also used as the C compiler; server-rendered HTML with a small vanilla script and server-rendered SVG charts for the UI (no htmx, D9); self-update with the previous binary as start guard (D29).

## Components and Interfaces

| Package | Responsibility | Requirements |
|---|---|---|
| `cmd/dootd` | Subcommands: `serve`, `init [--restore]`, `setup-host`, `update`, `update-guard`, `ctl`, `reset-password`, `version` | 1.2, 2.4–2.6, 14.6, 17 |
| `internal/buildinfo` | Version, commit and date injected with `-ldflags` | 1.2 |
| `internal/store` | SQLite open, single writer, embedded migrations, settings | 4 |
| `internal/secrets` | Master key lifecycle, AEAD seal/open | 3 |
| `internal/auth` | argon2id, sessions, CSRF, rate limiting | 5 |
| `internal/web` | Dashboard handlers and templates (embedded) | 5–7, 9, 13–17 |
| `internal/edge` | Listener, IP filter, TLS, router, proxy, counters | 11, 12, 16 |
| `internal/cloudflare` | Zones, DNS, Origin CA, AOP, settings, IPs | 6.2, 7.2, 11 |
| `internal/github` | PAT validation, repo and branch listing, go-git clone | 6.1, 9.2 |
| `internal/toolchain` | Zig index, download, verify, cache | 8.2, 8.3 |
| `internal/builder` | Build in cgroup as the app user, build logs | 8 |
| `internal/deployer` | Deploy state machine, releases, rollback, queue | 9 |
| `internal/supervisor` | Process lifecycle, restart policy | 10 |
| `internal/cgroup`, `internal/users` | cgroup v2 tree, per-app users | 7.2, 10.1, 10.3, 10.6 |
| `internal/logs` | Rotating files, ring buffer, live fan-out | 13 |
| `internal/backup`, `internal/s3` | Snapshot, archive, upload, retention, restore | 14, 15 |
| `internal/metrics` | Sampling, rollups, warnings | 16 |
| `internal/selfupdate` | Release check, verify, swap, update marker, start guard and rollback | 17 |
| `contrib/systemd` | The systemd unit, embedded for `dootd setup-host` | 2.4 |

### Phase 0 interfaces (fixed now, used by every later phase)

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
- Key file: 64 lowercase hex characters plus a newline (32 bytes). Hex is used so the key can be copied into the recovery kit and back.
- Ciphertext format: `0x01 (format version) ‖ 12-byte random nonce ‖ AES-256-GCM(ciphertext+tag)`. `purpose` is the GCM associated data, for example `settings:github_pat` or `app_env:<app_id>:<NAME>` (Req 3.4).
- The key file is created with `O_CREATE|O_EXCL`, written, fsynced, then the parent directory is fsynced. If the file already exists, it is loaded and never overwritten (Req 3.2).
- `Load` rejects any mode that includes group or other bits (`mode & 0o077 != 0`) and any content that isn't exactly 64 hex characters (Req 3.3).

**store**
```go
func Open(ctx context.Context, path string) (*Store, error) // opens, migrates, returns ready store
func (s *Store) Writer() *sql.DB   // MaxOpenConns=1, _txlock=immediate
func (s *Store) Reader() *sql.DB   // pool, query_only
func (s *Store) SchemaVersion(ctx) (int, error)
func (s *Store) GetSetting(ctx, key) (value []byte, ok bool, err error)
func (s *Store) SetSetting(ctx, key string, value []byte) error
func (s *Store) Close() error
```
- DSN pragmas: `journal_mode(WAL)`, `busy_timeout(5000)`, `foreign_keys(1)`, `synchronous(NORMAL)`.
- Migrations are embedded from `internal/store/migrations/NNNN_name.sql`. On startup the runner checks that versions are unique and contiguous starting at 1. It then applies each pending migration inside one transaction together with the `INSERT INTO schema_migrations` row (Req 4.3, 4.4). If the stored max version is greater than the highest embedded version, it returns `ErrSchemaTooNew` (Req 4.5).
- Each phase adds its own tables in a new migration. Applied migrations are never edited.

## Data Models

Defined in architecture.md §7. Phase 0 creates only `schema_migrations(version, name, applied_at)` and `settings(key, value BLOB, updated_at)`.

## CI / Release

- `ci.yml` (PRs and pushes to `main`): `gofmt -l` must print nothing, then `go vet`, `staticcheck`, `go test -race ./...`, and a matrix build for `amd64` and `arm64` with `CGO_ENABLED=0 -trimpath`.
- `release.yml` (tags `v*`): builds `dist/dootd-linux-{amd64,arm64}` with ldflags, runs `sha256sum > checksums.txt`, then `gh release create` with the binaries, checksums and `install.sh`.
- `install.sh`, Phase 0 subset: checks OS, arch, systemd and cgroup v2, then downloads, verifies and installs the binary. Phase 7 extends it (Req 2.4).

## Error Handling

- Startup errors (bad key permissions, a failed migration, a newer schema) are fatal. They print one clear line and exit non-zero, and systemd shows them in `journalctl`.
- Runtime operations (build, deploy, backup, Cloudflare/GitHub calls) never crash the process. They record the error on the relevant row (deployment, backup) and surface it in the UI.
- Every external call gets a context deadline, and idempotent calls get bounded retries.

## Testing Strategy

- Unit tests for pure logic: secrets round-trip and tamper cases, migration ordering and failure, `dootd.toml` validation, IP range matching, retention selection, the deploy state machine with fakes.
- Integration tests on Linux CI for cgroup, user and supervisor behaviour, gated by a `DOOTD_INTEGRATION=1` env var because they need root.
- Manual "done when" checks on a real 1 GB Ubuntu 24.04 VPS at the end of each phase (see tasks.md).
- Fuzzing (Phase 7): the `dootd.toml` parser, the Zig tar.xz extraction (structured archives; nothing may be written outside the destination), backup archive extraction and the recovery kit parser. The seeds run with every `go test`; `go test -fuzz` runs them longer.

## Phase 7 interfaces

**selfupdate**
```go
type Updater struct{ Repo, API, Current, Binary, DataRoot string; HTTP *http.Client }
func (u *Updater) Latest(ctx) (Release, error)       // GitHub releases/latest
func (u *Updater) Apply(ctx, rel Release) error      // verify, dootd.prev, update.json, rename
func Newer(tag, current string) bool                 // semver; dev builds always update
func Guard(binary, dataRoot string) (string, error)  // run by dootd.prev before each start
func Finish(dataRoot, current string) (Result, bool, error)
type Service struct{ *Updater; Store; Busy func() string; Restart func(); Log }  // Check, Install, Status, FinishAfter
```

**store**: `PendingMigrations(ctx, path) (current, latest int, err error)` — lets `serve` copy dootd.db to `dootd.db.pre-update` before migrating (Req 17.3).

**secrets**: `Install(path, hexKey) (*Box, error)` — installs a known key (recovery kit); never overwrites a different one (`ErrKeyMismatch`).

**backup**: `Kit` with `Format` / `ParseKit` (the recovery kit), `KeyPrefix`, `Newest`, `Decompress`, `UnpackInto` (verify, then unpack an archive into a DATA_DIR) for `dootd init --restore`.
