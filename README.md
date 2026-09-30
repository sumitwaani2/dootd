# dootd

A tiny, self-contained PaaS for hosting small server-rendered **Zig / C + SQLite** web apps on a single VPS behind Cloudflare.

One binary. No Docker, no extra reverse proxy. SSH in once and run one command; it prints a one-time password, and everything else happens in a web dashboard.

```
Cloudflare (proxied, Full strict) ──HTTPS──► dootd (TLS + routing + dashboard + deploys + backups) ──► your apps on 127.0.0.1
```

- **Deploy GitHub releases with a click.** Each app's own GitHub Actions workflow tests and builds it when you push a tag; dootd lists the releases and **Deploy** downloads, verifies and runs the one you pick. The server never compiles anything. Rollback is one click too.
- **Only your Cloudflare account can reach your apps**: Cloudflare Origin CA certificates (Full strict), a Cloudflare-only IP filter and Authenticated Origin Pulls with dootd's own certificate.
- **Light isolation**: each app runs as its own Linux user with memory, CPU and process limits.
- **SQLite backups to R2 or any S3-compatible bucket** every 3 hours and before every deploy, kept for 48 hours, with one-click restore.
- **Monitoring and logs**: CPU, memory, requests and response times per app, live logs, deploy logs.

Scope: 1 server (Ubuntu 24.04+, 1 GB RAM is enough), 1 user, about 5 apps. Status: **v1.0.0**.

## Before you start

- A fresh Ubuntu 24.04+ VPS with SSH access.
- Your domains on your Cloudflare account. For each zone, turn on **SSL/TLS → Edge Certificates → Always Use HTTPS** (dootd has no port 80).
- A Cloudflare API token (**My Profile → API Tokens → Create Custom Token**) for your zones with:

| Permission | Used for |
|---|---|
| Zone → Zone → Read | finding the zone of each domain |
| Zone → DNS → Edit | proxied DNS records |
| Zone → SSL and Certificates → Edit | Origin CA certificates, the origin pull certificate |
| Zone → Zone Settings → Edit | turning on Authenticated Origin Pulls, reading and setting the SSL/TLS mode |

- Optional: a GitHub fine-grained token with *Contents: Read-only* on your app repos (needed for private repos), and an R2 bucket with an R2 API token (*Object Read & Write* on that bucket).

## Install

```bash
curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
```

It asks nothing, takes a few seconds and ends with the address to open (`https://<server IP>`) and a **one-time password**. You can close SSH now.

## First steps in the browser

1. Open the address, accept the certificate warning (expected on this first visit), and sign in with the **one-time password**, leaving the email empty.
2. **Set up your account**: your email and a password (at least 12 characters).
3. **Settings → Cloudflare**: paste the API token. dootd checks it and lists the zones it can use and any missing permission.
4. **Settings → Dashboard domain**: e.g. `dootd.example.com`. dootd creates the DNS record, the certificate and the origin pull setup and shows the progress; the first time for a zone this takes about 10 minutes. If the zone's SSL/TLS mode isn't Full (strict), press **Set Full (strict)** under **Zones**. Then open `https://dootd.example.com` and sign in; from then on the IP address no longer answers.
5. **Settings → GitHub** and **Settings → Backups** (endpoint `https://<account id>.r2.cloudflarestorage.com`, region `auto`, bucket, keys).
6. **Your app repo** follows [docs/app-contract.md](docs/app-contract.md): a `dootd.toml` and the release workflow. Push a tag (`git tag v1.0.0 && git push origin v1.0.0`) and GitHub publishes a release a few minutes later.
7. **Apps → Add app**: pick the repo and a domain (e.g. `blog.example.com`). The app is named after the repo. Add env vars if needed, choose a release under **Deploy** and watch the deploy log.

The home page always lists what is still missing.

## Updating, and when you are locked out

Run the install command again. It installs the latest dootd, keeps all apps and settings, restarts them (a few seconds of downtime) and prints a new one-time password. Use that password only if you need it, e.g. after forgetting your password or when the dashboard domain stops working: for 24 hours the server's IP address accepts it.

## Moving to a new server

1. Shut the old server down (both would otherwise write to the same backup folders).
2. Install on the new server and repeat steps 1–5 with the same tokens and bucket.
3. **Add app** for each app. The form lists the bucket's folders and preselects the one with the app's name: dootd restores its newest backup before the first deploy. DNS records follow automatically.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `no Cloudflare zone found for …` when saving a domain | The domain isn't on this Cloudflare account, is misspelled, or the token doesn't cover that zone |
| `… lacks the … permission` | Add the missing permission to the token, then **Sync now** in Settings |
| Dashboard domain stays "being set up" | The page says what it waits for: Cloudflare rolling out the origin pull certificate (about 10 minutes the first time), a zone in Flexible mode (**Set Full (strict)**), or a token without Zone Settings permission |
| Cloudflare error 521 | The SSL/TLS mode is Flexible or Off, so Cloudflare connects to port 80: **Settings → Zones → Set Full (strict)**. A Page Rule or Configuration Rule can also set Flexible for one hostname; remove it |
| Cloudflare error 526 | Full (strict) but the certificate isn't installed yet: **Sync now** in Settings |
| Cloudflare error 525 / 520 | Origin pulls were changed in Cloudflare; dootd repairs them on the next sync (**Sync now**) and waits a few minutes before requiring them again |
| `has a CNAME record` | Delete the CNAME in Cloudflare; dootd creates the A record |
| Nothing answers at all | Re-run the install command and use the address and one-time password it prints |

## Development

Requires Go (version in `go.mod`).

```bash
make build       # dist/dootd for this machine
make build-all   # static linux amd64 + arm64 binaries + checksums.txt
make lint        # gofmt, go vet, staticcheck
make test        # go test -race ./...
```

Push a `vX.Y.Z` tag to release. How dootd works inside, and how to check a change on a real server before releasing it: [docs/architecture.md](docs/architecture.md).
