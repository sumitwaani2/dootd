#!/usr/bin/env bash
# Phase 2 end-to-end check on a real Ubuntu 24.04 host (GitHub Actions runner
# or a test VPS). Must run as root. It runs dootd as the real systemd unit
# and deploys apps from git with `dootd ctl`, verifying:
#   pinned Zig download, build as the app user, immutable releases,
#   broken builds and unhealthy releases never take the app down,
#   automatic + manual rollback, release pruning, data surviving deploys,
#   build timeouts, manifest errors, the deploy queue, restarts mid-deploy,
#   and (when E2E_GITHUB_BRANCH is set) a private GitHub clone with a token.
#
#   sudo ./scripts/e2e/phase2.sh
#   sudo E2E_GITHUB_REPO=owner/repo E2E_GITHUB_BRANCH=my-branch GITHUB_TOKEN=... ./scripts/e2e/phase2.sh
set -euo pipefail

UNIT="dootd"
DATA_ROOT="/var/lib/dootd"
E2E="/opt/dootd-e2e"
BARE="$E2E/git/sample-c.git"
WORK="$E2E/work/sample-c"
OUT="$E2E/out"
FAILS=0
N=0

say()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
check() { local desc="$1"; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
wait_for() { local t="$1"; shift; for _ in $(seq 1 $((t * 5))); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }

healthy()    { curl -fsS -m 2 "http://127.0.0.1:$1/healthz" >/dev/null; }
# Every successful GET / on sample-c counts one visit in its SQLite DB, so
# the expected counter is tracked here (GETS) to prove data persistence.
GETS=0
get()        { curl -fsS -m 5 "http://127.0.0.1:$1/" > "$OUT/page.html" || return 1; if [ "$1" = 20002 ]; then GETS=$((GETS + 1)); fi; }
page_has()   { get "$1" && grep -qF -- "$2" "$OUT/page.html"; }
visits_ok()  { get 20002 && grep -qF -- "Visits: $GETS<" "$OUT/page.html"; }
start_refused() { local out; out="$(dootd ctl start sample-c 2>&1)" && return 1; [[ "$out" == *"no release yet"* ]]; }
current()    { basename "$(readlink "$DATA_ROOT/apps/$1/current")"; }
nreleases()  { find "$DATA_ROOT/apps/$1/releases" -mindepth 1 -maxdepth 1 -type d | wc -l; }
app_pid()    { pgrep -u "dootd-$1" -x "$1" | head -n1; }
no_procs()   { ! pgrep -u "dootd-$1" >/dev/null; }
file_has()   { grep -qF -- "$2" "$1"; }
not_in_file() { ! grep -aqF -- "$2" "$1" 2>/dev/null; }
ctl()        { dootd ctl "$@"; }

# deploy <expect ok|fail> <app> [ctl args...]: runs a deploy, keeps its output in $LAST
deploy() {
  local expect="$1"; shift
  N=$((N + 1)); LAST="$OUT/deploy-$N.log"
  local rc=0
  ctl deploy "$@" >"$LAST" 2>&1 || rc=$?
  if { [ "$expect" = ok ] && [ $rc -eq 0 ]; } || { [ "$expect" = fail ] && [ $rc -ne 0 ]; }; then return 0; fi
  echo "   --- unexpected result (rc=$rc), output:"; tail -n 40 "$LAST" | sed 's/^/   | /'
  return 1
}
last_has() { grep -qF -- "$1" "$LAST"; }

commit() { # commit <message>  (in $WORK, pushes the current branch)
  git -C "$WORK" add -A && git -C "$WORK" commit -qm "$1" && git -C "$WORK" push -q origin HEAD
}
set_title() { sed -i "s|<h1>[^<]*</h1>|<h1>$1</h1>|" "$WORK/src/main.c"; }

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
cd "$(dirname "$0")/../.."
echo "kernel $(uname -r), $(. /etc/os-release && echo "$PRETTY_NAME")"

say "Build and install dootd"
make build
systemctl stop "$UNIT" 2>/dev/null || true
install -m 0755 dist/dootd /usr/local/bin/dootd
rm -rf "$E2E" "$DATA_ROOT" && mkdir -p "$E2E/git" "$E2E/work" "$OUT"

say "Create a local git repo for sample-c (main + test branches)"
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com
git init -q --bare -b main "$BARE"
cp -r examples/sample-c "$WORK" && rm -rf "$WORK/build" "$WORK/third_party"
git -C "$WORK" init -q -b main && git -C "$WORK" remote add origin "file://$BARE"
commit "initial sample-c"
git -C "$WORK" checkout -qb slow
printf 'contract = 1\nzig_version = "0.16.0"\nbuild = "id -un; sleep 120"\nrun = "build/sample-c"\n' > "$WORK/dootd.toml"
commit "slow build"
git -C "$WORK" checkout -q main && git -C "$WORK" checkout -qb bad-manifest
printf 'contract = 1\nrun = "../outside"\ntypo = true\n' > "$WORK/dootd.toml"
commit "broken manifest"
git -C "$WORK" checkout -q main && git -C "$WORK" checkout -qb no-zig
sed -i 's/zig_version *= *"[^"]*"/zig_version = "0.0.99"/' "$WORK/dootd.toml"
commit "unknown zig"
git -C "$WORK" checkout -q main

say "Install systemd unit with dev-apps"
mkdir -p /etc/dootd /etc/systemd/system/dootd.service.d
cat > /etc/dootd/dev-apps.toml <<EOF
[[app]]
name   = "sample-c"
type   = "c"
port   = 20002
repo   = "file://$BARE"
memory = "64M"

[[app]]
name          = "slow"
type          = "c"
port          = 20003
repo          = "file://$BARE"
branch        = "slow"
build_timeout = "5s"

[[app]]
name   = "badmanifest"
type   = "c"
port   = 20004
repo   = "file://$BARE"
branch = "bad-manifest"

[[app]]
name   = "nozig"
type   = "c"
port   = 20005
repo   = "file://$BARE"
branch = "no-zig"
EOF
if [ -n "${E2E_GITHUB_BRANCH:-}" ]; then
  cat >> /etc/dootd/dev-apps.toml <<EOF

[[app]]
name   = "gh-zig"
type   = "zig"
port   = 20001
repo   = "https://github.com/${E2E_GITHUB_REPO}"
branch = "${E2E_GITHUB_BRANCH}"
path   = "examples/sample-zig"
EOF
fi
install -m 0644 contrib/systemd/dootd.service /etc/systemd/system/dootd.service
cat > /etc/systemd/system/dootd.service.d/dev.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/bin/dootd serve --dev-apps /etc/dootd/dev-apps.toml
EOF
systemctl daemon-reload
systemctl start "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock

say "Before the first deploy"
ctl status | tee "$OUT/status0.txt"
check "sample-c has no release yet" grep -Eq '^sample-c +stopped .* - ' "$OUT/status0.txt"
check "starting an undeployed app is refused" start_refused

if [ -n "${E2E_GITHUB_BRANCH:-}" ]; then
  say "GitHub token is stored encrypted"
  printf '%s' "$GITHUB_TOKEN" | ctl github-token | tee "$OUT/token.txt"
  check "token saved" file_has "$OUT/token.txt" "saved (encrypted)"
  check "token not in dootd.db in plain text" not_in_file "$DATA_ROOT/dootd.db" "$GITHUB_TOKEN"
  check "token not in the WAL in plain text" not_in_file "$DATA_ROOT/dootd.db-wal" "$GITHUB_TOKEN"
fi

say "First deploy: dootd downloads the pinned Zig and builds as the app user"
check "deploy sample-c succeeds" deploy ok sample-c
R1="$(current sample-c)"
echo "  release R1 = $R1"
check "zig 0.16.0 installed by dootd" test -x "$DATA_ROOT/toolchains/zig/0.16.0/zig"
check "build log shows the download" last_has "downloading zig 0.16.0"
check "build log shows the build command" last_has '$ make'
check "sample-c healthy" wait_for 10 healthy 20002
check "page shows release R1" page_has 20002 "Release: $R1<"
check "visit counter works" visits_ok
check "release is owned by root" test "$(stat -c %U "$DATA_ROOT/apps/sample-c/releases/$R1/build/sample-c")" = root
check "app user cannot modify its release" bash -c "! sudo -u dootd-sample-c touch '$DATA_ROOT/apps/sample-c/releases/$R1/x' 2>/dev/null"
check "zig cache owned by the app user" test "$(stat -c %U "$DATA_ROOT/cache/zig/sample-c/global")" = dootd-sample-c
check "no .git kept in the release" test ! -e "$DATA_ROOT/apps/sample-c/releases/$R1/.git"
check "build workspace cleaned up" test -z "$(ls -A "$DATA_ROOT/builds/sample-c")"

if [ -n "${E2E_GITHUB_BRANCH:-}" ]; then
  say "Deploy from a private GitHub repo (subdirectory, token auth)"
  check "deploy gh-zig succeeds" deploy ok gh-zig
  check "gh-zig healthy" wait_for 10 healthy 20001
  check "gh-zig serves the Zig app" page_has 20001 "Hello from Zig on dootd"
fi

say "Broken build: the running release is untouched (no downtime)"
PID1="$(app_pid sample-c)"
echo '#error "intentionally broken"' >> "$WORK/src/main.c"
commit "break the build"
check "deploy fails" deploy fail sample-c
check "error mentions the build exit code" last_has "build failed with exit code"
check "still serving R1" page_has 20002 "Release: $R1<"
check "same process kept running" test "$(app_pid sample-c)" = "$PID1"
git -C "$WORK" revert --no-edit HEAD >/dev/null
  git -C "$WORK" push -q origin HEAD

say "Unhealthy release: automatic rollback"
sed -i 's|health_path *= *"[^"]*"|health_path = "/does-not-exist"|' "$WORK/dootd.toml"
commit "bad health path"
check "deploy fails" deploy fail sample-c
check "reported as rolled back to R1" last_has "rolled back to release $R1"
check "R1 serving again" wait_for 10 page_has 20002 "Release: $R1<"
check "data survived the failed deploy" visits_ok
check "failed release was removed" test "$(nreleases sample-c)" -eq 1
git -C "$WORK" revert --no-edit HEAD >/dev/null
  git -C "$WORK" push -q origin HEAD

say "Successful deploys, data persistence and pruning to 3 releases"
for v in 2 3 4; do
  set_title "sample-c v$v"
  commit "v$v"
  check "deploy v$v succeeds" deploy ok sample-c
  eval "R$v=\"\$(current sample-c)\""
  check "serving v$v" page_has 20002 "sample-c v$v"
done
check "visits persisted across deploys" visits_ok
check "3 releases kept" test "$(nreleases sample-c)" -eq 3
check "oldest release R1 pruned" test ! -e "$DATA_ROOT/apps/sample-c/releases/$R1"
ctl releases sample-c | tee "$OUT/releases.txt"
check "releases list marks R4 current" grep -Eq "^$R4 +\*" "$OUT/releases.txt"

say "Manual rollback (no build)"
N=$((N + 1)); LAST="$OUT/rollback.log"
check "rollback to R2 succeeds" bash -c "dootd ctl rollback sample-c $R2 > $LAST 2>&1"
check "rollback did not build" bash -c "! grep -qF 'cloning' $LAST"
check "serving v2 again" page_has 20002 "sample-c v2"
check "current -> R2" test "$(current sample-c)" = "$R2"
check "rolling back to the current release is refused" bash -c "! dootd ctl rollback sample-c $R2 >/dev/null 2>&1"

say "dootd restart keeps the current release and desired state"
systemctl restart "$UNIT"
check "sample-c healthy after restart" wait_for 30 healthy 20002
check "still on R2 after restart" page_has 20002 "sample-c v2"
check "visits persisted across dootd restart" visits_ok
ctl stop sample-c >/dev/null
systemctl restart "$UNIT"
sleep 3
check "a stopped app stays stopped after a restart" no_procs sample-c
ctl start sample-c >/dev/null
check "start brings it back" wait_for 30 healthy 20002

say "Build timeout, build user, manifest and toolchain errors"
check "slow deploy fails" deploy fail slow
check "timeout reported" last_has "build timed out after 5s"
check "build ran as dootd-slow" last_has "dootd-slow"
check "no build process left" no_procs slow
check "bad manifest fails" deploy fail badmanifest
check "missing zig_version reported" last_has "zig_version is required"
check "unknown key reported" last_has 'unknown key "typo"'
check "path escape reported" last_has "must stay inside the app root"
check "unknown zig fails" deploy fail nozig
check "unknown zig version reported" last_has "zig version not found in the official release index: 0.0.99"

say "Deploy queue: one at a time, no duplicates"
ctl deploy slow --detach >/dev/null
ctl deploy sample-c --detach > "$OUT/queued.txt"
QID="$(grep -oE '#[0-9]+' "$OUT/queued.txt" | tr -d '#')"
check "second deploy of the same app is refused" bash -c "dootd ctl deploy sample-c --detach 2>&1 | grep -q 'still in progress'"
queued_done() { ctl deployments sample-c | grep -Eq "^$QID +deploy +succeeded"; }
check "queued deploy runs after the slow one" wait_for 180 queued_done

say "dootd restart during a build"
RBEFORE="$(current sample-c)"
set_title "sample-c v5"
commit "v5"
ctl deploy sample-c --detach > "$OUT/interrupted.txt"
IID="$(grep -oE '#[0-9]+' "$OUT/interrupted.txt" | tr -d '#')"
sleep 2
systemctl restart "$UNIT"
check "app back on the previous release" wait_for 30 page_has 20002 "Release: $RBEFORE<"
check "interrupted deployment marked failed" bash -c "dootd ctl deployments sample-c | grep -Eq '^$IID +deploy +failed'"
check "no build workspace left" test -z "$(ls -A "$DATA_ROOT/builds/sample-c")"
ctl status
systemctl stop "$UNIT"

say "Result"
if [ "$FAILS" -gt 0 ]; then
  echo "---- journal ----"; journalctl -u "$UNIT" --no-pager -n 150 || true
  echo "---- sample-c app.log ----"; tail -n 60 "$DATA_ROOT/apps/sample-c/logs/app.log" || true
  echo "$FAILS check(s) failed"; exit 1
fi
echo "all checks passed"
