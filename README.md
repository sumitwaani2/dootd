# dootd

A tiny, self-contained PaaS for hosting small server-rendered **Zig / C + SQLite** web apps on a single VPS.

One binary. No Docker, no Kubernetes, no Traefik, no extra reverse proxy. SSH in once to install, then do everything from a web dashboard.

```
Cloudflare (proxied, Full strict) ──HTTPS──► dootd (TLS + routing + dashboard + builds + backups) ──► your apps on 127.0.0.1
```

## What it does

- **Deploy from GitHub with a click.** Add a repo, set env vars, press **Deploy**. dootd clones the repo, builds it with the Zig version the app pins, and runs it.
- **Routing and TLS built in.** Requests are routed by Host header. dootd issues Cloudflare Origin CA certificates for Full (strict) mode.
- **Only Cloudflare can reach your apps.** dootd checks that traffic comes from Cloudflare IPs and verifies Authenticated Origin Pulls with its own CA.
- **Light isolation.** Each app runs as its own Linux user, with cgroup v2 limits on memory, CPU and process count.
- **SQLite backups to R2 or any S3-compatible storage.** Every 3 hours and before every deploy, kept for 48 hours, with one-click restore.
- **Monitoring and logs.** Host and per-app CPU and memory, request stats, live logs, and build logs.

## Scope

1 machine · 1 user · 1 GitHub account · 1 Cloudflare account · about 5 apps · Ubuntu 24.04+.

## Status

🚧 Design phase. Nothing is built yet.

## Planned quick start

```bash
# on a fresh Ubuntu 24.04+ VPS, once:
curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
sudo dootd init     # asks for admin email/password, dashboard domain, Cloudflare API token
```

Then open `https://<dashboard-domain>` and never SSH again.

## Docs

| Doc | For |
|---|---|
| [docs/app-contract.md](docs/app-contract.md) | Writing a Zig/C app that dootd can host |
| [docs/architecture.md](docs/architecture.md) | How dootd works inside (for maintaining it long-term) |
| [docs/implementation-plan.md](docs/implementation-plan.md) | Phase-by-phase build plan |
