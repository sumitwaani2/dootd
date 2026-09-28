# Using dootd with your Cloudflare account

This is the one-time SSH setup on a VPS. After it, everything happens in the dashboard. Once `dootd init` exists (Phase 7), it will run these steps for you.

## 1. Create an API token

In Cloudflare, go to **My Profile → API Tokens → Create Token → Create Custom Token**, and give it these permissions:

| Scope | Permission | Used for |
|---|---|---|
| Zone → Zone | Read | finding the zone of each domain |
| Zone → DNS | Edit | proxied A/AAAA records |
| Zone → SSL and Certificates | Edit | Origin CA certificates and the AOP client certificate |
| Zone → Zone Settings | Edit | turning on Authenticated Origin Pulls; reading and setting the SSL mode |

Under **Zone Resources**, choose your domains or "All zones".

## 2. Configure dootd

`/etc/dootd/config.toml`:

```toml
[edge]
dashboard_domain = "dootd.example.com"
# public_ipv4 / public_ipv6 are detected automatically; set them if detection fails.
```

Then:

```bash
sudo systemctl restart dootd
printf '%s' 'YOUR_TOKEN' | sudo dootd ctl cloudflare-token   # verifies it and stores it encrypted
sudo dootd ctl edge sync                                      # DNS + certificate for the dashboard + AOP
sudo dootd ctl admin set-password --email you@example.com     # dashboard login (asks for the password)
```

The first sync for each zone waits until Cloudflare reports the AOP certificate as active, usually under a minute. Only then does dootd start requiring it.

Now open `https://dootd.example.com`, sign in, and:

1. **Settings → GitHub**: paste a fine-grained token with *Contents: Read-only* on your repos.
2. **Settings → Zones**: press **Set Full (strict)** if it's offered.
3. **Apps → Add app**: repo, branch, type, domain (e.g. `blog.example.com`). dootd creates the DNS record and certificate in the background.
4. Add env vars if needed, then press **Deploy** and watch the build log.
5. **Settings → Backups**: add an R2 bucket (endpoint `https://<account id>.r2.cloudflarestorage.com`, region `auto`, an R2 API token with Object Read & Write on the bucket) and download the recovery kit.

Over SSH, `sudo dootd ctl edge` shows the same status, and `sudo dootd reset-password` resets a forgotten password.

## 3. In the Cloudflare dashboard

- **SSL/TLS → Edge Certificates → Always Use HTTPS: On.** dootd has no port 80.
- If the server has a firewall, open only 22 and 443. dootd drops every connection that isn't from Cloudflare.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `no Cloudflare zone found for …` | The domain isn't on this account, or the token doesn't cover that zone |
| `… lacks the DNS: Edit permission` | Add the missing permission to the token |
| `has a CNAME record` | Delete the CNAME in Cloudflare; dootd creates the A record |
| Cloudflare error 526 | The SSL mode is Full (strict) but the certificate isn't installed yet; run `dootd ctl edge sync` |
| Cloudflare error 525 / 520 right after setup | AOP was toggled off in Cloudflare; the next sync turns it back on, or run `dootd ctl edge sync` |
