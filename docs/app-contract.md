# dootd App Contract

These are the rules a Zig or C app must follow to be hosted on dootd. Follow them and the app deploys with one click.

Contract version: **1**

---

## 1. Summary checklist

- [ ] Repo has a `dootd.toml` at the root, with an exact `zig_version`.
- [ ] `build` produces a runnable binary, and `run` points to it.
- [ ] The app serves plain HTTP on **`127.0.0.1:$PORT`**. No TLS.
- [ ] All SQLite files live in **`$DATA_DIR`**, and nothing else is written anywhere except `$TMPDIR`.
- [ ] `GET <health_path>` returns 2xx once the app is ready.
- [ ] On **SIGTERM**, the app finishes in-flight requests and exits within 10 s.
- [ ] Logs go to **stdout/stderr**, one line per entry, and stdout is line-buffered or flushed (see §7). The app does not daemonize or fork into the background.
- [ ] Secrets come from environment variables, never from the repo.

---

## 2. `dootd.toml`

Put this file at the repo root. It describes how to **build and run** the app. Operational settings such as the domain, env vars and resource limits are set in the dashboard, not here.

### Zig app

```toml
contract    = 1
zig_version = "0.14.1"                             # required, exact version
build       = "zig build -Doptimize=ReleaseSafe"   # optional, this is the default for type=zig
run         = "zig-out/bin/myapp"                  # required, relative to repo root
health_path = "/healthz"                           # optional, default "/"
```

### C app

```toml
contract    = 1
zig_version = "0.14.1"             # required: C is compiled with `zig cc` from this pinned Zig
build       = "make"               # optional, this is the default for type=c
run         = "build/myapp --foo"  # required
health_path = "/healthz"
```

| Field | Required | Default | Notes |
|---|---|---|---|
| `contract` | yes | — | Always `1` for now. |
| `zig_version` | yes | — | Exact release, for example `0.14.1`. dootd downloads it, verifies its checksum and caches it. The deploy fails if this version doesn't exist. |
| `build` | no | `zig build -Doptimize=ReleaseSafe` (zig) / `make` (c) | Runs with `/bin/sh -c` from the app root, so `&&` and `;` work. |
| `run` | yes | — | Binary path plus arguments. It is **not run through a shell**, so pipes, `&&` and `$VAR` expansion don't work. |
| `health_path` | no | `/` | Must return 2xx or 3xx within 30 s of start. |

The app type (`zig` or `c`) is chosen in the dashboard. It only changes the default `build` command.

**App root.** By default `dootd.toml` sits at the repo root. For a monorepo you can set an app **path** (for example `apps/blog`) in dootd; then `dootd.toml` lives in that folder, and the build, `run` and the app's working directory are all relative to it. Unknown keys in `dootd.toml` are rejected, so typos fail the deploy instead of being ignored.

### Build environment

- The pinned `zig` is first on `PATH`.
- `CC="zig cc"` and `CXX="zig c++"` are set, so plain Makefiles compile with Zig's toolchain.
- `make` is available on the host. **No other system libraries are installed.** If you need something like sqlite, add it as source (for example the sqlite amalgamation `sqlite3.c`) or as a Zig package dependency in `build.zig.zon`.
- Outbound network is allowed during the build, so `zig build` can fetch dependencies from `build.zig.zon`.
- Default build limits are **1 GB RAM** and a **15 minute** timeout. Both can be changed per app in the dashboard.
- Your env vars are **also available during the build**.
- The build runs as the app's own user (`dootd-<app>`). `HOME`, `TMPDIR`, `ZIG_GLOBAL_CACHE_DIR` and `ZIG_LOCAL_CACHE_DIR` point to a per-app cache that survives between deploys, so rebuilds (including `zig cc` C compiles and `build.zig.zon` packages) are fast. `DOOTD_BUILD=1` is set.
- The checkout is shallow (depth 1) and `.git` is removed, so the build can't read git history or run `git describe`.
- After the build, the whole tree becomes root-owned and read-only for the app. Anything the app must write at runtime goes in `$DATA_DIR` or `$TMPDIR`.

---

## 3. Environment variables dootd provides

| Variable | Example | Meaning |
|---|---|---|
| `PORT` | `20001` | The port to listen on. It stays the same for an app across deploys. |
| `HOST` | `127.0.0.1` | The address to bind to. Always bind here, never to `0.0.0.0`. |
| `DATA_DIR` | `/var/lib/dootd/apps/blog/data` | Persistent, backed up, and survives every deploy. |
| `TMPDIR` | `/var/lib/dootd/apps/blog/tmp` | Scratch space that may be wiped on every deploy. |
| `DOOTD_APP` | `blog` | The app's name. |
| `DOOTD_DOMAIN` | `blog.example.com` | The public domain. |
| `DOOTD_RELEASE` | `20260927-141500-a1b2c3d` | The deployed release ID (timestamp + git SHA). |
| `DOOTD_CONTRACT` | `1` | The contract version. |

Your own env vars from the dashboard are added on top. Names starting with `DOOTD_`, and the names in the table above, are reserved. Changing env vars requires a **Restart**, but not a rebuild.

