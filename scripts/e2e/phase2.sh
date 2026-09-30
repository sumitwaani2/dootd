#!/usr/bin/env bash
# Phase 2 end-to-end check on a real Ubuntu 24.04 host (GitHub Actions runner
# or a test VPS). Must run as root. Deploys apps from GitHub releases (fake
# GitHub API; sample-c's release made by its own workflow) and verifies:
# app names from repo names, private repos and the GitHub token, the Deploy
# menu, download + checksum + unpack, root-owned releases, every broken
# release (missing asset, missing checksums, wrong checksum, wrong CPU,
# contract 1, missing binary, unsafe tarball, draft) never touching the
# running app, automatic rollback of an unhealthy release, pruning to 3,
# rollback without download, deploying an older tag again, data surviving
# deploys, restarts, the deploy queue and a restart mid-download.
#
#   sudo ./scripts/e2e/phase2.sh
cd "$(dirname "$0")/../.."
. scripts/e2e/lib.sh

healthy() { curl -fsS -m 2 "http://127.0.0.1:$1/healthz" >/dev/null; }
# Every successful GET / on sample-c counts one visit in its SQLite DB, so
# the expected counter is tracked here (GETS) to prove data persistence.
GETS=0
lget()      { curl -fsS -m 5 "http://127.0.0.1:$1/" > "$OUT/page.html" || return 1; if [ "$1" = "${PC:-}" ]; then GETS=$((GETS + 1)); fi; }
lpage_has() { lget "$1" && grep -qF -- "$2" "$OUT/page.html"; }
visits_ok() { lget "$PC" && grep -qF -- "Visits: $GETS<" "$OUT/page.html"; }
nreleases() { find "$DATA_ROOT/apps/$1/releases" -mindepth 1 -maxdepth 1 -type d | wc -l; }
app_pid()   { pgrep -u "dootd-$1" -x sample-c | head -n1; }
no_procs()  { ! pgrep -u "dootd-$1" >/dev/null; }
not_in_file() { ! grep -aqF -- "$2" "$1" 2>/dev/null; }
dep_status() { db "SELECT status FROM deployments WHERE id = $1"; }
dep_is()     { [ "$(dep_status "$1")" = "$2" ]; }
no_downloads() { [ -z "$(ls -A "$DATA_ROOT/downloads/sample-c" 2>/dev/null)" ]; }
options()   { get /apps/sample-c | grep -c '<option value="'; }
# variant <name> <python>: a copy of sample-c's release files in $E2E/<name>,
# changed by a Python snippet (cwd = the copy; unpacked amd64 tree in ./x).
variant() {
  local d="$E2E/$1"; rm -rf "$d"; cp -r "$DIST" "$d"
  (cd "$d" && mkdir x && tar -xzf app-linux-amd64.tar.gz -C x && python3 -c "$2" && rm -rf x)
}
repack() { printf 'import subprocess; subprocess.run(["tar","-czf","app-linux-amd64.tar.gz","-C","x","."], check=True)'; }
# broken <tag> <expected error>: the release fails, the app keeps serving.
broken() {
  local pid; pid="$(app_pid sample-c)"
  deploy_is failed sample-c "$1" && last_has "$2" && lpage_has "$PC" "Release: $R_OK<" && [ "$(app_pid sample-c)" = "$pid" ] &&
    [ ! -e "$DATA_ROOT/apps/sample-c/releases/$1" ] && no_downloads
}

e2e_prepare
e2e_setup

say "sample-c's release workflow"
check "tests, two architectures, package, smoke test" sample_dist sample-c
DIST="$E2E/dist-sample-c"
mkrepo sample-c
publish sample-c v1 "$DIST"

say "Add apps: the name comes from the repository"
check "sample-c created" post_is "303 $DASH/apps/sample-c" /apps --data-urlencode "repo=https://github.com/e2e/sample-c" --data memory=64M
check "a repository whose name is not a valid app name is refused" post_is 422 /apps --data-urlencode "repo=https://github.com/e2e/9lives"
check "the form explains why" grep -q 'cannot be used as an app name' "$OUT/post.html"
check "only GitHub repositories" post_is 422 /apps --data-urlencode "repo=https://gitlab.com/e2e/x"
check "the same repository twice is refused" post_is 422 /apps --data-urlencode "repo=https://github.com/e2e/sample-c"
check "the form names the existing app" grep -q 'already exists' "$OUT/post.html"
PC="$(port_of sample-c)"
check "sample-c got a port" test -n "$PC"
check "sample-c page: not deployed yet" page_has /apps/sample-c "not deployed yet"
check "the Deploy menu offers v1 as the latest" page_has /apps/sample-c '<option value="v1" selected>v1 — Release v1 (latest)'
post /apps/sample-c/start >/dev/null
check "starting an undeployed app is refused" flash_has "no release yet" /apps/sample-c

