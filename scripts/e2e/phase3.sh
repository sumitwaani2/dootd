#!/usr/bin/env bash
# Phase 3 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# dootd runs as the real systemd unit on :443 against a fake Cloudflare API
# (scripts/e2e/e2etool cfmock). The fake /ips includes 198.18.0.0/15, and
# 198.18.0.10 is added to lo, so requests from it look like Cloudflare while
# requests from 127.0.0.1 do not. Verifies:
#   token handling, DNS records, Origin CA issuance + renewal + revocation,
#   AOP upload/enable/enforcement per zone (and after a restart with the
#   API down), the Cloudflare-only IP filter, SNI/Host checks, forwarded
#   headers, WebSocket upgrades, streaming, 503/404 pages, the SSL mode
#   warning and set-strict, and per-app request counters.
#
#   sudo ./scripts/e2e/phase3.sh
set -euo pipefail

UNIT="dootd"
DATA_ROOT="/var/lib/dootd"
E2E="/opt/dootd-e2e3"
MOCK="$E2E/mock"
CF_IP="198.18.0.10"
TOKEN="e2e-cf-token-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
FAILS=0

say()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
check() { local desc="$1"; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
wait_for() { local t="$1"; shift; for _ in $(seq 1 $((t * 5))); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }

# cf <host> <path> [curl args...]: a request "from Cloudflare": CF source IP,
# Origin CA trust, AOP client certificate, CF-Connecting-IP.
cf() {
  local host="$1" path="$2"; shift 2
  curl -sS -m 10 --resolve "$host:443:$CF_IP" --cacert "$MOCK/origin-ca.pem" \
    --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key" \
    -H "CF-Connecting-IP: 203.0.113.7" "$@" "https://$host$path"
}
code()    { cf "$1" "$2" -o /dev/null -w '%{http_code}' "${@:3}" 2>/dev/null; }
is_code() { [ "$(code "$2" "$3" "${@:4}")" = "$1" ]; }
body_has() { cf "$1" "$2" 2>/dev/null | grep -qF -- "$3"; }
json_field() { cf "$1" / | python3 -c "import json,sys; d=json.load(sys.stdin); print(d$2)"; }
state()   { curl -fsS http://127.0.0.1:8787/_mock/state; }
mock_py() { state | python3 -c "import json,sys; s=json.load(sys.stdin); $1"; }
fails_tls() { ! curl -sS -m 5 -o /dev/null "$@" 2>/dev/null; }

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
cd "$(dirname "$0")/../.."
echo "kernel $(uname -r), $(. /etc/os-release && echo "$PRETTY_NAME")"

say "Build dootd and the e2e tool"
make build
go build -o "$E2E-bin/e2etool" ./scripts/e2e/e2etool
systemctl stop "$UNIT" 2>/dev/null || true
install -m 0755 dist/dootd /usr/local/bin/dootd
rm -rf "$E2E" "$DATA_ROOT" /etc/dootd && mkdir -p "$E2E/echo" "$MOCK" /etc/dootd
install -m 0755 "$E2E-bin/e2etool" "$E2E/echo/echo-app"
chmod 0755 "$E2E" "$E2E/echo"
ip addr add "$CF_IP/32" dev lo 2>/dev/null || true

say "Start the fake Cloudflare API"
systemd-run --unit=cfmock --collect -q "$E2E/echo/echo-app" cfmock -listen 127.0.0.1:8787 -dir "$MOCK" \
  -token "$TOKEN" -zones example.test,other.test -extra-range 198.18.0.0/15 -short-first
wait_for 10 curl -fsS http://127.0.0.1:8787/_mock/state

say "Configure dootd"
cat > /etc/dootd/config.toml <<EOF
[edge]
listen           = ":443"
dashboard_domain = "dootd.example.test"
public_ipv4      = "203.0.113.10"
public_ipv6      = "off"
cloudflare_api   = "http://127.0.0.1:8787/client/v4"
EOF
cat > /etc/dootd/dev-apps.toml <<EOF
[[app]]
name        = "echo"
type        = "c"
port        = 20011
domain      = "echo.example.test"
release_dir = "$E2E/echo"
run         = "echo-app echo"
health_path = "/healthz"

[[app]]
name        = "other"
type        = "c"
port        = 20012
domain      = "app.other.test"
release_dir = "$E2E/echo"
run         = "echo-app echo"
health_path = "/healthz"

[[app]]
name        = "slowstart"
type        = "c"
port        = 20013
domain      = "slow.example.test"
release_dir = "$E2E/echo"
run         = "echo-app echo -delay 4s"
health_path = "/healthz"

[[app]]
name   = "lost"
type   = "c"
port   = 20014
domain = "app.missing.test"
repo   = "file:///nonexistent.git"
EOF
mkdir -p /etc/systemd/system/dootd.service.d
install -m 0644 contrib/systemd/dootd.service /etc/systemd/system/dootd.service
cat > /etc/systemd/system/dootd.service.d/dev.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/bin/dootd serve --dev-apps /etc/dootd/dev-apps.toml
EOF
systemctl daemon-reload
systemctl start "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock
wait_for 30 curl -fsS http://127.0.0.1:20011/healthz

say "Cloudflare token"
check "an invalid token is rejected" bash -c "! printf bad | dootd ctl cloudflare-token >/dev/null 2>&1"
printf '%s' "$TOKEN" | dootd ctl cloudflare-token | tee "$E2E/token.txt"
check "token saved; zones listed" grep -q "example.test, other.test" "$E2E/token.txt"
check "token not stored in plain text" bash -c "! grep -aqF '$TOKEN' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"

say "Sync: DNS, certificates, AOP"
rc=0; dootd ctl edge sync > "$E2E/sync1.txt" 2>&1 || rc=$?
cat "$E2E/sync1.txt"
check "sync reports the domain without a zone" grep -q "no Cloudflare zone found for app.missing.test" "$E2E/sync1.txt"
check "sync exits non-zero because of it" test "$rc" -ne 0
for h in dootd.example.test echo.example.test slow.example.test; do
  check "proxied A record for $h" mock_py "import sys; z=s['zones']['z-example-test']; sys.exit(0 if any(r['name']=='$h' and r['type']=='A' and r['content']=='203.0.113.10' and r['proxied'] for r in z['dns'].values()) else 1)"
done
check "proxied A record in the second zone" mock_py "import sys; z=s['zones']['z-other-test']; sys.exit(0 if any(r['name']=='app.other.test' and r['proxied'] for r in z['dns'].values()) else 1)"
check "certificate files are private (0600)" test "$(stat -c %a "$DATA_ROOT/certs/echo.example.test/key.pem")" = 600
check "AOP enabled on both zones" mock_py "import sys; sys.exit(0 if all(z['aop_enabled'] for z in s['zones'].values()) else 1)"
check "AOP client cert active on both zones" mock_py "import sys; sys.exit(0 if all(any(a['status']=='active' for a in z['aop'].values()) for z in s['zones'].values()) else 1)"
check "SSL mode not changed automatically" mock_py "import sys; sys.exit(0 if s['zones']['z-example-test']['ssl']=='full' else 1)"
dootd ctl edge > "$E2E/edge1.txt" || true
cat "$E2E/edge1.txt"
check "edge status warns about SSL mode" grep -q 'SSL/TLS mode is "full"' "$E2E/edge1.txt"
check "edge status shows AOP enforced" grep -Eq '^example.test +full +enforced' "$E2E/edge1.txt"

say "Traffic through 'Cloudflare'"
check "echo answers 200" is_code 200 echo.example.test /
check "app sees its own Host" test "$(json_field echo.example.test "['host']")" = echo.example.test
check "X-Forwarded-For is the visitor IP" test "$(json_field echo.example.test "['headers']['X-Forwarded-For']")" = 203.0.113.7
check "X-Real-Ip is the visitor IP" test "$(json_field echo.example.test "['headers']['X-Real-Ip']")" = 203.0.113.7
check "X-Forwarded-Proto is https" test "$(json_field echo.example.test "['headers']['X-Forwarded-Proto']")" = https
spoof() { [ "$(cf echo.example.test / -H 'X-Forwarded-For: 6.6.6.6' | python3 -c "import json,sys; print(json.load(sys.stdin)['headers']['X-Forwarded-For'])")" = 203.0.113.7 ]; }
check "a spoofed X-Forwarded-For is replaced" spoof
check "second zone app answers" is_code 200 app.other.test /
check "dashboard placeholder" body_has dootd.example.test / "dootd is running"
stream_ok() { [ "$(cf app.other.test /stream | grep -c tick)" = 3 ]; }
check "streaming response passes through" stream_ok
check "WebSocket-style upgrade works" "$E2E/echo/echo-app" upgrade -addr "$CF_IP:443" -host echo.example.test \
  -ca "$MOCK/origin-ca.pem" -cert "$MOCK/aop-client.pem" -key "$MOCK/aop-client.key"
check "HTTP/2 is offered" bash -c "curl -sS --http2 -o /dev/null -w '%{http_version}' --resolve echo.example.test:443:$CF_IP --cacert $MOCK/origin-ca.pem --cert $MOCK/aop-client.pem --key $MOCK/aop-client.key https://echo.example.test/ | grep -q 2"

say "Rejections"
check "connections from outside Cloudflare are dropped" fails_tls --resolve "echo.example.test:443:127.0.0.1" --cacert "$MOCK/origin-ca.pem" \
  --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key" https://echo.example.test/
check "no client certificate: handshake fails" fails_tls --resolve "echo.example.test:443:$CF_IP" --cacert "$MOCK/origin-ca.pem" https://echo.example.test/
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$E2E/rogue.key" -out "$E2E/rogue.pem" -days 30 -subj /CN=rogue 2>/dev/null
check "client certificate from another CA: handshake fails" fails_tls --resolve "echo.example.test:443:$CF_IP" --cacert "$MOCK/origin-ca.pem" \
  --cert "$E2E/rogue.pem" --key "$E2E/rogue.key" https://echo.example.test/
check "unknown server name: handshake fails" fails_tls --resolve "nope.example.test:443:$CF_IP" -k \
  --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key" https://nope.example.test/
check "Host that does not match SNI: 421" is_code 421 echo.example.test / -H "Host: app.other.test"
check "rejected connections are counted" bash -c "dootd ctl edge | grep -Eq 'rejected connections [1-9]'"

say "Availability pages"
dootd ctl stop echo >/dev/null
check "stopped app: 503" is_code 503 echo.example.test /
check "503 page says not running" body_has echo.example.test / "App not running"
dootd ctl start echo >/dev/null
check "started again: 200" is_code 200 echo.example.test /
dootd ctl restart slowstart >/dev/null &
sleep 1
check "app starting: 503 with Retry-After" bash -c "curl -sS -D - -o /dev/null --resolve slow.example.test:443:$CF_IP --cacert $MOCK/origin-ca.pem --cert $MOCK/aop-client.pem --key $MOCK/aop-client.key https://slow.example.test/ | grep -qi '^retry-after: 3'"
wait
check "slow app healthy afterwards" is_code 200 slow.example.test /

say "SSL mode fix and certificate renewal"
check "set-strict succeeds" dootd ctl edge set-strict example.test
check "zone is now strict" mock_py "import sys; sys.exit(0 if s['zones']['z-example-test']['ssl']=='strict' else 1)"
check "other zone untouched" mock_py "import sys; sys.exit(0 if s['zones']['z-other-test']['ssl']=='full' else 1)"
dootd ctl edge sync >/dev/null 2>&1 || true
check "short-lived first certificates were renewed (2 issued each)" mock_py "import sys; sys.exit(0 if all(n==2 for n in s['issued'].values()) and len(s['issued'])==4 else 1)"
check "replaced certificates were revoked" mock_py "import sys; sys.exit(0 if sum(c['revoked'] for c in s['certs'].values())==4 else 1)"
dootd ctl edge sync >/dev/null 2>&1 || true
check "next sync issues nothing new" mock_py "import sys; sys.exit(0 if all(n==2 for n in s['issued'].values()) else 1)"
check "renewed certificate is served (valid > 1 year)" bash -c "echo | openssl s_client -connect $CF_IP:443 -servername echo.example.test -cert $MOCK/aop-client.pem -key $MOCK/aop-client.key 2>/dev/null | openssl x509 -noout -checkend 31536000"
check "only one AOP certificate per zone" mock_py "import sys; sys.exit(0 if all(len(z['aop'])==1 for z in s['zones'].values()) else 1)"

say "Request counters"
dootd ctl status | tee "$E2E/status.txt"
check "echo request count > 0" bash -c "awk '\$1==\"echo\" {exit !(\$9 > 0)}' $E2E/status.txt"

say "Restart with the Cloudflare API down: TLS and AOP keep working"
systemctl stop cfmock
systemctl restart "$UNIT"
wait_for 30 curl -fsS http://127.0.0.1:20011/healthz
check "echo answers 200 right after restart" wait_for 10 is_code 200 echo.example.test /
check "still requires the AOP client certificate" fails_tls --resolve "echo.example.test:443:$CF_IP" --cacert "$MOCK/origin-ca.pem" https://echo.example.test/
check "still filters by the saved Cloudflare ranges" bash -c "dootd ctl edge | grep -q '(saved)'"
check "still drops non-Cloudflare sources" fails_tls --resolve "echo.example.test:443:127.0.0.1" --cacert "$MOCK/origin-ca.pem" \
  --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key" https://echo.example.test/
systemctl stop "$UNIT"

say "Result"
if [ "$FAILS" -gt 0 ]; then
  echo "---- journal ----"; journalctl -u "$UNIT" --no-pager -n 150 || true
  echo "---- cfmock ----"; journalctl -u cfmock --no-pager -n 30 || true
  echo "$FAILS check(s) failed"; exit 1
fi
echo "all checks passed"
