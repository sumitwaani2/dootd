# dootd App Contract

These are the rules a Zig or C app must follow to be hosted on dootd. Follow them and the app deploys with one click.

Contract version: **2**. Version 1 had dootd build apps on the server; since version 2 the app's own GitHub Actions workflow builds and tests it, and dootd only deploys the result (architecture D38).

---

## 1. Summary checklist

- [ ] The repo has a `dootd.toml` at the root with `contract = 2` and `run`. The repo name becomes the app name (e.g. `my_blog` → `my-blog`).
- [ ] The repo has the release workflow (`.github/workflows/release.yml`, copied from a sample, §2.2). Pushing a `v*` tag tests, builds and publishes a GitHub release.
- [ ] The release tarball holds everything the app needs at runtime: the binary and any templates or static files.
- [ ] The app serves plain HTTP on **`127.0.0.1:$PORT`**. No TLS.
- [ ] All SQLite files live in **`$DATA_DIR`**, and nothing else is written anywhere except `$TMPDIR`.
- [ ] `GET <health_path>` returns 2xx once the app is ready.
- [ ] On **SIGTERM**, the app finishes in-flight requests and exits within 10 s.
- [ ] Logs go to **stdout/stderr**, one line per entry, and stdout is line-buffered or flushed (see §7). The app does not daemonize or fork into the background.
- [ ] Secrets come from environment variables, never from the repo.

---

## 2. Releases

### 2.1 `dootd.toml`

It says how to **run** the app, and it travels inside every release tarball. Operational settings (domain, env vars, resource limits) are set in the dashboard.

```toml
contract    = 2
run         = "zig-out/bin/myapp"   # required: binary + arguments, relative to the tarball root
health_path = "/healthz"            # optional, default "/"
```

| Field | Required | Default | Notes |
|---|---|---|---|
| `contract` | yes | — | `2`. |
| `run` | yes | — | Binary path plus arguments. It is **not run through a shell**, so pipes, `&&` and `$VAR` expansion don't work. The binary must be a Linux ELF executable for the server's CPU. |
| `health_path` | no | `/` | Must return 2xx or 3xx within 30 s of start. |

Unknown keys are rejected, so typos fail the deploy instead of being ignored (`zig_version` and `build` from contract 1 included).

### 2.2 The release workflow

Copy the workflow of the matching sample into your repo as `.github/workflows/release.yml`:

| Language | Workflow |
|---|---|
| Zig | [`examples/sample-zig/.github/workflows/release.yml`](../examples/sample-zig/.github/workflows/release.yml) |
| C (compiled with `zig cc`) | [`examples/sample-c/.github/workflows/release.yml`](../examples/sample-c/.github/workflows/release.yml) |

Edit the `env:` block at its top:

| Variable | Meaning | Zig example | C example |
|---|---|---|---|
| `ZIG_VERSION` | The Zig release that builds the app (C too, via `zig cc`) | `0.16.0` | `0.16.0` |
| `TEST` | Your tests; they run first, and a failure publishes nothing | `zig build test` | `make test CC="zig cc"` |
| `CLEAN` | Removes the previous architecture's build output | `rm -rf zig-out` | `rm -rf build` |
| `BUILD` | Builds for `$TARGET` (`x86_64-linux-musl`, then `aarch64-linux-musl`) | `zig build -Doptimize=ReleaseSafe -Dtarget=$TARGET` | `make CC="zig cc -target $TARGET"` |
| `FILES` | What goes into the tarball next to `dootd.toml` (paths kept) | `zig-out/bin/myapp templates static` | `build/myapp static` |

Then release with:

```bash
git tag v1.0.0 && git push origin v1.0.0
```

The workflow runs `TEST`, builds both architectures, packages `app-linux-amd64.tar.gz` and `app-linux-arm64.tar.gz`, writes `checksums.txt`, starts the amd64 package exactly as dootd would (`HOST`, `PORT`, `DATA_DIR`, `TMPDIR`) and waits for `health_path`, and only then publishes the GitHub release. Nothing is deployed yet: the release appears in the app's **Deploy** menu in dootd, and you pick when.

- Tags must match `v*` and use only letters, digits, `.`, `_` and `-` (e.g. `v1.4.0`, `v1.4.0-rc1`); the tag becomes `DOOTD_RELEASE`.
- `musl` targets give static binaries, so they don't depend on the server's libc. If you need something like sqlite, compile it in from source (the sqlite amalgamation `sqlite3.c`, or a `build.zig.zon` dependency), as both samples do.
- Builds run on GitHub's runners (at least 7 GB RAM), never on your server. The free tier's 2,000 minutes a month on private repos is plenty; a sample release takes a few minutes.
- Private repos work: dootd downloads assets with the GitHub token from its settings (*Contents: Read-only*).
- The tarball is unpacked read-only (owned by root). Anything the app writes goes in `$DATA_DIR` or `$TMPDIR`.

---

## 3. Environment variables dootd provides

| Variable | Example | Meaning |
|---|---|---|
| `PORT` | `20001` | The port to listen on. It stays the same for an app across deploys. |
| `HOST` | `127.0.0.1` | The address to bind to. Always bind here, never to `0.0.0.0`. |
| `DATA_DIR` | `/var/lib/dootd/apps/blog/data` | Persistent, backed up, and survives every deploy. |
| `TMPDIR` | `/var/lib/dootd/apps/blog/tmp` | Scratch space that may be wiped on every deploy. |
| `DOOTD_APP` | `blog` | The app's name (from the repository name). |
| `DOOTD_DOMAIN` | `blog.example.com` | The public domain. |
| `DOOTD_RELEASE` | `v1.4.0` | The deployed release (its tag). |
| `DOOTD_CONTRACT` | `2` | The contract version. |

Your own env vars from the dashboard are added on top. Names starting with `DOOTD_`, and the names in the table above, are reserved. Changing env vars requires a **Restart**, not a new release.

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
| Release dir (the unpacked tarball, current working dir) | read-only | replaced | no |
| `$DATA_DIR` | read/write | ✅ yes | ✅ SQLite files |
| `$TMPDIR` | read/write | ❌ may be wiped | no |
| Everything else | no access | — | — |

- Static files and templates in the tarball can be read with relative paths, because the working directory is the release dir. List them in `FILES` (§2.2).
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

The std API changes between Zig versions, so check the docs for **your `ZIG_VERSION`**. In 0.14 / 0.15 it looks roughly like this:

```zig
const port_str = try std.process.getEnvVarOwned(allocator, "PORT");
const port = try std.fmt.parseInt(u16, port_str, 10);
const data_dir = try std.process.getEnvVarOwned(allocator, "DATA_DIR");
```

---

## 10. Minimal repo layout example

```
myapp/
├── .github/workflows/release.yml   # copied from a sample (§2.2)
├── dootd.toml
├── build.zig
├── build.zig.zon
├── src/main.zig
├── templates/     # in FILES; read at runtime via relative path
└── static/
```