say "A private repository needs the GitHub token"
mkrepo secret private
echo_dist "$E2E/echo-secret"
publish secret v1 "$E2E/echo-secret"
create_app secret >/dev/null
check "without a token the releases cannot be listed" page_has /apps/secret "Could not list the releases"
check "and the deploy fails, naming the token" deploy_is failed secret v1
check "  error mentions the GitHub token" last_has "set a GitHub token"
refresh_csrf /settings
post /settings/github-token --data-urlencode "token=$GH_TOKEN" >/dev/null
check "GitHub token saved" flash_has "It belongs to e2e" /settings
check "token not stored in plain text" bash -c "! grep -aqF '$GH_TOKEN' $DATA_ROOT/dootd.db $DATA_ROOT/dootd.db-wal 2>/dev/null"
check "the private repo's releases are listed now" page_has /apps/secret '<option value="v1" selected>'
check "Add app suggests the token's repositories" page_has /apps/new 'value="https://github.com/e2e/secret"'
check "the private release deploys" deploy_is succeeded secret v1
post /apps/secret/stop >/dev/null

say "First deploy: download, verify, unpack"
check "deploy v1 succeeds" deploy_is succeeded sample-c v1
R_OK=v1
check "log shows the download" last_has "downloading app-linux-amd64.tar.gz"
check "log shows the checksum" last_has "matches checksums.txt"
check "log shows dootd.toml" last_has 'dootd.toml: run "build/sample-c"'
check "sample-c healthy" wait_for 10 healthy "$PC"
check "page shows release v1" lpage_has "$PC" "Release: v1<"
check "visit counter works" visits_ok
check "release is owned by root" test "$(stat -c %U "$DATA_ROOT/apps/sample-c/releases/v1/build/sample-c")" = root
check "app user cannot modify its release" bash -c "! sudo -u dootd-sample-c touch '$DATA_ROOT/apps/sample-c/releases/v1/x' 2>/dev/null"
check "no download left behind" no_downloads
check "no build tooling on the server" bash -c "[ ! -e $DATA_ROOT/toolchains ] && [ ! -e $DATA_ROOT/cache ] && ! ls /sys/fs/cgroup/system.slice/dootd.service/builds >/dev/null 2>&1"
SUM="$(grep app-linux-amd64 "$DIST/checksums.txt" | cut -c1-10)"
check "kept releases list v1 with its checksum" page_has /apps/sample-c "<code>$SUM</code>"

say "Broken releases never touch the running app"
variant noasset 'import os; os.remove("app-linux-amd64.tar.gz")'
publish sample-c v2-noasset "$E2E/noasset"
check "missing asset for this CPU" broken v2-noasset "has no app-linux-amd64.tar.gz for this server (it has: app-linux-arm64.tar.gz, checksums.txt)"
variant nosums 'import os; os.remove("checksums.txt")'
publish sample-c v2-nosums "$E2E/nosums"
check "missing checksums.txt" broken v2-nosums "has no checksums.txt"
variant badsum 'open("checksums.txt","w").write("0"*64+"  app-linux-amd64.tar.gz\n")'
publish sample-c v2-badsum "$E2E/badsum"
check "checksum mismatch" broken v2-badsum "checksum mismatch for app-linux-amd64.tar.gz"
variant arm 'import shutil; shutil.copy("app-linux-arm64.tar.gz","app-linux-amd64.tar.gz")'
resum "$E2E/arm"
publish sample-c v2-arm "$E2E/arm"
check "binary for the wrong CPU" broken v2-arm "is built for arm64, but this server is amd64"
variant contract1 "open('x/dootd.toml','w').write('contract = 1\nzig_version = \"0.16.0\"\nrun = \"build/sample-c\"\n'); $(repack)"
resum "$E2E/contract1"
publish sample-c v2-contract1 "$E2E/contract1"
check "contract 1 manifest explained" broken v2-contract1 "contract = 1 (dootd builds the app) is no longer supported"
variant nobin "import os; os.remove('x/build/sample-c'); $(repack)"
resum "$E2E/nobin"
publish sample-c v2-nobin "$E2E/nobin"
check "binary missing from the tarball" broken v2-nobin "run: build/sample-c is not in the release tarball"
variant evil "import tarfile, io
t = tarfile.open('app-linux-amd64.tar.gz', 'w:gz')
i = tarfile.TarInfo('../../evil'); i.size = 2; t.addfile(i, io.BytesIO(b'hi')); t.close()"
resum "$E2E/evil"
publish sample-c v2-evil "$E2E/evil"
check "unsafe tarball refused" broken v2-evil 'unsafe path "../../evil"'
check "  nothing written outside" bash -c "[ ! -e $DATA_ROOT/downloads/evil ] && [ ! -e $DATA_ROOT/evil ]"
publish sample-c v2-draft "$DIST"; touch "$RELDIR/draft"
check "a draft cannot be deployed" broken v2-draft "has no release v2-draft"
check "an unsupported tag is refused" refused sample-c "v1/../x"

