# Setting up a server with your Cloudflare account

The only SSH session you need: install, then `dootd init`. After that, everything happens in the dashboard.

## 1. Put the domain on Cloudflare

The dashboard domain (e.g. `dootd.example.com`) and every app domain must be in a zone on your Cloudflare account. In the Cloudflare dashboard, turn on **SSL/TLS → Edge Certificates → Always Use HTTPS** for each zone (dootd has no port 80).

## 2. Create an API token

**My Profile → API Tokens → Create Token → Create Custom Token**, with these permissions:

| Scope | Permission | Used for |
|---|---|---|
| Zone → Zone | Read | finding the zone of each domain |
| Zone → DNS | Edit | proxied A/AAAA records |
| Zone → SSL and Certificates | Edit | Origin CA certificates and the AOP client certificate |
| Zone → Zone Settings | Edit | turning on Authenticated Origin Pulls; reading and setting the SSL mode |

Under **Zone Resources**, choose your domains or "All zones". dootd only uses API tokens: the legacy Origin CA service keys, which Cloudflare removes after 30 Sep 2026, are not needed.

## 3. Install and run `dootd init`

On a fresh Ubuntu 24.04+ VPS:

```bash
curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
sudo dootd init
```

The installer checks the host, verifies the download against `checksums.txt`, installs `make`, creates `/etc/dootd`, `/var/lib/dootd` and the master key, installs the systemd unit, and offers a 2 GB swapfile and a ufw firewall (SSH + 443 only).

`dootd init` asks for:

1. The admin email and password (at least 12 characters).
2. The dashboard domain.
3. The API token. It is verified, the zone is found, and the permissions dootd needs are checked. If the zone isn't in Full (strict) mode, you're asked whether to switch it.
4. The public IPv4/IPv6, detected for you to confirm.

It then creates the proxied DNS record, the Origin CA certificate and Authenticated Origin Pulls for the dashboard. It waits until Cloudflare reports the AOP certificate active (usually under a minute) and starts the service. Running it again later is allowed: it asks first and replaces the admin password.

For automation, pass everything with flags and environment variables. Without a terminal, `dootd init` asks nothing and fails if an answer is missing:

```bash
sudo DOOTD_ADMIN_PASSWORD='…' DOOTD_CLOUDFLARE_TOKEN='…' \
  dootd init --email you@example.com --domain dootd.example.com --yes [--ipv4 A.B.C.D] [--ipv6 off] [--ssl-strict]
```

## 4. In the dashboard

Open `https://dootd.example.com`, sign in, and:

1. **Settings → GitHub**: paste a fine-grained token with *Contents: Read-only* on your repos.
2. **Settings → Backups**: add an R2 bucket (endpoint `https://<account id>.r2.cloudflarestorage.com`, region `auto`, an R2 API token with Object Read & Write on the bucket), then **download the recovery kit** and keep it offline.
3. **Apps → Add app**: repo, branch, type and domain (e.g. `blog.example.com`). dootd creates the DNS record and certificate in the background.
4. Add env vars if needed, then press **Deploy** and watch the build log.

Over SSH, `sudo dootd ctl edge` shows the same status, `sudo dootd ctl edge sync` syncs now, and `sudo dootd reset-password` resets a forgotten password.

## Moving to a new server

With the recovery kit and the bucket's access key:

```bash
curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
sudo dootd init --restore dootd-recovery-kit-<host id>.txt
```

It asks for the S3 access key and secret (or `--s3-access-key` and `DOOTD_S3_SECRET`). It downloads the newest `dootd.db` backup and every app's newest backup, and confirms the new server's IPs. Then it points the DNS records at the new server, issues new certificates, sets up AOP again and starts dootd. Finally it queues a deploy of every app that was running. Settings, env vars, tokens, the admin password and backup history all come back. Once DNS has updated, shut the old server down if it is still running, because both would write to the same place in the bucket.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `no Cloudflare zone found for …` | The domain isn't on this account, or the token doesn't cover that zone |
| `… lacks the DNS: Edit permission` | Add the missing permission to the token |
| `authenticated origin pulls are not active … yet` | Cloudflare is still activating the certificate; run `dootd init` again in a few minutes |
| `has a CNAME record` | Delete the CNAME in Cloudflare; dootd creates the A record |
| `could not detect the public IPv4` | Pass `--ipv4` (and `--ipv6 off` or the address) |
| Cloudflare error 526 | The SSL mode is Full (strict) but the certificate isn't installed yet; run `dootd ctl edge sync` |
| Cloudflare error 525 / 520 right after setup | AOP was toggled off in Cloudflare; the next sync turns it back on, or run `dootd ctl edge sync` |
| Dashboard unreachable after an update | See `journalctl -u dootd`. A version that fails to start 3 times is rolled back automatically; `sudo dootd ctl update` shows the result |
