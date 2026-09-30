#!/usr/bin/env bash
# Phase 5 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# The dashboard through the edge (fake Cloudflare) plus a real
# S3-compatible server (rclone serve s3). Checks: S3 settings with a test
# upload, one folder per app in the bucket, pre-deploy / manual / scheduled
# backups, consistency under heavy writes, a delete-the-data-and-restore
# drill, restore from the bucket when the local copy is gone, a tampered
# backup being refused without touching the app, retention, uploads retried
# after a bucket outage, deleting an app's backups, and a new app that
# starts from a backup folder (its own name, or another one after a rename).
#
#   sudo ./scripts/e2e/phase5.sh
cd "$(dirname "$0")/../.."
. scripts/e2e/lib.sh

S3DIR="$E2E/s3"
BUCKET="backups"
APP_HOST="web.example.test"
S3_KEY="E2EACCESS"
S3_SECRET="e2e-secret-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"

visits()    { site "$APP_HOST" / | grep -oE 'Visits: [0-9]+' | grep -oE '[0-9]+'; }
site_up()   { site_is 200 "$APP_HOST" /healthz; }
objects()   { find "$S3DIR/$BUCKET/$1" -type f -name '*.tar.zst' 2>/dev/null | sort; }
nobjects()  { objects "$1" | wc -l; }
has_objects() { [ "$(nobjects "$1")" -ge "$2" ]; }
row()       { db "SELECT $1 FROM backups WHERE app = 'web' AND $2 ORDER BY id DESC LIMIT 1"; }
latest_id() { row id "status = 'ok'"; }
any_is()    { [ -n "$(row id "kind = '$1' AND status = 'ok' AND object_key != ''")" ]; }
uploaded()  { [ -n "$(db "SELECT object_key FROM backups WHERE id = $1")" ]; }
not_uploaded() { ! uploaded "$1"; }
app_pid()   { pgrep -u "dootd-$1" -x sample-c | head -n1; }
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
backup_now() { post /apps/web/backup >/dev/null; }
# --dir-cache-time 1s: the test also plants and tampers with files directly on disk.
s3_start() { systemd-run --unit=s3mock --collect -q "$BIN/rclone" serve s3 --addr 127.0.0.1:9000 \
  --dir-cache-time 1s --auth-key "$S3_KEY,$S3_SECRET" "$S3DIR"; wait_for 10 curl -s -o /dev/null http://127.0.0.1:9000/; }

command -v zstd >/dev/null || apt-get install -y -qq zstd >/dev/null
if [ ! -x "$BIN/rclone" ]; then
  mkdir -p "$BIN"
  curl -fsSL -o /tmp/rclone.zip "https://downloads.rclone.org/rclone-current-linux-$(dpkg --print-architecture).zip"
  unzip -qo /tmp/rclone.zip -d /tmp/rclone && install -m 0755 /tmp/rclone/rclone-*/rclone "$BIN/rclone"
fi
e2e_prepare
test_env BACKUP_INTERVAL=30s BACKUP_RETENTION=10m
mkdir -p "$S3DIR/$BUCKET"
s3_start
e2e_setup

say "Before a bucket is configured"
check "home warns that backups are local only" page_has / "No S3 bucket is set"
check "Add app offers no backup folders" page_lacks /apps/new "Start with data from"

say "S3 settings"
s3form() { post /settings/s3 --data endpoint=http://127.0.0.1:9000 --data region=us-east-1 --data bucket="$1" \
  --data access_key="$S3_KEY" --data-urlencode "secret_key=$2"; }
s3form "$BUCKET" wrong-secret >/dev/null
check "wrong secret: not saved, reason shown" flash_has "S3 settings not saved" /settings
s3form missing-bucket "$S3_SECRET" >/dev/null
check "missing bucket: not saved, reason shown" flash_has "NoSuchBucket" /settings
s3form "$BUCKET" "$S3_SECRET" >/dev/null
check "correct settings saved after a test upload" flash_has "A test file was uploaded, read back and deleted" /settings
check "test object was deleted again" test ! -e "$S3DIR/$BUCKET/.dootd-connection-test"
check "secret not stored in plain text" bash -c "! grep -aqF '$S3_SECRET' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"
check "settings page never shows the secret" page_lacks /settings "$S3_SECRET"
check "home no longer warns about the bucket" wait_for 10 page_lacks / "No S3 bucket is set"
check "dootd's own database is not backed up" test -z "$(ls -A "$S3DIR/$BUCKET")"

say "Create and deploy the app (no databases yet)"
check "sample-c's release workflow" sample_dist sample-c
DIST="$E2E/dist-sample-c"
mkrepo web
publish web v1 "$DIST"
check "Add app offers backup folders now" page_has /apps/new "Start with data from"
create_app web --data domain=$APP_HOST --data memory=128M --data restore_from=@auto >/dev/null
check "no folder with its name yet: starts empty" flash_has "App web created." /apps/web
wait_for 30 test -f "$DATA_ROOT/certs/$APP_HOST/cert.pem"
check "first deploy succeeds" deploy_is succeeded web v1
check "pre-deploy step notes there is nothing to back up yet" last_has 'no SQLite databases in DATA_DIR yet'
for _ in $(seq 1 5); do site "$APP_HOST" / >/dev/null; done
check "app counts visits in SQLite" test "$(visits)" = 6

say "Manual backup"
backup_now
check "backup taken and uploaded" flash_has "Uploaded to the bucket" /apps/web
check "archive is in the app's folder at the bucket root" has_objects web 1
check "app page names its bucket folder" page_has /apps/web "<code>web/</code>"
check "app page lists it with a restore button" page_has /apps/web 'action="/apps/web/restore"'
check "WAL files still belong to the app user" bash -c "for f in $DATA_ROOT/apps/web/data/app.db-*; do [ ! -e \"\$f\" ] || [ \"\$(stat -c %U \"\$f\")\" = dootd-web ] || exit 1; done"
BACKUP1="$(latest_id)"
SAVED="$(visits)"   # counted after the backup, so the backup holds SAVED-1
echo "  backup #$BACKUP1 holds $((SAVED - 1)) visits"
check "uploaded archive passes integrity_check" integrity "$(objects web | tail -n1)"

say "Pre-deploy backup"
publish web v2 "$DIST"
check "second deploy succeeds" deploy_is succeeded web v2
check "deploy log shows the pre-deploy backup" grep -qE 'backup #[0-9]+: 1 database' "$LAST"
check "pre-deploy backup uploaded" wait_for 30 any_is pre-deploy
check "data survived the deploy" test "$(visits)" -gt "$SAVED"

say "Consistent backup under heavy writes"
( end=$((SECONDS + 8)); while [ $SECONDS -lt $end ]; do site "$APP_HOST" / >/dev/null 2>&1 || true; done ) &
LOAD=$!
sleep 2
backup_now
check "backup during writes succeeds" flash_has "Uploaded to the bucket" /apps/web
wait "$LOAD"
check "that archive passes integrity_check" integrity "$(ls -t $DATA_ROOT/backups/local/web/*.tar.zst | head -n1)"

say "Drill: delete the data, restore from the dashboard"
post /apps/web/stop >/dev/null
rm -f "$DATA_ROOT"/apps/web/data/app.db*
post /apps/web/start >/dev/null
wait_for 20 site_up
check "data is gone (counter restarted)" test "$(visits)" -le 2
PID_BEFORE="$(app_pid web)"
post /apps/web/restore --data "id=$BACKUP1" >/dev/null
check "restore reported success" flash_has "Backup #$BACKUP1 restored (app.db)" /apps/web
check "app runs again" wait_for 20 site_up
check "app was restarted" test "$(app_pid web)" != "$PID_BEFORE"
check "visits are back to the backup's value" test "$(visits)" = "$SAVED"
check "previous database kept in .pre-restore-*" bash -c "ls $DATA_ROOT/apps/web/data/.pre-restore-*/app.db"
check "restored file belongs to the app user" test "$(stat -c %U "$DATA_ROOT/apps/web/data/app.db")" = dootd-web

say "Restore from the bucket when the local copy is gone"
rm -f "$DATA_ROOT"/backups/local/web/*
post /apps/web/restore --data "id=$BACKUP1" >/dev/null
check "restore from the bucket works" flash_has "Backup #$BACKUP1 restored" /apps/web
check "visits back to the backup's value again" test "$(visits)" = "$SAVED"

say "A tampered backup is refused and the app is not touched"
KEY_FILE="$(objects web | head -n1)"
TAMPER_ID="$(db "SELECT id FROM backups WHERE app='web' AND object_key = 'web/$(basename "$KEY_FILE")'")"
# Flip bytes in the middle, keeping the size, like silent corruption would.
python3 -c "import sys; p=sys.argv[1]; b=bytearray(open(p,'rb').read()); m=len(b)//2; b[m:m+8]=bytes(x^0xff for x in b[m:m+8]); open(p,'wb').write(b)" "$KEY_FILE"
sleep 2
rm -f "$DATA_ROOT"/backups/local/web/*
PID_BEFORE="$(app_pid web)"
post /apps/web/restore --data "id=$TAMPER_ID" >/dev/null
check "restore of the tampered backup fails, naming the checksum" flash_has "checksum does not match" /apps/web
check "app kept running (same process)" test "$(app_pid web)" = "$PID_BEFORE"
rm -f "$KEY_FILE"

say "Scheduled backups and retention"
old="$S3DIR/$BUCKET/web"
cp "$(objects web | tail -n1)" "$old/20200101T000000Z-scheduled-1.tar.zst"
cp "$(objects web | tail -n1)" "$old/20200102T000000Z-scheduled-2.tar.zst"
sleep 2
check "a scheduled backup ran (test interval 30s)" wait_for 45 any_is scheduled
check "objects older than the retention were deleted" wait_for 45 test ! -e "$old/20200101T000000Z-scheduled-1.tar.zst"
check "the other old one too" test ! -e "$old/20200102T000000Z-scheduled-2.tar.zst"
check "at most 2 local copies kept" test "$(ls $DATA_ROOT/backups/local/web | wc -l)" -le 2

say "Bucket outage: upload retried later, badge meanwhile"
systemctl stop s3mock
backup_now
check "backup still taken locally" flash_has "was taken but not uploaded" /apps/web
OUTAGE_ID="$(latest_id)"
check "not uploaded while the bucket is down" not_uploaded "$OUTAGE_ID"
check "dashboard shows the problem" wait_for 20 page_has / "backup failed"
s3_start
check "the upload is retried once the bucket is back" wait_for 90 uploaded "$OUTAGE_ID"
check "badge cleared" wait_for 60 page_lacks / "backup failed"

say "A new app starts from the bucket folder with its name (a new server)"
backup_now
SAVED="$(visits)"   # the backup holds SAVED-1; the next visit after restoring shows SAVED
post /apps/web/delete --data confirm=web >/dev/null
check "app deleted, backups kept" flash_has "Its backups were kept" /
check "its bucket folder is still there" has_objects web 1
check "Add app lists the folder" page_has /apps/new ">backup folder web/ (newest backup)<"
create_app web --data domain=$APP_HOST --data memory=128M --data restore_from=@auto >/dev/null
check "restored from its folder when created" flash_has "Its data was restored from the newest backup in web/ (app.db)" /apps/web
check "restored database belongs to the new app user" test "$(stat -c %U "$DATA_ROOT/apps/web/data/app.db")" = dootd-web
check "first deploy succeeds" deploy_is succeeded web v2
check "visits continue from the backup" test "$(visits)" = "$SAVED"

say "A renamed repository picks the old folder"
mkrepo blog
publish blog v1 "$DIST"
create_app blog --data memory=128M --data restore_from=web >/dev/null
check "restored from the chosen folder" flash_has "restored from the newest backup in web/" /apps/blog
check "blog has the database" test "$(stat -c %U "$DATA_ROOT/apps/blog/data/app.db")" = dootd-blog
check "deploy blog" deploy_is succeeded blog v1
backup_now_blog() { post /apps/blog/backup >/dev/null; }
backup_now_blog
check "blog's own backups go to its own folder" has_objects blog 1
mkrepo empty
create_app empty --data memory=128M --data restore_from= >/dev/null
check "'nothing' starts empty" test -z "$(ls -A "$DATA_ROOT/apps/empty/data")"

say "Delete an app together with its backups"
post /apps/blog/delete --data confirm=blog --data keep_data=1 --data delete_backups=1 >/dev/null
check "flash confirms backups were deleted" flash_has "Its backups were deleted" /
check "no objects left for the app" test "$(nobjects blog)" -eq 0
check "no local copies left" test ! -e "$DATA_ROOT/backups/local/blog"
check "other apps' backups untouched" has_objects web 1

e2e_result
