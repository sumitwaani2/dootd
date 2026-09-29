#!/usr/bin/env bash
# Phase 4 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# The whole first run as a user does it: install.sh prints a one-time
# password; the setup address (server IP, self-signed) takes it once and
# only allows setting up the account; then the Cloudflare token and the
# dashboard domain, after which the setup address closes. Then, through
# the edge: sign-in, CSRF/Origin protection, creating an app from git (name
# from the repo), env vars, deploy with a live log stream, rollback,
# stop/start, restart-required, live app logs, SSL mode fix, persistence
# across a restart, email and password change, session revocation, moving
# the dashboard domain, app deletion (DNS, certificate, user, files) and
# rate limiting.
#
#   sudo ./scripts/e2e/phase4.sh
cd "$(dirname "$0")/../.."
. scripts/e2e/lib.sh

APP_HOST="web.example.test"
PW2="second password 456"
D2="dash2.example.test"
app_pid()  { pgrep -u dootd-web -x sample-c | head -n1; }
env_has()  { tr '\0' '\n' < "/proc/$(app_pid)/environ" | grep -x -- "$1" >/dev/null; }
redirect_of() { dc -o /dev/null -w '%{redirect_url}' "$DASH$1"; }
no_dns()   { ! dns_is "$1"; }

e2e_prepare

say "install.sh prints the setup address and a one-time password"
check "installer finished" e2e_install
check "it names the server's address" grep -q "Open: *https://$PUB_IP" "$INSTALL_OUT"
check "it prints a one-time password" test -n "$OTP"
check "only a hash of it is stored" bash -c "! grep -aqF '$OTP' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"
check "service running" systemctl is-active --quiet "$UNIT"
check "installed unit is the one in contrib/" cmp -s /etc/systemd/system/dootd.service contrib/systemd/dootd.service

say "Setup address: self-signed, one-time password, account only"
use_setup; rm -f "$JAR"
check "setup address answers" wait_for 30 bash -c "curl -sk -o /dev/null https://127.0.0.1/login"
check "self-signed setup certificate" bash -c "echo | openssl s_client -connect 127.0.0.1:443 2>/dev/null | openssl x509 -noout -subject | grep -q 'dootd setup'"
check "sign-in page asks for the one-time password" page_has /login "One-time password"
check "wrong one-time password: 401" test "$(login "wrong-wrong-wrong-wrong" "")" = 401
check "right one-time password: to account setup" test "$(login "$OTP" "")" = 303
check "setup session is sent to /setup" test "$(redirect_of /)" = "$DASH/setup"
refresh_csrf /setup
check "setup session cannot change settings" post_is 403 /settings/cloudflare-token --data token=x
check "the one-time password works only once" test "$(login "$OTP" "" "$OUT/jar-otp2.txt")" = 401
check "mismatched passwords refused" post_is 422 /setup --data-urlencode "email=$ADMIN" --data-urlencode "password=$PW" --data "confirm=other password 1"
check "short password refused" post_is 422 /setup --data-urlencode "email=$ADMIN" --data "password=short" --data "confirm=short"
check "account set up" post_is "303 $DASH/" /setup --data-urlencode "email=Admin@Example.test" --data-urlencode "password=$PW" --data-urlencode "confirm=$PW"
refresh_csrf /
check "home lists the missing Cloudflare token" page_has / "no Cloudflare token is set"
check "home lists the missing dashboard domain" page_has / "no dashboard domain yet"
post /settings/dashboard-domain --data "domain=$D" >/dev/null
check "dashboard domain needs the token first" flash_has "save the Cloudflare token first" /settings
post /settings/cloudflare-token --data-urlencode "token=$TOKEN" >/dev/null
check "Cloudflare token saved; zone listed" flash_has "Zones: example.test" /settings
check "dashboard domain saved" post_is 303 /settings/dashboard-domain --data "domain=$D"
check "DNS record for the dashboard" wait_for 30 dns_is "$D"
check "certificate for the dashboard" wait_for 30 test -f "$DATA_ROOT/certs/$D/cert.pem"
check "setup address closes once the domain is ready" wait_for 120 setup_closed