say "Unhealthy release: automatic rollback"
variant unhealthy "open('x/dootd.toml','w').write('contract = 2\nrun = \"build/sample-c\"\nhealth_path = \"/does-not-exist\"\n'); $(repack)"
resum "$E2E/unhealthy"
publish sample-c v2-unhealthy "$E2E/unhealthy"
check "deploy fails" deploy_is failed sample-c v2-unhealthy
check "reported as rolled back to v1" last_has "rolled back: release v1 is serving again"
check "v1 serving again" wait_for 10 lpage_has "$PC" "Release: v1<"
check "data survived the failed deploy" visits_ok
check "failed release was removed" test "$(nreleases sample-c)" -eq 1

say "Successful deploys, data persistence and pruning to 3 releases"
for v in v2 v3 v4; do
  publish sample-c "$v" "$DIST"
  check "deploy $v succeeds" deploy_is succeeded sample-c "$v"
  check "serving $v" lpage_has "$PC" "Release: $v<"
  R_OK="$v"
done
check "visits persisted across deploys" visits_ok
check "3 releases kept" test "$(nreleases sample-c)" -eq 3
check "oldest release v1 pruned" test ! -e "$DATA_ROOT/apps/sample-c/releases/v1"
check "the Deploy menu offers the 10 newest releases" test "$(options)" = 10
check "... with the newest preselected as the latest" page_has /apps/sample-c '<option value="v4" selected>v4 — Release v4 (latest)'
rollbacks_offered() { page_has /apps/sample-c 'name="release" value="v2"' && page_has /apps/sample-c 'name="release" value="v3"'; }
check "kept releases offer rollback to v2 and v3" rollbacks_offered

say "Rollback (no download) and deploying an older tag again"
check "rollback to v2 succeeds" rollback_is succeeded sample-c v2
check "rollback did not download" bash -c "! grep -qF 'downloading' $LAST"
check "serving v2 again" lpage_has "$PC" "Release: v2<"
current_refused() { rollback sample-c v2; [ "$STATUS" = refused ]; }
check "rolling back to the current release is refused" current_refused
check "deploying a kept tag needs no download" deploy_is succeeded sample-c v3
check "  said so" last_has "still kept on this server"
check "deploying the running tag is refused" refused sample-c v3
check "the pruned v1 downloads again" deploy_is succeeded sample-c v1
check "  downloaded" last_has "downloading app-linux-amd64.tar.gz"
check "serving v1" lpage_has "$PC" "Release: v1<"
check "still 3 releases kept" test "$(nreleases sample-c)" -eq 3
check "visits persisted" visits_ok

say "dootd restart keeps the current release and desired state"
systemctl restart "$UNIT"
check "sample-c healthy after restart" wait_for 30 healthy "$PC"
check "still on v1 after restart" lpage_has "$PC" "Release: v1<"
check "visits persisted across dootd restart" visits_ok
wait_for 30 signed_in
post /apps/sample-c/stop >/dev/null
systemctl restart "$UNIT"
sleep 3
check "a stopped app stays stopped after a restart" no_procs sample-c
wait_for 30 signed_in
post /apps/sample-c/start >/dev/null
check "start brings it back" wait_for 30 healthy "$PC"

say "Deploy queue: one at a time, no duplicates"
publish sample-c v5 "$DIST"; echo 20 > "$RELDIR/app-linux-amd64.tar.gz.delay"
publish secret v2 "$E2E/echo-secret"
SLOW="$(post /apps/sample-c/deploy --data tag=v5)"; SID="${SLOW##*/deployments/}"
QLOC="$(post /apps/secret/deploy --data tag=v2)"; QID="${QLOC##*/deployments/}"
check "second deployment is queued" dep_is "$QID" queued
post /apps/sample-c/deploy --data tag=v5 >/dev/null
check "a second deploy of the same app is refused" flash_has "still in progress" /apps/sample-c
check "the slow download finishes" wait_for 90 dep_is "$SID" succeeded
check "then the queued deploy runs" wait_for 60 dep_is "$QID" succeeded

say "dootd restart during a download"
RBEFORE="$(current sample-c)"
publish sample-c v6 "$DIST"; echo 60 > "$RELDIR/app-linux-amd64.tar.gz.delay"
ILOC="$(post /apps/sample-c/deploy --data tag=v6)"
IID="${ILOC##*/deployments/}"
sleep 3
systemctl restart "$UNIT"
check "app back on the previous release" wait_for 30 lpage_has "$PC" "Release: $RBEFORE<"
check "interrupted deployment marked failed" wait_for 10 dep_is "$IID" failed
check "no download left behind" wait_for 10 no_downloads
check "no v6 release directory" test ! -e "$DATA_ROOT/apps/sample-c/releases/v6"

if [ "$FAILS" -gt 0 ]; then echo "---- sample-c app.log ----"; tail -n 60 "$DATA_ROOT/apps/sample-c/logs/app.log" || true; fi
e2e_result