---

## 4. HTTP rules

- Serve **plain HTTP/1.1**. dootd handles TLS in front of your app.
- Headers you can rely on:
  - `Host`: your domain.
  - `X-Forwarded-Proto: https` and `X-Forwarded-Host` (your domain).
  - `X-Forwarded-For` and `X-Real-IP`: the **real visitor IP**, taken from Cloudflare's `CF-Connecting-IP`. Whatever the visitor sent in these headers is replaced, so they are safe to trust.
  - `CF-Connecting-IP` and `CF-IPCountry`, passed through from Cloudflare.
- WebSockets, long-polling and streamed responses (SSE) work; responses are flushed to the visitor immediately.
- Cloudflare limits request bodies to 100 MB on the free plan, so keep uploads smaller than that.
- While a deploy is running, visitors see a short dootd "deploying" 503 page. Expect about 1–3 s of downtime, plus however long your app takes to become healthy.
- While your app is stopped or crashed, visitors get a dootd 503 page; if it stops answering unexpectedly, a 502.

---

## 5. Filesystem rules

| Path | Access | Survives deploy? | Backed up? |
|---|---|---|---|
| Release dir (current working dir) | read-only | replaced | no |
| `$DATA_DIR` | read/write | ✅ yes | ✅ SQLite files |
| `$TMPDIR` | read/write | ❌ may be wiped | no |
| Everything else | no access | — | — |

- Static files and templates in your repo can be read with relative paths, because the working directory is the release dir.
- **Only SQLite databases in `$DATA_DIR` are backed up** (sub-folders included). dootd finds them by their file header, whatever their name. Other files in `$DATA_DIR`, such as user uploads, are kept across deploys but **not** backed up in v1.
- Don't create folders named `.pre-restore-*` in `$DATA_DIR`: dootd uses them to keep the previous databases after a restore.

---

## 6. SQLite rules

1. Open databases **only** from `$DATA_DIR`, for example `$DATA_DIR/app.db`.
2. Recommended settings on every connection:
   ```sql
   PRAGMA journal_mode = WAL;
   PRAGMA busy_timeout = 5000;
   PRAGMA foreign_keys = ON;
   PRAGMA synchronous = NORMAL;
   ```
   Backups read the database while your app is running. `busy_timeout` makes sure a backup never causes a "database is locked" error in your app.
3. **Run migrations at startup**, before you start listening. dootd takes a backup right before every deploy, so a bad migration can be undone with **Restore** in the dashboard.
4. Migrations should only move forward, and ideally stay compatible with the previous release. Rolling back the code does **not** roll back the database.

---

## 7. Lifecycle

```
start ──► app runs migrations ──► listens on HOST:PORT ──► health_path returns 2xx ──► receives traffic
                                                                                           │
SIGTERM (deploy / restart / stop) ──► finish requests, close DB ──► exit 0 ◄──────────────┘
                                  └─► after 10 s: SIGKILL
```

- dootd starts your binary directly and supervises it. If it crashes, dootd restarts it with backoff (1 s, 2 s, 4 s … up to 60 s). After 5 crashes in 5 minutes, the app is marked **crashed** and left stopped.
- Don't daemonize, double-fork or write PID files.
- When the main process exits, dootd kills anything else still running in the app's cgroup.
- stdout is a pipe, not a terminal, so C's stdio buffers it fully by default. Call `setvbuf(stdout, NULL, _IOLBF, 0)` at startup (or write logs to stderr), otherwise log lines appear late or are lost on a crash. `std.debug.print` in Zig writes unbuffered to stderr.
- Lines longer than 16 KB are split into several log lines.

---

## 8. Resource limits (defaults, editable per app)

| Limit | Default |
|---|---|
| Memory (`memory.max`) | 256 MB. The app is OOM-killed and restarted if it goes above this. |
| CPU (`cpu.max`) | 1 core |
| Processes/threads (`pids.max`) | 256 |
| Open files | 4096 |

---

## 9. Reading the contract in code

### C

```c
#include <stdlib.h>

const char *host     = getenv("HOST");      // "127.0.0.1"
int         port     = atoi(getenv("PORT"));
const char *data_dir = getenv("DATA_DIR");
// db path: snprintf(path, sizeof path, "%s/app.db", data_dir);
```

Handle SIGTERM:

```c
#include <signal.h>
static volatile sig_atomic_t stop = 0;
static void on_term(int s) { (void)s; stop = 1; }
// in main: signal(SIGTERM, on_term);  then have your accept loop check `stop`
```

### Zig

The std API changes between Zig versions, so check the docs for **your pinned version**. In 0.14 / 0.15 it looks roughly like this:

```zig
const port_str = try std.process.getEnvVarOwned(allocator, "PORT");
const port = try std.fmt.parseInt(u16, port_str, 10);
const data_dir = try std.process.getEnvVarOwned(allocator, "DATA_DIR");
```

---

## 10. Minimal repo layout example

```
myapp/
├── dootd.toml
├── build.zig
├── build.zig.zon
├── src/main.zig
├── templates/     # read at runtime via relative path
└── static/
```