say "Sign-in on the dashboard domain"
use_domain; rm -f "$JAR"
check "signed-out visit redirects to /login" test "$(redirect_of /)" = "$DASH/login?next=%2F"
check "wrong password: 401" test "$(login 'nope nope nope')" = 401
check "cross-origin login refused: 403" bash -c "[ \"\$(curl -sS ${CF[*]} -o /dev/null -w '%{http_code}' -H 'Origin: https://evil.test' --data 'email=a&password=b' https://$D/login)\" = 403 ]"
cfcurl -D "$OUT/login-headers.txt" -o /dev/null -c "$JAR" -H "Origin: https://$D" \
  --data-urlencode "email=$ADMIN" --data-urlencode "password=$PW" "https://$D/login"
check "right password: redirect home" grep -Eqi '^location: /\s*$' "$OUT/login-headers.txt"
check "session cookie is __Host-, Secure, HttpOnly, SameSite=Strict" \
  grep -Eqi '^set-cookie: __Host-dootd=[^;]+; Path=/; Max-Age=[0-9]+; HttpOnly; Secure; SameSite=Strict' "$OUT/login-headers.txt"
dc -D "$OUT/home-headers.txt" -o "$OUT/home.html" "https://$D/"
check "home page renders" grep -q 'No apps yet' "$OUT/home.html"
check "no setup steps left" bash -c "! grep -q 'Setup:' $OUT/home.html"
check "strict Content-Security-Policy" grep -qi "^content-security-policy: default-src 'none'; script-src 'self'" "$OUT/home-headers.txt"
check "X-Frame-Options DENY" grep -qi '^x-frame-options: DENY' "$OUT/home-headers.txt"
check "no-store on pages" grep -qi '^cache-control: no-store' "$OUT/home-headers.txt"
check "warns that no GitHub token is set" grep -q 'No GitHub token is set' "$OUT/home.html"
refresh_csrf /
check "POST without CSRF token: 403" bash -c "[ \"\$(curl -sS ${CF[*]} -b $JAR -o /dev/null -w '%{http_code}' -H 'Origin: https://$D' --data type=c https://$D/apps)\" = 403 ]"
check "POST from another origin: 403" bash -c "[ \"\$(curl -sS ${CF[*]} -b $JAR -o /dev/null -w '%{http_code}' -H 'Origin: https://evil.test' --data-urlencode csrf=$CSRF https://$D/apps)\" = 403 ]"

say "Settings"
check "bad GitHub token rejected" post_is 303 /settings/github-token --data-urlencode token=bad
check "flash explains the GitHub failure" flash_has "token not saved" /settings
check "settings shows the zone SSL mode warning" page_has /settings "Set Full (strict)"
check "settings shows the dashboard domain as ready" page_has /settings "s-running\">ready"

say "Create an app"
mkrepo web examples/sample-c
echo "// xss subject" >> "$WORKS/web/src/main.c"; commit web 'first <script>alert(1)</script>'
check "invalid input: 422" post_is 422 /apps --data type=x --data repo=x
check "form lists every problem" bash -c "grep -q 'type must be zig or c' $OUT/post.html && grep -q 'unsupported repo' $OUT/post.html"
check "create web: redirect to its page" post_is "303 $DASH/apps/web" /apps --data type=c --data-urlencode "repo=file://$GIT/web.git" --data branch=main \
  --data domain=$APP_HOST --data memory=64M
check "the same repository twice is refused" post_is 422 /apps --data type=c --data-urlencode "repo=file://$GIT/web.git" --data branch=main
mkrepo web2 examples/sample-c
check "duplicate domain refused" post_is 422 /apps --data type=c --data-urlencode "repo=file://$GIT/web2.git" --data branch=main --data domain=$APP_HOST
check "the dashboard domain cannot be an app domain" post_is 422 /apps --data type=c --data-urlencode "repo=file://$GIT/web2.git" --data branch=main --data domain=$D
post /settings/dashboard-domain --data "domain=$APP_HOST" >/dev/null
check "an app domain cannot become the dashboard domain" flash_has "is the domain of the app web" /settings
check "system user created" id dootd-web
check "DNS record created in the background" wait_for 30 dns_is "$APP_HOST"
check "certificate issued" wait_for 30 test -f "$DATA_ROOT/certs/$APP_HOST/cert.pem"
check "site shows 'not running' before the first deploy" wait_for 20 site_is 503 "$APP_HOST" /

