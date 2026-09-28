#!/usr/bin/env bash
# Phase 4 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# Drives the dashboard over HTTPS through the edge (fake Cloudflare API, see
# phase3.sh) with curl: admin setup, sign-in, CSRF/Origin protection,
# Cloudflare token, creating an app from git, env vars, deploy with a live
# log stream, rollback, stop/start, restart-required, live app logs, SSL
# mode fix, persistence across a dootd restart, password change, session
# revocation, app deletion (DNS, certificate, user, files) and rate limiting.
#
#   sudo ./scripts/e2e/phase4.sh
set -euo pipefail

UNIT="dootd"
DATA_ROOT="/var/lib/dootd"
E2E="/opt/dootd-e2e4"
MOCK="$E2E/mock"
OUT="$E2E/out"
BARE="$E2E/git/web.git"
WORK="$E2E/work/web"
CF_IP="198.18.0.10"
D="dootd.example.test"
APP_HOST="web.example.test"
TOKEN="e2e-cf-token-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PW1="first password 123"
PW2="second password 456"
FAILS=0

say()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
check() { local desc="$1"; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
wait_for() { local t="$1"; shift; for _ in $(seq 1 $((t * 5))); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }

TLS=(--resolve "$D:443:$CF_IP" --resolve "$APP_HOST:443:$CF_IP" --cacert "$MOCK/origin-ca.pem" --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key")
JAR="$OUT/jar.txt"
# dc: a browser-like request to the dashboard with the cookie jar.
dc()   { curl -sS -m 60 "${TLS[@]}" -b "$JAR" -c "$JAR" "$@"; }
get()  { dc "https://$D$1"; }
code() { dc -o /dev/null -w '%{http_code}' "https://$D$1"; }
# post <path> [curl args]: form POST with Origin + CSRF; prints "code location".
post() {
  local p="$1"; shift
  dc -o "$OUT/post.html" -w '%{http_code} %{redirect_url}' -H "Origin: https://$D" --data-urlencode "csrf=$CSRF" "$@" "https://$D$p"
}
# post_is <prefix> <path> [args]: the POST's "code location" starts with prefix.
post_is() { local want="$1"; shift; local got; got="$(post "$@")"; [[ "$got" == "$want"* ]]; }
post_eq() { local want="$1"; shift; [ "$(post "$@")" = "$want" ]; }
refresh_csrf() { CSRF="$(get "${1:-/account}" | grep -oE 'name="csrf" value="[^"]+"' | head -n1 | sed 's/.*value="//; s/"$//')"; [ -n "$CSRF" ]; }
page_has()  { get "$1" | grep -qF -- "$2"; }
page_lacks() { ! get "$1" | grep -qF -- "$2"; }
flash_has() { get "${2:-/account}" | grep -qF -- "$1"; }
site()      { curl -sS -m 10 "${TLS[@]}" "https://$APP_HOST/"; }
site_has()  { site | grep -qF -- "$1"; }
site_code() { curl -sS -m 10 -o /dev/null -w '%{http_code}' "${TLS[@]}" "https://$APP_HOST/"; }
site_code_is() { [ "$(site_code)" = "$1" ]; }
mock_py()   { curl -fsS http://127.0.0.1:8787/_mock/state | python3 -c "import json,sys; s=json.load(sys.stdin); $1"; }
app_pid()   { pgrep -u dootd-web -x sample-c | head -n1; }
env_has()   { tr '\0' '\n' < "/proc/$(app_pid)/environ" | grep -x -- "$1" >/dev/null; }
# follow <deployment path>: stream the build log until the "done" event.
follow() { dc -N --max-time 600 "https://$D$1/stream" > "$OUT/stream.txt"; grep -A1 '^event: done' "$OUT/stream.txt" | tail -n1 | sed 's/^data: //'; }
commit() { git -C "$WORK" add -A && git -C "$WORK" commit -qm "$1" && git -C "$WORK" push -q origin HEAD; }

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
cd "$(dirname "$0")/../.."
echo "kernel $(uname -r), $(. /etc/os-release && echo "$PRETTY_NAME")"

say "Build and install"
make build
go build -o "$E2E-bin/e2etool" ./scripts/e2e/e2etool
systemctl stop "$UNIT" 2>/dev/null || true
systemctl stop cfmock 2>/dev/null || true
install -m 0755 dist/dootd /usr/local/bin/dootd
rm -rf "$E2E" "$DATA_ROOT" /etc/dootd /etc/systemd/system/dootd.service.d
mkdir -p "$MOCK" "$OUT" "$E2E/git" "$E2E/work" /etc/dootd
ip addr add "$CF_IP/32" dev lo 2>/dev/null || true
systemd-run --unit=cfmock --collect -q "$E2E-bin/e2etool" cfmock -listen 127.0.0.1:8787 -dir "$MOCK" \
  -token "$TOKEN" -zones example.test -extra-range 198.18.0.0/15
wait_for 10 curl -fsS http://127.0.0.1:8787/_mock/state

export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com
git init -q --bare -b main "$BARE"
cp -r examples/sample-c "$WORK" && rm -rf "$WORK/build" "$WORK/third_party"
git -C "$WORK" init -q -b main && git -C "$WORK" remote add origin "file://$BARE"
commit 'first <script>alert(1)</script>'

cat > /etc/dootd/config.toml <<EOF
[edge]
dashboard_domain = "$D"
public_ipv4      = "203.0.113.10"
public_ipv6      = "off"
cloudflare_api   = "http://127.0.0.1:8787/client/v4"
EOF
install -m 0644 contrib/systemd/dootd.service /etc/systemd/system/dootd.service
systemctl daemon-reload
systemctl start "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock
# The dashboard itself needs a certificate: set the token over SSH first
# (the dashboard form is tested below with a re-save).
printf '%s' "$TOKEN" | dootd ctl cloudflare-token >/dev/null
dootd ctl edge sync >/dev/null
wait_for 60 test -f "$MOCK/aop-client.pem"
wait_for 60 bash -c "dootd ctl edge | grep -Eq '^example.test .* enforced'"

say "Sign-in"
check "signed-out visit redirects to /login" bash -c "[ \"\$(curl -sS -o /dev/null -w '%{redirect_url}' ${TLS[*]} https://$D/)\" = https://$D/login?next=%2F ]"
check "login page explains how to create the admin" page_has /login "dootd ctl admin set-password"
check "short password refused" bash -c "! printf short | dootd ctl admin set-password --email admin@example.test 2>/dev/null"
printf '%s' "$PW1" | dootd ctl admin set-password --email Admin@Example.test
login() { # login <password> [jar] -> http code
  curl -sS -m 30 "${TLS[@]}" -b "${2:-$JAR}" -c "${2:-$JAR}" -o /dev/null -w '%{http_code}' -H "Origin: https://$D" \
    --data-urlencode email=admin@example.test --data-urlencode "password=$1" "https://$D/login"
}
check "wrong password: 401" test "$(login 'nope nope nope')" = 401
check "cross-origin login refused: 403" bash -c "[ \"\$(curl -sS ${TLS[*]} -o /dev/null -w '%{http_code}' -H 'Origin: https://evil.test' --data 'email=a&password=b' https://$D/login)\" = 403 ]"
curl -sS "${TLS[@]}" -D "$OUT/login-headers.txt" -o /dev/null -c "$JAR" -H "Origin: https://$D" \
  --data-urlencode email=admin@example.test --data-urlencode "password=$PW1" "https://$D/login"
check "right password: redirect home" grep -Eqi '^location: /\s*$' "$OUT/login-headers.txt"
check "session cookie is __Host-, Secure, HttpOnly, SameSite=Strict" \
  grep -Eqi '^set-cookie: __Host-dootd=[^;]+; Path=/; Max-Age=[0-9]+; HttpOnly; Secure; SameSite=Strict' "$OUT/login-headers.txt"
dc -D "$OUT/home-headers.txt" -o "$OUT/home.html" "https://$D/"
check "home page renders" grep -q 'No apps yet' "$OUT/home.html"
check "strict Content-Security-Policy" grep -qi "^content-security-policy: default-src 'none'; script-src 'self'" "$OUT/home-headers.txt"
check "X-Frame-Options DENY" grep -qi '^x-frame-options: DENY' "$OUT/home-headers.txt"
check "no-store on pages" grep -qi '^cache-control: no-store' "$OUT/home-headers.txt"
check "warns that no GitHub token is set" grep -q 'No GitHub token is set' "$OUT/home.html"
refresh_csrf /
check "POST without CSRF token: 403" bash -c "[ \"\$(curl -sS ${TLS[*]} -b $JAR -o /dev/null -w '%{http_code}' -H 'Origin: https://$D' --data name=x https://$D/apps)\" = 403 ]"
check "POST from another origin: 403" bash -c "[ \"\$(curl -sS ${TLS[*]} -b $JAR -o /dev/null -w '%{http_code}' -H 'Origin: https://evil.test' --data-urlencode csrf=$CSRF https://$D/apps)\" = 403 ]"

say "Settings"
check "Cloudflare token saved from the form" post_is "303" /settings/cloudflare-token --data-urlencode token=$TOKEN
check "flash lists the zone" flash_has "Zones: example.test" /settings
check "bad GitHub token rejected" post_is "303" /settings/github-token --data-urlencode token=bad
check "flash explains the GitHub failure" flash_has "token not saved" /settings
check "settings shows the zone SSL mode warning" page_has /settings "Set Full (strict)"

say "Create an app"
check "invalid input: 422" post_is 422 /apps --data name=Bad! --data type=c --data repo=x
check "form lists every problem" bash -c "grep -q 'invalid app name' $OUT/post.html && grep -q 'unsupported repo' $OUT/post.html"
check "create web: redirect to its page" post_is "303 https://$D/apps/web" /apps --data name=web --data type=c --data-urlencode repo=file://$BARE --data branch=main \
  --data domain=$APP_HOST --data memory=64M --data cpu=1 --data pids=256 --data build_memory=1G --data build_timeout=15m
check "duplicate name refused" post_is "422" /apps --data name=web --data type=c --data-urlencode repo=file://$BARE
check "duplicate domain refused" post_is "422" /apps --data name=web2 --data type=c --data-urlencode repo=file://$BARE --data domain=$APP_HOST
check "system user created" id dootd-web
check "DNS record created in the background" wait_for 30 mock_py "import sys; sys.exit(0 if any(r['name']=='$APP_HOST' and r['proxied'] for r in s['zones']['z-example-test']['dns'].values()) else 1)"
check "certificate issued" wait_for 30 test -f "$DATA_ROOT/certs/$APP_HOST/cert.pem"
check "site shows 'not running' before the first deploy" wait_for 20 site_code_is 503

say "Env vars"
check "reserved name refused" post_is "303" /apps/web/env --data key=PORT --data value=1
check "flash explains why" flash_has "is reserved" /apps/web
post /apps/web/env --data key=GREETING --data-urlencode 'value=hello world' >/dev/null
check "env var listed by name" page_has /apps/web '<code>GREETING</code>'
check "env value never shown" page_lacks /apps/web 'hello world'
check "env value not stored in plain text" bash -c "! grep -aqF 'hello world' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"

say "Deploy from the dashboard"
loc="$(post /apps/web/deploy)"
check "deploy redirects to the deployment page" test "${loc%%/deployments/*}" = "303 https://$D"
DEP1="${loc##*/deployments/}"
check "deployment page streams" page_has "/deployments/$DEP1" "data-stream="
check "live stream ends with succeeded" test "$(follow "/deployments/$DEP1")" = succeeded
check "stream carried the build log" grep -q 'cloning' "$OUT/stream.txt"
check "finished page shows the full log" page_has "/deployments/$DEP1" 'SUCCEEDED'
check "site serves the app" wait_for 10 site_has "Hello from C on dootd"
check "app got its env var" env_has "GREETING=hello world"
check "commit subject is HTML-escaped" page_has /apps/web '&lt;script&gt;alert(1)&lt;/script&gt;'
check "no raw script tag in the page" page_lacks /apps/web '<script>alert'
R1="$(readlink "$DATA_ROOT/apps/web/current" | xargs basename)"

say "Restart required, restart, stop and start"
post /apps/web/env --data key=GREETING --data value=changed >/dev/null
check "page says a restart is needed" page_has /apps/web "Restart it to apply them"
check "home warns too" page_has / "restart it to apply them"
post /apps/web/restart >/dev/null
check "restart applied the new value" wait_for 20 env_has "GREETING=changed"
check "no restart warning afterwards" page_lacks /apps/web "Restart it to apply them"
post /apps/web/stop >/dev/null
check "stopped: site 503" site_code_is 503
check "page shows stopped" page_has /apps/web 's-stopped'
post /apps/web/start >/dev/null
check "started: site 200" wait_for 20 site_code_is 200

say "Second deploy and rollback"
sed -i 's|<h1>[^<]*</h1>|<h1>web v2</h1>|' "$WORK/src/main.c"
commit "v2"
loc="$(post /apps/web/deploy)"
check "second deploy succeeds" test "$(follow "/deployments/${loc##*/deployments/}")" = succeeded
check "site shows v2" wait_for 10 site_has "web v2"
check "releases list has a rollback button" page_has /apps/web "value=\"$R1\""
loc="$(post /apps/web/rollback --data "release=$R1")"
check "rollback succeeds" test "$(follow "/deployments/${loc##*/deployments/}")" = succeeded
check "site back on the first release" wait_for 10 site_has "Release: $R1<"

say "Live app logs"
dc -N --max-time 3 "https://$D/apps/web/logs/stream" > "$OUT/logs.txt" || true
check "log stream sends recent lines" grep -q 'listening on 127.0.0.1' "$OUT/logs.txt"
check "logs page renders" page_has /apps/web/logs 'data-stream="/apps/web/logs/stream"'

say "SSL mode fix"
check "set-strict from the dashboard" post_is "303" /settings/ssl-strict --data zone=example.test
check "zone is strict at Cloudflare" mock_py "import sys; sys.exit(0 if s['zones']['z-example-test']['ssl']=='strict' else 1)"

say "dootd restart: apps and sessions persist"
systemctl restart "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock
check "session still valid" wait_for 20 page_has / '<b>web</b>'
check "site serving again" wait_for 30 site_code_is 200
check "ctl sees the dashboard app" bash -c "dootd ctl status | grep -q '^web '"

say "Password change and sessions"
JAR2="$OUT/jar2.txt"
check "second session signs in" test "$(login "$PW1" "$JAR2")" = 303
refresh_csrf
post /account/password --data current=wrong --data-urlencode "new=$PW2" --data-urlencode "confirm=$PW2" >/dev/null
check "wrong current password refused" flash_has "current password is wrong"
post /account/password --data-urlencode "current=$PW1" --data-urlencode "new=$PW2" --data-urlencode "confirm=$PW2" >/dev/null
check "password changed" flash_has "Password changed"
check "other session was signed out" bash -c "[ \"\$(curl -sS ${TLS[*]} -b $JAR2 -o /dev/null -w '%{http_code}' https://$D/)\" = 303 ]"
check "this session still works" page_has / '<b>web</b>'
check "old password no longer works" test "$(login "$PW1" "$OUT/jar3.txt")" = 401
check "new password works" test "$(login "$PW2" "$OUT/jar3.txt")" = 303

say "Delete the app"
refresh_csrf
post /apps/web/delete --data confirm=wrong --data keep_data=1 >/dev/null
check "wrong confirmation refused" flash_has "type the app name exactly" /apps/web
check "delete redirects home" post_eq "303 https://$D/" /apps/web/delete --data confirm=web --data keep_data=1
check "flash confirms and names the kept data" flash_has "Its data was kept in $DATA_ROOT/deleted/web-" /
check "kept SQLite database exists" bash -c "ls $DATA_ROOT/deleted/web-*/data/app.db"
check "app files removed" test ! -e "$DATA_ROOT/apps/web"
check "system user removed" bash -c "! id dootd-web 2>/dev/null"
check "no app process left" bash -c "! pgrep -x sample-c"
check "DNS record removed" mock_py "import sys; sys.exit(0 if not any(r['name']=='$APP_HOST' for r in s['zones']['z-example-test']['dns'].values()) else 1)"
check "certificate revoked" mock_py "import sys; sys.exit(0 if any(c['host']=='$APP_HOST' and c['revoked'] for c in s['certs'].values()) else 1)"
check "domain no longer served" bash -c "! curl -sS -m 5 -o /dev/null ${TLS[*]} https://$APP_HOST/ 2>/dev/null"
check "home is empty again" page_has / 'No apps yet'
check "dashboard DNS record kept" mock_py "import sys; sys.exit(0 if any(r['name']=='$D' for r in s['zones']['z-example-test']['dns'].values()) else 1)"

say "Sign out and rate limiting"
refresh_csrf
post /logout >/dev/null
check "signed out" test "$(code /)" = 303
many() { local c; for _ in 1 2 3 4 5 6; do c="$(curl -sS "${TLS[@]}" -o /dev/null -w '%{http_code}' -H "Origin: https://$D" -H 'CF-Connecting-IP: 203.0.113.99' \
  --data email=admin@example.test --data password=wrongwrongwrong "https://$D/login")"; done; [ "$c" = 429 ]; }
check "sixth failed sign-in from one IP: 429" many
check "other visitors can still sign in" test "$(login "$PW2" "$OUT/jar4.txt")" = 303
systemctl stop "$UNIT"

say "Result"
if [ "$FAILS" -gt 0 ]; then
  echo "---- journal ----"; journalctl -u "$UNIT" --no-pager -n 150 || true
  echo "$FAILS check(s) failed"; exit 1
fi
echo "all checks passed"
