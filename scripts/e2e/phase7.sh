#!/usr/bin/env bash
# Phase 7 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# Covers the whole life of a server with a fake Cloudflare API (cfmock), a
# real S3 server (rclone serve s3) and a fake GitHub Releases API (a static
# file server):
#   install.sh (checksum refusal, directories, master key, unit) →
#   dootd init (errors, non-interactive, interactive on a pty) →
#   dashboard + app + backups + recovery kit →
#   self-update from the dashboard (with a migration and the pre-update copy),
#   a tampered release refused, a release that cannot start rolled back →
#   the server destroyed and rebuilt with install.sh + dootd init --restore →
#   file-descriptor limits and restart timing with 5 apps.
#
#   sudo ./scripts/e2e/phase7.sh
set -euo pipefail

UNIT="dootd"
DATA_ROOT="/var/lib/dootd"
E2E="/opt/dootd-e2e7"
MOCK="$E2E/mock"
OUT="$E2E/out"
REL="$E2E/releases"      # fake GitHub: /repos/<repo>/releases/latest + /dl/<tag>/<file>
S3DIR="$E2E/s3"
BUCKET="backups"
BARE="$E2E/git/web.git"
WORK="$E2E/work/web"
CF_IP="198.18.0.10"
D="dootd.example.test"
APP_HOST="web.example.test"
IP1="203.0.113.10"
IP2="203.0.113.20"
CF_API="http://127.0.0.1:8787/client/v4"
GH="http://127.0.0.1:8900"
TOKEN="e2e-cf-token-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
S3_KEY="E2EACCESS"
S3_SECRET="e2e-secret-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PW="phase seven password"
PW2="interactive password 7"
ARCH="$(dpkg --print-architecture)"
ASSET="dootd-linux-$ARCH"
MIGRATIONS="internal/store/migrations"
FAILS=0

