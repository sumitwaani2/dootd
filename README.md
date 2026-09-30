# dootd

A tiny, self-contained PaaS for hosting small server-rendered **Zig / C + SQLite** web apps on a single VPS.

One binary. No Docker, no Kubernetes, no Traefik, no extra reverse proxy. SSH in once and run one command; it prints a one-time password, and everything else happens in a web dashboard. One way to do each thing: no admin CLI, no config file.

```
Cloudflare (proxied, Full strict) ──HTTPS──► dootd (TLS + routing + dashboard + builds + backups) ──► your apps on 127.0.0.1
```

## What it does

- **Deploy from GitHub with a click.** Add a repo, set env vars, press **Deploy**. dootd clones the repo, builds it with the Zig version the app pins, and runs it.
- **Routing and TLS built in.** Requests are routed by Host header. dootd issues Cloudflare Origin CA certificates for Full (strict) mode.
- **Only Cloudflare can reach your apps.** dootd checks that traffic comes from Cloudflare IPs and verifies Authenticated Origin Pulls with its own CA.
- **Light isolation.** Each app runs as its own Linux user, with cgroup v2 limits on memory, CPU and process count.
- **SQLite backups to R2 or any S3-compatible storage.** Every 3 hours and before every deploy, kept for 48 hours, one folder per app in the bucket, with one-click restore. A new app can start from a backup folder, which is also how you move to a new server.
- **Monitoring and logs.** Host and per-app CPU and memory, request stats, live logs, and build logs.
- **Updates and recovery with the same command.** Re-running the installer updates dootd and prints a new one-time password.

## Scope

1 machine · 1 user · 1 GitHub account · 1 Cloudflare account · about 5 apps · Ubuntu 24.04+.

## Status

🚧 Phase 7 of 7: the simplification (one command, everything in the dashboard) is in progress. Still to do before v1.0.0: a 7-day soak test and a run against real Cloudflare and R2 on a VPS. Progress is tracked in the [Kiro spec](.kiro/specs/dootd/tasks.md).

## Quick start

```bash
# on a fresh Ubuntu 24.04+ VPS, once:
curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
```

Open the printed `https://<server IP>`, sign in with the printed one-time password, and follow the dashboard: your email and password, the Cloudflare token, the dashboard domain, then apps. [docs/setup.md](docs/setup.md) walks through it, including updates and moving to a new server.

### Test it on a VPS

```bash
git clone https://github.com/sumitwaani2/dootd && cd dootd
sudo ./scripts/e2e/phase1.sh   # ... phase7.sh
```

The scripts need Go, install dootd as a systemd service and drive it only through the installer and the dashboard (with a fake Cloudflare API and `rclone serve s3` as the bucket). They run on every PR on GitHub's Ubuntu 24.04 VMs.

## Development

Requires Go (the version is in `go.mod`; older Go versions download it automatically).

```bash
make build       # dist/dootd for this machine
make build-all   # static linux amd64 + arm64 binaries + checksums.txt
make lint        # gofmt, go vet, staticcheck
make test
```

Releases: push a `vX.Y.Z` tag and GitHub Actions publishes the binaries, `checksums.txt` and `install.sh`.

## Docs

| Doc | For |
|---|---|
| [docs/app-contract.md](docs/app-contract.md) | Writing a Zig/C app that dootd can host |
| [docs/architecture.md](docs/architecture.md) | How dootd works inside (for maintaining it long-term) |
| [docs/implementation-plan.md](docs/implementation-plan.md) | Phase-by-phase build plan |
| [docs/setup.md](docs/setup.md) | Install, first sign-in, Cloudflare token, updates, moving servers, troubleshooting |
| [.kiro/specs/dootd/](.kiro/specs/dootd/) | Kiro spec: requirements, design and task list |
