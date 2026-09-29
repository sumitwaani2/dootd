#!/usr/bin/env bash
# Phase 2 end-to-end check on a real Ubuntu 24.04 host (GitHub Actions runner
# or a test VPS). Must run as root. Deploys apps from git with the dashboard
# and verifies: app names from repo names, pinned Zig download, build as the
# app user, immutable releases, broken builds and unhealthy releases never
# take the app down, automatic + manual rollback, release pruning, data
# surviving deploys, build timeouts, manifest errors, the deploy queue,
# restarts mid-deploy.
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
dep_status() { db "SELECT status FROM deployments WHERE id = $1"; }
branch_repo() { # branch_repo <name> <branch>: push a branch of sample-c into its own repo <name>
  git init -q --bare -b main "$GIT/$1.git"
  git -C "$WORKS/sample-c" push -q "file://$GIT/$1.git" "$2:main"
}

e2e_prepare
e2e_setup

say "Local git repos"
mkrepo sample-c examples/sample-c
W="$WORKS/sample-c"
git -C "$W" checkout -qb slow
printf 'contract = 1\nzig_version = "0.16.0"\nbuild = "id -un; sleep 120"\nrun = "build/sample-c"\n' > "$W/dootd.toml"
commit sample-c "slow build"
git -C "$W" checkout -q main && git -C "$W" checkout -qb bad-manifest
printf 'contract = 1\nrun = "../outside"\ntypo = true\n' > "$W/dootd.toml"
commit sample-c "broken manifest"
git -C "$W" checkout -q main && git -C "$W" checkout -qb no-zig
sed -i 's/zig_version *= *"[^"]*"/zig_version = "0.0.99"/' "$W/dootd.toml"
commit sample-c "unknown zig"
git -C "$W" checkout -q main
branch_repo slow slow
branch_repo badmanifest bad-manifest
branch_repo nozig no-zig

say "Add apps: the name comes from the repository"
check "sample-c created" post_is "303 $DASH/apps/sample-c" /apps --data type=c --data-urlencode "repo=file://$GIT/sample-c.git" --data branch=main --data memory=64M
check "a repository whose name is not a valid app name is refused" post_is 422 /apps --data type=c --data-urlencode "repo=file://$GIT/9lives.git" --data branch=main
check "the form explains why" grep -q 'cannot be used as an app name' "$OUT/post.html"
check "the same repository twice is refused" post_is 422 /apps --data type=c --data-urlencode "repo=file://$GIT/sample-c.git" --data branch=slow
check "the form names the existing app" grep -q 'already exists' "$OUT/post.html"
create_app slow --data build_timeout=10s >/dev/null
create_app badmanifest >/dev/null
create_app nozig >/dev/null
PC="$(port_of sample-c)"
check "sample-c got a port" test -n "$PC"
check "sample-c page: not deployed yet" page_has /apps/sample-c "not deployed yet"
post /apps/sample-c/start >/dev/null
check "starting an undeployed app is refused" flash_has "no release yet" /apps/sample-c

say "First deploy: dootd downloads the pinned Zig and builds as the app user"
check "deploy sample-c succeeds" deploy_is succeeded sample-c
R1="$(current sample-c)"
echo "  release R1 = $R1"
check "zig 0.16.0 installed by dootd" test -x "$DATA_ROOT/toolchains/zig/0.16.0/zig"
check "build log shows the download" last_has "downloading zig 0.16.0"
check "build log shows the build command" last_has '$ make'
check "sample-c healthy" wait_for 10 healthy "$PC"
check "page shows release R1" lpage_has "$PC" "Release: $R1<"
check "visit counter works" visits_ok
check "release is owned by root" test "$(stat -c %U "$DATA_ROOT/apps/sample-c/releases/$R1/build/sample-c")" = root
check "app user cannot modify its release" bash -c "! sudo -u dootd-sample-c touch '$DATA_ROOT/apps/sample-c/releases/$R1/x' 2>/dev/null"
check "zig cache owned by the app user" test "$(stat -c %U "$DATA_ROOT/cache/zig/sample-c/global")" = dootd-sample-c
check "no .git kept in the release" test ! -e "$DATA_ROOT/apps/sample-c/releases/$R1/.git"
check "build workspace cleaned up" test -z "$(ls -A "$DATA_ROOT/builds/sample-c")"

say "Broken build: the running release is untouched (no downtime)"
PID1="$(app_pid sample-c)"
echo '#error "intentionally broken"' >> "$W/src/main.c"
commit sample-c "break the build"
check "deploy fails" deploy_is failed sample-c
check "error mentions the build exit code" last_has "build failed with exit code"
check "still serving R1" lpage_has "$PC" "Release: $R1<"
check "same process kept running" test "$(app_pid sample-c)" = "$PID1"
git -C "$W" revert --no-edit HEAD >/dev/null
git -C "$W" push -q origin HEAD

