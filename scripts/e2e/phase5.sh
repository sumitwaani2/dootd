#!/usr/bin/env bash
# Phase 5 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# Uses the Phase 4 setup (dashboard through the edge, fake Cloudflare) plus
# a real S3-compatible server (rclone serve s3) and checks: S3 settings with
# a test upload, pre-deploy / manual / scheduled backups, consistency under
# heavy writes, a delete-the-data-and-restore drill, restore from the bucket
# when the local copy is gone, a tampered backup being refused without
# touching the app, retention, uploads retried after a bucket outage, the
# daily dootd.db backup, the recovery kit and deleting an app's backups.
#
#   sudo ./scripts/e2e/phase5.sh
set -euo pipefail

UNIT="dootd"
DATA_ROOT="/var/lib/dootd"
E2E="/opt/dootd-e2e5"
MOCK="$E2E/mock"
OUT="$E2E/out"
S3DIR="$E2E/s3"
BUCKET="backups"
BARE="$E2E/git/web.git"
WORK="$E2E/work/web"
CF_IP="198.18.0.10"
D="dootd.example.test"
APP_HOST="web.example.test"
TOKEN="e2e-cf-token-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
S3_KEY="E2EACCESS"
S3_SECRET="e2e-secret-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PW="phase five password"
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
# Capture first: `curl | grep -q` fails under pipefail when grep exits early.
page_has()  { local b; b="$(get "$1")" || return 1; grep -qF -- "$2" <<<"$b"; }
page_lacks() { local b; b="$(get "$1")" || return 1; ! grep -qF -- "$2" <<<"$b"; }
flash_has() { local b; b="$(get "${2:-/account}")" || return 1; grep -qF -- "$1" <<<"$b"; }
visit()     { curl -sS -m 10 "${TLS[@]}" "https://$APP_HOST/"; }
visits()    { visit | grep -oE 'Visits: [0-9]+' | grep -oE '[0-9]+'; }
site_up()   { [ "$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "${TLS[@]}" "https://$APP_HOST/healthz")" = 200 ]; }
follow()    { dc -N --max-time 600 "https://$D$1/stream" > "$OUT/stream.txt"; grep -A1 '^event: done' "$OUT/stream.txt" | tail -n1 | sed 's/^data: //'; }
commit()    { git -C "$WORK" add -A && git -C "$WORK" commit -qm "$1" && git -C "$WORK" push -q origin HEAD; }
sqlite3_py() { python3 -c "import sqlite3,sys; c=sqlite3.connect('file:$DATA_ROOT/dootd.db?mode=ro', uri=True); r=c.execute(sys.argv[1]).fetchone(); print(r[0].decode() if isinstance(r[0], bytes) else r[0])" "$1"; }
objects()   { find "$S3DIR/$BUCKET/dootd/$HOST_ID/$1" -type f -name '*.zst' 2>/dev/null | sort; }
nobjects()  { objects "$1" | wc -l; }
has_objects() { [ "$(nobjects "$1")" -ge "$2" ]; }
latest_id() { dootd ctl backups web | awk 'NR==2 {print $1}'; }
any_is()    { dootd ctl backups web | awk -v k="$1" -v u="$2" 'NR>1 && $4==k && $5=="ok" && $7==u {f=1} END {exit !f}'; }
uploaded()  { python3 -c "import sqlite3,sys; c=sqlite3.connect('file:$DATA_ROOT/dootd.db?mode=ro',uri=True); r=c.execute('SELECT object_key FROM backups WHERE id=?',(int(sys.argv[1]),)).fetchone(); sys.exit(0 if r and r[0] else 1)" "$1"; }
not_uploaded() { ! uploaded "$1"; }
integrity() { # integrity <archive>: unpack and run integrity_check on every database
  local d; d="$(mktemp -d)"
  zstd -dq -c "$1" | tar -x -C "$d" || return 1
  python3 - "$d" <<'PY'
import json, sqlite3, sys, os
d = sys.argv[1]
m = json.load(open(os.path.join(d, "manifest.json")))
assert m["files"], "no files"
for f in m["files"]:
    r = sqlite3.connect(os.path.join(d, "data", f["path"])).execute("PRAGMA integrity_check").fetchone()[0]
    assert r == "ok", (f["path"], r)
print("ok", len(m["files"]))
PY
}
# --dir-cache-time 1s: the test also plants and tampers with files directly on disk.
s3_start() { systemd-run --unit=s3mock --collect -q "$E2E-bin/rclone" serve s3 --addr 127.0.0.1:9000 \
  --dir-cache-time 1s --auth-key "$S3_KEY,$S3_SECRET" "$S3DIR"; wait_for 10 curl -s -o /dev/null http://127.0.0.1:9000/; }

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
cd "$(dirname "$0")/../.."
echo "kernel $(uname -r), $(. /etc/os-release && echo "$PRETTY_NAME")"
command -v zstd >/dev/null || apt-get install -y -qq zstd >/dev/null

say "Build and install"
make build
mkdir -p "$E2E-bin"
go build -o "$E2E-bin/e2etool" ./scripts/e2e/e2etool
if [ ! -x "$E2E-bin/rclone" ]; then
  arch="$(dpkg --print-architecture)"
  curl -fsSL -o /tmp/rclone.zip "https://downloads.rclone.org/rclone-current-linux-$arch.zip"
  unzip -qo /tmp/rclone.zip -d /tmp/rclone && install -m 0755 /tmp/rclone/rclone-*/rclone "$E2E-bin/rclone"
fi
"$E2E-bin/rclone" version | head -n1
for u in "$UNIT" cfmock s3mock; do systemctl stop "$u" 2>/dev/null || true; done
install -m 0755 dist/dootd /usr/local/bin/dootd
rm -rf "$E2E" "$DATA_ROOT" /etc/dootd /etc/systemd/system/dootd.service.d
mkdir -p "$MOCK" "$OUT" "$E2E/git" "$E2E/work" "$S3DIR/$BUCKET" /etc/dootd
ip addr add "$CF_IP/32" dev lo 2>/dev/null || true
systemd-run --unit=cfmock --collect -q "$E2E-bin/e2etool" cfmock -listen 127.0.0.1:8787 -dir "$MOCK" \
  -token "$TOKEN" -zones example.test -extra-range 198.18.0.0/15
wait_for 10 curl -fsS http://127.0.0.1:8787/_mock/state
s3_start

export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com
git init -q --bare -b main "$BARE"
cp -r examples/sample-c "$WORK" && rm -rf "$WORK/build" "$WORK/third_party"
git -C "$WORK" init -q -b main && git -C "$WORK" remote add origin "file://$BARE"
commit "v1"

cat > /etc/dootd/config.toml <<EOF
[edge]
dashboard_domain = "$D"
public_ipv4      = "203.0.113.10"
public_ipv6      = "off"
cloudflare_api   = "http://127.0.0.1:8787/client/v4"

[backups]
interval  = "30s"
retention = "10m"
EOF
install -m 0644 contrib/systemd/dootd.service /etc/systemd/system/dootd.service
systemctl daemon-reload
systemctl start "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock
printf '%s' "$TOKEN" | dootd ctl cloudflare-token >/dev/null
dootd ctl edge sync >/dev/null
wait_for 60 bash -c "dootd ctl edge | grep -Eq '^example.test .* enforced'"
printf '%s' "$PW" | dootd ctl admin set-password --email admin@example.test >/dev/null
curl -sS "${TLS[@]}" -c "$JAR" -o /dev/null -H "Origin: https://$D" --data-urlencode email=admin@example.test --data-urlencode "password=$PW" "https://$D/login"
refresh_csrf /

say "Before a bucket is configured"
check "home warns that backups are local only" page_has / "No S3 bucket is set"
check "home asks for the recovery kit" page_has / "Download the recovery kit"
check "dootd.db was backed up locally at startup" bash -c "ls $DATA_ROOT/backups/local/_dootd/*-dootd.db.zst"

say "S3 settings"
s3form() { post /settings/s3 --data endpoint=http://127.0.0.1:9000 --data region=us-east-1 --data bucket="$1" --data prefix=dootd \
  --data access_key="$S3_KEY" --data-urlencode "secret_key=$2"; }
s3form "$BUCKET" wrong-secret >/dev/null
check "wrong secret: not saved, reason shown" flash_has "S3 settings not saved" /settings
s3form missing-bucket "$S3_SECRET" >/dev/null
check "missing bucket: not saved, reason shown" flash_has "NoSuchBucket" /settings
s3form "$BUCKET" "$S3_SECRET" >/dev/null
check "correct settings saved after a test upload" flash_has "A test file was uploaded, read back and deleted" /settings
check "test object was deleted again" test ! -e "$S3DIR/$BUCKET/dootd/.dootd-connection-test"
check "secret not stored in plain text" bash -c "! grep -aqF '$S3_SECRET' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"
check "settings page never shows the secret" page_lacks /settings "$S3_SECRET"
check "home no longer warns about the bucket" wait_for 10 page_lacks / "No S3 bucket is set"
HOST_ID="$(sqlite3_py "SELECT value FROM settings WHERE key='host_id'")"
echo "  host id $HOST_ID"
check "local-only dootd.db backup uploaded once a bucket exists" wait_for 30 has_objects _dootd 1

say "Create and deploy the app (no databases yet)"
post /apps --data name=web --data type=c --data-urlencode "repo=file://$BARE" --data branch=main --data domain=$APP_HOST \
  --data memory=128M --data cpu=1 --data pids=256 --data build_memory=1G --data build_timeout=15m >/dev/null
wait_for 30 test -f "$DATA_ROOT/certs/$APP_HOST/cert.pem"
loc="$(post /apps/web/deploy)"
check "first deploy succeeds" test "$(follow "/deployments/${loc##*/deployments/}")" = succeeded
check "pre-deploy step notes there is nothing to back up yet" grep -q 'no SQLite databases in DATA_DIR yet' "$OUT/stream.txt"
for _ in $(seq 1 5); do visit >/dev/null; done
check "app counts visits in SQLite" test "$(visits)" = 6

say "Manual backup"
post /apps/web/backup >/dev/null
check "backup taken and uploaded" flash_has "Uploaded to the bucket" /apps/web
check "archive is in the bucket" has_objects web 1
check "app page lists it with a restore button" page_has /apps/web 'action="/apps/web/restore"'
check "WAL files still belong to the app user" bash -c "for f in $DATA_ROOT/apps/web/data/app.db-*; do [ ! -e \"\$f\" ] || [ \"\$(stat -c %U \"\$f\")\" = dootd-web ] || exit 1; done"
BACKUP1="$(latest_id)"
SAVED="$(visits)"   # counted after the backup, so the backup holds SAVED-1
echo "  backup #$BACKUP1 holds $((SAVED - 1)) visits"
check "uploaded archive passes integrity_check" integrity "$(objects web | tail -n1)"

say "Pre-deploy backup"
sed -i 's|<h1>[^<]*</h1>|<h1>web v2</h1>|' "$WORK/src/main.c"
commit "v2"
loc="$(post /apps/web/deploy)"
check "second deploy succeeds" test "$(follow "/deployments/${loc##*/deployments/}")" = succeeded
check "build log shows the pre-deploy backup" grep -qE 'backup #[0-9]+: 1 database' "$OUT/stream.txt"
check "pre-deploy backup uploaded" wait_for 30 any_is pre-deploy true
check "data survived the deploy" test "$(visits)" -gt "$SAVED"

say "Consistent backup under heavy writes"
( end=$((SECONDS + 8)); while [ $SECONDS -lt $end ]; do visit >/dev/null 2>&1 || true; done ) &
LOAD=$!
sleep 2
check "backup during writes succeeds" dootd ctl backup web
wait "$LOAD"
check "that archive passes integrity_check" integrity "$(ls -t $DATA_ROOT/backups/local/web/*.tar.zst | head -n1)"

say "Drill: delete the data, restore from the dashboard"
dootd ctl stop web >/dev/null
rm -f "$DATA_ROOT"/apps/web/data/app.db*
dootd ctl start web >/dev/null
wait_for 20 site_up
check "data is gone (counter restarted)" test "$(visits)" -le 2
PID_BEFORE="$(pgrep -u dootd-web -x sample-c)"
refresh_csrf /apps/web
post /apps/web/restore --data "id=$BACKUP1" >/dev/null
check "restore reported success" flash_has "Backup #$BACKUP1 restored (app.db)" /apps/web
check "app runs again" wait_for 20 site_up
check "app was restarted" test "$(pgrep -u dootd-web -x sample-c)" != "$PID_BEFORE"
check "visits are back to the backup's value" test "$(visits)" = "$SAVED"
check "previous database kept in .pre-restore-*" bash -c "ls $DATA_ROOT/apps/web/data/.pre-restore-*/app.db"
check "restored file belongs to the app user" test "$(stat -c %U "$DATA_ROOT/apps/web/data/app.db")" = dootd-web

say "Restore from the bucket when the local copy is gone"
rm -f "$DATA_ROOT"/backups/local/web/*
check "restore via ctl works" dootd ctl restore web "$BACKUP1"
check "visits back to the backup's value again" test "$(visits)" = "$SAVED"

say "A tampered backup is refused and the app is not touched"
KEY_FILE="$(objects web | head -n1)"
TAMPER_ID="$(python3 -c "import sqlite3; c=sqlite3.connect('file:$DATA_ROOT/dootd.db?mode=ro',uri=True); print(c.execute(\"SELECT id FROM backups WHERE app='web' AND object_key LIKE '%$(basename "$KEY_FILE")'\").fetchone()[0])")"
# Flip bytes in the middle, keeping the size, like silent corruption would.
python3 -c "import sys; p=sys.argv[1]; b=bytearray(open(p,'rb').read()); m=len(b)//2; b[m:m+8]=bytes(x^0xff for x in b[m:m+8]); open(p,'wb').write(b)" "$KEY_FILE"
sleep 2
rm -f "$DATA_ROOT"/backups/local/web/*
PID_BEFORE="$(pgrep -u dootd-web -x sample-c)"
check "restore of tampered backup fails" bash -c "! dootd ctl restore web $TAMPER_ID 2> $OUT/tamper.txt"
cat "$OUT/tamper.txt"
check "error names the checksum" grep -q 'checksum does not match' "$OUT/tamper.txt"
check "app kept running (same process)" test "$(pgrep -u dootd-web -x sample-c)" = "$PID_BEFORE"

say "Scheduled backups and retention"
old="$S3DIR/$BUCKET/dootd/$HOST_ID/web"
cp "$(objects web | tail -n1)" "$old/20200101T000000Z-scheduled.tar.zst"
cp "$(objects web | tail -n1)" "$old/20200102T000000Z-scheduled.tar.zst"
sleep 2
sched_ran() { any_is scheduled true; }
check "a scheduled backup ran (interval 30s)" wait_for 45 sched_ran
check "objects older than the retention were deleted" wait_for 45 test ! -e "$old/20200101T000000Z-scheduled.tar.zst"
check "the other old one too" test ! -e "$old/20200102T000000Z-scheduled.tar.zst"
check "at most 2 local copies kept" test "$(ls $DATA_ROOT/backups/local/web | wc -l)" -le 2

say "Bucket outage: upload retried later, badge meanwhile"
systemctl stop s3mock
dootd ctl backup web > "$OUT/outage.txt" 2>&1 || true
cat "$OUT/outage.txt"
OUTAGE_ID="$(grep -oE 'backup #[0-9]+' "$OUT/outage.txt" | grep -oE '[0-9]+')"
check "backup still taken locally" grep -q 'this server only' "$OUT/outage.txt"
check "not uploaded while the bucket is down" not_uploaded "$OUTAGE_ID"
check "dashboard shows the problem" wait_for 20 page_has / "backup failed"
s3_start
check "the upload is retried once the bucket is back" wait_for 90 uploaded "$OUTAGE_ID"
check "badge cleared" wait_for 60 page_lacks / "backup failed"

say "Recovery kit"
refresh_csrf /settings
dc -o "$OUT/kit.txt" -H "Origin: https://$D" --data-urlencode "csrf=$CSRF" "https://$D/settings/recovery-kit"
check "kit contains the master key" grep -qF "$(cat /etc/dootd/master.key)" "$OUT/kit.txt"
check "kit names the bucket location" grep -qF "bucket $BUCKET  under dootd/$HOST_ID/" "$OUT/kit.txt"
check "kit does not contain the S3 secret" bash -c "! grep -qF '$S3_SECRET' $OUT/kit.txt"
check "home stops asking for the kit" page_lacks / "Download the recovery kit"
check "kit download needs the CSRF token" bash -c "[ \"\$(curl -sS ${TLS[*]} -b $JAR -o /dev/null -w '%{http_code}' -H 'Origin: https://$D' -X POST https://$D/settings/recovery-kit)\" = 403 ]"

say "Delete the app together with its backups"
refresh_csrf /apps/web
post /apps/web/delete --data confirm=web --data keep_data=1 --data delete_backups=1 >/dev/null
check "flash confirms backups were deleted" flash_has "Its backups were deleted" /
check "no objects left for the app" test "$(nobjects web)" -eq 0
check "no local copies left" test ! -e "$DATA_ROOT/backups/local/web"
check "dootd.db backups untouched" has_objects _dootd 1
systemctl stop "$UNIT"

say "Result"
if [ "$FAILS" -gt 0 ]; then
  echo "---- journal ----"; journalctl -u "$UNIT" --no-pager -n 150 || true
  echo "$FAILS check(s) failed"; exit 1
fi
echo "all checks passed"