say()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
check() { local desc="$1"; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
wait_for() { local t="$1"; shift; for _ in $(seq 1 $((t * 5))); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }

TLS=(--resolve "$D:443:$CF_IP" --resolve "$APP_HOST:443:$CF_IP" --cacert "$MOCK/origin-ca.pem" --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key")
JAR="$OUT/jar.txt"
dc()   { curl -sS -m 120 "${TLS[@]}" -b "$JAR" -c "$JAR" "$@"; }
get()  { dc "https://$D$1"; }
post() { local p="$1"; shift; dc -o "$OUT/post.html" -w '%{http_code} %{redirect_url}' -H "Origin: https://$D" --data-urlencode "csrf=$CSRF" "$@" "https://$D$p"; }
refresh_csrf() { CSRF="$(get "${1:-/account}" | grep -oE 'name="csrf" value="[^"]+"' | head -n1 | sed 's/.*value="//; s/"$//')"; [ -n "$CSRF" ]; }
page_has()  { local b; b="$(get "$1")" || return 1; grep -qF -- "$2" <<<"$b"; }
page_lacks() { local b; b="$(get "$1")" || return 1; ! grep -qF -- "$2" <<<"$b"; }
flash_has() { local b; b="$(get "${2:-/account}")" || return 1; grep -qF -- "$1" <<<"$b"; }
login() { rm -f "$JAR"; curl -sS -m 30 "${TLS[@]}" -c "$JAR" -o /dev/null -w '%{http_code}' -H "Origin: https://$D" \
  --data-urlencode email=admin@example.test --data-urlencode "password=$1" "https://$D/login"; }
logged_in() { [ "$(login "$1")" = 303 ] && refresh_csrf /; }
visit()     { curl -sS -m 10 "${TLS[@]}" "https://$APP_HOST/"; }
visits()    { visit | grep -oE 'Visits: [0-9]+' | grep -oE '[0-9]+'; }
site_up()   { [ "$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "${TLS[@]}" "https://$APP_HOST/healthz")" = 200 ]; }
follow()    { dc -N --max-time 600 "https://$D$1/stream" > "$OUT/stream.txt"; grep -A1 '^event: done' "$OUT/stream.txt" | tail -n1 | sed 's/^data: //'; }
commit()    { git -C "$WORK" add -A && git -C "$WORK" commit -qm "$1" && git -C "$WORK" push -q origin HEAD; }
mock_py()   { curl -fsS http://127.0.0.1:8787/_mock/state | python3 -c "import json,sys; s=json.load(sys.stdin); $1"; }
dns_is()    { mock_py "import sys; sys.exit(0 if any(r['name']=='$1' and r['type']=='A' and r['content']=='$2' and r['proxied'] for r in s['zones']['z-example-test']['dns'].values()) else 1)"; }
db()        { python3 -c "import sqlite3,sys; c=sqlite3.connect('file:$DATA_ROOT/dootd.db?mode=ro',uri=True); r=c.execute(sys.argv[1]).fetchone(); print('' if r is None else (r[0].decode() if isinstance(r[0], bytes) else r[0]))" "$1"; }
schema()    { db "SELECT MAX(version) FROM schema_migrations"; }
pre_schema() { python3 -c "import sqlite3; print(sqlite3.connect('file:$DATA_ROOT/dootd.db.pre-update?mode=ro',uri=True).execute('SELECT MAX(version) FROM schema_migrations').fetchone()[0])"; }
running_version() { dootd version | awk '{print $2}'; }
is_version() { [ "$(running_version)" = "$1" ] && systemctl is-active --quiet "$UNIT" && dootd ctl status >/dev/null 2>&1; }
edge_enforced() { dootd ctl edge | grep -Eq '^example.test .* enforced'; }
s3_start() { systemd-run --unit=s3mock --collect -q "$E2E-bin/rclone" serve s3 --addr 127.0.0.1:9000 \
  --dir-cache-time 1s --auth-key "$S3_KEY,$S3_SECRET" "$S3DIR"; wait_for 10 curl -s -o /dev/null http://127.0.0.1:9000/; }
# publish <tag> [bad]: make <tag> the "latest" release of the fake GitHub.
publish() {
  local tag="$1" dir="$REL/dl/$1"
  if [ "${2:-}" = bad ]; then echo "$(printf '0%.0s' $(seq 64))  $ASSET" > "$dir/checksums.txt"; fi
  mkdir -p "$REL/repos/sumitwaani2/dootd/releases"
  cat > "$REL/repos/sumitwaani2/dootd/releases/latest" <<EOF
{"tag_name": "$tag", "name": "$tag", "html_url": "https://github.com/sumitwaani2/dootd/releases/tag/$tag",
 "published_at": "2026-09-28T12:00:00Z",
 "assets": [{"name": "$ASSET", "browser_download_url": "$GH/dl/$tag/$ASSET"},
            {"name": "checksums.txt", "browser_download_url": "$GH/dl/$tag/checksums.txt"}]}
EOF
}
# release <tag> <binary>: stage a release directory with its checksums.
release() { mkdir -p "$REL/dl/$1"; install -m 0755 "$2" "$REL/dl/$1/$ASSET"; (cd "$REL/dl/$1" && sha256sum "$ASSET" > checksums.txt); }
install_from() { DOOTD_BASE_URL="$GH/dl/$1" DOOTD_SWAP=no DOOTD_UFW=no bash ./install.sh; }

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
cd "$(dirname "$0")/../.."
echo "kernel $(uname -r), $(. /etc/os-release && echo "$PRETTY_NAME"), $(nproc) CPUs"
command -v zstd >/dev/null || apt-get install -y -qq zstd >/dev/null

say "Build four releases"
trap 'rm -f $MIGRATIONS/0*_e2e_*.sql' EXIT
for u in "$UNIT" cfmock s3mock ghmock; do systemctl stop "$u" 2>/dev/null || true; done
rm -rf "$E2E" "$DATA_ROOT" /etc/dootd /etc/systemd/system/dootd.service /etc/systemd/system/dootd.service.d /usr/local/bin/dootd*
systemctl daemon-reload
mkdir -p "$MOCK" "$OUT" "$E2E-bin" "$E2E/git" "$E2E/work" "$S3DIR/$BUCKET" "$REL"
make build VERSION=v0.7.0 && release v0.7.0 dist/dootd
# v0.8.0 adds a migration, so updating to it must copy dootd.db first.
echo "CREATE TABLE e2e_update (x INTEGER) STRICT;" > "$MIGRATIONS/0007_e2e_update.sql"
make build VERSION=v0.8.0 && release v0.8.0 dist/dootd
# v0.9.0 migrates again and then refuses to start: the guard must roll back.
echo "CREATE TABLE e2e_broken (x INTEGER) STRICT;" > "$MIGRATIONS/0008_e2e_broken.sql"
PKG=github.com/sumitwaani2/dootd/internal/buildinfo
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X $PKG.Version=v0.9.0 -X $PKG.FailAfterMigrate=1" -o "$E2E-bin/broken" ./cmd/dootd
release v0.9.0 "$E2E-bin/broken"
rm -f "$MIGRATIONS"/0*_e2e_*.sql
# v1.0.0: a genuine binary whose download does not match checksums.txt.
release v1.0.0 "$REL/dl/v0.8.0/$ASSET" && publish v1.0.0 bad
publish v0.7.0

go build -o "$E2E-bin/e2etool" ./scripts/e2e/e2etool
if [ ! -x "$E2E-bin/rclone" ]; then
  curl -fsSL -o /tmp/rclone.zip "https://downloads.rclone.org/rclone-current-linux-$ARCH.zip"
  unzip -qo /tmp/rclone.zip -d /tmp/rclone && install -m 0755 /tmp/rclone/rclone-*/rclone "$E2E-bin/rclone"
fi
ip addr add "$CF_IP/32" dev lo 2>/dev/null || true
systemd-run --unit=ghmock --collect -q python3 -m http.server 8900 --bind 127.0.0.1 --directory "$REL"
systemd-run --unit=cfmock --collect -q "$E2E-bin/e2etool" cfmock -listen 127.0.0.1:8787 -dir "$MOCK" \
  -token "$TOKEN" -zones example.test -extra-range 198.18.0.0/15
wait_for 10 curl -fsS http://127.0.0.1:8787/_mock/state
wait_for 10 curl -fsS "$GH/dl/v0.7.0/checksums.txt"
s3_start

say "install.sh"
check "tampered download refused" bash -c "! DOOTD_BASE_URL=$GH/dl/v1.0.0 DOOTD_SWAP=no DOOTD_UFW=no bash ./install.sh > $OUT/bad-install.txt 2>&1"
check "error names the checksum mismatch" grep -q 'checksum mismatch' "$OUT/bad-install.txt"
check "nothing was installed" test ! -e /usr/local/bin/dootd
install_from v0.7.0 | tee "$OUT/install.txt"
check "binary installed" test "$(running_version)" = v0.7.0
check "master key created with mode 0600" test "$(stat -c %a /etc/dootd/master.key)" = 600
check "/etc/dootd is 0700" test "$(stat -c %a /etc/dootd)" = 700
check "/var/lib/dootd is 0711" test "$(stat -c %a $DATA_ROOT)" = 711
check "installed unit is the one in contrib/" cmp -s /etc/systemd/system/dootd.service contrib/systemd/dootd.service
check "unit enabled" systemctl is-enabled --quiet "$UNIT"
check "service not started before init" bash -c "! systemctl is-active --quiet $UNIT"
check "unit has the update guard" grep -q 'dootd.prev update-guard' /etc/systemd/system/dootd.service
check "installer points to dootd init" grep -q 'sudo dootd init' "$OUT/install.txt"
KEY1="$(sha256sum /etc/dootd/master.key)"
install_from v0.7.0 > /dev/null
check "running it again keeps the master key" test "$(sha256sum /etc/dootd/master.key)" = "$KEY1"

say "dootd init: refusals"
INIT=(dootd init --email admin@example.test --domain "$D" --ipv4 "$IP1" --ipv6 off --yes --ssl-strict --cloudflare-api "$CF_API")
check "no password and no terminal: clear error" bash -c "! DOOTD_CLOUDFLARE_TOKEN=$TOKEN ${INIT[*]} </dev/null > $OUT/init1.txt 2>&1 && grep -q DOOTD_ADMIN_PASSWORD $OUT/init1.txt"
check "short password refused" bash -c "! DOOTD_ADMIN_PASSWORD=short DOOTD_CLOUDFLARE_TOKEN=$TOKEN ${INIT[*]} </dev/null >/dev/null 2>&1"
check "wrong Cloudflare token refused" bash -c "! DOOTD_ADMIN_PASSWORD='$PW' DOOTD_CLOUDFLARE_TOKEN=wrong ${INIT[*]} </dev/null > $OUT/init2.txt 2>&1 && grep -q 'token rejected' $OUT/init2.txt"
check "domain outside the account refused" bash -c "! DOOTD_ADMIN_PASSWORD='$PW' DOOTD_CLOUDFLARE_TOKEN=$TOKEN dootd init --email admin@example.test --domain dootd.other.test --ipv4 $IP1 --ipv6 off --yes --cloudflare-api $CF_API </dev/null > $OUT/init3.txt 2>&1 && grep -q 'no Cloudflare zone found' $OUT/init3.txt"
check "service still not started" bash -c "! systemctl is-active --quiet $UNIT"

say "dootd init"
DOOTD_ADMIN_PASSWORD="$PW" DOOTD_CLOUDFLARE_TOKEN="$TOKEN" "${INIT[@]}" </dev/null | tee "$OUT/init.txt"
check "service running" systemctl is-active --quiet "$UNIT"
check "config names the dashboard domain" grep -q "dashboard_domain = \"$D\"" /etc/dootd/config.toml
check "config keeps the given IPv4" grep -q "public_ipv4 = \"$IP1\"" /etc/dootd/config.toml
check "proxied DNS record for the dashboard" dns_is "$D" "$IP1"
check "Origin CA certificate for the dashboard" test -f "$DATA_ROOT/certs/$D/cert.pem"
check "AOP enforced for the zone" wait_for 30 edge_enforced
check "zone set to Full (strict)" mock_py "import sys; sys.exit(0 if s['zones']['z-example-test']['ssl']=='strict' else 1)"
check "token stored encrypted" bash -c "! grep -aqF '$TOKEN' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"
check "admin can sign in through the edge" logged_in "$PW"
check "running init again without --yes on no terminal is refused" bash -c "! DOOTD_ADMIN_PASSWORD='x' dootd init --cloudflare-api $CF_API </dev/null > $OUT/init4.txt 2>&1 && grep -q cancelled $OUT/init4.txt"
check "service still running after the refusal" wait_for 30 is_version v0.7.0

say "dootd init on a terminal (script(1) pty)"
# Each answer is typed once its prompt is on the screen, like a person would.
mkfifo "$OUT/tty.in"
script -qefc "dootd init --cloudflare-api $CF_API" /dev/null < "$OUT/tty.in" > "$OUT/init-tty.txt" 2>&1 &
SPID=$!
exec 3>"$OUT/tty.in"
answer() { wait_for 90 grep -qF -- "$1" "$OUT/init-tty.txt" && printf '%s\n' "$2" >&3; }
answer "Run the setup again?" y
answer "Admin email" admin@example.test
answer "Password (at least" "$PW2"
answer "Repeat password" "$PW2"
answer "Dashboard domain (e.g." ""
answer "API token:" "$TOKEN"
wait "$SPID" || true
exec 3>&-
tr -d '\r' < "$OUT/init-tty.txt" | tail -n 5
check "interactive run finished" grep -q 'Done. Open https://' "$OUT/init-tty.txt"
check "prompts were shown" grep -q 'Admin email' "$OUT/init-tty.txt"
check "password was not echoed" bash -c "! grep -qF '$PW2' $OUT/init-tty.txt"
check "new password works" wait_for 30 logged_in "$PW2"
check "old password no longer works" test "$(login "$PW")" = 401
logged_in "$PW2"

say "An app with data, backups and the recovery kit"
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com
git init -q --bare -b main "$BARE"
cp -r examples/sample-c "$WORK" && rm -rf "$WORK/build" "$WORK/third_party"
git -C "$WORK" init -q -b main && git -C "$WORK" remote add origin "file://$BARE"
commit "v1"
post /settings/s3 --data endpoint=http://127.0.0.1:9000 --data region=us-east-1 --data bucket="$BUCKET" --data prefix=dootd \
  --data access_key="$S3_KEY" --data-urlencode "secret_key=$S3_SECRET" >/dev/null
check "S3 settings saved" flash_has "A test file was uploaded" /settings
post /apps --data name=web --data type=c --data-urlencode "repo=file://$BARE" --data branch=main --data domain=$APP_HOST \
  --data memory=128M --data cpu=1 --data pids=256 --data build_memory=1G --data build_timeout=15m >/dev/null
check "app DNS record" wait_for 30 dns_is "$APP_HOST" "$IP1"
wait_for 30 test -f "$DATA_ROOT/certs/$APP_HOST/cert.pem"
loc="$(post /apps/web/deploy)"
check "deploy succeeds" test "$(follow "/deployments/${loc##*/deployments/}")" = succeeded
for _ in $(seq 1 6); do visit >/dev/null; done
HOST_ID="$(db "SELECT value FROM settings WHERE key='host_id'")"
echo "  host id $HOST_ID"

say "Self-update from the dashboard"
cat >> /etc/dootd/config.toml <<EOF

[update]
api = "$GH"
EOF
systemctl restart "$UNIT"
wait_for 30 is_version v0.7.0
wait_for 30 site_up
logged_in "$PW2"
check "settings page shows the running version" page_has /settings "runs dootd <b>v0.7.0</b>"
post /settings/update/check >/dev/null
check "no update while v0.7.0 is the latest" flash_has "You run the newest version" /settings
publish v0.8.0
refresh_csrf /settings
post /settings/update/check >/dev/null
check "v0.8.0 offered" flash_has "dootd v0.8.0 is available" /settings
check "update button shown" page_has /settings "Update to v0.8.0"
SCHEMA_BEFORE="$(schema)"
BEFORE="$(visits)"
refresh_csrf /settings
post /settings/update/install >/dev/null
check "dootd restarted into v0.8.0" wait_for 90 is_version v0.8.0
check "previous binary kept as dootd.prev" bash -c "[ \"\$(/usr/local/bin/dootd.prev version | awk '{print \$2}')\" = v0.7.0 ]"
check "dootd.db copied before the migration" test -f "$DATA_ROOT/dootd.db.pre-update"
check "the copy has the old schema" test "$(pre_schema)" = "$SCHEMA_BEFORE"
check "database migrated" test "$(schema)" = $((SCHEMA_BEFORE + 1))
check "app serving again" wait_for 60 site_up
check "app data kept" test "$(visits)" -gt "$BEFORE"
check "journal shows the guard counting the start" bash -c "journalctl -u $UNIT --no-pager | grep -q 'update v0.7.0 -> v0.8.0: start 1 of 3'"
check "update recorded once it ran for a while" wait_for 60 test ! -e "$DATA_ROOT/update.json"
logged_in "$PW2"
check "settings page reports the update" page_has /settings "updated from v0.7.0 to v0.8.0"

say "A tampered release is refused"
publish v1.0.0
refresh_csrf /settings
post /settings/update/install >/dev/null
check "install refused" flash_has "checksum mismatch" /settings
check "still v0.8.0, no restart" is_version v0.8.0
check "no marker left" test ! -e "$DATA_ROOT/update.json"

say "A release that cannot start is rolled back"
publish v0.9.0
check "ctl sees v0.9.0" bash -c "dootd ctl update | grep -q 'v0.9.0 is available'"
dootd ctl update --install
check "v0.9.0 was installed" wait_for 20 bash -c "[ \"\$(running_version)\" = v0.9.0 ]"
check "rolled back to v0.8.0 after 3 failed starts" wait_for 180 is_version v0.8.0
check "journal shows the rollback" bash -c "journalctl -u $UNIT --no-pager | grep -q 'v0.9.0 did not start 3 times'"
check "database restored to the v0.8.0 schema" test "$(schema)" = $((SCHEMA_BEFORE + 1))
check "no table from the broken release" test -z "$(db "SELECT name FROM sqlite_master WHERE name='e2e_broken'")"
check "app serving again" wait_for 60 site_up
logged_in "$PW2"
check "settings page reports the rollback" page_has /settings "v0.9.0 did not start 3 times"
publish v0.8.0

say "Destroy the server and rebuild it with dootd init --restore"
dc -o "$OUT/kit.txt" -H "Origin: https://$D" --data-urlencode "csrf=$CSRF" "https://$D/settings/recovery-kit"
check "kit names the region" grep -q '^Region: *us-east-1' "$OUT/kit.txt"
check "kit explains the rebuild" grep -q 'dootd init --restore' "$OUT/kit.txt"
dootd ctl backup web
dootd ctl backup _dootd
SAVED="$(visits)"   # counted after the backup, so the backup holds SAVED-1
KEY_BEFORE="$(cat /etc/dootd/master.key)"
systemctl stop "$UNIT"
userdel dootd-web
rm -rf "$DATA_ROOT" /etc/dootd /etc/systemd/system/dootd.service /usr/local/bin/dootd*
systemctl daemon-reload
check "server wiped" bash -c "! id dootd-web 2>/dev/null && [ ! -e $DATA_ROOT ]"
install_from v0.8.0 >/dev/null
check "fresh install made a new master key" test "$(cat /etc/dootd/master.key)" != "$KEY_BEFORE"
check "restore refuses a missing secret without a terminal" bash -c "! dootd init --restore $OUT/kit.txt --s3-access-key $S3_KEY </dev/null > $OUT/r1.txt 2>&1 && grep -q DOOTD_S3_SECRET $OUT/r1.txt"
check "restore refuses a wrong secret" bash -c "! DOOTD_S3_SECRET=wrong dootd init --restore $OUT/kit.txt --s3-access-key $S3_KEY --ipv4 $IP2 --ipv6 off --yes </dev/null >/dev/null 2>&1"
check "nothing restored after the refusals" test ! -e "$DATA_ROOT/dootd.db"
DOOTD_S3_SECRET="$S3_SECRET" dootd init --restore "$OUT/kit.txt" --s3-access-key "$S3_KEY" --ipv4 "$IP2" --ipv6 off --yes </dev/null | tee "$OUT/restore.txt"
check "master key from the kit" test "$(cat /etc/dootd/master.key)" = "$KEY_BEFORE"
check "unused install key kept aside" bash -c "ls /etc/dootd/master.key.replaced-*"
check "same host id" test "$(db "SELECT value FROM settings WHERE key='host_id'")" = "$HOST_ID"
check "app user recreated" id dootd-web
check "app database restored, owned by the app user" test "$(stat -c %U $DATA_ROOT/apps/web/data/app.db)" = dootd-web
check "restore queued a deploy" grep -q 'web: deployment #' "$OUT/restore.txt"
check "DNS for the dashboard points at the new IP" dns_is "$D" "$IP2"
check "DNS for the app points at the new IP" wait_for 30 dns_is "$APP_HOST" "$IP2"
check "AOP enforced again (new client certificate)" wait_for 60 edge_enforced
check "app rebuilt and serving" wait_for 300 site_up
check "visits are back to the backup's value" test "$(visits)" = "$SAVED"
check "old password still works" logged_in "$PW2"
check "app page lists the old backups" page_has /apps/web 'action="/apps/web/restore"'
dootd ctl backup web > "$OUT/after.txt"
check "new backups go to the same bucket prefix" grep -q 'bucket + server' "$OUT/after.txt"
check "the restore refuses to run twice" bash -c "! DOOTD_S3_SECRET=$S3_SECRET dootd init --restore $OUT/kit.txt --s3-access-key $S3_KEY --yes </dev/null >/dev/null 2>&1"

say "Hardening: file descriptors and restart time with 5 apps"
check "dootd may open 65536 files" grep -Eq '^Max open files +65536 +65536' "/proc/$(systemctl show -p MainPID --value $UNIT)/limits"
APP_PID="$(pgrep -u dootd-web -x sample-c | head -n1)"
check "apps may open 4096 files" grep -Eq '^Max open files +4096 +4096' "/proc/$APP_PID/limits"
mkdir -p "$E2E/echo" /etc/systemd/system/dootd.service.d
install -m 0755 "$E2E-bin/e2etool" "$E2E/echo/echo-app"
chmod 0755 "$E2E" "$E2E/echo"
for i in 1 2 3 4 5; do
  printf '[[app]]\nname = "echo%d"\ntype = "c"\nport = %d\nrelease_dir = "%s"\nrun = "echo-app echo"\nhealth_path = "/healthz"\n\n' "$i" $((20070 + i)) "$E2E/echo"
done > /etc/dootd/dev-apps.toml
cat > /etc/systemd/system/dootd.service.d/dev.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/bin/dootd serve --dev-apps /etc/dootd/dev-apps.toml
EOF
systemctl daemon-reload
systemctl restart "$UNIT"
all_up() { for p in 20071 20072 20073 20074 20075; do curl -fsS -m 2 "http://127.0.0.1:$p/healthz" >/dev/null || return 1; done; site_up; }
wait_for 120 all_up
start=$(date +%s.%N)
systemctl restart "$UNIT"
check "6 apps healthy again after a restart" wait_for 120 all_up
took="$(python3 -c "print(round($(date +%s.%N) - $start, 1))")"
echo "  restart with 6 apps (5 echo + web) took ${took}s"
check "restart with 6 apps under 30s" python3 -c "import sys; sys.exit(0 if $took < 30 else 1)"
RSS="$(awk '/^VmRSS/ {print $2}' "/proc/$(systemctl show -p MainPID --value $UNIT)/status")"
echo "  dootd RSS ${RSS} kB"
systemctl stop "$UNIT"

say "Result"
if [ "$FAILS" -gt 0 ]; then
  echo "---- journal ----"; journalctl -u "$UNIT" --no-pager -n 200 || true
  echo "$FAILS check(s) failed"; exit 1
fi
echo "all checks passed"