say "Unhealthy release: automatic rollback"
sed -i 's|health_path *= *"[^"]*"|health_path = "/does-not-exist"|' "$W/dootd.toml"
commit sample-c "bad health path"
check "deploy fails" deploy_is failed sample-c
check "reported as rolled back to R1" last_has "rolled back: release $R1 is serving again"
check "R1 serving again" wait_for 10 lpage_has "$PC" "Release: $R1<"
check "data survived the failed deploy" visits_ok
check "failed release was removed" test "$(nreleases sample-c)" -eq 1
git -C "$W" revert --no-edit HEAD >/dev/null
git -C "$W" push -q origin HEAD

say "Successful deploys, data persistence and pruning to 3 releases"
for v in 2 3 4; do
  set_title sample-c "sample-c v$v"
  commit sample-c "v$v"
  check "deploy v$v succeeds" deploy_is succeeded sample-c
  eval "R$v=\"\$(current sample-c)\""
  check "serving v$v" lpage_has "$PC" "sample-c v$v"
done
check "visits persisted across deploys" visits_ok
check "3 releases kept" test "$(nreleases sample-c)" -eq 3
check "oldest release R1 pruned" test ! -e "$DATA_ROOT/apps/sample-c/releases/$R1"
rollbacks_offered() { page_has /apps/sample-c "value=\"$R2\"" && page_has /apps/sample-c "value=\"$R3\""; }
check "releases list offers rollback to R2 and R3" rollbacks_offered

say "Manual rollback (no build)"
check "rollback to R2 succeeds" deploy_is succeeded sample-c "$R2"
check "rollback did not build" bash -c "! grep -qF 'cloning' $LAST"
check "serving v2 again" lpage_has "$PC" "sample-c v2"
check "current -> R2" test "$(current sample-c)" = "$R2"
check "rolling back to the current release is refused" test "$(deploy sample-c "$R2")" = refused

say "dootd restart keeps the current release and desired state"
systemctl restart "$UNIT"
check "sample-c healthy after restart" wait_for 30 healthy "$PC"
check "still on R2 after restart" lpage_has "$PC" "sample-c v2"
check "visits persisted across dootd restart" visits_ok
wait_for 30 signed_in
post /apps/sample-c/stop >/dev/null
systemctl restart "$UNIT"
sleep 3
check "a stopped app stays stopped after a restart" no_procs sample-c
wait_for 30 signed_in
post /apps/sample-c/start >/dev/null
check "start brings it back" wait_for 30 healthy "$PC"

say "Build timeout, build user, manifest and toolchain errors"
check "slow deploy fails" deploy_is failed slow
check "timeout reported" last_has "build timed out after 10s"
check "build ran as dootd-slow" last_has "dootd-slow"
check "no build process left" no_procs slow
check "bad manifest fails" deploy_is failed badmanifest
check "missing zig_version reported" last_has "zig_version is required"
check "unknown key reported" last_has 'unknown key "typo"'
check "path escape reported" last_has "must stay inside the app root"
check "unknown zig fails" deploy_is failed nozig
check "unknown zig version reported" last_has "zig version not found in the official release index: 0.0.99"

say "Deploy queue: one at a time, no duplicates"
post /apps/slow/deploy >/dev/null
QLOC="$(post /apps/sample-c/deploy)"
QID="${QLOC##*/deployments/}"
check "second deployment is queued" test "$(dep_status "$QID")" = queued
post /apps/sample-c/deploy >/dev/null
check "a second deploy of the same app is refused" flash_has "still in progress" /apps/sample-c
check "queued deploy runs after the slow one" wait_for 180 test "$(dep_status "$QID")" = succeeded

say "dootd restart during a build"
RBEFORE="$(current sample-c)"
set_title sample-c "sample-c v5"
# Builds are cached (about 1 s now), so make this one slow enough to interrupt.
grep -q '^build' "$W/dootd.toml" && sed -i 's|^build *=.*|build = "sleep 60; make"|' "$W/dootd.toml" || echo 'build = "sleep 60; make"' >> "$W/dootd.toml"
commit sample-c "v5 (slow build)"
ILOC="$(post /apps/sample-c/deploy)"
IID="${ILOC##*/deployments/}"
sleep 2
systemctl restart "$UNIT"
check "app back on the previous release" wait_for 30 lpage_has "$PC" "Release: $RBEFORE<"
check "interrupted deployment marked failed" test "$(dep_status "$IID")" = failed
check "no build workspace left" test -z "$(ls -A "$DATA_ROOT/builds/sample-c")"

if [ "$FAILS" -gt 0 ]; then echo "---- sample-c app.log ----"; tail -n 60 "$DATA_ROOT/apps/sample-c/logs/app.log" || true; fi
e2e_result