say "Env vars"
check "reserved name refused" post_is 303 /apps/web/env --data key=PORT --data value=1
check "flash explains why" flash_has "is reserved" /apps/web
post /apps/web/env --data key=GREETING --data-urlencode 'value=hello world' >/dev/null
check "env var listed by name" page_has /apps/web '<code>GREETING</code>'
check "env value never shown" page_lacks /apps/web 'hello world'
check "env value not stored in plain text" bash -c "! grep -aqF 'hello world' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"

say "Deploy from the dashboard"
check "live stream ends with succeeded" deploy_is succeeded web
DEP1="$DEP"
check "stream carried the build log" last_has 'cloning'
check "finished page shows the full log" page_has "/deployments/$DEP1" 'SUCCEEDED'
check "site serves the app" wait_for 10 site_has "$APP_HOST" / "Hello from C on dootd"
check "app got its env var" env_has "GREETING=hello world"
check "commit subject is HTML-escaped" page_has /apps/web '&lt;script&gt;alert(1)&lt;/script&gt;'
check "no raw script tag in the page" page_lacks /apps/web '<script>alert'
R1="$(current web)"

say "Restart required, restart, stop and start"
post /apps/web/env --data key=GREETING --data value=changed >/dev/null
check "page says a restart is needed" page_has /apps/web "Restart it to apply them"
check "home warns too" page_has / "restart it to apply them"
post /apps/web/restart >/dev/null
check "restart applied the new value" wait_for 20 env_has "GREETING=changed"
check "no restart warning afterwards" page_lacks /apps/web "Restart it to apply them"
post /apps/web/stop >/dev/null
check "stopped: site 503" site_is 503 "$APP_HOST" /
check "page shows stopped" page_has /apps/web 's-stopped'
post /apps/web/start >/dev/null
check "started: site 200" wait_for 20 site_is 200 "$APP_HOST" /

say "Second deploy and rollback"
set_title web "web v2"
commit web "v2"
check "second deploy succeeds" deploy_is succeeded web
check "site shows v2" wait_for 10 site_has "$APP_HOST" / "web v2"
check "releases list has a rollback button" page_has /apps/web "value=\"$R1\""
check "rollback succeeds" deploy_is succeeded web "$R1"
check "site back on the first release" wait_for 10 site_has "$APP_HOST" / "Release: $R1<"

say "Live app logs"
dc -N --max-time 3 "https://$D/apps/web/logs/stream" > "$OUT/logs.txt" || true
check "log stream sends recent lines" grep -q 'listening on 127.0.0.1' "$OUT/logs.txt"
check "logs page renders" page_has /apps/web/logs 'data-stream="/apps/web/logs/stream"'

say "SSL mode fix"
check "set-strict from the dashboard" post_is 303 /settings/ssl-strict --data zone=example.test
check "zone is strict at Cloudflare" mock_py "import sys; sys.exit(0 if s['zones']['z-example-test']['ssl']=='strict' else 1)"

say "dootd restart: apps and sessions persist"
systemctl restart "$UNIT"
check "session still valid" wait_for 30 page_has / '<b>web</b>'
check "site serving again" wait_for 30 site_is 200 "$APP_HOST" /
check "setup address still closed" setup_closed

