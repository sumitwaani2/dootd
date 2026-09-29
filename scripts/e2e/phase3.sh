#!/usr/bin/env bash
# Phase 3 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# dootd runs on :443 against a fake Cloudflare API (see lib.sh), set up
# through the dashboard. Verifies: token handling, DNS records, Origin CA
# issuance + renewal + revocation, AOP upload/enable/enforcement per zone
# (and after a restart with the API down), the Cloudflare-only IP filter,
# SNI/Host checks, forwarded headers, WebSocket upgrades, streaming,
# 503/404 pages, the SSL mode warning and fix, and per-app request counters.
#
#   sudo ./scripts/e2e/phase3.sh
cd "$(dirname "$0")/../.."
. scripts/e2e/lib.sh

json_field() { site "$1" / | python3 -c "import json,sys; d=json.load(sys.stdin); print(d$2)"; }
fails_tls()  { ! curl -sS -m 5 -o /dev/null "$@" 2>/dev/null; }
zone_row()   { page_has /settings "<td>$1</td><td>$2</td><td>$3</td>"; }
both_enforced() { zone_row example.test full enforced && zone_row other.test full enforced; }
no_dns()     { ! dns_is "$1"; }
rejections_counted() { local b; b="$(get /settings)" || return 1; grep -Eq 'Rejected connections: [1-9]' <<<"$b"; }

e2e_prepare -zones example.test,other.test -short-first
e2e_setup

say "Apps with domains in two zones"
mkecho echo
mkecho other
mkecho slowstart -delay 4s
create_app echo --data domain=echo.example.test >/dev/null
create_app other --data domain=app.other.test >/dev/null
create_app slowstart --data domain=slow.example.test >/dev/null
# A domain in no zone of the account (the repo is never deployed).
post /apps --data type=c --data-urlencode "repo=file://$GIT/lost.git" --data branch=main --data domain=app.missing.test >/dev/null
for a in echo other slowstart; do check "deploy $a" deploy_is succeeded "$a"; done

say "Cloudflare token"
post /settings/cloudflare-token --data token=bad >/dev/null
check "an invalid token is rejected" flash_has "token not saved" /settings
post /settings/cloudflare-token --data-urlencode "token=$TOKEN" >/dev/null
check "token saved; zones listed" flash_has "Zones: example.test, other.test" /settings
check "token not stored in plain text" bash -c "! grep -aqF '$TOKEN' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"

say "Sync: DNS, certificates, AOP"
refresh_csrf /settings
post /settings/edge-sync >/dev/null
check "sync reports the domain without a zone" flash_has "no Cloudflare zone found for app.missing.test" /settings
for h in dootd.example.test echo.example.test slow.example.test app.other.test; do
  check "proxied A record for $h" dns_is "$h"
done
check "no record for the domain without a zone" no_dns app.missing.test
check "certificate files are private (0600)" test "$(stat -c %a "$DATA_ROOT/certs/echo.example.test/key.pem")" = 600
check "AOP enabled on both zones" mock_py "import sys; sys.exit(0 if all(z['aop_enabled'] for z in s['zones'].values()) else 1)"
check "AOP client cert active on both zones" mock_py "import sys; sys.exit(0 if all(any(a['status']=='active' for a in z['aop'].values()) for z in s['zones'].values()) else 1)"
check "SSL mode not changed automatically" mock_py "import sys; sys.exit(0 if s['zones']['z-example-test']['ssl']=='full' else 1)"
check "settings warn about the SSL mode" page_has / "use Full (strict) so Cloudflare verifies"
check "settings show AOP enforced for both zones" both_enforced

say "Traffic through 'Cloudflare'"
check "echo answers 200" site_is 200 echo.example.test /
check "app sees its own Host" test "$(json_field echo.example.test "['host']")" = echo.example.test
check "X-Forwarded-For is the visitor IP" test "$(json_field echo.example.test "['headers']['X-Forwarded-For']")" = 203.0.113.7
check "X-Real-Ip is the visitor IP" test "$(json_field echo.example.test "['headers']['X-Real-Ip']")" = 203.0.113.7
check "X-Forwarded-Proto is https" test "$(json_field echo.example.test "['headers']['X-Forwarded-Proto']")" = https
spoof() { [ "$(site echo.example.test / -H 'X-Forwarded-For: 6.6.6.6' | python3 -c "import json,sys; print(json.load(sys.stdin)['headers']['X-Forwarded-For'])")" = 203.0.113.7 ]; }
check "a spoofed X-Forwarded-For is replaced" spoof
check "second zone app answers" site_is 200 app.other.test /
check "dashboard answers (sign-in page)" bash -c "cfcurl https://$D/login | grep -q 'Sign in'"
stream_ok() { [ "$(site app.other.test /stream | grep -c tick)" = 3 ]; }
check "streaming response passes through" stream_ok
check "WebSocket-style upgrade works" "$BIN/e2etool" upgrade -addr "$CF_IP:443" -host echo.example.test \
  -ca "$MOCK/origin-ca.pem" -cert "$MOCK/aop-client.pem" -key "$MOCK/aop-client.key"
