#!/usr/bin/env bash
# Phase 7 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# The life of a server with the only command a user ever runs, install.sh
# (served from local release directories), a fake Cloudflare API and a real
# S3 server (rclone serve s3):
#   install (checksum refusal, directories, master key, unit, one-time
#   password) → setup in the dashboard → an app with data and backups →
#   update by running install.sh again (a release with a migration; apps,
#   data, settings and sessions kept) → recovery with the new one-time
#   password (forgotten password) → the server destroyed and a new one set
#   up from scratch, the app coming back from its bucket folder → file
#   descriptor limits and restart timing with 6 apps.
#
#   sudo ./scripts/e2e/phase7.sh
cd "$(dirname "$0")/../.."
. scripts/e2e/lib.sh

S3DIR="$E2E/s3"
BUCKET="backups"
APP_HOST="web.example.test"
S3_KEY="E2EACCESS"
S3_SECRET="e2e-secret-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PW2="recovered password 7"
IP2="203.0.113.20"
ASSET="dootd-linux-$ARCH"
MIGRATIONS="internal/store/migrations"

visits()    { site "$APP_HOST" / | grep -oE 'Visits: [0-9]+' | grep -oE '[0-9]+'; }
site_up()   { site_is 200 "$APP_HOST" /healthz; }
installed_version() { /usr/local/bin/dootd version | awk '{print $2}'; }
schema()    { db "SELECT MAX(version) FROM schema_migrations"; }
setup_open() { ! setup_closed; }
s3_start() { systemd-run --unit=s3mock --collect -q "$BIN/rclone" serve s3 --addr 127.0.0.1:9000 \
  --dir-cache-time 1s --auth-key "$S3_KEY,$S3_SECRET" "$S3DIR"; wait_for 10 curl -s -o /dev/null http://127.0.0.1:9000/; }
# release <tag> [binary]: a release directory $REL/<tag> with its checksums.
release() {
  mkdir -p "$REL/$1"
  if [ -n "${2:-}" ]; then install -m 0755 "$2" "$REL/$1/$ASSET"; else make build VERSION="$1" >/dev/null && install -m 0755 dist/dootd "$REL/$1/$ASSET"; fi
  (cd "$REL/$1" && sha256sum "$ASSET" > checksums.txt)
}

command -v zstd >/dev/null || apt-get install -y -qq zstd >/dev/null
if [ ! -x "$BIN/rclone" ]; then
  mkdir -p "$BIN"
  curl -fsSL -o /tmp/rclone.zip "https://downloads.rclone.org/rclone-current-linux-$ARCH.zip"
  unzip -qo /tmp/rclone.zip -d /tmp/rclone && install -m 0755 /tmp/rclone/rclone-*/rclone "$BIN/rclone"
fi
e2e_prepare
mkdir -p "$S3DIR/$BUCKET"
s3_start

say "Build three releases"
trap 'rm -f $MIGRATIONS/0*_e2e_*.sql' EXIT
release v0.7.0
# v0.8.0 adds a migration: updating to it must keep every app and setting.
N_MIG="$(ls "$MIGRATIONS" | wc -l)"
echo "CREATE TABLE e2e_update (x INTEGER) STRICT;" > "$MIGRATIONS/$(printf '%04d' $((N_MIG + 1)))_e2e_update.sql"
release v0.8.0
rm -f "$MIGRATIONS"/0*_e2e_*.sql
# v1.0.0: a genuine binary whose download does not match checksums.txt.
release v1.0.0 "$REL/v0.8.0/$ASSET"
echo "$(printf '0%.0s' $(seq 64))  $ASSET" > "$REL/v1.0.0/checksums.txt"
make build >/dev/null

say "install.sh"
check "tampered download refused" bash -c "! DOOTD_BASE_URL=http://127.0.0.1:8900/v1.0.0 bash ./install.sh > $OUT/bad-install.txt 2>&1"
check "error names the checksum mismatch" grep -q 'checksum mismatch' "$OUT/bad-install.txt"
check "nothing was installed" test ! -e /usr/local/bin/dootd
check "install v0.7.0" e2e_install v0.7.0
check "binary installed" test "$(installed_version)" = v0.7.0
check "master key created with mode 0600" test "$(stat -c %a /etc/dootd/master.key)" = 600
check "/etc/dootd is 0700" test "$(stat -c %a /etc/dootd)" = 700
check "/var/lib/dootd is 0711" test "$(stat -c %a $DATA_ROOT)" = 711
check "dootd.db is 0600" test "$(stat -c %a $DATA_ROOT/dootd.db)" = 600
check "installed unit is the one in contrib/" cmp -s /etc/systemd/system/dootd.service contrib/systemd/dootd.service
check "unit enabled" systemctl is-enabled --quiet "$UNIT"
check "service running" systemctl is-active --quiet "$UNIT"
check "installer asked nothing and printed a one-time password" test -n "$OTP"
check "there is no config file to edit" test ! -e /etc/dootd/config.toml
check "dootd has no admin commands" bash -c "! dootd ctl status >/dev/null 2>&1 && ! dootd init >/dev/null 2>&1"
KEY1="$(sha256sum /etc/dootd/master.key)"

say "Setup in the dashboard"
use_setup; rm -f "$JAR"
check "one-time password signs in" test "$(login "$OTP" "")" = 303
refresh_csrf /setup
post /setup --data-urlencode "email=$ADMIN" --data-urlencode "password=$PW" --data-urlencode "confirm=$PW" >/dev/null
refresh_csrf /settings
post /settings/cloudflare-token --data-urlencode "token=$TOKEN" >/dev/null
post /settings/dashboard-domain --data "domain=$D" >/dev/null
check "setup address closes" wait_for 120 setup_closed
use_domain
check "admin signs in on the dashboard domain" wait_for 30 signed_in

say "An app with data and backups"
post /settings/s3 --data endpoint=http://127.0.0.1:9000 --data region=us-east-1 --data bucket="$BUCKET" \
  --data access_key="$S3_KEY" --data-urlencode "secret_key=$S3_SECRET" >/dev/null
check "S3 settings saved" flash_has "A test file was uploaded" /settings
mkrepo web examples/sample-c
create_app web --data domain=$APP_HOST --data memory=128M >/dev/null
check "app DNS record" wait_for 30 dns_is "$APP_HOST"
wait_for 30 test -f "$DATA_ROOT/certs/$APP_HOST/cert.pem"
check "deploy succeeds" deploy_is succeeded web
for _ in $(seq 1 6); do site "$APP_HOST" / >/dev/null; done

say "Update: run install.sh again"
SCHEMA_BEFORE="$(schema)"
BEFORE="$(visits)"
check "install v0.8.0" e2e_install v0.8.0
check "binary updated" test "$(installed_version)" = v0.8.0
check "installer said dootd was stopped for the update" grep -q 'Stopping dootd' "$INSTALL_OUT"
check "master key kept" test "$(sha256sum /etc/dootd/master.key)" = "$KEY1"
check "database migrated" test "$(schema)" = $((SCHEMA_BEFORE + 1))
check "app serving again" wait_for 60 site_up
check "app data kept" test "$(visits)" -gt "$BEFORE"
check "the session survived the update" wait_for 30 page_has / '<b>web</b>'
check "installer points to the dashboard domain too" grep -q "Dashboard: *https://$D" "$INSTALL_OUT"
check "running version shown in the footer" page_has / "dootd v0.8.0"

say "Recovery: the new one-time password (forgotten password)"
check "setup address opens again for the new one-time password" wait_for 30 setup_open
use_setup; rm -f "$JAR"
check "the sign-in page there only asks for the one-time password" page_has /login "Only the one-time password"
check "the admin password is not accepted outside Cloudflare" test "$(login "$PW" "$ADMIN")" = 401
check "the new one-time password signs in" test "$(login "$OTP" "")" = 303
refresh_csrf /setup
check "a new password is set" post_is 303 /setup --data-urlencode "email=$ADMIN" --data-urlencode "password=$PW2" --data-urlencode "confirm=$PW2"
check "setup address closes again" wait_for 30 setup_closed
use_domain; rm -f "$JAR"
check "old password no longer works" test "$(login "$PW")" = 401
PW="$PW2"
check "new password works" wait_for 10 signed_in

say "A new server: destroy this one, set up another from scratch"
post /apps/web/backup >/dev/null
check "fresh backup uploaded" flash_has "Uploaded to the bucket" /apps/web
SAVED="$(visits)"   # counted after the backup, so the backup holds SAVED-1
systemctl stop "$UNIT"
userdel dootd-web
rm -rf "$DATA_ROOT" /etc/dootd /etc/systemd/system/dootd.service /usr/local/bin/dootd
systemctl daemon-reload
check "server wiped" bash -c "! id dootd-web 2>/dev/null && [ ! -e $DATA_ROOT ]"
PUB_IP="$IP2"; test_env
e2e_setup v0.8.0
check "fresh install made a new master key" test "$(sha256sum /etc/dootd/master.key)" != "$KEY1"
check "dashboard DNS points at the new server" wait_for 30 dns_is "$D" "$IP2"
post /settings/s3 --data endpoint=http://127.0.0.1:9000 --data region=us-east-1 --data bucket="$BUCKET" \
  --data access_key="$S3_KEY" --data-urlencode "secret_key=$S3_SECRET" >/dev/null
check "Add app preselects restoring from the folder with the app's name" page_has /apps/new ">backup folder web/ (newest backup)<"
create_app web --data domain=$APP_HOST --data memory=128M --data restore_from=@auto >/dev/null
check "data restored from web/" flash_has "Its data was restored from the newest backup in web/" /apps/web
check "app DNS points at the new server" wait_for 30 dns_is "$APP_HOST" "$IP2"
wait_for 30 test -f "$DATA_ROOT/certs/$APP_HOST/cert.pem"
check "deploy succeeds" deploy_is succeeded web
check "visits are back to the backup's value" test "$(visits)" = "$SAVED"

say "Hardening: file descriptors and restart time with 6 apps"
check "dootd may open 65536 files" grep -Eq '^Max open files +65536 +65536' "/proc/$(systemctl show -p MainPID --value $UNIT)/limits"
check "apps may open 4096 files" grep -Eq '^Max open files +4096 +4096' "/proc/$(pgrep -u dootd-web -x sample-c | head -n1)/limits"
for i in 1 2 3 4 5; do
  mkecho "echo$i"
  create_app "echo$i" >/dev/null
  deploy_is succeeded "echo$i" >/dev/null || fail "deploy echo$i"
done
PORTS="$(for i in 1 2 3 4 5; do port_of "echo$i"; done)"
all_up() { for p in $PORTS; do curl -fsS -m 2 "http://127.0.0.1:$p/healthz" >/dev/null || return 1; done; site_up; }
check "6 apps healthy" wait_for 120 all_up
start=$(date +%s.%N)
systemctl restart "$UNIT"
check "6 apps healthy again after a restart" wait_for 120 all_up
took="$(python3 -c "print(round($(date +%s.%N) - $start, 1))")"
echo "  restart with 6 apps took ${took}s"
check "restart with 6 apps under 30s" between "$took" 0 30
RSS="$(awk '/^VmRSS/ {print $2}' "/proc/$(systemctl show -p MainPID --value $UNIT)/status")"
echo "  dootd RSS ${RSS} kB"

e2e_result