say "Email, password and sessions"
JAR2="$OUT/jar2.txt"
check "second session signs in" test "$(login "$PW" "$ADMIN" "$JAR2")" = 303
refresh_csrf
post /account/email --data email=owner@example.test --data current=wrong >/dev/null
check "email change needs the current password" flash_has "current password is wrong"
post /account/email --data email=Owner@Example.test --data-urlencode "current=$PW" >/dev/null
check "email changed" flash_has "Email changed"
check "the page shows the new email" page_has /account "owner@example.test"
ADMIN="owner@example.test"
post /account/password --data current=wrong --data-urlencode "new=$PW2" --data-urlencode "confirm=$PW2" >/dev/null
check "wrong current password refused" flash_has "current password is wrong"
post /account/password --data-urlencode "current=$PW" --data-urlencode "new=$PW2" --data-urlencode "confirm=$PW2" >/dev/null
check "password changed" flash_has "Password changed"
check "other session was signed out" bash -c "[ \"\$(curl -sS ${CF[*]} -b $JAR2 -o /dev/null -w '%{http_code}' https://$D/)\" = 303 ]"
check "this session still works" page_has / '<b>web</b>'
check "old password no longer works" test "$(login "$PW" "$ADMIN" "$OUT/jar3.txt")" = 401
check "old email no longer works" test "$(login "$PW2" admin@example.test "$OUT/jar3.txt")" = 401
check "new email and password work" test "$(login "$PW2" "$ADMIN" "$OUT/jar3.txt")" = 303
PW="$PW2"

say "Move the dashboard to another domain"
refresh_csrf /settings
check "new dashboard domain accepted" post_is 200 /settings/dashboard-domain --data "domain=$D2"
check "the answer points to the new domain" grep -q "https://$D2/" "$OUT/post.html"
check "DNS record for the new domain" wait_for 30 dns_is "$D2"
check "old domain's DNS record removed" wait_for 30 no_dns "$D"
check "old domain's certificate revoked" wait_for 30 mock_py "import sys; sys.exit(0 if any(c['host']=='$D' and c['revoked'] for c in s['certs'].values()) else 1)"
check "old domain no longer served" wait_for 30 bash -c "! curl -sS -m 5 -o /dev/null ${CF[*]} https://$D/login 2>/dev/null"
D="$D2"; use_domain
check "sign in on the new domain" wait_for 60 signed_in
check "the app is still there" page_has / '<b>web</b>'
check "apps keep serving" site_is 200 "$APP_HOST" /

say "Delete the app"
post /apps/web/delete --data confirm=wrong --data keep_data=1 >/dev/null
check "wrong confirmation refused" flash_has "type the app name exactly" /apps/web
check "delete redirects home" post_is "303 $DASH/" /apps/web/delete --data confirm=web --data keep_data=1
check "flash confirms and names the kept data" flash_has "Its data was kept in $DATA_ROOT/deleted/web-" /
check "kept SQLite database exists" bash -c "ls $DATA_ROOT/deleted/web-*/data/app.db"
check "app files removed" test ! -e "$DATA_ROOT/apps/web"
check "system user removed" bash -c "! id dootd-web 2>/dev/null"
check "no app process left" bash -c "! pgrep -x sample-c"
check "DNS record removed" no_dns "$APP_HOST"
check "certificate revoked" mock_py "import sys; sys.exit(0 if any(c['host']=='$APP_HOST' and c['revoked'] for c in s['certs'].values()) else 1)"
check "domain no longer served" bash -c "! curl -sS -m 5 -o /dev/null ${CF[*]} https://$APP_HOST/ 2>/dev/null"
check "home is empty again" page_has / 'No apps yet'
check "dashboard DNS record kept" dns_is "$D"

say "Sign out and rate limiting"
refresh_csrf
post /logout >/dev/null
check "signed out" test "$(code /)" = 303
many() { local c; for _ in 1 2 3 4 5 6; do c="$(cfcurl -o /dev/null -w '%{http_code}' -H "Origin: https://$D" -H 'CF-Connecting-IP: 203.0.113.99' \
  --data "email=$ADMIN" --data password=wrongwrongwrong "https://$D/login")"; done; [ "$c" = 429 ]; }
check "sixth failed sign-in from one IP: 429" many
check "other visitors can still sign in" test "$(login "$PW" "$ADMIN" "$OUT/jar4.txt")" = 303

e2e_result
