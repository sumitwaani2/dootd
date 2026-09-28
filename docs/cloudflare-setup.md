# Using dootd with your Cloudflare account

This covers the Phase 3 setup on a VPS. Once `dootd init` exists (Phase 7), it will run these steps for you.

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

In `/etc/dootd/dev-apps.toml`, give each app a `domain = "blog.example.com"`. Then:

```bash
sudo systemctl restart dootd
printf '%s' 'YOUR_TOKEN' | sudo dootd ctl cloudflare-token   # verifies it and stores it encrypted
sudo dootd ctl edge sync                                      # DNS + certificates + AOP
sudo dootd ctl edge                                           # status and warnings
sudo dootd ctl edge set-strict example.com                    # if it warns about the SSL mode
```

The first sync for each zone waits until Cloudflare reports the AOP certificate as active, usually under a minute. Only then does dootd start requiring it.

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
