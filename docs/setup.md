# Setting up dootd

One command over SSH, then everything happens in the browser.

## 1. Before you start

- A fresh Ubuntu 24.04+ VPS (1 GB RAM is enough) with SSH access.
- Your domains on your Cloudflare account. In the Cloudflare dashboard, turn on **SSL/TLS → Edge Certificates → Always Use HTTPS** for each zone (dootd has no port 80).
- A Cloudflare API token: **My Profile → API Tokens → Create Token → Create Custom Token**, with these permissions:

| Scope | Permission | Used for |
|---|---|---|
| Zone → Zone | Read | finding the zone of each domain |
| Zone → DNS | Edit | proxied A/AAAA records |
| Zone → SSL and Certificates | Edit | Origin CA certificates and the origin pull certificate |
| Zone → Zone Settings | Edit | turning on Authenticated Origin Pulls; reading and setting the SSL mode |

  Under **Zone Resources**, choose your domains or "All zones".

## 2. Install (the only SSH step)

```bash
curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
```

It asks nothing. At the end it prints something like:

```
Open https://203.0.113.5 in your browser.
Your browser will warn that the certificate is not trusted: that is expected for this first visit, continue anyway.
One-time password: k7qp9-xmz2r-4hwtc-8vdna   (works once, valid for 24 hours)
```

You can close SSH now.

## 3. In the browser

1. Open the address, accept the certificate warning, and sign in with the **one-time password** (leave the email empty).
2. **Set up your account**: your email and a password (at least 12 characters).
3. **Settings → Cloudflare**: paste the API token. dootd checks it and shows which zones it can use.
4. **Settings → Dashboard domain**: enter e.g. `dootd.example.com`. dootd creates the DNS record, the certificate and the origin pull setup, and shows the progress. When it is ready, open `https://dootd.example.com` and sign in with your email and password. From then on the IP address no longer answers.
5. **Settings → GitHub** (for private repos): a fine-grained token with *Contents: Read-only* on your repos.
6. **Settings → Backups**: an R2 bucket (endpoint `https://<account id>.r2.cloudflarestorage.com`, region `auto`, an R2 API token with Object Read & Write on that bucket).
7. **Apps → Add app**: pick the repo, branch, type and domain (e.g. `blog.example.com`). The app is named after the repo. Add env vars if needed, press **Deploy** and watch the build log.

The home page lists any step that is still missing.

## Updating, and when you are locked out

Run the same command again:

```bash
curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
```

It installs the latest dootd, keeps all apps and settings, restarts (a few seconds of downtime for the apps) and prints a new one-time password. Use it only if you need it, for example after forgetting your password or when the dashboard domain stopped working: for 24 hours the printed address accepts that one-time password.

## Moving to a new server

1. Shut the old server down (both would otherwise write to the same backup folders).
2. Install on the new server and do steps 1–6 above with the same tokens and bucket.
3. **Add app** for each app. The form shows the bucket's folders and preselects the one with the app's name: dootd restores its newest backup before the first deploy. DNS records follow automatically.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `no Cloudflare zone found for …` | The domain isn't on this account, or the token doesn't cover that zone |
| `… lacks the DNS: Edit permission` | Add the missing permission to the token |
| Dashboard domain stays "pending" | Cloudflare is still activating the origin pull certificate; it usually takes under a minute, the page refreshes by itself |
| `has a CNAME record` | Delete the CNAME in Cloudflare; dootd creates the A record |
| Cloudflare error 526 | The SSL mode is Full (strict) but the certificate isn't installed yet; press **Sync now** in Settings |
| Cloudflare error 525 / 520 right after setup | Origin pulls were turned off in Cloudflare; the next sync turns them back on, or press **Sync now** |
| Nothing answers at all | Re-run the install command and use the address and one-time password it prints |
