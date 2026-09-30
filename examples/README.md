# Sample apps

Two minimal apps that follow the [app contract](../docs/app-contract.md). They are used to test dootd, and they're the starting point for new apps: copy a folder into its own repository (the folder becomes the repo root) and push a tag.

| App | Language | Tests | Release build (per `$TARGET`) | Binary |
|---|---|---|---|---|
| `sample-zig/` | Zig 0.16.0 | `zig build test` | `zig build -Doptimize=ReleaseSafe -Dtarget=$TARGET` (SQLite via `build.zig.zon`) | `zig-out/bin/sample-zig` |
| `sample-c/` | C, compiled with `zig cc` | `make test CC="zig cc"` | `make CC="zig cc -target $TARGET"` (downloads and verifies the SQLite amalgamation) | `build/sample-c` |

Each has `.github/workflows/release.yml`: on a `v*` tag it runs the tests, builds static binaries for `x86_64-linux-musl` and `aarch64-linux-musl`, packages `app-linux-amd64.tar.gz`, `app-linux-arm64.tar.gz` and `checksums.txt`, starts the amd64 package as dootd would, and publishes the GitHub release. dootd's CI runs both workflows on every PR (without publishing) with `scripts/release-workflow.py`, which also works locally:

```bash
python3 scripts/release-workflow.py examples/sample-c     # needs zig 0.16.0 or downloads it; output in examples/sample-c/dist/
```

Both serve the same routes:

| Route | What it does |
|---|---|
| `GET /` | Counts a visit in `$DATA_DIR/app.db` and shows the total and the release |
| `GET /healthz` | Returns `ok`; this is the health check |
| `GET /alloc?mb=N` | Allocates and keeps N MB, to test the memory limit and OOM handling |
| `GET /crash` | Exits with code 1, to test restarts and the crashed state |

Run one locally without dootd:

```bash
cd sample-c && make CC="zig cc"
mkdir -p /tmp/data && HOST=127.0.0.1 PORT=8080 DATA_DIR=/tmp/data ./build/sample-c
```