check "HTTP/2 is offered" bash -c "curl -sS --http2 -o /dev/null -w '%{http_version}' ${CF[*]} https://echo.example.test/ | grep -q 2"

say "Rejections"
check "connections from outside Cloudflare are dropped" fails_tls --resolve "echo.example.test:443:127.0.0.1" --cacert "$MOCK/origin-ca.pem" \
  --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key" https://echo.example.test/
check "no client certificate: handshake fails" fails_tls --connect-to "::$CF_IP:443" --cacert "$MOCK/origin-ca.pem" https://echo.example.test/
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$E2E/rogue.key" -out "$E2E/rogue.pem" -days 30 -subj /CN=rogue 2>/dev/null
check "client certificate from another CA: handshake fails" fails_tls --connect-to "::$CF_IP:443" --cacert "$MOCK/origin-ca.pem" \
  --cert "$E2E/rogue.pem" --key "$E2E/rogue.key" https://echo.example.test/
check "unknown server name: handshake fails" fails_tls --connect-to "::$CF_IP:443" -k \
  --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key" https://nope.example.test/
check "Host that does not match SNI: 421" site_is 421 echo.example.test / -H "Host: app.other.test"
check "rejected connections are counted" rejections_counted

say "Availability pages"
post /apps/echo/stop >/dev/null
check "stopped app: 503" site_is 503 echo.example.test /
check "503 page says not running" site_has echo.example.test / "App not running"
post /apps/echo/start >/dev/null
check "started again: 200" wait_for 20 site_is 200 echo.example.test /
post /apps/slowstart/restart >/dev/null &
sleep 1.5
check "app starting: 503 with Retry-After" bash -c "curl -sS -D - -o /dev/null ${CF[*]} https://slow.example.test/ | grep -qi '^retry-after: 3'"
wait
check "slow app healthy afterwards" wait_for 20 site_is 200 slow.example.test /

say "SSL mode fix and certificate renewal"
refresh_csrf /settings
check "Set Full (strict) from the dashboard" post_is 303 /settings/ssl-strict --data zone=example.test
check "zone is now strict" mock_py "import sys; sys.exit(0 if s['zones']['z-example-test']['ssl']=='strict' else 1)"
check "other zone untouched" mock_py "import sys; sys.exit(0 if s['zones']['z-other-test']['ssl']=='full' else 1)"
post /settings/edge-sync >/dev/null
check "short-lived first certificates were renewed (2 issued each)" mock_py "import sys; sys.exit(0 if all(n==2 for n in s['issued'].values()) and len(s['issued'])==4 else 1)"
check "replaced certificates were revoked" mock_py "import sys; sys.exit(0 if sum(c['revoked'] for c in s['certs'].values())==4 else 1)"
post /settings/edge-sync >/dev/null
check "next sync issues nothing new" mock_py "import sys; sys.exit(0 if all(n==2 for n in s['issued'].values()) else 1)"
check "renewed certificate is served (valid > 1 year)" bash -c "echo | openssl s_client -connect $CF_IP:443 -servername echo.example.test -cert $MOCK/aop-client.pem -key $MOCK/aop-client.key 2>/dev/null | openssl x509 -noout -checkend 31536000"
check "only one AOP certificate per zone" mock_py "import sys; sys.exit(0 if all(len(z['aop'])==1 for z in s['zones'].values()) else 1)"

say "Request counters"
requests() { get /apps/echo | grep -oE '[0-9]+ requests' | head -n1 | awk '{print $1}'; }
check "echo request count > 0 on its page" test "$(requests)" -gt 0

say "Restart with the Cloudflare API down: TLS and AOP keep working"
systemctl stop cfmock
systemctl restart "$UNIT"
check "echo answers 200 right after restart" wait_for 30 site_is 200 echo.example.test /
check "still requires the AOP client certificate" fails_tls --connect-to "::$CF_IP:443" --cacert "$MOCK/origin-ca.pem" https://echo.example.test/
check "still filters by the saved Cloudflare ranges" page_has /settings "(saved)"
check "still drops non-Cloudflare sources" fails_tls --resolve "echo.example.test:443:127.0.0.1" --cacert "$MOCK/origin-ca.pem" \
  --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key" https://echo.example.test/
check "setup address stays closed" setup_closed

e2e_result
